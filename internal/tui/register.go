package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// NewRegisterPage lets an unknown SSH key claim a username.
func NewRegisterPage(ctx *Ctx, first bool) Page {
	title := "Welcome! Choose a username to register your SSH key"
	if first {
		title = "Welcome! You are the first user and will become the administrator"
	}
	form := NewForm(ctx, title, []*Field{{Label: "Username", Placeholder: "lowercase letters, digits, - _ ."}},
		func(v map[string]string) (tea.Cmd, error) {
			u, err := ctx.F.Register(ctx.Ctx, v["Username"], ctx.PubKey)
			if err != nil {
				return nil, err
			}
			ctx.User = u
			return tea.Sequence(replace(NewHome(ctx)), toast(fmt.Sprintf("registered as %s", u.Name))), nil
		})
	return &registerPage{FormPage: form, ctx: ctx}
}

type registerPage struct {
	*FormPage
	ctx *Ctx
}

func (r *registerPage) Title() string { return "Register" }

func (r *registerPage) Update(msg tea.Msg) (Page, tea.Cmd) {
	if km, ok := msg.(tea.KeyMsg); ok && km.String() == "esc" {
		// skip registration and browse anonymously
		return r, replace(NewHome(r.ctx))
	}
	_, cmd := r.FormPage.Update(msg)
	return r, cmd
}

func (r *registerPage) View() string {
	fp := r.ctx.St.Faint.Render(fmt.Sprintf("  key %s", fingerprint(r.ctx)))
	return lipgloss.JoinVertical(lipgloss.Left, r.FormPage.View(), fp, r.ctx.St.Faint.Render("  esc to browse anonymously"))
}

func fingerprint(ctx *Ctx) string {
	if ctx.PubKey == nil {
		return "(none)"
	}
	return ctxFingerprint(ctx)
}

func ctxFingerprint(ctx *Ctx) string {
	return forgeFingerprint(ctx.PubKey)
}
