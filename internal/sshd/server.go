// Package sshd serves the TUI, the SSH command-line interface and the git
// transport (upload-pack / receive-pack) on a single SSH port.
package sshd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	bm "github.com/charmbracelet/wish/bubbletea"
	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/gitx"
	"github.com/cooperspencer/forge/internal/store"
	"github.com/cooperspencer/forge/internal/tui"
	"github.com/muesli/termenv"
	gossh "golang.org/x/crypto/ssh"
)

type Server struct {
	F   *forge.Forge
	srv *ssh.Server
}

func New(f *forge.Forge) (*Server, error) {
	s := &Server{F: f}
	opts := []ssh.Option{
		wish.WithAddress(f.Cfg.SSH.Listen),
		wish.WithHostKeyPath(f.Cfg.Path("ssh_host_ed25519")),
		wish.WithPublicKeyAuth(s.allowKey),
		// Allow key-less anonymous browsing.
		wish.WithKeyboardInteractiveAuth(func(ctx ssh.Context, ch gossh.KeyboardInteractiveChallenge) bool {
			return f.Cfg.AnonymousRead
		}),
		wish.WithMiddleware(s.handle),
	}
	if f.Cfg.SSH.IdleTimeout > 0 {
		opts = append(opts, wish.WithIdleTimeout(time.Duration(f.Cfg.SSH.IdleTimeout)*time.Second))
	}
	srv, err := wish.NewServer(opts...)
	if err != nil {
		return nil, err
	}
	s.srv = srv
	return s, nil
}

// allowKey decides at authentication time. Registered keys are always
// accepted. Unknown keys get in only when they have something to do: browse
// public content (anonymous_read), sign up (allow_registration), or create
// the first account of a fresh install. Otherwise the client sees a normal
// "Permission denied (publickey)" and tries its next key.
func (s *Server) allowKey(ctx ssh.Context, key ssh.PublicKey) bool {
	if _, err := s.F.UserForKey(ctx, key); err == nil {
		return true
	}
	if s.F.Cfg.AnonymousRead || s.F.Cfg.AllowRegistration {
		return true
	}
	if n, err := s.F.Store.CountUsers(ctx); err == nil && n == 0 {
		return true
	}
	s.F.Log.Debug().Str("fingerprint", forge.Fingerprint(key)).Str("remote", ctx.RemoteAddr().String()).Msg("ssh: rejected unknown key")
	return false
}

