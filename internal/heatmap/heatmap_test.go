package heatmap

import (
	"strings"
	"testing"
	"time"

	"github.com/cooperspencer/forge/internal/store"
)

func TestLevel(t *testing.T) {
	for _, c := range []struct{ n, most, want int }{
		{0, 0, 0}, {0, 10, 0}, {1, 10, 1}, {3, 10, 2}, {5, 10, 2}, {6, 10, 3}, {8, 10, 4}, {10, 10, 4}, {1, 1, 4},
	} {
		if got := Level(c.n, c.most); got != c.want {
			t.Errorf("Level(%d, %d) = %d, want %d", c.n, c.most, got, c.want)
		}
	}
}

func TestGraph(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 11, 26, 15, 0, 0, 0, loc) // a Thursday
	at := func(m time.Month, d, n int) store.Contribution {
		return store.Contribution{Timestamp: time.Date(2026, m, d, 12, 0, 0, 0, loc).Unix(), Count: n}
	}
	g := New([]store.Contribution{
		at(11, 26, 2), at(11, 25, 1), at(11, 24, 4), // streak of three up to today
		at(11, 22, 8), // the busiest day, a Sunday
		at(1, 1, 1),   // before the 8 shown weeks
	}, now, loc)

	if s := Since(now, loc, 8); !s.Equal(time.Date(2026, 10, 4, 0, 0, 0, 0, loc)) || s.Weekday() != time.Sunday {
		t.Fatalf("Since = %v", s)
	}
	s := g.Stats(8)
	if s.Total != 15 || s.Days != 4 || s.Most != 8 || s.Busiest.Day() != 22 || s.Streak != 3 {
		t.Fatalf("stats = %+v", s)
	}
	if y := g.Stats(MaxWeeks); y.Total != 16 {
		t.Fatalf("year total = %d", y.Total)
	}
	if got := Summary(s, 8); got != "15 contributions in the last 8 weeks" {
		t.Fatalf("summary %q", got)
	}

	lines := g.Render(8, func(l int) string { return string(rune('0' + l)) })
	want := []string{
		"    Oct     Nov",
		"    0 0 0 0 0 0 0 4", // Sunday
		"Mon 0 0 0 0 0 0 0 0",
		"    0 0 0 0 0 0 0 2",
		"Wed 0 0 0 0 0 0 0 1",
		"    0 0 0 0 0 0 0 1", // Thursday, today
		"Fri 0 0 0 0 0 0 0",   // the future stays blank
		"    0 0 0 0 0 0 0",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("render:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

func TestStreakSkipsUnfinishedToday(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 11, 26, 9, 0, 0, 0, loc)
	g := New([]store.Contribution{{Timestamp: time.Date(2026, 11, 25, 9, 0, 0, 0, loc).Unix(), Count: 1}}, now, loc)
	if s := g.Stats(MaxWeeks); s.Streak != 1 {
		t.Fatalf("streak = %d, want 1 (yesterday)", s.Streak)
	}
}

func TestWeeksFor(t *testing.T) {
	if WeeksFor(200) != MaxWeeks || WeeksFor(24) != 10 || WeeksFor(3) != 0 {
		t.Fatal(WeeksFor(200), WeeksFor(24), WeeksFor(3))
	}
}
