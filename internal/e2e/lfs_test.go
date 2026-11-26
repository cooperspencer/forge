package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cooperspencer/forge/internal/sshd"
	gossh "golang.org/x/crypto/ssh"
)

type lfsBatchResp struct {
	Message string
	Objects []struct {
		OID     string
		Size    int64
		Actions map[string]struct {
			Href   string
			Header map[string]string
		}
		Error *struct {
			Code    int
			Message string
		}
	}
}

func oidOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func pointer(content []byte) string {
	return fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oidOf(content), len(content))
}

// TestLFS speaks the Git LFS protocol the way git-lfs does: batch API,
// uploads and downloads, SSH authentication, permissions, forks, merged pull
// requests and cleanup.
func TestLFS(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	e := setup(t)
	ctx := context.Background()
	f := e.f

	// req sends an LFS request with the given Authorization header ("" = none)
	req := func(method, url, auth string, body []byte, hdr ...string) (int, []byte) {
		t.Helper()
		r, _ := http.NewRequest(method, url, bytes.NewReader(body))
		r.Header.Set("Accept", "application/vnd.git-lfs+json")
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	admin := "Bearer " + e.token
	batch := func(repo, auth, op string, content ...[]byte) (int, lfsBatchResp) {
		t.Helper()
		var objs []map[string]any
		for _, c := range content {
			objs = append(objs, map[string]any{"oid": oidOf(c), "size": len(c)})
		}
		b, _ := json.Marshal(map[string]any{"operation": op, "transfers": []string{"basic"}, "objects": objs})
		code, body := req("POST", e.srv.URL+"/"+repo+".git/info/lfs/objects/batch", auth, b)
		var out lfsBatchResp
		json.Unmarshal(body, &out)
		return code, out
	}
	upload := func(repo, auth string, content []byte) {
		t.Helper()
		code, res := batch(repo, auth, "upload", content)
		if code != 200 || len(res.Objects) != 1 {
			t.Fatalf("upload batch for %s: %d %+v", repo, code, res)
		}
		a, ok := res.Objects[0].Actions["upload"]
		if !ok {
			return // already there
		}
		if code, b := req("PUT", a.Href, a.Header["Authorization"], content); code != 200 {
			t.Fatalf("PUT %s: %d %s", a.Href, code, b)
		}
	}
	download := func(repo, auth string, content []byte) (int, []byte) {
		t.Helper()
		code, res := batch(repo, auth, "download", content)
		if code != 200 {
			return code, nil
		}
		o := res.Objects[0]
		if o.Error != nil {
			return o.Error.Code, nil
		}
		a := o.Actions["download"]
		return req("GET", a.Href, a.Header["Authorization"], nil)
	}

	big := bytes.Repeat([]byte("forge lfs test data\n"), 5000)
	if e.api("POST", "/api/v1/repos", map[string]any{"name": "assets"}, nil) != 201 {
		t.Fatal("create repo")
	}

	// LFS off: uploads are refused with a hint, downloads find nothing
	if code, res := batch("admin/assets", admin, "upload", big); code != 403 || !strings.Contains(res.Message, "not enabled") {
		t.Errorf("upload with LFS off: %d %q", code, res.Message)
	}
	var repo struct{ LFS bool }
	if e.api("PATCH", "/api/v1/repos/admin/assets", map[string]any{"lfs": true}, &repo); !repo.LFS {
		t.Fatal("enable LFS via API")
	}

	// upload: wrong content is rejected, right content stored
	code, res := batch("admin/assets", admin, "upload", big)
	a := res.Objects[0].Actions["upload"]
	if code != 200 || !strings.HasPrefix(a.Href, e.srv.URL+"/admin/assets.git/info/lfs/objects/") || a.Header["Authorization"] != admin {
		t.Fatalf("upload batch: %d %+v", code, res)
	}
	if code, _ := req("PUT", a.Href, admin, []byte("not the content")); code != 422 {
		t.Errorf("mismatched upload: %d", code)
	}
	if code, b := req("PUT", a.Href, admin, big); code != 200 {
		t.Fatalf("upload: %d %s", code, b)
	}
	if _, res := batch("admin/assets", admin, "upload", big); len(res.Objects[0].Actions) != 0 {
		t.Errorf("stored object asked for again: %+v", res)
	}
	if code, b := download("admin/assets", admin, big); code != 200 || !bytes.Equal(b, big) {
		t.Errorf("download: %d, %d bytes", code, len(b))
	}
	// resumed downloads (Range)
	if code, b := req("GET", e.srv.URL+"/admin/assets.git/info/lfs/objects/"+oidOf(big), admin, nil, "Range", "bytes=10-19"); code != 206 || string(b) != string(big[10:20]) {
		t.Errorf("range download: %d %q", code, b)
	}
	// the remote URL without .git works too
	if code, _ := req("POST", e.srv.URL+"/admin/assets/info/lfs/locks/verify", admin, []byte(`{}`)); code != 200 {
		t.Errorf("locks/verify: %d", code)
	}
	if code, _ := download("admin/assets", admin, []byte("never uploaded")); code != 404 {
		t.Errorf("missing object: %d", code)
	}

	// anonymous: may read a public repo, must authenticate to write
	if code, b := download("admin/assets", "", big); code != 200 || !bytes.Equal(b, big) {
		t.Errorf("anonymous download: %d", code)
	}
	if code, _ := batch("admin/assets", "", "upload", []byte("x")); code != 401 {
		t.Errorf("anonymous upload: %d", code)
	}
	// a reader can't upload
	bob, _ := f.Store.CreateUser(ctx, "bob", false, false)
	bobTok, _ := f.Store.CreateAccessToken(ctx, bob.ID, "t")
	if code, _ := batch("admin/assets", "Bearer "+bobTok, "upload", []byte("x")); code != 403 {
		t.Errorf("reader upload: %d", code)
	}
	if code, _ := req("PUT", e.srv.URL+"/admin/assets.git/info/lfs/objects/"+oidOf([]byte("x")), "Bearer "+bobTok, []byte("x")); code != 403 {
		t.Errorf("reader PUT: %d", code)
	}

	// knowing a hash grants nothing: another repo can't serve the object
	// until it was uploaded there
	e.api("POST", "/api/v1/repos", map[string]any{"name": "other", "lfs": true}, nil)
	if code, _ := download("admin/other", admin, big); code != 404 {
		t.Errorf("object leaked into another repo: %d", code)
	}
	if _, res := batch("admin/other", admin, "upload", big); res.Objects[0].Actions["upload"].Href == "" {
		t.Error("other repo must prove it has the content")
	}

	// a private repo is hidden from others
	e.api("POST", "/api/v1/repos", map[string]any{"name": "secret", "private": true, "lfs": true}, nil)
	upload("admin/secret", admin, big)
	if code, _ := batch("admin/secret", "Bearer "+bobTok, "download", big); code != 404 {
		t.Errorf("private repo LFS visible to a stranger: %d", code)
	}

	// SSH: git-lfs-authenticate hands out a token for that repo only
	f.Cfg.SSH.Listen = freePort(t)
	srv, err := sshd.New(f)
	if err != nil {
		t.Fatal(err)
	}
	go srv.ListenAndServe()
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	signer, _ := newKey(t, t.TempDir(), "id")
	if err := f.AddKey(ctx, e.admin, string(gossh.MarshalAuthorizedKey(signer.PublicKey()))); err != nil {
		t.Fatal(err)
	}
	var out string
	for i := 0; i < 50; i++ {
		if out, err = sshRun(f.Cfg.SSH.Listen, "git-lfs-authenticate admin/assets.git download", signer); err == nil || !strings.Contains(err.Error(), "connect") {
			break
		}
	}
	var auth struct {
		Href   string
		Header map[string]string
	}
	if err := json.Unmarshal([]byte(out), &auth); err != nil || auth.Href != e.srv.URL+"/admin/assets.git/info/lfs" || !strings.HasPrefix(auth.Header["Authorization"], "Bearer lfs.") {
		t.Fatalf("git-lfs-authenticate: %v %q", err, out)
	}
	sshTok := auth.Header["Authorization"]
	if code, b := download("admin/assets", sshTok, big); code != 200 || !bytes.Equal(b, big) {
		t.Errorf("download with SSH token: %d", code)
	}
	if code, _ := batch("admin/assets", sshTok, "upload", []byte("y")); code != 403 {
		t.Errorf("download token used for upload: %d", code)
	}
	if code, _ := batch("admin/other", sshTok, "download", big); code != 404 {
		t.Errorf("token for another repo: %d", code)
	}
	if code := func() int { c, _ := req("GET", e.srv.URL+"/api/v1/user", sshTok, nil); return c }(); code != 401 {
		t.Errorf("LFS token accepted by the REST API: %d", code)
	}
	out, _ = sshRun(f.Cfg.SSH.Listen, "git-lfs-authenticate admin/assets upload", signer)
	json.Unmarshal([]byte(out), &auth)
	upTok := auth.Header["Authorization"]
	upload("admin/assets", upTok, []byte("uploaded over ssh auth"))
	e.api("PATCH", "/api/v1/repos/admin/other", map[string]any{"lfs": false}, nil)
	if out, err := sshRun(f.Cfg.SSH.Listen, "git-lfs-authenticate admin/other upload", signer); err == nil || !strings.Contains(out, "not enabled") {
		t.Errorf("upload auth with LFS off: %v %q", err, out)
	}

	// git history with a pointer, then a fork and a pull request from it
	work := t.TempDir()
	write := func(name, body string) { os.WriteFile(filepath.Join(work, name), []byte(body), 0o644) }
	git(t, work, "init", "-q", "-b", "main")
	write(".gitattributes", "*.bin filter=lfs diff=lfs merge=lfs -text\n")
	write("big.bin", pointer(big))
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "add big file")
	e.pushAs(t, work, "admin", e.token, "admin/assets", "main")

	base, _ := f.Store.RepoByName(ctx, "admin", "assets")
	fork, err := f.Fork(ctx, bob, base, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if !fork.LFS {
		t.Error("fork should inherit the LFS setting")
	}
	if code, b := download("bob/assets", "Bearer "+bobTok, big); code != 200 || !bytes.Equal(b, big) {
		t.Errorf("fork serves the parent's objects: %d", code)
	}
	more := []byte("added in the fork")
	upload("bob/assets", "Bearer "+bobTok, more)
	git(t, work, "checkout", "-q", "-b", "feature")
	write("more.bin", pointer(more))
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "more")
	e.pushAs(t, work, "bob", bobTok, "bob/assets", "feature")
	if code, _ := download("admin/assets", admin, more); code != 404 {
		t.Errorf("fork's object visible in the parent before merging: %d", code)
	}
	pr, err := f.OpenPR(ctx, bob, base, "main", fork, "feature", "more", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.MergePR(ctx, e.admin, base, pr, "merge"); err != nil {
		t.Fatal(err)
	}
	if code, b := download("admin/assets", admin, more); code != 200 || !bytes.Equal(b, more) {
		t.Errorf("merged pull request's LFS object: %d", code)
	}

	// deleting repositories removes objects nobody references any more
	path := func(c []byte) string {
		o := oidOf(c)
		return filepath.Join(f.Cfg.DataDir, "lfs", "objects", o[:2], o[2:4], o)
	}
	if _, err := os.Stat(path(big)); err != nil {
		t.Fatalf("object file: %v", err)
	}
	for _, name := range []string{"admin/assets", "admin/secret"} {
		if e.api("DELETE", "/api/v1/repos/"+name, nil, nil) != 204 {
			t.Fatalf("delete %s", name)
		}
	}
	if _, err := os.Stat(path(big)); err != nil {
		t.Error("object still used by the fork was deleted")
	}
	if err := f.DeleteRepo(ctx, e.admin, fork); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path(big)); !os.IsNotExist(err) {
		t.Errorf("unreferenced object kept: %v", err)
	}
	if _, err := os.Stat(path(more)); !os.IsNotExist(err) {
		t.Errorf("unreferenced object kept: %v", err)
	}
}
