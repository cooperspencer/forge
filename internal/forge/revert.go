package forge

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cooperspencer/forge/internal/gitx"
	"github.com/cooperspencer/forge/internal/protect"
	"github.com/cooperspencer/forge/internal/store"
)

// Reverts and cherry-picks: a commit's changes undone or applied on a
// branch with a new commit, directly or through a pull request. Neither needs
// a work tree (git merge-tree), and both refuse on conflicts.

// RevertOptions says where a revert or cherry-pick goes.
type RevertOptions struct {
	Branch      string // the branch to change (revert: default the default branch, for PRs their base)
	PullRequest bool   // commit to a new branch and open a pull request into Branch instead
}

// PickOptions: cherry-picks use the same options (Branch is required).
type PickOptions = RevertOptions

// RevertResult is the new commit and where it went.
type RevertResult struct {
	SHA    string
	Branch string       // the branch the commit is on (Branch, or the new branch)
	PR     *store.Issue // the pull request, with PullRequest
}

// change is one revert or cherry-pick.
type change struct {
	c        *gitx.Commit
	revert   bool
	title    string
	body     string
	prefix   string // branch name prefix for pull requests
	author   *gitx.Signature
	authored gitx.Commit // author date source (cherry-picks keep it)
}

// RevertCommit undoes commit sha with a new commit, like `git revert` (merge
// commits are reverted relative to their first parent).
func (f *Forge) RevertCommit(ctx context.Context, actor *store.User, r *store.Repo, sha string, opt RevertOptions) (*RevertResult, error) {
	c, err := f.Git(r).Commit(ctx, sha)
	if err != nil {
		return nil, fmt.Errorf("unknown commit %q", sha)
	}
	body := "This reverts commit " + c.SHA + "."
	if len(c.Parents) > 1 {
		body = "This reverts commit " + c.SHA + ",\nreversing changes made to " + c.Parents[0] + "."
	}
	return f.apply(ctx, actor, r, opt, change{c: c, revert: true, title: `Revert "` + c.Subject + `"`, body: body, prefix: "revert-"})
}

// RevertPR undoes a merged pull request: its merge or squash commit.
func (f *Forge) RevertPR(ctx context.Context, actor *store.User, r *store.Repo, i *store.Issue, opt RevertOptions) (*RevertResult, error) {
	c, err := f.prCommit(ctx, r, i, "reverted", "revert")
	if err != nil {
		return nil, err
	}
	if opt.Branch == "" {
		opt.Branch = i.PR.BaseBranch
	}
	body := fmt.Sprintf("This reverts pull request #%d (commit %s).", i.Number, c.SHA)
	return f.apply(ctx, actor, r, opt, change{c: c, revert: true, title: fmt.Sprintf(`Revert "%s" (#%d)`, i.Title, i.Number), body: body, prefix: "revert-"})
}

// CherryPick applies commit sha to opt.Branch, like `git cherry-pick -x`:
// the original author and message are kept (merge commits: relative to their
// first parent).
func (f *Forge) CherryPick(ctx context.Context, actor *store.User, r *store.Repo, sha string, opt PickOptions) (*RevertResult, error) {
	c, err := f.Git(r).Commit(ctx, sha)
	if err != nil {
		return nil, fmt.Errorf("unknown commit %q", sha)
	}
	body := strings.TrimSpace(c.Body)
	if body != "" {
		body += "\n\n"
	}
	body += "(cherry picked from commit " + c.SHA + ")"
	return f.apply(ctx, actor, r, opt, change{c: c, title: c.Subject, body: body, prefix: "cherry-pick-",
		author: &gitx.Signature{Name: c.Author, Email: c.AuthorEmail}, authored: *c})
}

// CherryPickPR applies a merged pull request to another branch (a backport):
// its merge or squash commit.
func (f *Forge) CherryPickPR(ctx context.Context, actor *store.User, r *store.Repo, i *store.Issue, opt PickOptions) (*RevertResult, error) {
	c, err := f.prCommit(ctx, r, i, "cherry-picked", "cherry-pick")
	if err != nil {
		return nil, err
	}
	if opt.Branch == "" || opt.Branch == i.PR.BaseBranch {
		return nil, fmt.Errorf("pick a branch other than %s, where #%d is already merged", i.PR.BaseBranch, i.Number)
	}
	body := fmt.Sprintf("%s\n\n(cherry picked from pull request #%d, commit %s)", strings.TrimSpace(i.Body), i.Number, c.SHA)
	return f.apply(ctx, actor, r, opt, change{c: c, title: fmt.Sprintf("%s (#%d)", i.Title, i.Number), body: strings.TrimSpace(body), prefix: "cherry-pick-"})
}

