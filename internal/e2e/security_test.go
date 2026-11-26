package e2e

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// TestGitOptionInjection: user-supplied refs must never be parsed as git
// options (here `git log --output=<file>` would write a file on the server).
func TestGitOptionInjection(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if c := e.api("POST", "/api/v1/repos", map[string]any{"name": "pub"}, nil); c != 201 {
		t.Fatal(c)
	}
	r, _ := e.f.Store.RepoByName(ctx, "admin", "pub")
	work := t.TempDir()
	git(t, work, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(work, "a"), []byte("x"), 0o644)
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "c")
	git(t, work, "push", "-q", e.f.RepoPath(r), "main")
	target := filepath.Join(t.TempDir(), "pwned")
	for _, path := range []string{
		"/api/v1/repos/admin/pub/commits?ref=" + url.QueryEscape("--output="+target),
		"/api/v1/repos/admin/pub/commits/" + url.PathEscape("--output="+target),
		"/api/v1/repos/admin/pub/tree?ref=" + url.QueryEscape("--output="+target),
	} {
		resp, err := http.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Errorf("%s: status %d, want an error", path, resp.StatusCode)
		}
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("a ref was interpreted as a git option: file written on the server")
	}
}