func (s *Server) ListenAndServe() error {
	err := s.srv.ListenAndServe()
	if errors.Is(err, ssh.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }

func (s *Server) user(sess ssh.Session) *store.User {
	pk := sess.PublicKey()
	if pk == nil {
		return nil
	}
	u, err := s.F.UserForKey(sess.Context(), pk)
	if err != nil {
		return nil
	}
	return u
}

func (s *Server) handle(next ssh.Handler) ssh.Handler {
	return func(sess ssh.Session) {
		defer next(sess)
		u := s.user(sess)
		cmd := sess.Command()
		who := "anonymous"
		if u != nil {
			who = u.Name
		}
		if len(cmd) > 0 && strings.HasPrefix(cmd[0], "git-") {
			s.F.Log.Info().Str("user", who).Str("cmd", strings.Join(cmd, " ")).Str("remote", sess.RemoteAddr().String()).Msg("git")
			code := s.git(sess, u, cmd)
			sess.Exit(code)
			return
		}
		if len(cmd) > 0 {
			s.F.Log.Info().Str("user", who).Str("cmd", cmd[0]).Str("remote", sess.RemoteAddr().String()).Msg("cli")
			cli := &CLI{F: s.F, User: u, PubKey: sess.PublicKey(), In: sess, Out: sess, Err: sess.Stderr(), Ctx: sess.Context()}
			sess.Exit(cli.Run(cmd))
			return
		}
		s.tui(sess, u)
	}
}

// ---------------------------------------------------------------- TUI

func (s *Server) tui(sess ssh.Session, u *store.User) {
	_, winCh, ok := sess.Pty()
	if !ok {
		fmt.Fprintln(sess, "No terminal requested. Use `ssh -t` for the TUI or pass a command (try `help`).")
		sess.Exit(1)
		return
	}
	s.F.Log.Info().Str("user", nameOf(u)).Str("remote", sess.RemoteAddr().String()).Msg("tui session")
	r := makeRenderer(sess)
	ctx := &tui.Ctx{F: s.F, User: u, PubKey: sess.PublicKey(), St: tui.NewStyles(r), Ctx: sess.Context()}
	var first tui.Page
	if u == nil && sess.PublicKey() != nil {
		n, _ := s.F.Store.CountUsers(sess.Context())
		if n == 0 || s.F.Cfg.AllowRegistration {
			first = tui.NewRegisterPage(ctx, n == 0)
		}
	}
	if first == nil {
		if u == nil && !s.F.Cfg.AnonymousRead {
			fmt.Fprintln(sess, "Access denied: your key is not registered.")
			if sess.PublicKey() != nil {
				fmt.Fprintln(sess, "Key fingerprint:", forge.Fingerprint(sess.PublicKey()))
			}
			sess.Exit(1)
			return
		}
		first = tui.NewHome(ctx)
	}
	app := tui.New(ctx, first)
	defer app.Close()
	opts := append(bm.MakeOptions(sess), tea.WithAltScreen())
	p := tea.NewProgram(app, opts...)
	cctx, cancel := context.WithCancel(sess.Context())
	defer cancel()
	go func() {
		for {
			select {
			case <-cctx.Done():
				p.Quit()
				return
			case w := <-winCh:
				p.Send(tea.WindowSizeMsg{Width: w.Width, Height: w.Height})
			}
		}
	}()
	if _, err := p.Run(); err != nil {
		s.F.Log.Error().Err(err).Msg("tui")
	}
	p.Kill()
}

func nameOf(u *store.User) string {
	if u == nil {
		return "anonymous"
	}
	return u.Name
}

// ---------------------------------------------------------------- git

func (s *Server) git(sess ssh.Session, u *store.User, cmd []string) int {
	ctx := sess.Context()
	stderr := sess.Stderr()
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "forge: "+format+"\n", a...)
		return 1
	}
	switch cmd[0] {
	case "git-lfs-authenticate":
		return s.lfsAuthenticate(sess, u, cmd)
	case "git-lfs-transfer":
		// the pure-SSH LFS protocol; the client falls back to HTTP via
		// git-lfs-authenticate when this fails
		return fail("git-lfs-transfer is not supported, use git-lfs-authenticate")
	}
	if len(cmd) != 2 {
		return fail("invalid git command")
	}
	service := cmd[0]
	var need store.Access
	switch service {
	case "git-upload-pack", "git-upload-archive":
		need = store.AccessRead
	case "git-receive-pack":
		need = store.AccessWrite
	default:
		return fail("unsupported command %s", service)
	}
	full := strings.TrimSuffix(strings.Trim(cmd[1], "/'\" "), ".git")
	full, isWiki := cutWiki(full)
	r, _, err := s.F.ResolveRepo(ctx, u, full, need)
	if errors.Is(err, store.ErrNotFound) && service == "git-receive-pack" && u != nil && !isWiki {
		// push-to-create: pushing to a missing repo you could own creates it (private by default)
		owner, name, _ := strings.Cut(full, "/")
		r, err = s.F.CreateRepo(ctx, u, owner, name, "", true)
		if err == nil {
			fmt.Fprintf(stderr, "forge: created private repository %s\n", r.FullName())
		}
	}
	if err != nil {
		if errors.Is(err, forge.ErrForbidden) {
			return fail("permission denied for %s", full)
		}
		if errors.Is(err, store.ErrNotFound) {
			return fail("repository %s not found", full)
		}
		return fail("%v", err)
	}
	path := s.F.RepoPath(r)
	g := s.F.Git(r)
	if service == "git-receive-pack" && r.Mirror && !isWiki {
		return fail("%v", s.F.ReadOnlyError(ctx, r))
	}
	if isWiki {
		path = s.F.WikiPath(r)
		if service == "git-receive-pack" {
			if err := s.F.EnsureWiki(ctx, r); err != nil {
				return fail("%v", err)
			}
		} else if _, err := os.Stat(path); err != nil {
			return fail("the wiki of %s has no pages yet", r.FullName())
		}
		g = gitx.Open(path)
	}
	var before map[string]string
	if service == "git-receive-pack" {
		before, _ = g.RefMap(ctx)
	}
	args := []string{}
	var hookEnv []string
	if service == "git-receive-pack" {
		// refs/pull/* are managed by forge; refuse client updates to them.
		args = append(args, "-c", "receive.hideRefs=refs/pull")
		if !isWiki {
			cfg, env := s.F.PushHook(ctx, u, r) // branch protection
			for _, kv := range cfg {
				args = append(args, "-c", kv)
			}
			hookEnv = env
		}
	}
	args = append(args, strings.TrimPrefix(service, "git-"), path)
	c := exec.CommandContext(ctx, "git", args...)
	c.Stdin, c.Stdout, c.Stderr = sess, sess, stderr
	c.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), hookEnv...)
	for _, e := range sess.Environ() {
		if strings.HasPrefix(e, "GIT_PROTOCOL=") {
			c.Env = append(c.Env, e)
		}
	}
	err = c.Run()
	if service == "git-receive-pack" {
		after, _ := g.RefMap(context.Background())
		ups := forge.DiffRefs(before, after)
		if len(ups) > 0 && isWiki {
			s.F.WikiPostPush(context.Background(), u, r)
		} else if len(ups) > 0 {
			// Run post-receive work detached from the session so a client
			// disconnect doesn't abort CI dispatch.
			s.F.PostReceive(context.Background(), u, r, ups)
			s.printPushHints(stderr, r, ups)
		}
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return 1
	}
	return 0
}