// prCommit is the single commit a merged pull request landed as.
func (f *Forge) prCommit(ctx context.Context, r *store.Repo, i *store.Issue, done, verb string) (*gitx.Commit, error) {
	if !i.IsPR || i.PR == nil || i.PR.MergeSHA == "" {
		return nil, fmt.Errorf("only merged pull requests can be %s", done)
	}
	c, err := f.Git(r).Commit(ctx, i.PR.MergeSHA)
	if err != nil {
		return nil, fmt.Errorf("the merge commit of #%d is gone", i.Number)
	}
	if len(c.Parents) < 2 && c.SHA == i.PR.HeadSHA {
		return nil, fmt.Errorf("#%d was fast-forwarded, so it has no single commit to %s; %s its commits in the Commits tab", i.Number, verb, verb)
	}
	return c, nil
}

func (f *Forge) apply(ctx context.Context, actor *store.User, r *store.Repo, opt RevertOptions, ch change) (*RevertResult, error) {
	if actor == nil || f.Store.AccessLevel(ctx, actor, r) < store.AccessWrite {
		return nil, ErrForbidden
	}
	if r.Mirror {
		return nil, f.ReadOnlyError(ctx, r)
	}
	g, c := f.Git(r), ch.c
	branch := opt.Branch
	if branch == "" {
		if !ch.revert {
			return nil, errors.New("which branch should the commit be picked onto?")
		}
		branch = r.DefaultBranch
	}
	ref := "refs/heads/" + branch
	head, err := g.ResolveRef(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("unknown branch %q", branch)
	}
	verb := "reverting"
	var tree string
	var conflicts []string
	if ch.revert {
		if !g.IsAncestor(ctx, c.SHA, head) {
			return nil, fmt.Errorf("commit %s is not on %s", c.Short(), branch)
		}
		tree, conflicts, err = g.RevertTree(ctx, head, c.SHA)
	} else {
		verb = "cherry-picking"
		if g.IsAncestor(ctx, c.SHA, head) {
			return nil, fmt.Errorf("commit %s is already on %s", c.Short(), branch)
		}
		tree, conflicts, err = g.PickTree(ctx, head, c.SHA)
	}
	switch {
	case errors.Is(err, gitx.ErrConflict):
		return nil, fmt.Errorf("%s %s conflicts with %s (%s); do it locally and resolve the conflicts", verb, c.Short(), branch, strings.Join(conflicts, ", "))
	case err != nil:
		return nil, err
	}
	if cur, _ := g.TreeOf(ctx, head); cur == tree {
		if ch.revert {
			return nil, fmt.Errorf("nothing to revert: the changes of %s are no longer on %s", c.Short(), branch)
		}
		return nil, fmt.Errorf("nothing to cherry-pick: the changes of %s are already on %s", c.Short(), branch)
	}
	committer := f.signature(actor)
	author, authored := committer, ch.authored.When
	if ch.author != nil {
		author = *ch.author
	}
	sha, err := g.CommitTreeAs(ctx, tree, []string{head}, ch.title+"\n\n"+ch.body, author, committer, authored)
	if err != nil {
		return nil, err
	}
	rules, _ := f.Store.Protections(ctx, r.ID)
	access := f.protectAccess(ctx, actor, r)
	res := &RevertResult{SHA: sha, Branch: branch}
	before, _ := g.RefMap(ctx)

	if opt.PullRequest {
		name := ch.prefix + c.Short()
		for n := 2; ; n++ {
			if _, err := g.ResolveRef(ctx, "refs/heads/"+name); err != nil {
				break
			}
			name = fmt.Sprintf("%s%s-%d", ch.prefix, c.Short(), n)
		}
		if err := protect.CheckPush(rules, access, protect.Update{Old: protect.ZeroSHA, New: sha, Ref: "refs/heads/" + name}, nil); err != nil {
			return nil, err
		}
		if err := g.UpdateRef(ctx, "refs/heads/"+name, sha, gitx.ZeroSHA); err != nil {
			return nil, err
		}
		after, _ := g.RefMap(ctx)
		f.PostReceive(ctx, actor, r, DiffRefs(before, after))
		pr, err := f.OpenPR(ctx, actor, r, branch, r, name, ch.title, ch.body)
		if err != nil {
			return nil, err
		}
		res.Branch, res.PR = name, pr
		return res, nil
	}

	isAncestor := func(a, b string) bool { return g.IsAncestor(ctx, a, b) }
	if err := protect.CheckPush(rules, access, protect.Update{Old: head, New: sha, Ref: ref}, isAncestor); err != nil {
		return nil, fmt.Errorf("%w; use a pull request instead", err)
	}
	if err := g.UpdateRef(ctx, ref, sha, head); err != nil {
		return nil, fmt.Errorf("%s moved meanwhile; try again", branch)
	}
	after, _ := g.RefMap(ctx)
	f.PostReceive(ctx, actor, r, DiffRefs(before, after))
	return res, nil
}
