package sshd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/alecthomas/kong"
	"github.com/cooperspencer/forge/internal/ci"
	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/gitx"
	"github.com/cooperspencer/forge/internal/store"
	gossh "golang.org/x/crypto/ssh"
)

// CLI implements `ssh forge <command>`: a scriptable interface to everything
// the TUI can do. Output is plain text suitable for pipes. Commands are
// parsed by kong; each one is a struct below (and in cli_*.go) with a Run
// method.
type CLI struct {
	F      *forge.Forge
	User   *store.User
	PubKey gossh.PublicKey
	In     io.Reader
	Out    io.Writer
	Err    io.Writer
	Ctx    context.Context
	// Prog is the program name in usage lines (default: the ssh command).
	Prog string
}

var Version = "dev"

// root is the command tree.
type root struct {
	Repo          repoCmds    `cmd:"" help:"Repositories: create, browse, branches, tags, revert, cherry-pick."`
	Issue         issueCmds   `cmd:"" help:"Issues."`
	PR            prCmds      `cmd:"" name:"pr" help:"Pull requests."`
	Wiki          wikiCmds    `cmd:"" help:"Wiki pages."`
	Board         boardCmds   `cmd:"" help:"Kanban board columns."`
	Release       releaseCmds `cmd:"" help:"Releases."`
	CI            ciCmds      `cmd:"" name:"ci" help:"Pipelines and runs."`
	Mirror        mirrorCmds  `cmd:"" help:"Pull and push mirrors."`
	Protect       protectCmds `cmd:"" help:"Branch protection."`
	Search        searchCmds  `cmd:"" help:"Code search."`
	Secret        secretCmds  `cmd:"" help:"CI secrets."`
	Package       packageCmds `cmd:"" help:"Container images and generic packages."`
	Runner        runnerCmds  `cmd:"" help:"CI runners (admin)."`
	User          userCmds    `cmd:"" help:"Users."`
	Key           keyCmds     `cmd:"" help:"Your SSH keys."`
	Token         tokenCmds   `cmd:"" help:"Your API tokens."`
	Org           orgCmds     `cmd:"" help:"Organizations."`
	Notifications notifCmds   `cmd:"" help:"Your inbox."`
	Activity      activityCmd `cmd:"" help:"Contribution graph of the last year: yours, or a user's."`
	Whoami        whoamiCmd   `cmd:"" help:"Show your identity."`
	Version       versionCmd  `cmd:"" help:"Show the server version."`
}

// errUsage asks Run to print the command's usage.
var errUsage = errors.New("usage")

// cliExit carries kong's exit status (after --help or a parse error) out of
// the parser without ending the server process.
type cliExit struct{ code int }

func (c *CLI) parser(r *root) (*kong.Kong, error) {
	prog := c.Prog
	if prog == "" {
		prog = c.F.Cfg.SSHCommand()
	}
	return kong.New(r,
		kong.Name(prog),
		kong.Description(fmt.Sprintf("%s — run without a command for the interactive TUI.", c.F.Cfg.Name)),
		kong.Writers(c.Out, c.Err),
		kong.Exit(func(code int) { panic(cliExit{code}) }),
		kong.ConfigureHelp(kong.HelpOptions{Compact: false}),
		kong.Bind(c),
	)
}

// Run parses and runs one command line and returns its exit status.
func (c *CLI) Run(args []string) (code int) {
	r := newRoot()
	p, err := c.parser(r)
	if err != nil {
		fmt.Fprintln(c.Err, "error:", err)
		return 1
	}
	defer func() {
		if e := recover(); e != nil {
			x, ok := e.(cliExit)
			if !ok {
				panic(e)
			}
			if x.code != 0 {
				x.code = 1
			}
			code = x.code
		}
	}()
	switch {
	case len(args) == 0:
		args = []string{"--help"}
	case args[0] == "help":
		args = append(args[1:], "--help")
	}
	if len(args) == 1 && p.Model.Node != nil && hasChild(p.Model.Node, args[0]) && isGroup(p.Model.Node, args[0]) {
		// a group without a subcommand: list its commands
		p.Stdout = c.Err
		func() {
			defer func() { recover() }() // --help ends with kong's exit hook
			p.Parse([]string{args[0], "--help"})
		}()
		return 1
	}
	args = compatFlags(p, args)
	ctx, err := p.Parse(args)
	if err != nil {
		if !strings.HasPrefix(args[0], "-") && p.Model.Node != nil && !hasChild(p.Model.Node, args[0]) {
			fmt.Fprintf(c.Err, "unknown command %q — try `help`\n", args[0])
			return 1
		}
		var pe *kong.ParseError
		if errors.As(err, &pe) && pe.Context != nil {
			pe.Context.Stdout = c.Err
			pe.Context.PrintUsage(true)
			fmt.Fprintln(c.Err)
		}
		fmt.Fprintln(c.Err, "error:", err)
		return 1
	}
	if err := ctx.Run(); err != nil {
		switch {
		case errors.Is(err, errUsage):
			ctx.Stdout = c.Err
			ctx.PrintUsage(true)
		case errors.Is(err, forge.ErrForbidden):
			fmt.Fprintln(c.Err, "error: permission denied")
		case errors.Is(err, store.ErrNotFound):
			fmt.Fprintln(c.Err, "error: not found")
		default:
			fmt.Fprintln(c.Err, "error:", err)
		}
		return 1
	}
	return 0
}

