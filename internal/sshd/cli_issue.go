package sshd

import (
	"fmt"
	"strings"

	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/gitx"
	"github.com/cooperspencer/forge/internal/store"
)

type issueCmds struct {
	List    issueListCmd    `cmd:"" help:"List issues."`
	Create  issueCreateCmd  `cmd:"" help:"Open an issue (-b - reads the body from stdin)."`
	View    issueViewCmd    `cmd:"" help:"Show an issue and its comments."`
	Comment issueCommentCmd `cmd:"" help:"Comment on an issue or pull request."`
	Close   issueStateCmd   `cmd:"" help:"Close an issue or pull request."`
	Reopen  issueStateCmd   `cmd:"" help:"Reopen an issue or pull request."`
	Label   issueLabelCmd   `cmd:"" help:"Set labels."`
	Move    issueMoveCmd    `cmd:"" help:"Move an issue on the board (column name or 1-based position)."`
}

type prCmds struct {
	List      issueListCmd    `cmd:"" help:"List pull requests."`
	Create    prCreateCmd     `cmd:"" help:"Open a pull request."`
	View      issueViewCmd    `cmd:"" help:"Show a pull request, its checks and comments."`
	Diff      prDiffCmd       `cmd:"" help:"Print the diff."`
	Approve   issueCommentCmd `cmd:"" help:"Approve."`
	Reject    issueCommentCmd `cmd:"" help:"Request changes."`
	Merge     prMergeCmd      `cmd:"" help:"Merge."`
	Revert    prRevertCmd     `cmd:"" help:"Undo a merged pull request on its base branch."`
	Pick      prPickCmd       `cmd:"" help:"Cherry-pick a merged pull request onto another branch (a backport)."`
	Revisions prRevisionsCmd  `cmd:"" help:"List the heads a pull request has had (pushes, force pushes, jj rewrites)."`
	Interdiff prInterdiffCmd  `cmd:"" help:"What changed between two revisions (default: the last push)."`
}

type releaseCmds struct {
	List   releaseListCmd   `cmd:"" help:"List releases."`
	Create releaseCreateCmd `cmd:"" help:"Publish a release."`
}

// ---------------------------------------------------------------- issues

type issueListCmd struct {
	prs   bool
	State string `default:"open" enum:"open,closed,all" help:"open, closed or all."`
	Repo  string `arg:"" help:"owner/name"`
}

func (x *issueListCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessRead)
	if err != nil {
		return err
	}
	st := x.State
	if st == "all" {
		st = ""
	}
	is, err := c.F.Store.ListIssues(c.Ctx, r.ID, store.IssueFilter{PRs: x.prs, State: st})
	if err != nil {
		return err
	}
	w := c.table()
	if x.prs {
		fmt.Fprintln(w, "#\tSTATE\tTITLE\tBRANCH\tAUTHOR\tUPDATED")
	} else {
		fmt.Fprintln(w, "#\tSTATE\tTITLE\tAUTHOR\tUPDATED")
	}
	for _, i := range is {
		if x.prs {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s→%s\t%s\t%s\n", i.Number, i.State, i.Title, i.PR.HeadBranch, i.PR.BaseBranch, i.Author, ts(i.UpdatedAt))
		} else {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", i.Number, i.State, i.Title, i.Author, ts(i.UpdatedAt))
		}
	}
	return w.Flush()
}

type issueCreateCmd struct {
	Title string `short:"t" required:"" help:"Title."`
	Body  string `short:"b" help:"Body (- reads stdin)."`
	Repo  string `arg:"" help:"owner/name"`
}

func (x *issueCreateCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessRead)
	if err != nil {
		return err
	}
	b, err := c.bodyArg(x.Body)
	if err != nil {
		return err
	}
	i, err := c.F.CreateIssue(c.Ctx, c.User, r, x.Title, b)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "opened #%d\n", i.Number)
	return nil
}

type issueViewCmd struct {
	Repo   string `arg:"" help:"owner/name"`
	Number string `arg:""`
}

