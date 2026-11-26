package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/cooperspencer/forge/internal/heatmap"
	"github.com/cooperspencer/forge/internal/store"
)

// activityGraph loads and draws a user's contribution graph, as it is seen
// by the session's user (contributions to repositories they can't read are
// left out).
type activityGraph struct {
	ctx  *Ctx
	who  *store.User
	g    *heatmap.Graph
	stat heatmap.Stats
}

// graphLines is the height of the graph block: summary, months, seven
// weekdays and a blank line.
const graphLines = 10

func newActivityGraph(ctx *Ctx, who *store.User) *activityGraph {
	return &activityGraph{ctx: ctx, who: who, g: heatmap.New(nil, time.Now(), time.Local)}
}

func (a *activityGraph) Load() {
	now := time.Now()
	cs, _ := a.ctx.F.Store.Contributions(a.ctx.Ctx, a.ctx.User, a.who.ID, heatmap.Since(now, time.Local, heatmap.MaxWeeks))
	a.g = heatmap.New(cs, now, time.Local)
	a.stat = a.g.Stats(heatmap.MaxWeeks)
}

// View renders the graph for a w×h page. It is empty when the page is too
// small to keep room for what's below it, and otherwise always graphLines
// tall, loaded or not, so the page's layout doesn't depend on the data.
func (a *activityGraph) View(w, h int) string {
	weeks := heatmap.WeeksFor(w - 1)
	if weeks < 8 || h < graphLines+12 {
		return ""
	}
	st := a.ctx.St
	cell := func(l int) string { return st.Heat[l].Render("■") }
	stat := a.g.Stats(weeks)

	graphW := 4 + 2*weeks - 1
	summary := st.Bold.Render(heatmap.Summary(stat, weeks))
	var extras []string
	if stat.Streak > 1 {
		extras = append(extras, fmt.Sprintf(" · %d-day streak", stat.Streak))
	}
	if stat.Total > 0 {
		extras = append(extras, fmt.Sprintf(" · busiest %s (%d)", stat.Busiest.Format("Jan 2"), stat.Most))
	}
	for _, x := range extras {
		if ansi.StringWidth(summary)+ansi.StringWidth(x) > graphW {
			break
		}
		summary += st.Faint.Render(x)
	}
	legend := st.Faint.Render("Less ") + strings.Join(levelCells(cell), " ") + st.Faint.Render(" More")
	if gap := graphW - ansi.StringWidth(summary) - ansi.StringWidth(legend); gap >= 2 {
		summary += strings.Repeat(" ", gap) + legend
	}

	lines := []string{" " + summary}
	for i, l := range a.g.Render(weeks, cell) {
		if i == 0 {
			l = st.Faint.Render(l)
		} else {
			// weekday labels are the first four columns
			lbl, rest := l[:min(4, len(l))], l[min(4, len(l)):]
			l = st.Faint.Render(lbl) + rest
		}
		lines = append(lines, " "+l)
	}
	return strings.Join(lines, "\n") + "\n"
}

func levelCells(cell func(int) string) []string {
	out := make([]string, heatmap.Levels)
	for l := range out {
		out[l] = cell(l)
	}
	return out
}
