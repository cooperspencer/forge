package ci

import (
	"testing"
	"time"
)

func TestCron(t *testing.T) {
	at := func(s string) time.Time {
		tm, err := time.Parse("2006-01-02 15:04 Mon", s)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	cases := []struct {
		expr string
		when string
		want bool
	}{
		{"0 18 * * *", "2026-11-25 18:00 Wed", true},
		{"0 18 * * *", "2026-11-25 18:01 Wed", false},
		{"0 18 * * *", "2026-11-25 17:00 Wed", false},
		{"*/15 * * * *", "2026-11-25 10:45 Wed", true},
		{"*/15 * * * *", "2026-11-25 10:46 Wed", false},
		{"5/20 * * * *", "2026-11-25 10:25 Wed", true},
		{"0 9-17/4 * * *", "2026-11-25 13:00 Wed", true},
		{"0 9-17/4 * * *", "2026-11-25 15:00 Wed", false},
		{"30 2 * * mon-fri", "2026-11-28 02:30 Sat", false},
		{"30 2 * * mon-fri", "2026-11-27 02:30 Fri", true},
		{"0 0 * * 7", "2026-11-29 00:00 Sun", true},
		{"0 0 1 jan,jul *", "2026-07-01 00:00 Wed", true},
		// dom and dow both restricted: either matches
		{"0 0 13 * fri", "2026-12-13 00:00 Sun", true},
		{"0 0 13 * fri", "2026-11-20 00:00 Fri", true},
		{"0 0 13 * fri", "2026-11-14 00:00 Sat", false},
		{"@daily", "2026-11-25 00:00 Wed", true},
		{"@hourly", "2026-11-25 07:00 Wed", true},
		{"@weekly", "2026-11-29 00:00 Sun", true},
	}
	for _, c := range cases {
		cr, err := ParseCron(c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got := cr.Matches(at(c.when)); got != c.want {
			t.Errorf("%q at %s = %v, want %v", c.expr, c.when, got, c.want)
		}
	}
	for _, bad := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "* * * * 8", "*/0 * * * *", "a * * * *", "5-1 * * * *", "@sometimes"} {
		if _, err := ParseCron(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestSchedulePipeline(t *testing.T) {
	p, err := Parse([]byte(`
on:
  push: {branches: [main]}
  schedule:
    - cron: "0 18 * * *"
      timezone: Europe/Vienna
    - "30 6 * * 1"
jobs:
  a: {image: alpine, steps: [true]}
`))
	if err != nil {
		t.Fatal(err)
	}
	vienna, _ := time.LoadLocation("Europe/Vienna")
	if !p.DueAt(time.Date(2026, 11, 25, 18, 0, 30, 0, vienna)) {
		t.Error("not due at 18:00 Vienna")
	}
	if p.DueAt(time.Date(2026, 11, 25, 18, 0, 0, 0, time.UTC)) {
		t.Error("due at 18:00 UTC (timezone ignored)")
	}
	if !p.DueAt(time.Date(2026, 11, 30, 6, 30, 0, 0, time.UTC)) {
		t.Error("scalar entry (UTC) not due")
	}
	if !p.Matches(Event{Kind: "schedule"}) {
		t.Error("schedule event does not match")
	}
	// Pipelines without on: run for push/PR/manual, never on a schedule.
	q, _ := Parse([]byte("jobs:\n  a: {image: alpine, steps: [true]}\n"))
	if q.Matches(Event{Kind: "schedule"}) || q.DueAt(time.Now()) {
		t.Error("pipeline without schedule matched")
	}
	for _, bad := range []string{
		"on: {schedule: [\"61 * * * *\"]}\njobs: {a: {image: alpine, steps: [true]}}",
		"on: {schedule: [{cron: \"@daily\", timezone: Mars/Olympus}]}\njobs: {a: {image: alpine, steps: [true]}}",
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
