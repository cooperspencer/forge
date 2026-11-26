package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cooperspencer/forge/internal/store"
)

// TestScheduledPipelines: scheduled pipelines run on the default branch at
// their cron time (and only then), receive secrets, skip forks, and pick up
// changes to the schedule.
func TestScheduledPipelines(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	e := setup(t)
	ctx := context.Background()
	f := e.f
	e.startRunner(t, "shell")

	r, err := f.CreateRepo(ctx, e.admin, "admin", "nightly", "", false)
	if err != nil {
		t.Fatal(err)
	}
	f.SetRepoSecret(ctx, e.admin, r, "TOKEN", "tok-value")
	work := t.TempDir()
	git(t, work, "init", "-q", "-b", "main")
	write := func(name, body string) {
		os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o755)
		os.WriteFile(filepath.Join(work, name), []byte(body), 0o644)
	}
	write(".forge/pipelines/nightly.yml", `name: nightly
on:
  schedule:
    - cron: "0 18 * * *"
      timezone: Europe/Vienna
jobs:
  build:
    image: alpine:3
    secrets: [TOKEN]
    steps: ["echo trigger=$FORGE_TRIGGER branch=$FORGE_BRANCH token=$TOKEN"]
`)
	// runs on push, never on a schedule
	write(".forge/pipelines/ci.yml", "name: ci\non: {push: {branches: [main]}}\njobs:\n  t: {image: alpine:3, steps: [true]}\n")
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "init")
	e.pushAs(t, work, "admin", e.token, "admin/nightly", "main")
	e.waitRun("admin/nightly", 1, store.StatusSuccess) // the push run of ci.yml

	vienna, _ := time.LoadLocation("Europe/Vienna")
	due := time.Date(2026, 11, 25, 18, 0, 0, 0, vienna)
	if n, err := f.RunSchedules(ctx, due.Add(time.Minute)); err != nil || n != 0 {
		t.Fatalf("18:01: %d runs, %v", n, err)
	}
	if n, err := f.RunSchedules(ctx, due.UTC()); err != nil || n != 1 {
		t.Fatalf("18:00 Vienna: %d runs, %v", n, err)
	}
	run := e.waitRun("admin/nightly", 2, store.StatusSuccess)
	if run.Trigger != "schedule" || run.Ref != "refs/heads/main" {
		t.Errorf("scheduled run: %+v", run)
	}
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/v1/repos/admin/nightly/jobs/%d/log", e.srv.URL, run.Jobs[0].ID), nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "trigger=schedule branch=main token=***") {
		t.Errorf("job log:\n%s", b)
	}

	// Forks don't run their parent's schedules.
	bob, _ := f.Store.CreateUser(ctx, "bob", false, false)
	if _, err := f.Fork(ctx, bob, r, "bob"); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.RunSchedules(ctx, due); n != 1 {
		t.Errorf("with a fork: %d runs, want 1", n)
	}
	e.waitRun("admin/nightly", 3, store.StatusSuccess)

	// A new commit changes the schedule.
	write(".forge/pipelines/nightly.yml", strings.Replace(mustRead(t, filepath.Join(work, ".forge/pipelines/nightly.yml")), `"0 18 * * *"`, `"30 6 * * *"`, 1))
	git(t, work, "commit", "-q", "-am", "later")
	e.pushAs(t, work, "admin", e.token, "admin/nightly", "main")
	e.waitRun("admin/nightly", 4, store.StatusSuccess) // push run
	if n, _ := f.RunSchedules(ctx, due); n != 0 {
		t.Errorf("old schedule still fires: %d", n)
	}
	if n, _ := f.RunSchedules(ctx, time.Date(2026, 11, 26, 6, 30, 0, 0, vienna)); n != 1 {
		t.Errorf("new schedule: %d runs", n)
	}
	e.waitRun("admin/nightly", 5, store.StatusSuccess)
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
