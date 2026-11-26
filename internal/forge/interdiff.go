package forge

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/cooperspencer/forge/internal/gitx"
	"github.com/cooperspencer/forge/internal/store"
)

// Pull request revisions and interdiffs. Every head a PR gets is recorded
// (and kept reachable as refs/pull/N/rev/K), so a reviewer can see what
// changed between two pushes even when history was rewritten — the normal
// case with jj, which amends and rebases changes instead of adding commits.
// Commits are matched across revisions by their jj change ID, or by an
// identical patch (git patch-id) for plain git.

func revRef(pr int64, n int) string { return fmt.Sprintf("refs/pull/%d/rev/%d", pr, n) }

func (f *Forge) recordRevision(ctx context.Context, base *store.Repo, pr *store.Issue, sha string, actorID int64) {
	g := f.Git(base)
	mb, err := g.MergeBase(ctx, "refs/heads/"+pr.PR.BaseBranch, sha)
	if err != nil {
		return
	}
	n, err := f.Store.AddPRRevision(ctx, pr.ID, sha, mb, actorID)
	if err != nil || n == 0 {
		return
	}
	if err := g.UpdateRef(ctx, revRef(pr.Number, n), sha, ""); err != nil {
		f.Log.Error().Err(err).Msg("pr revision ref")
	}
}

// PRRevisions lists the heads a pull request has had, oldest first.
func (f *Forge) PRRevisions(ctx context.Context, actor *store.User, r *store.Repo, i *store.Issue) ([]store.PRRevision, error) {
	if f.Store.AccessLevel(ctx, actor, r) < store.AccessRead {
		return nil, ErrNotFound
	}
	if !i.IsPR {
		return nil, errors.New("not a pull request")
	}
	return f.Store.PRRevisions(ctx, i.ID)
}

// Change statuses in an interdiff.
const (
	ChangeUnchanged = "unchanged" // the same commit
	ChangeRebased   = "rebased"   // same changes and message, new parent
	ChangeReworded  = "reworded"  // same changes, new message
	ChangeRewritten = "rewritten" // the changes differ
	ChangeNew       = "new"
	ChangeDropped   = "dropped"
)

// ChangeDiff is one change (commit) between two revisions.
type ChangeDiff struct {
	ChangeID string
	Status   string
	Old, New *gitx.Commit
	Files    []gitx.FileDiff // rewritten: what changed in the change itself
	Note     string
}

// Interdiff compares two revisions of a pull request.
type Interdiff struct {
	From, To store.PRRevision
	Rebased  bool            // the base moved between the revisions
	Changes  []ChangeDiff    // To's commits oldest first, then dropped ones
	Files    []gitx.FileDiff // the whole pull request: From's changes vs To's
	Note     string
}

// PRInterdiff compares revision from with revision to (0: the latest, and
// the one before it).
func (f *Forge) PRInterdiff(ctx context.Context, actor *store.User, r *store.Repo, i *store.Issue, from, to int) (*Interdiff, error) {
	revs, err := f.PRRevisions(ctx, actor, r, i)
	if err != nil {
		return nil, err
	}
	if len(revs) < 2 {
		return nil, errors.New("this pull request has only one revision; nothing to compare yet")
	}
	if to == 0 {
		to = revs[len(revs)-1].N
	}
	if from == 0 {
		from = to - 1
	}
	if from < 1 || to > len(revs) || from >= to {
		return nil, fmt.Errorf("revisions run from 1 to %d; compare an older one with a newer one", len(revs))
	}
	g := f.Git(r)
	d := &Interdiff{From: revs[from-1], To: revs[to-1]}
	d.Rebased = d.From.BaseSHA != d.To.BaseSHA

	// the whole PR: From's changes replayed onto To's base, against To
	old := d.From.HeadSHA
	if d.Rebased {
		tree, _, err := g.MergeTrees(ctx, d.From.BaseSHA, d.To.BaseSHA, d.From.HeadSHA)
		if err == nil {
			old = tree
		} else {
			d.Note = "the base moved and the old revision doesn't apply cleanly to the new base; this diff includes changes from the base branch"
		}
	}
	if d.Files, err = g.Diff(ctx, old, d.To.HeadSHA); err != nil {
		return nil, err
	}

	oldCs, err := g.CommitsBetween(ctx, d.From.BaseSHA, d.From.HeadSHA)
	if err != nil {
		return nil, err
	}
	newCs, err := g.CommitsBetween(ctx, d.To.BaseSHA, d.To.HeadSHA)
	if err != nil {
		return nil, err
	}
	slices.Reverse(oldCs)
	slices.Reverse(newCs)
	pids := map[string]string{}
	pid := func(c *gitx.Commit) string {
		if p, ok := pids[c.SHA]; ok {
			return p
		}
		pids[c.SHA] = g.PatchID(ctx, c.SHA)
		return pids[c.SHA]
	}
	used := make([]bool, len(oldCs))
	match := func(n *gitx.Commit) int {
		for j := range oldCs {
			if !used[j] && oldCs[j].SHA == n.SHA {
				return j
			}
		}
		if n.ChangeID != "" {
			for j := range oldCs {
				if !used[j] && oldCs[j].ChangeID == n.ChangeID {
					return j
				}
			}
		}
		if p := pid(n); p != "" {
			for j := range oldCs {
				if !used[j] && pid(&oldCs[j]) == p {
					return j
				}
			}
		}
		return -1
	}
	for k := range newCs {
		n := &newCs[k]
		cd := ChangeDiff{ChangeID: n.ChangeID, New: n, Status: ChangeNew}
		if j := match(n); j >= 0 {
			used[j] = true
			o := &oldCs[j]
			cd.Old = o
			switch {
			case o.SHA == n.SHA:
				cd.Status = ChangeUnchanged
			case pid(o) != "" && pid(o) == pid(n):
				cd.Status = ChangeRebased
				if o.Subject != n.Subject || o.Body != n.Body {
					cd.Status = ChangeReworded
				}
			default:
				cd.Status = ChangeRewritten
				cd.Files, cd.Note = f.changeInterdiff(ctx, g, o, n)
			}
		}
		d.Changes = append(d.Changes, cd)
	}
	for j := range oldCs {
		if !used[j] {
			d.Changes = append(d.Changes, ChangeDiff{ChangeID: oldCs[j].ChangeID, Old: &oldCs[j], Status: ChangeDropped})
		}
	}
	return d, nil
}

