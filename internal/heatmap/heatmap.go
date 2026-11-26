// Package heatmap lays out a contribution graph: one column per week, one
// row per weekday (Sunday first), each cell shaded by how much happened that
// day. The TUI colours the cells; the CLI draws them with shade characters.
package heatmap

import (
	"fmt"
	"strings"
	"time"

	"github.com/cooperspencer/forge/internal/store"
)

// MaxWeeks is a year of weeks, the most a graph shows.
const MaxWeeks = 53

// Levels is the number of shades, 0 (nothing) included.
const Levels = 5

const labelW = 4 // "Mon "

// Graph is the per-day contribution count of the weeks ending with the week
// of Today.
type Graph struct {
	Today time.Time      // midnight, in the graph's location
	Days  map[string]int // by key(day)
}

// New buckets contributions into calendar days of loc, up to now.
func New(cs []store.Contribution, now time.Time, loc *time.Location) *Graph {
	g := &Graph{Today: day(now.In(loc)), Days: map[string]int{}}
	for _, c := range cs {
		g.Days[key(time.Unix(c.Timestamp, 0).In(loc))] += c.Count
	}
	return g
}

// Since is the earliest moment a graph of `weeks` weeks ending at now shows:
// what to query contributions from.
func Since(now time.Time, loc *time.Location, weeks int) time.Time {
	return firstDay(day(now.In(loc)), weeks)
}

func key(t time.Time) string { return t.Format(time.DateOnly) }

// Count is the number of contributions on the day of t.
func (g *Graph) Count(t time.Time) int { return g.Days[key(t)] }

func day(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// firstDay is the Sunday that starts the first of `weeks` weeks ending with
// the week of today.
func firstDay(today time.Time, weeks int) time.Time {
	return today.AddDate(0, 0, -int(today.Weekday())-7*(weeks-1))
}

// WeeksFor is how many weeks fit in width columns (each week takes two).
func WeeksFor(width int) int { return min(MaxWeeks, max(0, (width-labelW)/2)) }

// Stats summarises the shown weeks.
type Stats struct {
	Total   int
	Days    int       // days with any contribution
	Busiest time.Time // zero when Total is 0
	Most    int
	Streak  int // consecutive days up to today (or yesterday) with contributions
}

func (g *Graph) Stats(weeks int) Stats {
	var s Stats
	first := firstDay(g.Today, weeks)
	for d := first; !d.After(g.Today); d = d.AddDate(0, 0, 1) {
		n := g.Count(d)
		s.Total += n
		if n > 0 {
			s.Days++
		}
		if n > s.Most {
			s.Most, s.Busiest = n, d
		}
	}
	d := g.Today
	if g.Count(d) == 0 {
		d = d.AddDate(0, 0, -1) // today isn't over yet
	}
	for ; !d.Before(first) && g.Count(d) > 0; d = d.AddDate(0, 0, -1) {
		s.Streak++
	}
	return s
}

// Level maps a day's count to a shade: 0 for none, then 1…Levels-1 in
// quarters of the busiest day.
func Level(n, most int) int {
	if n <= 0 || most <= 0 {
		return 0
	}
	return min(Levels-1, max(1, (n*(Levels-1)+most-1)/most))
}

// Render draws the graph of the last `weeks` weeks: a month row, then seven
// weekday rows. cell renders one day's shade and must be one column wide;
// days after today stay blank.
func (g *Graph) Render(weeks int, cell func(level int) string) []string {
	if weeks <= 0 {
		return nil
	}
	first := firstDay(g.Today, weeks)
	most := g.Stats(weeks).Most

	// months: a label over the first week that starts in that month, when
	// it doesn't run into the previous label
	months := []byte(strings.Repeat(" ", labelW+2*weeks))
	end := 0
	for w := range weeks {
		start := first.AddDate(0, 0, 7*w)
		if w > 0 && start.Month() == start.AddDate(0, 0, -7).Month() {
			continue
		}
		if w == 0 && start.AddDate(0, 0, 7).Month() != start.Month() {
			continue // the month only has a few days left: its label would sit over the next one's
		}
		col := labelW + 2*w
		label := start.Format("Jan")
		if col < end || col+len(label) > len(months) {
			continue
		}
		copy(months[col:], label)
		end = col + len(label) + 1
	}
	lines := []string{strings.TrimRight(string(months), " ")}

	for wd := range 7 {
		var b strings.Builder
		switch time.Weekday(wd) {
		case time.Monday, time.Wednesday, time.Friday:
			b.WriteString(fmt.Sprintf("%-*s", labelW, time.Weekday(wd).String()[:3]))
		default:
			b.WriteString(strings.Repeat(" ", labelW))
		}
		for w := range weeks {
			d := first.AddDate(0, 0, 7*w+wd)
			if d.After(g.Today) {
				break
			}
			if w > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(cell(Level(g.Count(d), most)))
		}
		lines = append(lines, b.String())
	}
	return lines
}

// Summary is the line above the graph: "123 contributions in the last year".
func Summary(s Stats, weeks int) string {
	span := "the last year"
	if weeks < MaxWeeks {
		span = fmt.Sprintf("the last %d weeks", weeks)
	}
	noun := "contributions"
	if s.Total == 1 {
		noun = "contribution"
	}
	return fmt.Sprintf("%d %s in %s", s.Total, noun, span)
}
