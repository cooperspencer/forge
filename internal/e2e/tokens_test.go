package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/store"
)

// TestScopedTokens: tokens are limited by their scopes, their repositories
// and their expiry, over the API, git, LFS and the package registry.
func TestScopedTokens(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	e := setup(t)
	ctx := context.Background()
	f := e.f

	app, _ := f.CreateRepoWith(ctx, e.admin, "admin", "app", "", forge.RepoOptions{Private: true, LFS: true})
	f.CreateRepo(ctx, e.admin, "admin", "other", "", true)
	bob, _ := f.Store.CreateUser(ctx, "bob", false, false)
	f.CreateRepo(ctx, bob, "bob", "secret", "", true)
	f.CreateRepo(ctx, bob, "bob", "open", "", false)

	work := t.TempDir()
	git(t, work, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(work, "f.txt"), []byte("one"), 0o644)
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "one")
	e.pushAs(t, work, "admin", e.token, "admin/app", "main")

	mk := func(scopes, repos string, exp time.Time) string {
		t.Helper()
		g, err := forge.GrantFrom(scopes, repos)
		if err != nil {
			t.Fatal(err)
		}
		tok, err := f.CreateToken(ctx, e.admin, forge.TokenSpec{Name: scopes, Grant: g, Expires: exp})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	call := func(tok, method, path string, body any) (int, string) {
		t.Helper()
		var r io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			r = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, e.srv.URL+path, r)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	expect := func(tok, method, path string, body any, code int, contains string) {
		t.Helper()
		got, b := call(tok, method, path, body)
		if got != code || !strings.Contains(b, contains) {
			t.Errorf("%s %s: %d %s (want %d %q)", method, path, got, strings.TrimSpace(b), code, contains)
		}
	}
	gitURL := func(tok, repo string) string {
		return strings.Replace(e.srv.URL, "http://", "http://admin:"+tok+"@", 1) + "/" + repo + ".git"
	}
	const A = "/api/v1/repos/admin/app"

	// --- read-only code access to one repository
	ro := mk("code:read", "admin/app", time.Now().Add(time.Hour))
	expect(ro, "GET", A+"/branches", nil, 200, "main")
	expect(ro, "GET", A+"/raw?path=f.txt", nil, 200, "one")
	expect(ro, "GET", "/api/v1/repos/admin/other/branches", nil, 404, "")
	expect(ro, "GET", "/api/v1/repos/bob/open/branches", nil, 200, "") // public: like anonymous
	expect(ro, "GET", A+"/issues", nil, 403, "lacks the issues:read scope")
	expect(ro, "POST", A+"/issues", map[string]string{"title": "x"}, 403, "issues:write")
	expect(ro, "PATCH", A, map[string]any{"description": "x"}, 403, "lacks the admin scope")
	expect(ro, "GET", "/api/v1/runners", nil, 403, "site-admin")
	if _, b := call(ro, "GET", "/api/v1/repos", nil); !strings.Contains(b, `"admin/app"`) || strings.Contains(b, "admin/other") {
		t.Errorf("repo list of a one-repo token: %s", b)
	}
	clone := t.TempDir()
	if out, err := runGit(clone, "clone", "-q", gitURL(ro, "admin/app"), "c"); err != nil {
		t.Errorf("clone with code:read: %v\n%s", err, out)
	}
	if out, err := runGit(clone, "clone", "-q", gitURL(ro, "admin/other"), "o"); err == nil {
		t.Errorf("cloned a repository outside the token's list:\n%s", out)
	}
	if out, err := runGit(work, "push", gitURL(ro, "admin/app"), "main:ro"); err == nil || !strings.Contains(out, "code:write") {
		t.Errorf("push with code:read: %v\n%s", err, out)
	}
	lfsBatch := func(tok, op string) (int, string) {
		b, _ := json.Marshal(map[string]any{"operation": op, "objects": []map[string]any{{"oid": strings.Repeat("a", 64), "size": 1}}})
		req, _ := http.NewRequest("POST", e.srv.URL+"/admin/app.git/info/lfs/objects/batch", bytes.NewReader(b))
		req.Header.Set("Accept", "application/vnd.git-lfs+json")
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if code, b := lfsBatch(ro, "upload"); code != 403 {
		t.Errorf("LFS upload with code:read: %d %s", code, b)
	}
	if u, _ := f.Store.UserByToken(ctx, ro); f.Store.AccessLevel(ctx, u, app) != store.AccessRead {
		t.Errorf("a code:read token on a repository its user owns: %v", f.Store.AccessLevel(ctx, u, app))
	}
	// write access for issues doesn't make code writable
	mixed := mk("code:read issues:write", "", time.Time{})
	if code, b := lfsBatch(mixed, "upload"); code != 403 {
		t.Errorf("LFS upload with code:read issues:write: %d %s", code, b)
	}
	if out, err := runGit(work, "push", gitURL(mixed, "admin/app"), "main:mixed"); err == nil || !strings.Contains(out, "code:write") {
		t.Errorf("push with code:read issues:write: %v\n%s", err, out)
	}

	// --- git push, nothing else
	pusher := mk("code:write", "", time.Time{})
	if out, err := runGit(work, "push", gitURL(pusher, "admin/app"), "main:pushed"); err != nil {
		t.Errorf("push with code:write: %v\n%s", err, out)
	}
	if code, b := lfsBatch(pusher, "upload"); code != 200 {
		t.Errorf("LFS upload with code:write: %d %s", code, b)
	}
	expect(pusher, "POST", "/api/v1/repos", map[string]any{"name": "new"}, 403, "admin")
	expect(pusher, "GET", A+"/runs", nil, 403, "ci:read")

	// --- issues only
	iss := mk("issues:write", "admin/*", time.Time{})
	expect(iss, "POST", A+"/issues", map[string]string{"title": "from a token"}, 201, "from a token")
	expect(iss, "GET", A+"/tree", nil, 403, "code:read")
	if out, err := runGit(clone, "clone", "-q", gitURL(iss, "admin/app"), "i"); err == nil || !strings.Contains(out, "code:read") {
		t.Errorf("clone with an issues token: %v\n%s", err, out)
	}

	// --- admin scope, but not site admin: bob's private repo is out of reach
	adm := mk("admin code:read", "", time.Time{})
	expect(adm, "PATCH", A, map[string]any{"description": "set by token"}, 200, "set by token")
	expect(adm, "GET", "/api/v1/repos/bob/secret/branches", nil, 404, "")
	expect(adm, "POST", "/api/v1/repos", map[string]any{"name": "made-by-token"}, 201, "made-by-token")
	site := mk("site-admin code:read", "", time.Time{})
	expect(site, "GET", "/api/v1/repos/bob/secret/branches", nil, 200, "")
	expect(site, "GET", "/api/v1/runners", nil, 200, "")
	// a repo-limited admin token can't create repositories elsewhere
	limited := mk("admin", "admin/app", time.Time{})
	expect(limited, "POST", "/api/v1/repos", map[string]any{"name": "nope"}, 403, "")
	// the full-access token (the old kind) still does everything
	expect(e.token, "GET", "/api/v1/repos/bob/secret/branches", nil, 200, "")

	// --- packages
	pkg := mk("packages:write", "", time.Time{})
	expect(pkg, "PUT", "/api/packages/admin/generic/tool/1.0/tool.txt", "bits", 201, "")
	expect(ro, "PUT", "/api/packages/admin/generic/tool/1.1/tool.txt", "bits", 403, "")
	expect(pkg, "GET", "/api/packages/admin/generic/tool/1.0/tool.txt", nil, 200, "bits")
	expect(pusher, "GET", "/api/packages/admin/generic/tool/1.0/tool.txt", nil, 404, "")
	readPkg := mk("packages:read", "", time.Time{})
	expect(readPkg, "DELETE", "/api/packages/admin/generic/tool/1.0", nil, 403, "")

	// --- expiry
	old, _ := f.Store.CreateToken(ctx, e.admin.ID, store.NewToken{Name: "old", Expires: time.Now().Add(-time.Minute)})
	expect(old, "GET", "/api/v1/user", nil, 401, "token expired")
	if out, err := runGit(clone, "clone", "-q", gitURL(old, "admin/app"), "x"); err == nil {
		t.Errorf("clone with an expired token:\n%s", out)
	}
	if _, err := f.CreateToken(ctx, e.admin, forge.TokenSpec{Name: "past", Expires: time.Now().Add(-time.Hour)}); err == nil {
		t.Error("created a token that is already expired")
	}
	toks, _ := f.Store.AccessTokens(ctx, e.admin.ID)
	var sawRO bool
	for _, tk := range toks {
		if tk.Name == "code:read" {
			sawRO = tk.Grant.String() == "code:read" && strings.Join(tk.Grant.Repos, ",") == "admin/app" && !tk.ExpiresAt.IsZero() && !tk.Expired()
		}
		if tk.Name == "old" && !tk.Expired() {
			t.Error("old token not shown as expired")
		}
	}
	if !sawRO {
		t.Errorf("token listing: %+v", toks)
	}

	// --- who may create what
	if _, err := f.CreateToken(ctx, bob, forge.TokenSpec{Name: "x", Grant: &store.Grant{Scopes: map[string]store.Access{store.ScopeSite: store.AccessAdmin}}}); err == nil {
		t.Error("a non-admin created a site-admin token")
	}
	u, _ := f.Store.UserByToken(ctx, ro)
	if _, err := f.CreateToken(ctx, u, forge.TokenSpec{Name: "child"}); err == nil {
		t.Error("a token minted a token")
	}
}
