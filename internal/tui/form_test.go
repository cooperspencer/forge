package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func TestConfirmName(t *testing.T) {
	for in, want := range map[string]bool{
		"webapp": true, "acme/webapp": true, " webapp ": true,
		"": false, "web": false, "other/webapp": false, "acme": false,
	} {
		if got := confirmName(in, "webapp", "acme/webapp"); got != want {
			t.Errorf("confirmName(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestFormCheckbox(t *testing.T) {
	ctx := &Ctx{St: stylesFor(termenv.TrueColor)}
	var got map[string]string
	f := NewForm(ctx, "x", []*Field{{Label: "Name", Value: "a"}, {Label: "LFS", Checkbox: true, Placeholder: "Use LFS"}},
		func(v map[string]string) (tea.Cmd, error) { got = v; return nil, nil })
	f.Update(tea.KeyMsg{Type: tea.KeyTab})
	if !strings.Contains(ansi.Strip(f.View()), "[ ] Use LFS") {
		t.Errorf("unchecked view:\n%s", ansi.Strip(f.View()))
	}
	f.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")})
	if !strings.Contains(ansi.Strip(f.View()), "[✓] Use LFS") {
		t.Errorf("checked view:\n%s", ansi.Strip(f.View()))
	}
	f.Update(tea.KeyMsg{Type: tea.KeyEnter}) // last field: submits
	if got["LFS"] != "true" || got["Name"] != "a" {
		t.Errorf("values: %v", got)
	}
	g := NewForm(ctx, "x", []*Field{{Label: "LFS", Checkbox: true, Value: "true"}}, func(v map[string]string) (tea.Cmd, error) { got = v; return nil, nil })
	g.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	g.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if got["LFS"] != "false" {
		t.Errorf("pre-checked then toggled: %v", got)
	}
}
