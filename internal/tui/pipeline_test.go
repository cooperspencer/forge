package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/cooperspencer/forge/internal/store"
	"github.com/muesli/termenv"
)

func job(key, status string, needs ...string) *store.Job {
	return &store.Job{Key: key, Name: key, Status: status, Needs: needs}
}

func TestStages(t *testing.T) {
	jobs := []*store.Job{
		job("lint", store.StatusSuccess),
		job("test", store.StatusSuccess, "lint"),
		job("test", store.StatusFailure, "lint"), // matrix cell
		job("docs", store.StatusSuccess),
		job("build", store.StatusSkipped, "test", "docs"),
		job("notify", store.StatusSuccess, "build"),
	}
	var got []string
	for _, st := range stages(jobs) {
		var ks []string
		for _, j := range st {
			ks = append(ks, j.Key)
		}
		got = append(got, strings.Join(ks, ","))
	}
	if want := "lint,docs|test,test|build|notify"; strings.Join(got, "|") != want {
		t.Errorf("stages = %v, want %s", got, want)
	}
	st := stylesFor(termenv.TrueColor)
	if s := ansi.Strip(st.JobStrip(jobs)); s != "✓✓›✓✗›↷›✓" {
		t.Errorf("strip = %q", s)
	}
	g := ansi.Strip(st.StageGraph(jobs, 200))
	if g != " ✓ lint  ✓ docs  →  ✓✗ test  →  ↷ build  →  ✓ notify" {
		t.Errorf("graph = %q", g)
	}
	if w := ansi.StringWidth(st.StageGraph(jobs, 20)); w > 20 {
		t.Errorf("graph not cut to width: %d", w)
	}
	// a failed matrix cell colours the group's name red
	if !strings.Contains(st.StageGraph(jobs, 200), st.StatusFail.Bold(true).Render("test")) {
		t.Error("failed matrix group not highlighted")
	}
}

func TestLogLineColours(t *testing.T) {
	st := stylesFor(termenv.TrueColor)
	for line, want := range map[string]string{
		"error: linker failed":           "fail",
		"  ERROR something":              "fail",
		"--- FAIL: TestX (0.01s)":        "fail",
		"FAIL\texample/app\t0.4s":        "fail",
		"panic: runtime error":           "fail",
		"warning: deprecated":            "warn",
		"[WARN] disk almost full":        "warn",
		"no errors here":                 "",
		"errors: 0":                      "",
		"\x1b[31merror: coloured\x1b[0m": "",
	} {
		got := st.logLine(line)
		switch want {
		case "fail":
			if got != st.StatusFail.Render(line) {
				t.Errorf("%q not shown as error", line)
			}
		case "warn":
			if got != st.StatusRun.Render(line) {
				t.Errorf("%q not shown as warning", line)
			}
		default:
			if got != line {
				t.Errorf("%q changed: %q", line, got)
			}
		}
	}
}

func TestStatusBadge(t *testing.T) {
	st := stylesFor(termenv.TrueColor)
	for status, label := range map[string]string{store.StatusSuccess: "passed", store.StatusFailure: "failed", store.StatusApproval: "needs approval", "weird": "weird"} {
		if got := ansi.Strip(st.StatusBadge(status)); !strings.Contains(got, label) {
			t.Errorf("badge %s = %q", status, got)
		}
	}
}
