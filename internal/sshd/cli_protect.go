package sshd

import (
	"fmt"
	"strings"

	"github.com/cooperspencer/forge/internal/protect"
	"github.com/cooperspencer/forge/internal/store"
)

type protectCmds struct {
	List protectListCmd `cmd:"" help:"List protected branches."`
	Set  protectSetCmd  `cmd:"" help:"Protect branches (repository admin)."`
	Rm   protectRmCmd   `cmd:"" help:"Remove a protection rule (repository admin)."`
}

type protectListCmd struct {
	Repo string `arg:"" help:"owner/name"`
}

func (x *protectListCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessRead)
	if err != nil {
		return err
	}
	rules, err := c.F.Protections(c.Ctx, c.User, r)
	if err != nil {
		return err
	}
	if len(rules) == 0 {
		fmt.Fprintln(c.Out, "no protected branches")
		return nil
	}
	w := c.table()
	for _, pr := range rules {
		fmt.Fprintf(w, "%s\t%s\n", pr.Pattern, pr.Summary())
	}
	return w.Flush()
}

type protectSetCmd struct {
	Push           string `default:"write" enum:"write,admin,none" help:"Who may push directly: write, admin, or none (pull requests only)."`
	Approvals      int    `help:"Approvals required to merge a pull request."`
	Checks         string `placeholder:"ci,lint|*" help:"Pipelines that must pass to merge, comma separated (* = all)."`
	AllowForce     bool   `help:"Allow force pushes."`
	AllowDelete    bool   `help:"Allow deleting the branch."`
	BlockOnChanges bool   `default:"true" negatable:"" help:"No merging while changes are requested."`
	AdminsBypass   bool   `help:"Repository admins can bypass the rules."`
	Repo           string `arg:"" help:"owner/name"`
	Pattern        string `arg:"" help:"Branch name or pattern, e.g. main or release/*."`
}

func (x *protectSetCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessAdmin)
	if err != nil {
		return err
	}
	rule := protect.Rule{Pattern: x.Pattern, Push: x.Push, Approvals: x.Approvals, AllowForce: x.AllowForce, AllowDelete: x.AllowDelete,
		BlockOnChanges: x.BlockOnChanges, AdminBypass: x.AdminsBypass}
	if x.Checks != "" {
		rule.Checks = strings.Split(x.Checks, ",")
	}
	if err := c.F.SetProtection(c.Ctx, c.User, r, rule); err != nil {
		return err
	}
	rule.Validate()
	fmt.Fprintf(c.Out, "protected %s: %s\n", rule.Pattern, rule.Summary())
	return nil
}

type protectRmCmd struct {
	Repo    string `arg:"" help:"owner/name"`
	Pattern string `arg:""`
}

func (x *protectRmCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessAdmin)
	if err != nil {
		return err
	}
	return c.F.RemoveProtection(c.Ctx, c.User, r, x.Pattern)
}
