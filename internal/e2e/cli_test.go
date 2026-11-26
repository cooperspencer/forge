package e2e

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cooperspencer/forge/internal/sshd"
)

// TestCLI runs SSH CLI commands in-process (as the SSH server does), in the
// double-dash style and in the single-dash style of earlier versions.
func TestCLI(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := e.f
	type result struct {
		out, err string
		code     int
	}
	run := func(stdin string, line ...string) result {
		t.Helper()
		var out, errb bytes.Buffer
		c := &sshd.CLI{F: f, User: e.admin, In: strings.NewReader(stdin), Out: &out, Err: &errb, Ctx: ctx}
		code := c.Run(line)
		return result{out.String(), errb.String(), code}
	}
	ok := func(want string, line ...string) string {
		t.Helper()
		r := run("", line...)
		if r.code != 0 || !strings.Contains(r.out, want) {
			t.Errorf("%s: exit %d, out %q, err %q (want %q)", strings.Join(line, " "), r.code, r.out, r.err, want)
		}
		return r.out
	}
	fails := func(want string, line ...string) {
		t.Helper()
		r := run("", line...)
		if r.code == 0 || !strings.Contains(r.out+r.err, want) {
			t.Errorf("%s: exit %d, out %q, err %q (want failure with %q)", strings.Join(line, " "), r.code, r.out, r.err, want)
		}
	}

	// repositories, both flag styles
	ok("created admin/tool", "repo", "create", "-private", "-lfs", "-d", "a tool", "admin/tool")
	ok("created admin/app", "repo", "create", "--description=the app", "admin/app")
	info := ok("visibility", "repo", "info", "admin/tool")
	if !strings.Contains(info, "private") || !strings.Contains(info, "a tool") || !strings.Contains(info, "git lfs") {
		t.Errorf("repo info: %s", info)
	}
	ok("updated", "repo", "edit", "-private", "false", "-d", "", "admin/tool")
	ok("updated", "repo", "edit", "--lfs=false", "admin/tool")
	if r, _ := f.Store.RepoByName(ctx, "admin", "tool"); r.Private || r.LFS || r.Description != "" {
		t.Errorf("after edit: %+v", r)
	}
	fails("true", "repo", "edit", "--private", "maybe", "admin/tool")

	work := t.TempDir()
	git(t, work, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(work, "hello.go"), []byte("package main // hello there\n"), 0o644)
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "first")
	os.WriteFile(filepath.Join(work, "b.txt"), []byte("b\n"), 0o644)
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "second")
	e.pushAs(t, work, "admin", e.token, "admin/app", "main")
	if out := ok("second", "repo", "log", "-n", "1", "admin/app"); strings.Contains(out, "first") {
		t.Errorf("repo log -n 1: %s", out)
	}
	ok("first", "repo", "log", "--limit", "2", "admin/app")
	ok("created branch feat", "repo", "branch", "admin/app", "feat", "main")
	ok("created tag v1", "repo", "tag", "-m", "First", "admin/app", "v1")
	ok("feat", "repo", "branches", "admin/app")
	ok("hello.go", "repo", "tree", "admin/app")
	ok("hello there", "repo", "cat", "admin/app", "main", "hello.go")

	// issues: -b - reads stdin, -t is required
	if r := run("from stdin", "issue", "create", "-t", "Crash", "-b", "-", "admin/app"); r.code != 0 || !strings.Contains(r.out, "opened #1") {
		t.Errorf("issue create from stdin: %+v", r)
	}
	if i, _ := f.Store.IssueByNumber(ctx, mustRepo(t, f, "admin/app").ID, 1); i == nil || i.Body != "from stdin" {
		t.Errorf("issue body: %+v", i)
	}
	fails("--title", "issue", "create", "admin/app")
	ok("ok", "issue", "comment", "-b", "a comment", "admin/app", "1")
	if r := run("", "issue", "label", "admin/app", "#1", "bug,ui"); r.code != 0 {
		t.Errorf("issue label: %+v", r)
	}
	view := ok("Crash", "issue", "view", "admin/app", "1")
	if !strings.Contains(view, "labels: bug, ui") || !strings.Contains(view, "a comment") {
		t.Errorf("issue view: %s", view)
	}
	ok("#1 closed", "issue", "close", "admin/app", "1")
	if out := ok("Crash", "issue", "list", "-state", "all", "admin/app"); !strings.Contains(out, "closed") {
		t.Errorf("issue list -state all: %s", out)
	}
	if out := ok("#", "issue", "list", "admin/app"); strings.Contains(out, "Crash") {
		t.Errorf("closed issue in the default list: %s", out)
	}
	fails(`"closed"`, "issue", "list", "--state", "sometimes", "admin/app")

	// pull requests
	os.WriteFile(filepath.Join(work, "c.txt"), []byte("c\n"), 0o644)
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "third")
	e.pushAs(t, work, "admin", e.token, "admin/app", "main:feat")
	ok("PR #2", "pr", "create", "-t", "Feature", "admin/app", "feat")
	ok("Feature", "pr", "list", "admin/app")
	fails(`"squash"`, "pr", "merge", "-method", "rebase", "admin/app", "2")
	ok("merged #2", "pr", "merge", "-method", "squash", "admin/app", "2")

	// branch protection: bools with values, negation, defaults
	ok("protected main", "protect", "set", "-push", "none", "-approvals", "1", "-block-on-changes=false", "admin/app", "main")
	rules, _ := f.Store.Protections(ctx, mustRepo(t, f, "admin/app").ID)
	if len(rules) != 1 || rules[0].Push != "none" || rules[0].Approvals != 1 || rules[0].BlockOnChanges {
		t.Errorf("protect set (old style): %+v", rules)
	}
	ok("protected main", "protect", "set", "--push=admin", "--no-block-on-changes", "--admins-bypass", "admin/app", "main")
	ok("protected release/*", "protect", "set", "admin/app", "release/*")
	rules, _ = f.Store.Protections(ctx, mustRepo(t, f, "admin/app").ID)
	for _, r := range rules {
		if r.Pattern == "main" && (r.Push != "admin" || r.BlockOnChanges || !r.AdminBypass) || r.Pattern == "release/*" && !r.BlockOnChanges {
			t.Errorf("protect set: %+v", r)
		}
	}
	ok("main", "protect", "list", "admin/app")

	// tokens, secrets, search, wiki, board
	if out := ok("fgp_", "token", "create", "-scopes", "code:read", "-repos", "admin/app", "-expires", "30d", "ci"); strings.Count(out, "\n") != 1 {
		t.Errorf("token create: %q", out)
	}
	ok("code:read", "token", "list")
	if r := run("s3cret\n", "secret", "set", "admin/app", "api_key"); r.code != 0 || !strings.Contains(r.out, "API_KEY saved") {
		t.Errorf("secret set: %+v", r)
	}
	ok("API_KEY", "secret", "list", "admin/app")
	if r := run("", "search", "code", "-repo", "admin/app", "-n", "5", "hello", "there"); r.code != 0 || !strings.Contains(r.out, "admin/app:hello.go:1:") {
		t.Errorf("search code: %+v", r)
	}
	if r := run("# Getting started\n", "wiki", "edit", "-m", "start", "admin/app", "Getting", "Started"); r.code != 0 || !strings.Contains(r.out, "saved Getting Started") {
		t.Errorf("wiki edit: %+v", r)
	}
	ok("# Getting started", "wiki", "view", "admin/app", "Getting", "Started")
	// after --, words that look like old flags are left alone
	if r := run("", "board", "add", "admin/app", "--", "-private"); r.code != 0 || !strings.Contains(r.out, `added column "-private"`) {
		t.Errorf("board add -- -private: %+v", r)
	}
	ok("Getting Started", "wiki", "list", "admin/app")
	ok("added column", "board", "add", "admin/app", "In", "review")
	ok("In review", "board", "view", "admin/app")

	// users, orgs, runners
	ok("created user bob", "user", "create", "-admin", "bob")
	ok("bob", "user", "list")
	if r := run("", "user", "admin", "bob", "false"); r.code != 0 {
		t.Errorf("user admin: %+v", r)
	}
	fails("true or false", "user", "admin", "bob", "perhaps")
	ok("created organization acme", "org", "create", "acme")
	if r := run("", "org", "add", "acme", "bob"); r.code != 0 {
		t.Errorf("org add: %+v", r)
	}
	ok("bob\tmember", "org", "members", "acme")
	fails(`"member"`, "org", "add", "acme", "bob", "boss")
	ok("fgr_", "runner", "create", "r1", "docker,linux")
	if r := run("", "runner", "disable", "r1"); r.code != 0 {
		t.Errorf("runner disable: %+v", r)
	}
	ok("(disabled)", "runner", "list")
	ok("admin (admin)", "whoami")
	ok("forge", "version")

	// help, usage and errors
	ok("repo create <name>", "help")
	ok("--into=owner/name", "help", "pr", "create")
	ok("pr merge", "pr", "--help")
	if r := run("", "pr"); r.code != 1 || !strings.Contains(r.err, "pr merge") {
		t.Errorf("bare group: %+v", r)
	}
	fails(`unknown command "nope"`, "nope")
	fails("unknown flag --frob", "repo", "list", "--frob")
	fails("expected", "repo", "info")
	fails("not found", "repo", "info", "admin/nothing")
	anon := &sshd.CLI{F: f, In: strings.NewReader(""), Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, Ctx: ctx}
	if code := anon.Run([]string{"user", "list"}); code != 1 || !strings.Contains(anon.Err.(*bytes.Buffer).String(), "permission denied") {
		t.Errorf("anonymous user list: %d %s", code, anon.Err)
	}
}
