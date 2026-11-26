package e2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"github.com/rs/zerolog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cooperspencer/forge/internal/config"
	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/sshd"
	gossh "golang.org/x/crypto/ssh"
)

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func newKey(t *testing.T, dir, name string) (gossh.Signer, string) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := gossh.NewSignerFromKey(priv)
	block, err := gossh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	os.WriteFile(path, pem.EncodeToMemory(block), 0o600)
	return signer, path
}

func TestSSH(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh client not installed")
	}
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.SSH.Listen = freePort(t)
	cfg.SSH.PublicAddr = cfg.SSH.Listen
	f, err := forge.New(cfg, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	srv, err := sshd.New(f)
	if err != nil {
		t.Fatal(err)
	}
	go srv.ListenAndServe()
	defer srv.Shutdown(context.Background())

	keys := t.TempDir()
	signer, keyPath := newKey(t, keys, "id_alice")
	ctx := context.Background()
	alice, err := f.Store.CreateUser(ctx, "alice", true, false)
	if err != nil {
		t.Fatal(err)
	}
	f.AddKey(ctx, alice, string(gossh.MarshalAuthorizedKey(signer.PublicKey())))

	// wait for listener
	for i := 0; i < 50; i++ {
		if c, err := net.Dial("tcp", cfg.SSH.Listen); err == nil {
			c.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	clientCfg := &gossh.ClientConfig{User: "git", Auth: []gossh.AuthMethod{gossh.PublicKeys(signer)}, HostKeyCallback: gossh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second}
	runCmd := func(cmd string) (string, error) {
		c, err := gossh.Dial("tcp", cfg.SSH.Listen, clientCfg)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		s, _ := c.NewSession()
		defer s.Close()
		out, err := s.CombinedOutput(cmd)
		return string(out), err
	}

	if out, _ := runCmd("whoami"); !strings.Contains(out, "alice (admin)") {
		t.Errorf("whoami: %q", out)
	}

	// git over SSH with push-to-create
	host, port, _ := net.SplitHostPort(cfg.SSH.Listen)
	sshCmd := fmt.Sprintf("ssh -i %s -p %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes -o LogLevel=ERROR", keyPath, port)
	work := t.TempDir()
	git(t, work, "init", "-q", "-b", "trunk")
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("hi\n"), 0o644)
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "first")
	url := fmt.Sprintf("ssh://git@%s:%s/alice/new.git", host, port)
	push := exec.Command("git", "push", "-q", url, "trunk")
	push.Dir = work
	push.Env = append(os.Environ(), "GIT_SSH_COMMAND="+sshCmd)
	out, err := push.CombinedOutput()
	if err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "created private repository alice/new") {
		t.Errorf("push-to-create message missing: %s", out)
	}
	r, err := f.Store.RepoByName(ctx, "alice", "new")
	if err != nil || !r.Private || r.DefaultBranch != "trunk" {
		t.Fatalf("repo after push: %+v %v", r, err)
	}

	clone := t.TempDir()
	cl := exec.Command("git", "clone", "-q", url, clone)
	cl.Env = push.Env
	if out, err := cl.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(clone, "a.txt")); string(b) != "hi\n" {
		t.Errorf("clone content %q", b)
	}

	// CLI round trip
	if out, err := runCmd(`issue create -t "Bug report" -b "it broke" alice/new`); err != nil || !strings.Contains(out, "opened #1") {
		t.Errorf("issue create: %v %s", err, out)
	}
	if out, _ := runCmd("issue list alice/new"); !strings.Contains(out, "Bug report") {
		t.Errorf("issue list: %s", out)
	}
	if out, _ := runCmd("repo log alice/new"); !strings.Contains(out, "first") {
		t.Errorf("repo log: %s", out)
	}
	if out, _ := runCmd("runner create builder docker,linux"); !strings.Contains(out, "fgr_") {
		t.Errorf("runner create: %s", out)
	}
	if out, err := runCmd("nope"); err == nil || !strings.Contains(out, "unknown command") {
		t.Errorf("unknown command: %v %s", err, out)
	}

	// anonymous key: read-only on public repos, cannot push
	other, otherPath := newKey(t, keys, "id_eve")
	_ = other
	anonCmd := strings.Replace(sshCmd, keyPath, otherPath, 1)
	ap := exec.Command("git", "push", "-q", fmt.Sprintf("ssh://git@%s:%s/alice/evil.git", host, port), "trunk")
	ap.Dir = work
	ap.Env = append(os.Environ(), "GIT_SSH_COMMAND="+anonCmd)
	if out, err := ap.CombinedOutput(); err == nil {
		t.Errorf("anonymous push succeeded: %s", out)
	}

	// TUI smoke test over a PTY
	c, err := gossh.Dial("tcp", cfg.SSH.Listen, clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, _ := c.NewSession()
	var buf syncBuf
	s.Stdout = &buf
	stdin, _ := s.StdinPipe()
	if err := s.RequestPty("xterm-256color", 40, 120, gossh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Shell(); err != nil {
		t.Fatal(err)
	}
	waitFor := func(substr string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(buf.String(), substr) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("TUI never showed %q; got:\n%q", substr, buf.String())
	}
	waitFor("alice/new")
	stdin.Write([]byte("\r")) // open repo
	waitFor("a.txt")
	stdin.Write([]byte("5")) // issues tab
	waitFor("Bug report")
	stdin.Write([]byte("q"))
	time.Sleep(200 * time.Millisecond) // separate reads; "qq" would arrive as one key event
	stdin.Write([]byte("q"))
	done := make(chan error, 1)
	go func() { done <- s.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("TUI did not exit on q")
	}
}

type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