func (s *Server) printPushHints(w io.Writer, r *store.Repo, ups []forge.RefUpdate) {
	ctx := context.Background()
	runs := 0
	for _, u := range ups {
		if rs, _ := s.F.Store.RunsForSHA(ctx, r.ID, u.New); len(rs) > 0 {
			runs += len(rs)
		}
	}
	if runs > 0 {
		fmt.Fprintf(w, "forge: %d pipeline run(s) started — watch with: %s ci runs %s\n", runs, s.F.Cfg.SSHCommand(), r.FullName())
	}
	for _, u := range ups {
		if strings.HasPrefix(u.Ref, "refs/heads/") && u.Old == "0000000000000000000000000000000000000000" {
			br := strings.TrimPrefix(u.Ref, "refs/heads/")
			if br != r.DefaultBranch {
				fmt.Fprintf(w, "forge: open a pull request: %s pr create %s %s\n", s.F.Cfg.SSHCommand(), r.FullName(), br)
			}
		}
	}
}

// makeRenderer builds a colour-aware renderer from the client's TERM and
// environment. We deliberately do not query the terminal (OSC 11): clients
// that never answer (tmux, mosh, scripts) would block the session.
// Clients can hint a light theme with `ssh -o SetEnv=FORGE_THEME=light` or
// COLORFGBG.
func makeRenderer(sess ssh.Session) *lipgloss.Renderer {
	pty, _, _ := sess.Pty()
	env := sshEnviron(append(sess.Environ(), "TERM="+pty.Term))
	if pty.Term == "" || pty.Term == "dumb" {
		return lipgloss.NewRenderer(sess, termenv.WithProfile(termenv.Ascii))
	}
	r := lipgloss.NewRenderer(sess, termenv.WithEnvironment(env), termenv.WithUnsafe(), termenv.WithColorCache(true))
	dark := true
	if t := env.Getenv("FORGE_THEME"); t == "light" {
		dark = false
	} else if fgbg := env.Getenv("COLORFGBG"); fgbg != "" {
		parts := strings.Split(fgbg, ";")
		if bg := parts[len(parts)-1]; bg == "7" || bg == "15" {
			dark = false
		}
	}
	r.SetHasDarkBackground(dark)
	return r
}

type sshEnviron []string

func (e sshEnviron) Environ() []string { return e }
func (e sshEnviron) Getenv(k string) string {
	for i := len(e) - 1; i >= 0; i-- {
		if strings.HasPrefix(e[i], k+"=") {
			return e[i][len(k)+1:]
		}
	}
	return ""
}

// lfsAuthenticate implements `git-lfs-authenticate <repo> <download|upload>`:
// it tells git-lfs where the LFS API is and hands out a short-lived token,
// so LFS works with nothing but the SSH key.
func (s *Server) lfsAuthenticate(sess ssh.Session, u *store.User, cmd []string) int {
	ctx := sess.Context()
	fail := func(format string, a ...any) int {
		fmt.Fprintf(sess.Stderr(), "forge: "+format+"\n", a...)
		return 1
	}
	if len(cmd) < 3 || (cmd[2] != "download" && cmd[2] != "upload") {
		return fail("usage: git-lfs-authenticate <repo> download|upload")
	}
	write := cmd[2] == "upload"
	need := store.AccessRead
	if write {
		need = store.AccessWrite
	}
	full := strings.Trim(cmd[1], "/'\" ")
	r, _, err := s.F.ResolveRepo(ctx, u, full, need)
	if err != nil {
		if errors.Is(err, forge.ErrForbidden) {
			return fail("permission denied for %s", full)
		}
		return fail("repository %s not found", full)
	}
	if write && !r.LFS {
		return fail("Git LFS is not enabled for %s; enable it in the repository settings or with: %s repo edit --lfs true %s",
			r.FullName(), s.F.Cfg.SSHCommand(), r.FullName())
	}
	const ttl = time.Hour
	resp := map[string]any{"href": s.F.Cfg.HTTP.PublicURL + "/" + r.FullName() + ".git/info/lfs", "expires_in": int(ttl.Seconds())}
	if u != nil {
		resp["header"] = map[string]string{"Authorization": "Bearer " + s.F.LFSToken(u.ID, r.ID, write, time.Now().Add(ttl))}
	}
	b, _ := json.Marshal(resp)
	sess.Write(append(b, '\n'))
	return 0
}

// cutWiki splits "owner/name.wiki" (the wiki repository) into "owner/name".
func cutWiki(full string) (string, bool) {
	if len(full) > 5 && strings.EqualFold(full[len(full)-5:], ".wiki") {
		return full[:len(full)-5], true
	}
	return full, false
}