// newRoot sets up commands that share a type but differ in behaviour.
func newRoot() *root {
	r := &root{}
	r.PR.List.prs = true
	r.Issue.Comment.review = ""
	r.PR.Approve.review, r.PR.Reject.review = "approve", "changes"
	r.Issue.Close.state, r.Issue.Reopen.state = "closed", "open"
	r.Runner.Disable.on, r.Runner.Enable.on = false, true
	return r
}

// isGroup reports whether the top-level command name has subcommands.
func isGroup(n *kong.Node, name string) bool {
	for _, ch := range n.Children {
		if ch.Name == name {
			return len(ch.Children) > 0
		}
	}
	return false
}

func hasChild(n *kong.Node, name string) bool {
	for _, ch := range n.Children {
		if ch.Name == name {
			return true
		}
		for _, a := range ch.Aliases {
			if a == name {
				return true
			}
		}
	}
	return false
}

// compatFlags keeps the single-dash long flags of earlier versions working
// (-private, -state all, -block-on-changes=false): a single-dash word that
// names a long flag of any command becomes its double-dash form.
func compatFlags(p *kong.Kong, args []string) []string {
	long := map[string]bool{}
	var walk func(n *kong.Node)
	walk = func(n *kong.Node) {
		for _, f := range n.Flags {
			if len(f.Name) > 1 {
				long[f.Name] = true
			}
		}
		for _, ch := range n.Children {
			walk(ch)
		}
	}
	walk(p.Model.Node)
	out := append([]string{}, args...)
	for i, a := range out {
		if a == "--" {
			break
		}
		if len(a) > 2 && a[0] == '-' && a[1] != '-' {
			name, _, _ := strings.Cut(a[1:], "=")
			if long[name] {
				out[i] = "-" + a
			}
		}
	}
	return out
}

type whoamiCmd struct{}

func (whoamiCmd) Run(c *CLI) error {
	if c.User == nil {
		fmt.Fprintln(c.Out, "anonymous")
		if c.PubKey != nil {
			fmt.Fprintln(c.Out, "key:", forge.Fingerprint(c.PubKey))
		}
		return nil
	}
	role := "user"
	if c.User.SiteAdmin() {
		role = "admin"
	}
	fmt.Fprintf(c.Out, "%s (%s)\n", c.User.Name, role)
	return nil
}

type versionCmd struct{}

func (versionCmd) Run(c *CLI) error {
	fmt.Fprintln(c.Out, "forge", Version)
	return nil
}

// ---------------------------------------------------------------- helpers

func (c *CLI) table() *tabwriter.Writer { return tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0) }

func (c *CLI) needUser() error {
	if c.User == nil {
		return fmt.Errorf("this command requires a registered SSH key")
	}
	return nil
}

func (c *CLI) admin() error {
	if c.User == nil || !c.User.SiteAdmin() {
		return forge.ErrForbidden
	}
	return nil
}

func (c *CLI) repo(full string, need store.Access) (*store.Repo, error) {
	r, _, err := c.F.ResolveRepo(c.Ctx, c.User, full, need)
	return r, err
}

func (c *CLI) issue(full, num string, need store.Access) (*store.Repo, *store.Issue, error) {
	r, err := c.repo(full, need)
	if err != nil {
		return nil, nil, err
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(num, "#"), 10, 64)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid number %q", num)
	}
	i, err := c.F.Store.IssueByNumber(c.Ctx, r.ID, n)
	return r, i, err
}

func (c *CLI) run(full, num string) (*store.Repo, *store.Run, error) {
	r, err := c.repo(full, store.AccessRead)
	if err != nil {
		return nil, nil, err
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(num, "#"), 10, 64)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid run number %q", num)
	}
	run, err := c.F.Store.RunByNumber(c.Ctx, r.ID, n)
	return r, run, err
}

// bodyArg returns b, or stdin when b is "-".
func (c *CLI) bodyArg(b string) (string, error) {
	if b != "-" {
		return b, nil
	}
	data, err := io.ReadAll(io.LimitReader(c.In, 1<<20))
	return string(data), err
}

func ts(t time.Time) string {
	if t.Unix() <= 0 {
		return "-"
	}
	return t.Format("2006-01-02 15:04")
}

func printDiffs(w io.Writer, ds []gitx.FileDiff) {
	for _, d := range ds {
		fmt.Fprintf(w, "\n--- %s\n+++ %s\n%s", d.OldPath, d.NewPath, d.Patch)
	}
}

func indentLines(s, p string) string {
	ls := strings.Split(s, "\n")
	for i := range ls {
		ls[i] = p + ls[i]
	}
	return strings.Join(ls, "\n")
}

var loadPipelines = ci.LoadPipelines

func sizeStr(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	case n < 1<<30:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
}
