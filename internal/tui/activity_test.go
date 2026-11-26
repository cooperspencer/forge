package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/cooperspencer/forge/internal/heatmap"
	"github.com/cooperspencer/forge/internal/store"
	"github.com/muesli/termenv"
)

// TestActivityGraphView: the graph keeps its height whether or not it has
// data, fits the width, and gives way on small terminals.
func TestActivityGraphView(t *testing.T) {
	a := &activityGraph{ctx: &Ctx{St: stylesFor(termenv.TrueColor)}}
	now := time.Now()
	var cs []store.Contribution
	for d := range 200 {
		cs = append(cs, store.Contribution{Timestamp: now.AddDate(0, 0, -d).Unix(), Count: d % 7})
	}
	for _, data := range [][]store.Contribution{nil, cs} {
		a.g = heatmap.New(data, now, time.Local)
		for _, w := range []int{40, 80, 120, 200} {
			out := a.View(w, 40)
			if h := lipgloss.Height(out); h != graphLines {
				t.Errorf("w=%d: height %d, want %d", w, h, graphLines)
			}
			for i, l := range strings.Split(out, "\n") {
				if lw := ansi.StringWidth(l); lw > w {
					t.Errorf("w=%d line %d is %d wide: %q", w, i, lw, ansi.Strip(l))
				}
			}
			if testing.Verbose() && data != nil {
				t.Logf("w=%d\n%s", w, out)
			}
		}
	}
	if a.View(20, 40) != "" || a.View(120, 15) != "" {
		t.Error("graph should hide on tiny terminals")
	}
}