// changeInterdiff: what changed in one change between versions o and n —
// o's changes replayed onto n's parent, against n.
func (f *Forge) changeInterdiff(ctx context.Context, g *gitx.Repo, o, n *gitx.Commit) ([]gitx.FileDiff, string) {
	parent := func(c *gitx.Commit) string {
		if len(c.Parents) > 0 {
			return c.Parents[0]
		}
		return gitx.EmptyTree
	}
	from, note := o.SHA, ""
	if parent(o) != parent(n) {
		if tree, _, err := g.MergeTrees(ctx, parent(o), parent(n), o.SHA); err == nil {
			from = tree
		} else {
			note = "the old version doesn't apply cleanly on the new parent; this diff includes changes from earlier commits"
		}
	}
	files, err := g.Diff(ctx, from, n.SHA)
	if err != nil {
		return nil, err.Error()
	}
	return files, note
}

// ------------------------------------------------------------------ stacks

// PRStack returns the open pull requests a PR is stacked on (bottom first)
// and those stacked on it (nearest first): PRs whose base branch is another
// PR's head branch, in the same repository.
func (f *Forge) PRStack(ctx context.Context, r *store.Repo, i *store.Issue) (below, above []*store.Issue) {
	if !i.IsPR || i.PR == nil {
		return nil, nil
	}
	seen := map[int64]bool{i.ID: true}
	cur := i
	for len(below) < 10 {
		prs, _ := f.Store.OpenPRsForHead(ctx, r.ID, cur.PR.BaseBranch)
		var next *store.Issue
		for _, p := range prs {
			if p.RepoID == r.ID && !seen[p.ID] {
				next = p
				break
			}
		}
		if next == nil {
			break
		}
		seen[next.ID] = true
		below = append([]*store.Issue{next}, below...)
		cur = next
	}
	cur = i
	for len(above) < 10 && cur.PR.HeadRepoID == r.ID {
		prs, _ := f.Store.OpenPRsForBase(ctx, r.ID, cur.PR.HeadBranch)
		var next *store.Issue
		for _, p := range prs {
			if !seen[p.ID] {
				next = p
				break
			}
		}
		if next == nil {
			break
		}
		seen[next.ID] = true
		above = append(above, next)
		cur = next
	}
	return below, above
}

// retargetStack: when a PR merges, the PRs stacked on it now target its base.
func (f *Forge) retargetStack(ctx context.Context, actor *store.User, r *store.Repo, merged *store.Issue) {
	if merged.PR.HeadRepoID != r.ID {
		return
	}
	prs, _ := f.Store.OpenPRsForBase(ctx, r.ID, merged.PR.HeadBranch)
	for _, p := range prs {
		if err := f.Store.SetPRBase(ctx, p.ID, merged.PR.BaseBranch); err != nil {
			continue
		}
		f.Store.RecordEvent(ctx, r.ID, actor.ID, "pr", fmt.Sprintf("PR #%d now targets %s (#%d, which it was stacked on, was merged)", p.Number, merged.PR.BaseBranch, merged.Number))
		f.Broker.Publish(fmt.Sprintf("issue:%d", p.ID))
	}
}
