package sshd

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/store"
)

type repoCmds struct {
	List     repoListCmd     `cmd:"" help:"List repositories you can see."`
	Create   repoCreateCmd   `cmd:"" help:"Create a repository."`
	Info     repoInfoCmd     `cmd:"" help:"Show repository details."`
	Delete   repoDeleteCmd   `cmd:"" help:"Delete a repository (admin)."`
	Fork     repoForkCmd     `cmd:"" help:"Fork a repository."`
	Edit     repoEditCmd     `cmd:"" help:"Change settings."`
	Rename   repoRenameCmd   `cmd:"" help:"Rename a repository."`
	Collab   repoCollabCmd   `cmd:"" help:"List collaborators, or set one's role (read, write, admin, remove)."`
	Watch    repoWatchCmd    `cmd:"" help:"Watch for notifications (or stop: off)."`
	Tree     repoTreeCmd     `cmd:"" help:"List files."`
	Cat      repoCatCmd      `cmd:"" help:"Print a file."`
	Log      repoLogCmd      `cmd:"" help:"List commits."`
	Show     repoShowCmd     `cmd:"" help:"Show a commit with its diff."`
	Branches repoBranchesCmd `cmd:"" help:"List branches and tags."`
	Branch   repoBranchCmd   `cmd:"" help:"Create a branch (from a branch, tag or commit; default: the default branch)."`
	Tag      repoTagCmd      `cmd:"" help:"Create a tag (annotated with -m)."`
	Revert   repoRevertCmd   `cmd:"" help:"Undo a commit with a new commit."`
	Pick     repoPickCmd     `cmd:"" help:"Cherry-pick a commit onto a branch."`
}

type repoListCmd struct{}

func (repoListCmd) Run(c *CLI) error {
	rs, err := c.F.Store.VisibleRepos(c.Ctx, c.User)
	if err != nil {
		return err
	}
	w := c.table()
	fmt.Fprintln(w, "REPOSITORY\tVISIBILITY\tUPDATED\tDESCRIPTION")
	for _, r := range rs {
		vis := "public"
		if r.Private {
			vis = "private"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.FullName(), vis, ts(r.UpdatedAt), r.Description)
	}
	return w.Flush()
}

type repoCreateCmd struct {
	Private     bool   `help:"Private repository."`
	LFS         bool   `name:"lfs" help:"Accept Git LFS uploads."`
	Description string `short:"d" help:"Description."`
	Name        string `arg:"" placeholder:"[owner/]name" help:"Name, optionally under an organization."`
}

func (x *repoCreateCmd) Run(c *CLI) error {
	if err := c.needUser(); err != nil {
		return err
	}
	owner, name := "", x.Name
	if o, n, ok := strings.Cut(x.Name, "/"); ok {
		owner, name = o, n
	}
	r, err := c.F.CreateRepoWith(c.Ctx, c.User, owner, name, x.Description, forge.RepoOptions{Private: x.Private, LFS: x.LFS})
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "created %s\n  %s\n  %s\n", r.FullName(), c.F.Cfg.SSHCloneURL(r.FullName()), c.F.Cfg.HTTPCloneURL(r.FullName()))
	return nil
}

type repoInfoCmd struct {
	Repo string `arg:"" help:"owner/name"`
}

