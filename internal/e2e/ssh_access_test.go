package e2e

import (
	"context"
	"github.com/rs/zerolog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/cooperspencer/forge/internal/config"
	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/sshd"
	"github.com/cooperspencer/forge/internal/store"
	gossh "golang.org/x/crypto/ssh"
)

func startSSH(t *testing.T, mutate func(*config.Config)) (*forge.Forge, string) {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.SSH.Listen = freePort(t)
	mutate(cfg)
	f, err := forge.New(cfg, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := sshd.New(f)
	if err != nil {
		t.Fatal(err)
	}
	go srv.ListenAndServe()
	t.Cleanup(func() { srv.Shutdown(context.Background()); f.Close() })
	for i := 0; i < 50; i++ {
		if c, err := net.Dial("tcp", cfg.SSH.Listen); err == nil {
			c.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return f, cfg.SSH.Listen
}

// sshRun connects offering the given keys in order and runs cmd.
func sshRun(addr, cmd string, keys ...gossh.Signer) (string, error) {
	c, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User: "git", Auth: []gossh.AuthMethod{gossh.PublicKeys(keys...)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second,
	})
	if err != nil {
		return "", err
	}
	defer c.Close()
	s, err := c.NewSession()
	if err != nil {
		return "", err
	}
	out, err := s.CombinedOutput(cmd)
	return string(out), err
}

func TestSSHUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	known, _ := newKey(t, dir, "known")
	unknown, _ := newKey(t, dir, "unknown")
	addUser := func(f *forge.Forge) {
		u, _ := f.Store.CreateUser(context.Background(), "alice", true, false)
		f.AddKey(context.Background(), u, string(gossh.MarshalAuthorizedKey(known.PublicKey())))
	}

	t.Run("closed instance rejects unknown keys at login", func(t *testing.T) {
		f, addr := startSSH(t, func(c *config.Config) { c.AnonymousRead, c.AllowRegistration = false, false })
		addUser(f)
		if _, err := sshRun(addr, "whoami", unknown); err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
			t.Fatalf("unknown key: %v", err)
		}
		out, err := sshRun(addr, "whoami", known)
		if err != nil || !strings.Contains(out, "alice") {
			t.Fatalf("known key: %q %v", out, err)
		}
		// an agent offering an unregistered key first still logs in with the next one
		out, err = sshRun(addr, "whoami", unknown, known)
		if err != nil || !strings.Contains(out, "alice") {
			t.Fatalf("unknown then known key: %q %v", out, err)
		}
	})

	t.Run("fresh install accepts the first key so an admin can register", func(t *testing.T) {
		_, addr := startSSH(t, func(c *config.Config) { c.AnonymousRead, c.AllowRegistration = false, false })
		if _, err := sshRun(addr, "version", unknown); err != nil {
			t.Fatalf("fresh install rejected the first key: %v", err)
		}
	})

	t.Run("open registration accepts unknown keys", func(t *testing.T) {
		f, addr := startSSH(t, func(c *config.Config) { c.AnonymousRead, c.AllowRegistration = false, true })
		addUser(f)
		if _, err := sshRun(addr, "version", unknown); err != nil {
			t.Fatalf("unknown key with registration open: %v", err)
		}
		// but it can't read anything
		if _, err := f.CreateRepo(context.Background(), mustUser(t, f, "alice"), "alice", "pub", "", false); err != nil {
			t.Fatal(err)
		}
		if out, _ := sshRun(addr, "repo info alice/pub", unknown); !strings.Contains(out, "not found") {
			t.Errorf("unknown key read a repo with anonymous_read off: %q", out)
		}
	})

	t.Run("anonymous_read keeps public browsing", func(t *testing.T) {
		f, addr := startSSH(t, func(c *config.Config) { c.AnonymousRead = true })
		addUser(f)
		if _, err := sshRun(addr, "version", unknown); err != nil {
			t.Fatalf("anonymous_read on: %v", err)
		}
	})
}

func mustUser(t *testing.T, f *forge.Forge, name string) *store.User {
	u, err := f.Store.UserByName(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestNoAnonymousHTTP: with anonymous_read off, nothing is served over HTTP
// without credentials — git, API, container registry, generic packages.
func TestNoAnonymousHTTP(t *testing.T) {
	e := setup(t)
	e.f.Cfg.AnonymousRead = false
	ctx := context.Background()
	e.f.CreateRepo(ctx, e.admin, "admin", "pub", "", false)
	c := &reg{t: t, base: e.srv.URL, user: "admin", pass: e.token}
	c.do("PUT", "/api/packages/admin/generic/pub/1/f", []byte("x"), nil)
	anon := &reg{t: t, base: e.srv.URL}
	for _, p := range []string{
		"/api/v1/repos", "/api/v1/repos/admin/pub", "/api/v1/users/admin", "/api/v1/packages",
		"/admin/pub.git/info/refs?service=git-upload-pack",
		"/v2/", "/v2/admin/pub/tags/list", "/v2/_catalog",
		"/api/packages/admin/generic/pub/1/f",
	} {
		if resp, _ := anon.do("GET", p, nil, nil); resp.StatusCode != 401 {
			t.Errorf("anonymous GET %s: %d, want 401", p, resp.StatusCode)
		}
	}
	if resp, _ := c.do("GET", "/api/v1/repos/admin/pub", nil, nil); resp.StatusCode != 200 {
		t.Errorf("authenticated request: %d", resp.StatusCode)
	}
}