func (x *issueViewCmd) Run(c *CLI) error {
	r, i, err := c.issue(x.Repo, x.Number, store.AccessRead)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "#%d %s [%s]\nby %s on %s\n", i.Number, i.Title, i.State, i.Author, ts(i.CreatedAt))
	if len(i.Labels) > 0 {
		fmt.Fprintf(c.Out, "labels: %s\n", strings.Join(i.Labels, ", "))
	}
	if i.IsPR {
		fmt.Fprintf(c.Out, "%s:%s → %s\n", i.PR.HeadRepo, i.PR.HeadBranch, i.PR.BaseBranch)
		if st, err := c.F.PRStatus(c.Ctx, r, i); err == nil {
			fmt.Fprintf(c.Out, "%d commit(s), %d file(s) changed", len(st.Commits), len(st.Files))
			if i.State == "open" {
				if st.Mergeable {
					fmt.Fprint(c.Out, ", mergeable")
				} else {
					fmt.Fprintf(c.Out, ", CONFLICTS: %s", strings.Join(st.Conflicts, ", "))
				}
			}
			fmt.Fprintln(c.Out)
			for _, run := range st.Checks {
				fmt.Fprintf(c.Out, "  check %s #%d: %s\n", run.Pipeline, run.Number, run.Status)
			}
			if len(st.Approvals) > 0 {
				fmt.Fprintf(c.Out, "  approved by: %s\n", strings.Join(st.Approvals, ", "))
			}
			if len(st.Changes) > 0 {
				fmt.Fprintf(c.Out, "  changes requested by: %s\n", strings.Join(st.Changes, ", "))
			}
		}
		if below, above := c.F.PRStack(c.Ctx, r, i); len(below)+len(above) > 0 {
			var parts []string
			for _, p := range below {
				parts = append(parts, fmt.Sprintf("#%d", p.Number))
			}
			parts = append(parts, fmt.Sprintf("[#%d]", i.Number))
			for _, p := range above {
				parts = append(parts, fmt.Sprintf("#%d", p.Number))
			}
			fmt.Fprintf(c.Out, "stack: %s\n", strings.Join(parts, " ← "))
		}
	}
	if i.Body != "" {
		fmt.Fprintf(c.Out, "\n%s\n", i.Body)
	}
	cs, _ := c.F.Store.Comments(c.Ctx, i.ID)
	for _, cm := range cs {
		tag := ""
		if cm.Review != "" {
			tag = " [" + cm.Review + "]"
		}
		if cm.Path != "" {
			tag += fmt.Sprintf(" %s:%d", cm.Path, cm.Line)
		}
		fmt.Fprintf(c.Out, "\n--- %s on %s%s\n%s\n", cm.Author, ts(cm.CreatedAt), tag, cm.Body)
	}
	return nil
}

// issueCommentCmd is also pr approve (review "approve") and pr reject ("changes").
type issueCommentCmd struct {
	review string
	Body   string `short:"b" help:"Comment (- reads stdin)."`
	File   string `help:"Path, for a review comment on a line."`
	Line   int    `help:"Line, for a review comment."`
	Repo   string `arg:"" help:"owner/name"`
	Number string `arg:""`
}

func (x *issueCommentCmd) Run(c *CLI) error {
	r, i, err := c.issue(x.Repo, x.Number, store.AccessRead)
	if err != nil {
		return err
	}
	b, err := c.bodyArg(x.Body)
	if err != nil {
		return err
	}
	rv := x.review
	if rv == "" && x.File != "" {
		rv = "comment"
	}
	if err := c.F.Comment(c.Ctx, c.User, r, i, &store.Comment{Body: b, Review: rv, Path: x.File, Line: x.Line}); err != nil {
		return err
	}
	fmt.Fprintln(c.Out, "ok")
	return nil
}

type issueStateCmd struct {
	state  string
	Repo   string `arg:"" help:"owner/name"`
	Number string `arg:""`
}

func (x *issueStateCmd) Run(c *CLI) error {
	r, i, err := c.issue(x.Repo, x.Number, store.AccessRead)
	if err != nil {
		return err
	}
	if err := c.F.SetIssueState(c.Ctx, c.User, r, i, x.state); err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "#%d %s\n", i.Number, x.state)
	return nil
}

type issueLabelCmd struct {
	Repo   string `arg:"" help:"owner/name"`
	Number string `arg:""`
	Labels string `arg:"" help:"Comma-separated labels (replaces the current ones)."`
}