func (x *repoInfoCmd) Run(c *CLI) error {
	r, acc, err := c.F.ResolveRepo(c.Ctx, c.User, x.Repo, store.AccessRead)
	if err != nil {
		return err
	}
	vis := "public"
	if r.Private {
		vis = "private"
	}
	w := c.table()
	fmt.Fprintf(w, "name\t%s\n", r.FullName())
	fmt.Fprintf(w, "description\t%s\n", r.Description)
	fmt.Fprintf(w, "visibility\t%s\n", vis)
	fmt.Fprintf(w, "default branch\t%s\n", r.DefaultBranch)
	if n, size, _ := c.F.Store.LFSUsage(c.Ctx, r.ID); r.LFS || n > 0 {
		state := "on"
		if !r.LFS {
			state = "off (uploads refused)"
		}
		fmt.Fprintf(w, "git lfs\t%s, %d object(s), %s\n", state, n, sizeStr(size))
	}
	if r.ForkOfName != "" {
		fmt.Fprintf(w, "fork of\t%s\n", r.ForkOfName)
	}
	fmt.Fprintf(w, "your access\t%s\n", acc)
	fmt.Fprintf(w, "ssh\t%s\n", c.F.Cfg.SSHCloneURL(r.FullName()))
	fmt.Fprintf(w, "http\t%s\n", c.F.Cfg.HTTPCloneURL(r.FullName()))
	fmt.Fprintf(w, "created\t%s\n", ts(r.CreatedAt))
	return w.Flush()
}

type repoDeleteCmd struct {
	Repo string `arg:"" help:"owner/name"`
}

func (x *repoDeleteCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessAdmin)
	if err != nil {
		return err
	}
	if err := c.F.DeleteRepo(c.Ctx, c.User, r); err != nil {
		return err
	}
	fmt.Fprintln(c.Out, "deleted", r.FullName())
	return nil
}

type repoForkCmd struct {
	Repo  string `arg:"" help:"owner/name"`
	Owner string `arg:"" optional:"" name:"target-owner" help:"Fork into this user or organization (default: you)."`
}

func (x *repoForkCmd) Run(c *CLI) error {
	if err := c.needUser(); err != nil {
		return err
	}
	src, err := c.repo(x.Repo, store.AccessRead)
	if err != nil {
		return err
	}
	r, err := c.F.Fork(c.Ctx, c.User, src, x.Owner)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "forked to %s\n  %s\n", r.FullName(), c.F.Cfg.SSHCloneURL(r.FullName()))
	return nil
}

type repoEditCmd struct {
	Description *string `short:"d" help:"Description."`
	Private     string  `enum:"true,false," default:"" placeholder:"true|false" help:"Make the repository private or public."`
	LFS         string  `name:"lfs" enum:"true,false," default:"" placeholder:"true|false" help:"Accept Git LFS uploads."`
	Branch      string  `help:"Default branch."`
	Repo        string  `arg:"" help:"owner/name"`
}

func (x *repoEditCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessAdmin)
	if err != nil {
		return err
	}
	if x.Description != nil {
		r.Description = *x.Description
	}
	if x.Private != "" {
		r.Private = x.Private == "true"
	}
	if x.LFS != "" {
		r.LFS = x.LFS == "true"
	}
	if x.Branch != "" {
		r.DefaultBranch = x.Branch
	}
	if err := c.F.UpdateRepo(c.Ctx, c.User, r); err != nil {
		return err
	}
	fmt.Fprintln(c.Out, "updated", r.FullName())
	return nil
}

type repoRenameCmd struct {
	Repo string `arg:"" help:"owner/name"`
	New  string `arg:"" name:"new-name"`
}

func (x *repoRenameCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessAdmin)
	if err != nil {
		return err
	}
	if err := c.F.RenameRepo(c.Ctx, c.User, r, x.New); err != nil {
		return err
	}
	fmt.Fprintln(c.Out, "renamed to", r.FullName())
	return nil
}

type repoCollabCmd struct {
	Repo string `arg:"" help:"owner/name"`
	User string `arg:"" optional:""`
	Role string `arg:"" optional:"" help:"read, write, admin, or remove."`
}

