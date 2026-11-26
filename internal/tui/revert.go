package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/protect"
	"github.com/cooperspencer/forge/internal/store"
)

const (
	applyDirect = "commit to the branch"
	applyPR     = "open a pull request"
)

// revertForm asks where a revert goes and runs it.
func revertForm(ctx *Ctx, r *store.Repo, title, branch string, run func(forge.RevertOptions) (*forge.RevertResult, error)) Page {
	return changeForm(ctx, r, title, branch, "where the changes are undone", "", run, "reverted")
}

// pickForm asks which branch a cherry-pick goes to and runs it.
func pickForm(ctx *Ctx, r *store.Repo, title, branch string, run func(forge.RevertOptions) (*forge.RevertResult, error)) Page {
	return changeForm(ctx, r, title, branch, "the branch that gets the changes", "release/1.x", run, "cherry-picked")
}

func changeForm(ctx *Ctx, r *store.Repo, title, branch, hint, placeholder string, run func(forge.RevertOptions) (*forge.RevertResult, error), done string) Page {
	how := applyDirect
	if rule, ok := ctx.F.BranchRule(ctx.Ctx, r, branch); ok && rule.Push == protect.PushNone {
		how = applyPR // changes to this branch must go through pull requests
	}
	return NewForm(ctx, title, []*Field{
		{Label: "Branch", Value: branch, Placeholder: placeholder, Hint: hint},
		{Label: "How", Options: []string{applyDirect, applyPR}, Value: how},
	}, func(v map[string]string) (tea.Cmd, error) {
		res, err := run(forge.RevertOptions{Branch: v["Branch"], PullRequest: v["How"] == applyPR})
		if err != nil {
			return nil, err
		}
		if res.PR != nil {
			return tea.Sequence(pop, toast(fmt.Sprintf("opened #%d from %s", res.PR.Number, res.Branch)), refreshCmd), nil
		}
		return tea.Sequence(pop, toast(done+" on "+res.Branch+" as "+shortSHA(res.SHA)), refreshCmd), nil
	})
}
