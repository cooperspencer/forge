package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/cooperspencer/forge/internal/gitx"
	"github.com/muesli/termenv"
)

func TestPickReadme(t *testing.T) {
	ents := func(names ...string) []gitx.TreeEntry {
		var out []gitx.TreeEntry
		for _, n := range names {
			typ := "blob"
			if strings.HasSuffix(n, "/") {
				typ, n = "tree", strings.TrimSuffix(n, "/")
			}
			out = append(out, gitx.TreeEntry{Name: n, Type: typ})
		}
		return out
	}
	for want, names := range map[string][]string{
		"README.md":   {"main.go", "README.txt", "README.md", "readme"},
		"readme":      {"readme", "readme.txt"},
		"Readme.rst":  {"Readme.rst", "docs/"},
		"":            {"main.go", "README/", "READMEFIRST.md"},
		"README.adoc": {"README.adoc"},
	} {
		if got := pickReadme(ents(names...)); got != want {
			t.Errorf("pickReadme(%v) = %q, want %q", names, got, want)
		}
	}
}

// TestTabBelow: a tab with content below its list shrinks the list to its
// rows, keeps the page height, and cuts long content with a hint.
func TestTabBelow(t *testing.T) {
	st := stylesFor(termenv.TrueColor)
	ctx := &Ctx{St: st}
	readme := ""
	tab := &Tab{Name: "Code", List: NewList(st, Column{Title: "NAME", Flex: 1})}
	tab.List.NoHeader = true
	tab.Load = func() []Row { return []Row{{Cols: []string{"a.go"}}, {Cols: []string{"b.go"}}} }
	tab.Below = func(w int) string { return readme }
	tab.BelowMore = "… more"
	p := NewTabPage(ctx, "x", []*Tab{tab})
	p.SetSize(80, 30)

	readme = strings.Repeat("line\n", 100)
	p.Init()
	v := ansi.Strip(p.View())
	lines := strings.Split(v, "\n")
	if lipgloss.Height(v) != 30 {
		t.Errorf("height %d, want 30:\n%s", lipgloss.Height(v), v)
	}
	if !strings.Contains(lines[2], "a.go") || !strings.Contains(lines[3], "b.go") || !strings.HasPrefix(lines[4], "────") || lines[5] != "line" {
		t.Errorf("list then README:\n%s", strings.Join(lines[:8], "\n"))
	}
	if !strings.Contains(lines[len(lines)-1], "… more") {
		t.Errorf("long README not cut with the hint: %q", lines[len(lines)-1])
	}

	readme = "short\n"
	p.reload()
	v = ansi.Strip(p.View())
	if !strings.Contains(v, "short") || strings.Contains(v, "… more") {
		t.Errorf("short README:\n%s", v)
	}

	// without a README the list gets the whole height
	readme = ""
	p.reload()
	seps := 0
	v = ansi.Strip(p.View())
	for _, l := range strings.Split(v, "\n") {
		if strings.HasPrefix(l, "────") {
			seps++
		}
	}
	if seps != 1 || lipgloss.Height(v) != 30 {
		t.Errorf("no README:\n%s", v)
	}
	if tab.List.h != 28 {
		t.Errorf("list height without README: %d", tab.List.h)
	}
}
