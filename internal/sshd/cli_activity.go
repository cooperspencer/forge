package sshd

import (
	"fmt"
	"strings"
	"time"

	"github.com/cooperspencer/forge/internal/heatmap"
	"github.com/cooperspencer/forge/internal/store"
)

type activityCmd struct {
	User  string `arg:"" optional:"" help:"User or organization (default: you)."`
	Weeks int    `default:"53" help:"Number of weeks to show (1-53)."`
}

// shades draw the graph without colour, so it survives pipes.
var shades = [heatmap.Levels]string{"·", "░", "▒", "▓", "█"}

func (x activityCmd) Run(c *CLI) error {
	who := c.User
	if x.User != "" {
		u, err := c.F.Store.UserByName(c.Ctx, x.User)
		if err != nil {
			return fmt.Errorf("user %q: %w", x.User, err)
		}
		who = u
	} else if err := c.needUser(); err != nil {
		return err
	}
	weeks := min(max(x.Weeks, 1), heatmap.MaxWeeks)
	now := time.Now()
	cs, err := c.F.Store.Contributions(c.Ctx, c.User, who.ID, heatmap.Since(now, time.Local, weeks))
	if err != nil {
		return err
	}
	g := heatmap.New(cs, now, time.Local)
	return writeGraph(c, who, g, weeks)
}

func writeGraph(c *CLI, who *store.User, g *heatmap.Graph, weeks int) error {
	cell := func(l int) string { return shades[l] }
	s := g.Stats(weeks)
	fmt.Fprintf(c.Out, "%s: %s\n\n", who.Name, heatmap.Summary(s, weeks))
	for _, l := range g.Render(weeks, cell) {
		fmt.Fprintln(c.Out, l)
	}
	legend := []string{"Less"}
	for _, sh := range shades {
		legend = append(legend, sh)
	}
	fmt.Fprintf(c.Out, "\n%s\n", strings.Join(append(legend, "More"), " "))
	if s.Total > 0 {
		fmt.Fprintf(c.Out, "active on %d day(s), busiest %s (%d), current streak %d day(s)\n",
			s.Days, s.Busiest.Format("2006-01-02"), s.Most, s.Streak)
	}
	return nil
}