func (x *repoCollabCmd) Run(c *CLI) error {
	if (x.User == "") != (x.Role == "") {
		return errUsage
	}
	r, err := c.repo(x.Repo, store.AccessRead)
	if err != nil {
		return err
	}
	if x.User == "" {
		cs, err := c.F.Store.Collaborators(c.Ctx, r.ID)
		if err != nil {
			return err
		}
		w := c.table()
		fmt.Fprintln(w, "USER\tROLE")
		for _, cb := range cs {
			fmt.Fprintf(w, "%s\t%s\n", cb.User.Name, cb.Role)
		}
		return w.Flush()
	}
	if c.F.Store.AccessLevel(c.Ctx, c.User, r) < store.AccessAdmin {
		return forge.ErrForbidden
	}
	u, err := c.F.Store.UserByName(c.Ctx, x.User)
	if err != nil {
		return fmt.Errorf("unknown user %q", x.User)
	}
	if x.Role == "remove" {
		return c.F.Store.RemoveCollaborator(c.Ctx, r.ID, u.ID)
	}
	if err := c.F.Store.SetCollaborator(c.Ctx, r.ID, u.ID, x.Role); err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "%s now has %s access to %s\n", u.Name, x.Role, r.FullName())
	return nil
}

type repoWatchCmd struct {
	Repo string `arg:"" help:"owner/name"`
	Off  string `arg:"" optional:"" enum:"off," default:"" help:"off: stop watching."`
}

func (x *repoWatchCmd) Run(c *CLI) error {
	if err := c.needUser(); err != nil {
		return err
	}
	r, err := c.repo(x.Repo, store.AccessRead)
	if err != nil {
		return err
	}
	return c.F.Store.Watch(c.Ctx, c.User.ID, r.ID, x.Off != "off")
}

type repoTreeCmd struct {
	Repo string `arg:"" help:"owner/name"`
	Ref  string `arg:"" optional:"" help:"Branch, tag or commit (default: the default branch)."`
	Path string `arg:"" optional:""`
}

func (x *repoTreeCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessRead)
	if err != nil {
		return err
	}
	ref := x.Ref
	if ref == "" {
		ref = r.DefaultBranch
	}
	es, err := c.F.Git(r).Tree(c.Ctx, ref, x.Path)
	if err != nil {
		return err
	}
	w := c.table()
	for _, e := range es {
		name := e.Name
		if e.IsDir() {
			name += "/"
		}
		fmt.Fprintf(w, "%s\t%s\t%d\n", e.Mode, name, e.Size)
	}
	return w.Flush()
}

type repoCatCmd struct {
	Repo string `arg:"" help:"owner/name"`
	Ref  string `arg:""`
	Path string `arg:""`
}

func (x *repoCatCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessRead)
	if err != nil {
		return err
	}
	b, _, err := c.F.Git(r).Blob(c.Ctx, x.Ref, x.Path, 0)
	if err != nil {
		return err
	}
	_, err = c.Out.Write(b)
	return err
}

type repoLogCmd struct {
	N    int    `short:"n" name:"limit" default:"20" help:"Number of commits."`
	Repo string `arg:"" help:"owner/name"`
	Ref  string `arg:"" optional:""`
}

func (x *repoLogCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessRead)
	if err != nil {
		return err
	}
	ref := x.Ref
	if ref == "" {
		ref = r.DefaultBranch
	}
	cs, err := c.F.Git(r).Log(c.Ctx, ref, 0, x.N, "")
	if err != nil {
		return err
	}
	w := c.table()
	for _, cm := range cs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", cm.Short(), ts(cm.When), cm.Author, cm.Subject)
	}
	return w.Flush()
}

type repoShowCmd struct {
	Repo string `arg:"" help:"owner/name"`
	SHA  string `arg:"" name:"sha"`
}

func (x *repoShowCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessRead)
	if err != nil {
		return err
	}
	g := c.F.Git(r)
	cm, err := g.Commit(c.Ctx, x.SHA)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "commit %s\nAuthor: %s <%s>\nDate:   %s\n", cm.SHA, cm.Author, cm.AuthorEmail, ts(cm.When))
	if cm.ChangeID != "" {
		fmt.Fprintf(c.Out, "Change: %s\n", cm.ChangeID)
	}
	fmt.Fprintf(c.Out, "\n    %s\n", cm.Subject)
	if cm.Body != "" {
		fmt.Fprintf(c.Out, "\n%s\n", indentLines(cm.Body, "    "))
	}
	ds, err := g.Diff(c.Ctx, "", cm.SHA)
	if err != nil {
		return err
	}
	printDiffs(c.Out, ds)
	return nil
}

