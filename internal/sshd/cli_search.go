package sshd

import (
	"fmt"
	"strings"

	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/store"
)

type searchCmds struct {
	Code codeSearchCmd `cmd:"" help:"Search code in all your repositories (grep-like output)."`
}

type codeSearchCmd struct {
	Repo  string   `placeholder:"owner/name" help:"Only this repository."`
	N     int      `short:"n" name:"limit" default:"100" help:"Maximum matches."`
	Query []string `arg:"" help:"Words, \"a phrase\", /regex/, and filters path:*.go repo:x owner:y case:yes fork:yes."`
}

func (x *codeSearchCmd) Run(c *CLI) error {
	q, err := forge.ParseCodeQuery(strings.Join(x.Query, " "))
	if err != nil {
		return err
	}
	q.Limit = x.N
	var scope *store.Repo
	if x.Repo != "" {
		if scope, err = c.repo(x.Repo, store.AccessRead); err != nil {
			return err
		}
	}
	res, err := c.F.SearchCode(c.Ctx, c.User, scope, q)
	if err != nil {
		return err
	}
	for _, m := range res.Matches {
		fmt.Fprintf(c.Out, "%s:%s:%d: %s\n", m.Repo.FullName(), m.Path, m.Line, strings.TrimSpace(m.Text))
	}
	fmt.Fprintf(c.Err, "%d match(es) in %d of %d repositories (%.2fs)", len(res.Matches), res.Hits, res.Repos, res.Took.Seconds())
	if res.Truncated {
		fmt.Fprint(c.Err, ", limit reached")
	}
	fmt.Fprintln(c.Err)
	return nil
}
