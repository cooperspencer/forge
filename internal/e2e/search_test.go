package e2e

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestCodeSearch: search across repositories with permissions, filters,
// smart case, regexes, limits and odd file names.
func TestCodeSearch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	e := setup(t)
	ctx := context.Background()
	f := e.f
	bob, _ := f.Store.CreateUser(ctx, "bob", false, false)
	bobTok, _ := f.Store.CreateAccessToken(ctx, bob.ID, "t")
	asBob := &env{t: t, f: f, srv: e.srv, token: bobTok}

	seed := func(as *env, user, token, name string, private bool, files map[string]string) {
		t.Helper()
		if code := as.api("POST", "/api/v1/repos", map[string]any{"name": name, "private": private}, nil); code != 201 {
			t.Fatalf("create %s: %d", name, code)
		}
		dir := t.TempDir()
		git(t, dir, "init", "-q", "-b", "main")
		for p, body := range files {
			os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
			os.WriteFile(filepath.Join(dir, p), []byte(body), 0o644)
		}
		git(t, dir, "add", ".")
		git(t, dir, "commit", "-q", "-m", "init")
		e.pushAs(t, dir, user, token, user+"/"+name, "main")
	}
	seed(e, "admin", e.token, "api", false, map[string]string{
		"main.go":        "package main\n\nfunc main() {\n\thttp.ListenAndServe(\":8080\", nil)\n}\n",
		"docs/guide.md":  "# Guide\n\nlisten carefully\n",
		"odd:name ü.txt": "listen to the odd file\n",
		"bin/blob.dat":   "listen\x00binary",
		"many.txt":       strings.Repeat("needle\n", 150),
	})
	seed(e, "admin", e.token, "vault", true, map[string]string{"cfg.txt": "listen on the private port\n"})
	seed(asBob, "bob", bobTok, "other", false, map[string]string{"README": "Listen here\n"})
	api, _ := f.Store.RepoByName(ctx, "admin", "api")
	if _, err := f.Fork(ctx, bob, api, "bob"); err != nil {
		t.Fatal(err)
	}

	type result struct {
		Matches []struct {
			Repo, Path, Text string
			Line             int
		}
		Repositories int
		Truncated    bool
	}
	search := func(as *env, q string, extra ...string) (int, result) {
		t.Helper()
		var r result
		u := "/api/v1/search/code?q=" + url.QueryEscape(q)
		for _, x := range extra {
			u += "&" + x
		}
		code := as.api("GET", u, nil, &r)
		return code, r
	}
	hits := func(r result) string {
		var s []string
		for _, m := range r.Matches {
			s = append(s, fmt.Sprintf("%s:%s:%d", m.Repo, m.Path, m.Line))
		}
		sort.Strings(s)
		return strings.Join(s, " ")
	}

	_, r := search(e, "listen")
	if got := hits(r); got != "admin/api:docs/guide.md:3 admin/api:main.go:4 admin/api:odd:name ü.txt:1 admin/vault:cfg.txt:1 bob/other:README:1" {
		t.Errorf("listen (admin):\n%s", got)
	}
	_, r = search(asBob, "listen")
	if got := hits(r); strings.Contains(got, "vault") || !strings.Contains(got, "bob/other") {
		t.Errorf("bob sees: %s", got)
	}
	if got := hits(func() result { _, r := search(asBob, "listen fork:yes"); return r }()); !strings.Contains(got, "bob/api:main.go:4") {
		t.Errorf("fork:yes should include forks: %s", got)
	}
	_, r = search(e, "Listen") // smart case
	if got := hits(r); got != "admin/api:main.go:4 bob/other:README:1" {
		t.Errorf("Listen: %s", got)
	}
	_, r = search(e, "listen path:*.go")
	if got := hits(r); got != "admin/api:main.go:4" {
		t.Errorf("path:*.go: %s", got)
	}
	_, r = search(e, "listen path:docs/")
	if got := hits(r); got != "admin/api:docs/guide.md:3" {
		t.Errorf("path:docs/: %s", got)
	}
	_, r = search(e, "listen repo:other")
	if got := hits(r); got != "bob/other:README:1" {
		t.Errorf("repo:other: %s", got)
	}
	_, r = search(e, "listen owner:admin case:yes")
	if got := hits(r); got != "admin/api:docs/guide.md:3 admin/api:odd:name ü.txt:1 admin/vault:cfg.txt:1" {
		t.Errorf("owner:admin case:yes: %s", got)
	}
	_, r = search(e, `/Listen(And)?Serve\(/`)
	if got := hits(r); got != "admin/api:main.go:4" || !strings.Contains(r.Matches[0].Text, "http.ListenAndServe") {
		t.Errorf("regex: %s %+v", got, r.Matches)
	}
	_, r = search(e, `"listen carefully"`)
	if got := hits(r); got != "admin/api:docs/guide.md:3" {
		t.Errorf("phrase: %s", got)
	}
	_, r = search(e, "listen", "repo=bob/other")
	if got := hits(r); got != "bob/other:README:1" || r.Repositories != 1 {
		t.Errorf("scoped: %s", got)
	}
	if code, _ := search(asBob, "listen", "repo=admin/vault"); code != 404 {
		t.Errorf("scoped to an invisible repo: %d", code)
	}
	// limits: 100 per repository, then "truncated"
	_, r = search(e, "needle")
	if len(r.Matches) != 100 || !r.Truncated {
		t.Errorf("per-repo limit: %d %v", len(r.Matches), r.Truncated)
	}
	_, r = search(e, "needle", "limit=10")
	if len(r.Matches) != 10 || !r.Truncated {
		t.Errorf("limit=10: %d %v", len(r.Matches), r.Truncated)
	}
	for _, bad := range []string{"", "x", "/[unclosed/", "path:*.go"} {
		if code, _ := search(e, bad); code != 400 {
			t.Errorf("query %q: %d", bad, code)
		}
	}
	// empty repositories are skipped quietly
	e.api("POST", "/api/v1/repos", map[string]any{"name": "empty"}, nil)
	if code, _ := search(e, "listen"); code != 200 {
		t.Errorf("with an empty repo: %d", code)
	}
}