func (x *issueLabelCmd) Run(c *CLI) error {
	r, i, err := c.issue(x.Repo, x.Number, store.AccessRead)
	if err != nil {
		return err
	}
	i.Labels = nil
	for _, l := range strings.Split(x.Labels, ",") {
		if l = strings.TrimSpace(l); l != "" {
			i.Labels = append(i.Labels, l)
		}
	}
	return c.F.EditIssue(c.Ctx, c.User, r, i)
}

// ---------------------------------------------------------------- PRs

type prCreateCmd struct {
	Base   string `help:"Base branch (default: the target's default branch)."`
	Title  string `short:"t" help:"Title (default: the first commit's subject)."`
	Body   string `short:"b" help:"Description (- reads stdin)."`
	Into   string `placeholder:"owner/name" help:"Target repository (default: the parent of a fork, else the same repository)."`
	Repo   string `arg:"" help:"owner/name of the head branch"`
	Branch string `arg:"" name:"head-branch"`
}

func (x *prCreateCmd) Run(c *CLI) error {
	head, err := c.repo(x.Repo, store.AccessRead)
	if err != nil {
		return err
	}
	target := head
	if x.Into != "" {
		if target, err = c.repo(x.Into, store.AccessRead); err != nil {
			return err
		}
	} else if head.ForkOf != 0 {
		if p, err := c.F.Store.RepoByID(c.Ctx, head.ForkOf); err == nil && c.F.Store.AccessLevel(c.Ctx, c.User, p) >= store.AccessRead {
			target = p
		}
	}
	b, err := c.bodyArg(x.Body)
	if err != nil {
		return err
	}
	i, err := c.F.OpenPR(c.Ctx, c.User, target, x.Base, head, x.Branch, x.Title, b)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "opened %s PR #%d: %s\n", target.FullName(), i.Number, i.Title)
	return nil
}

type prDiffCmd struct {
	Repo   string `arg:"" help:"owner/name"`
	Number string `arg:""`
}

func (x *prDiffCmd) Run(c *CLI) error {
	r, i, err := c.issue(x.Repo, x.Number, store.AccessRead)
	if err != nil {
		return err
	}
	if !i.IsPR {
		return fmt.Errorf("#%d is not a pull request", i.Number)
	}
	st, err := c.F.PRStatus(c.Ctx, r, i)
	if err != nil {
		return err
	}
	printDiffs(c.Out, st.Files)
	return nil
}

type prMergeCmd struct {
	Method string `default:"merge" enum:"merge,squash,ff" help:"merge, squash or ff."`
	Repo   string `arg:"" help:"owner/name"`
	Number string `arg:""`
}

func (x *prMergeCmd) Run(c *CLI) error {
	r, i, err := c.issue(x.Repo, x.Number, store.AccessWrite)
	if err != nil {
		return err
	}
	sha, err := c.F.MergePR(c.Ctx, c.User, r, i, x.Method)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "merged #%d as %s\n", i.Number, sha[:8])
	return nil
}

type prRevertCmd struct {
	PR     bool   `name:"pr" help:"Put the revert on a new branch and open a pull request."`
	Branch string `help:"Branch to revert on (default: the pull request's base)."`
	Repo   string `arg:"" help:"owner/name"`
	Number string `arg:""`
}

func (x *prRevertCmd) Run(c *CLI) error {
	r, i, err := c.issue(x.Repo, x.Number, store.AccessWrite)
	if err != nil {
		return err
	}
	res, err := c.F.RevertPR(c.Ctx, c.User, r, i, forge.RevertOptions{Branch: x.Branch, PullRequest: x.PR})
	if err != nil {
		return err
	}
	printRevert(c, res, "reverted")
	return nil
}

type prPickCmd struct {
	PR     bool   `name:"pr" help:"Put the commit on a new branch and open a pull request."`
	Branch string `required:"" help:"Branch that gets the pull request's changes."`
	Repo   string `arg:"" help:"owner/name"`
	Number string `arg:""`
}

func (x *prPickCmd) Run(c *CLI) error {
	r, i, err := c.issue(x.Repo, x.Number, store.AccessWrite)
	if err != nil {
		return err
	}
	res, err := c.F.CherryPickPR(c.Ctx, c.User, r, i, forge.PickOptions{Branch: x.Branch, PullRequest: x.PR})
	if err != nil {
		return err
	}
	printRevert(c, res, "cherry-picked")
	return nil
}