type repoBranchesCmd struct {
	Repo string `arg:"" help:"owner/name"`
}

func (x *repoBranchesCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessRead)
	if err != nil {
		return err
	}
	g := c.F.Git(r)
	w := c.table()
	bs, _ := g.Branches(c.Ctx)
	for _, b := range bs {
		mark := " "
		if b.Name == r.DefaultBranch {
			mark = "*"
		}
		fmt.Fprintf(w, "%s branch\t%s\t%s\t%s\n", mark, b.Name, b.SHA[:8], b.Subject)
	}
	tags, _ := g.Tags(c.Ctx)
	for _, t := range tags {
		fmt.Fprintf(w, "  tag\t%s\t%s\t%s\n", t.Name, t.SHA[:8], t.Subject)
	}
	return w.Flush()
}

type repoBranchCmd struct {
	Repo string `arg:"" help:"owner/name"`
	Name string `arg:""`
	From string `arg:"" optional:"" help:"Branch, tag or commit (default: the default branch)."`
}

func (x *repoBranchCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessWrite)
	if err != nil {
		return err
	}
	if err := c.F.CreateBranch(c.Ctx, c.User, r, x.Name, x.From); err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "created branch %s\n", x.Name)
	return nil
}

type repoTagCmd struct {
	Message string `short:"m" help:"Message (makes an annotated tag)."`
	Repo    string `arg:"" help:"owner/name"`
	Name    string `arg:""`
	Target  string `arg:"" optional:"" help:"Branch, tag or commit (default: the default branch)."`
}

func (x *repoTagCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessWrite)
	if err != nil {
		return err
	}
	if err := c.F.CreateTag(c.Ctx, c.User, r, x.Name, x.Target, x.Message); err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "created tag %s\n", x.Name)
	return nil
}

// changeFlags are shared by revert and cherry-pick.
type changeFlags struct {
	PR     bool   `name:"pr" help:"Put the commit on a new branch and open a pull request."`
	Branch string `help:"Branch to change."`
}

func printRevert(c *CLI, res *forge.RevertResult, verb string) {
	if res.PR != nil {
		fmt.Fprintf(c.Out, "opened #%d (%s → %s) with commit %s\n", res.PR.Number, res.Branch, res.PR.PR.BaseBranch, res.SHA[:8])
		return
	}
	fmt.Fprintf(c.Out, "%s on %s as %s\n", verb, res.Branch, res.SHA[:8])
}

type repoRevertCmd struct {
	changeFlags `embed:""`
	Repo        string `arg:"" help:"owner/name"`
	SHA         string `arg:"" name:"sha"`
}

func (x *repoRevertCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessWrite)
	if err != nil {
		return err
	}
	res, err := c.F.RevertCommit(c.Ctx, c.User, r, x.SHA, forge.RevertOptions{Branch: x.Branch, PullRequest: x.PR})
	if err != nil {
		return err
	}
	printRevert(c, res, "reverted")
	return nil
}

type repoPickCmd struct {
	PR     bool   `name:"pr" help:"Put the commit on a new branch and open a pull request."`
	Branch string `required:"" help:"Branch that gets the commit."`
	Repo   string `arg:"" help:"owner/name"`
	SHA    string `arg:"" name:"sha"`
}

func (x *repoPickCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessWrite)
	if err != nil {
		return err
	}
	res, err := c.F.CherryPick(c.Ctx, c.User, r, x.SHA, forge.PickOptions{Branch: x.Branch, PullRequest: x.PR})
	if err != nil {
		return err
	}
	printRevert(c, res, "cherry-picked")
	return nil
}

// atoi64 parses an id argument.
func atoi64(s, what string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimPrefix(s, "#"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q", what, s)
	}
	return n, nil
}
