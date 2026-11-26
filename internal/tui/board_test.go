package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/store"
	"github.com/muesli/termenv"
)

func testBoard(cards int) *forge.Board {
	b := &forge.Board{}
	for i, name := range []string{"To do", "In progress", "Review", "Done"} {
		b.Lanes = append(b.Lanes, forge.BoardLane{Column: store.BoardColumn{ID: int64(i + 1), Name: name}})
	}
	for n := 1; n <= cards; n++ {
		b.Lanes[0].Cards = append(b.Lanes[0].Cards, &store.Issue{Number: int64(n), State: "open", Author: "andy",
			Title: fmt.Sprintf("issue number %d with a fairly long title that wraps", n), Labels: []string{"bug"}})
	}
	b.Lanes[3].Cards = []*store.Issue{{Number: 99, State: "closed", Title: "done thing"}}
	b.Lanes[3].Hidden = 4
	return b
}

// TestBoardView: every line fills the width exactly, the height is kept, and
// the selected card stays visible when the column scrolls.
func TestBoardView(t *testing.T) {
	st := stylesFor(termenv.TrueColor)
	for _, w := range []int{60, 80, 130} {
		p := newBoardPane(&Ctx{St: st}, nil)
		p.b = testBoard(12)
		p.SetSize(w, 20)
		p.sel[0] = 11 // the last card, far below the fold
		out := p.View()
		lines := strings.Split(out, "\n")
		if len(lines) != 20 {
			t.Errorf("w=%d: %d lines, want 20", w, len(lines))
		}
		for i, l := range lines {
			if lw := ansi.StringWidth(l); lw > w {
				t.Errorf("w=%d line %d is %d wide: %q", w, i, lw, ansi.Strip(l))
			}
		}
		plain := ansi.Strip(out)
		if !strings.Contains(plain, "#12 ") || !strings.Contains(plain, "↑") {
			t.Errorf("w=%d: selected card not scrolled into view:\n%s", w, plain)
		}
		if strings.Contains(plain, "#1 ") {
			t.Errorf("w=%d: first card should have scrolled away:\n%s", w, plain)
		}
	}
	// wide enough: all four columns, and the done column's hidden count
	p := newBoardPane(&Ctx{St: st}, nil)
	p.b = testBoard(1)
	p.SetSize(130, 20)
	plain := ansi.Strip(p.View())
	for _, s := range []string{"To do", "In progress", "Review", "Done", "✓ #99", "+ 4 older closed"} {
		if !strings.Contains(plain, s) {
			t.Errorf("missing %q:\n%s", s, plain)
		}
	}
	// narrow: only the lanes around the selection
	p.SetSize(60, 20)
	p.col = 3
	plain = ansi.Strip(p.View())
	if strings.Contains(plain, "To do") || !strings.Contains(plain, "Done") {
		t.Errorf("narrow board should show the selected lane:\n%s", plain)
	}
	if h := ansi.Strip(p.Header()); !strings.Contains(h, "columns 3–4 of 4") || !strings.Contains(h, "1 open") || !strings.Contains(h, "5 closed") {
		t.Errorf("header: %q", h)
	}
}

func TestTabsView(t *testing.T) {
	st := stylesFor(termenv.TrueColor)
	names := []string{"Code", "Commits", "Branches", "Tags", "Issues", "Board", "Pull requests", "Pipelines", "Releases", "Packages", "Activity", "Settings"}
	tb := Tabs{Names: names}
	if v := ansi.Strip(tb.View(st, 200)); !strings.Contains(v, "1 Code") || !strings.Contains(v, "0 Packages") || strings.Contains(v, "1 Settings") {
		t.Errorf("wide: %q", v)
	}
	if v := ansi.Strip(tb.View(st, 120)); strings.Contains(v, "1 Code") || !strings.Contains(v, "Settings") {
		t.Errorf("120: %q", v)
	}
	tb.Active = len(names) - 1
	v := ansi.Strip(tb.View(st, 80))
	if !strings.Contains(v, "Settings") || !strings.HasPrefix(v, "‹") || ansi.StringWidth(v) > 80 {
		t.Errorf("80, last tab active: %q", v)
	}
	tb.Active = 0
	if v := ansi.Strip(tb.View(st, 80)); !strings.HasPrefix(v, " Code") || !strings.HasSuffix(v, "…") {
		t.Errorf("80, first tab active: %q", v)
	}
}

// TestBoardArrowEdges: ←/→ move between columns, but at the first/last
// column they fall through to tab switching.
func TestBoardArrowEdges(t *testing.T) {
	p := newBoardPane(&Ctx{St: stylesFor(termenv.TrueColor)}, nil)
	if p.OwnsKey("left") || p.OwnsKey("right") {
		t.Error("an unloaded board must not hold the arrows")
	}
	p.b = testBoard(1)
	for _, c := range []struct {
		col         int
		left, right bool
	}{{0, false, true}, {1, true, true}, {3, true, false}} {
		p.col = c.col
		if p.OwnsKey("left") != c.left || p.OwnsKey("h") != c.left || p.OwnsKey("right") != c.right || p.OwnsKey("l") != c.right {
			t.Errorf("column %d: left=%v right=%v, want %v %v", c.col, p.OwnsKey("left"), p.OwnsKey("right"), c.left, c.right)
		}
	}
	if p.OwnsKey("up") || p.OwnsKey("tab") {
		t.Error("only ←→/h/l are claimed")
	}
}