type prRevisionsCmd struct {
	Repo   string `arg:"" help:"owner/name"`
	Number string `arg:""`
}

func (x *prRevisionsCmd) Run(c *CLI) error {
	r, i, err := c.issue(x.Repo, x.Number, store.AccessRead)
	if err != nil {
		return err
	}
	revs, err := c.F.PRRevisions(c.Ctx, c.User, r, i)
	if err != nil {
		return err
	}
	w := c.table()
	fmt.Fprintln(w, "REV\tHEAD\tBASE\tPUSHED BY\tWHEN")
	for _, v := range revs {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", v.N, v.HeadSHA[:8], v.BaseSHA[:8], v.Pusher, ts(v.CreatedAt))
	}
	return w.Flush()
}

type prInterdiffCmd struct {
	Patch  bool   `short:"p" help:"Print patches."`
	Repo   string `arg:"" help:"owner/name"`
	Number string `arg:""`
	From   int    `arg:"" optional:"" help:"Older revision (default: the one before To)."`
	To     int    `arg:"" optional:"" help:"Newer revision (default: the latest)."`
}

func (x *prInterdiffCmd) Run(c *CLI) error {
	r, i, err := c.issue(x.Repo, x.Number, store.AccessRead)
	if err != nil {
		return err
	}
	d, err := c.F.PRInterdiff(c.Ctx, c.User, r, i, x.From, x.To)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "revision %d → %d", d.From.N, d.To.N)
	if d.Rebased {
		fmt.Fprintf(c.Out, " (rebased: base %s → %s)", d.From.BaseSHA[:8], d.To.BaseSHA[:8])
	}
	fmt.Fprintln(c.Out)
	for _, ch := range d.Changes {
		cm := ch.New
		if cm == nil {
			cm = ch.Old
		}
		id := cm.Short()
		if ch.ChangeID != "" {
			id = gitx.ShortChangeID(ch.ChangeID)
		}
		fmt.Fprintf(c.Out, "  %s  %-9s  %s\n", id, ch.Status, cm.Subject)
	}
	printFiles := func(title string, files []gitx.FileDiff) {
		fmt.Fprintln(c.Out, "\n"+title)
		if len(files) == 0 {
			fmt.Fprintln(c.Out, "  no differences in content")
		}
		for _, f := range files {
			fmt.Fprintf(c.Out, "  %s %s +%d -%d\n", f.Status, f.NewPath, f.Additions, f.Deletions)
		}
		if x.Patch {
			for _, f := range files {
				fmt.Fprint(c.Out, "\n"+f.Patch)
			}
		}
	}
	for _, ch := range d.Changes {
		if ch.Status == forge.ChangeRewritten {
			printFiles(fmt.Sprintf("what changed in %q:", ch.New.Subject), ch.Files)
		}
	}
	printFiles("the whole pull request:", d.Files)
	if d.Note != "" {
		fmt.Fprintln(c.Out, "note: "+d.Note)
	}
	return nil
}

// ---------------------------------------------------------------- releases

type releaseListCmd struct {
	Repo string `arg:"" help:"owner/name"`
}

func (x *releaseListCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessRead)
	if err != nil {
		return err
	}
	rs, err := c.F.Store.Releases(c.Ctx, r.ID)
	if err != nil {
		return err
	}
	w := c.table()
	fmt.Fprintln(w, "TAG\tTITLE\tAUTHOR\tPUBLISHED")
	for _, rel := range rs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", rel.Tag, rel.Title, rel.Author, ts(rel.CreatedAt))
	}
	return w.Flush()
}

type releaseCreateCmd struct {
	Target string `help:"Ref to tag, if the tag doesn't exist yet."`
	Title  string `short:"t" help:"Title."`
	Notes  string `short:"n" help:"Notes (- reads stdin)."`
	Repo   string `arg:"" help:"owner/name"`
	Tag    string `arg:""`
}

func (x *releaseCreateCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessWrite)
	if err != nil {
		return err
	}
	n, err := c.bodyArg(x.Notes)
	if err != nil {
		return err
	}
	rel, err := c.F.CreateRelease(c.Ctx, c.User, r, x.Tag, x.Target, x.Title, n)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "published %s\n", rel.Tag)
	return nil
}
