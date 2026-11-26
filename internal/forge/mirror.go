package forge

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cooperspencer/forge/internal/store"
	gossh "golang.org/x/crypto/ssh"
)

// Mirrors. A pull mirror keeps a repository as a read-only copy of a remote
// (fetched every Interval); push mirrors send a repository's branches and
// tags to remotes after every push (and every Interval, if set). Both copy
// refs/heads and refs/tags exactly, deleting what the source doesn't have.
//
// Remotes are https/http (with an optional username and password or token)
// or ssh (with a deploy key forge generates). Local paths, file:// and
// git's command transports are refused, and so are loopback and link-local
// addresses unless mirror.allow_loopback is set, so a mirror can't be used
// to read the server's own files or cloud metadata.

const (
	MirrorPull = "pull"
	MirrorPush = "push"

	MinMirrorInterval     = 10 * time.Minute
	DefaultMirrorInterval = 8 * time.Hour
	mirrorTimeout         = 15 * time.Minute
)

var ErrReadOnlyMirror = errors.New("this repository is a read-only mirror")

// ReadOnlyError explains why a pull mirror can't be changed directly.
func (f *Forge) ReadOnlyError(ctx context.Context, r *store.Repo) error {
	if m, err := f.Store.PullMirror(ctx, r.ID); err == nil {
		return fmt.Errorf("%w of %s: change the original, forge syncs it (or stop mirroring in Settings)", ErrReadOnlyMirror, m.URL)
	}
	return ErrReadOnlyMirror
}

var scpLike = regexp.MustCompile(`^([A-Za-z0-9._-]+@)?([A-Za-z0-9.-]+):([^/].*)$`)

// MirrorRemote is a checked remote URL.
type MirrorRemote struct {
	URL      string // without password
	SSH      bool
	Username string // from the URL, if it had one
	Password string // from the URL, if it had one
}

// ParseMirrorURL validates a remote and splits off credentials.
func (f *Forge) ParseMirrorURL(ctx context.Context, raw string) (MirrorRemote, error) {
	raw = strings.TrimSpace(raw)
	var rem MirrorRemote
	var host string
	if strings.HasPrefix(raw, "-") || strings.ContainsAny(raw, " \t\n\r") {
		return rem, fmt.Errorf("invalid remote URL")
	}
	if m := scpLike.FindStringSubmatch(raw); m != nil && !strings.Contains(raw, "://") {
		// git@github.com:owner/repo.git
		rem.URL, rem.SSH, host = raw, true, m[2]
	} else {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return rem, fmt.Errorf("invalid remote URL %q: use https://host/owner/repo.git or git@host:owner/repo.git", raw)
		}
		switch u.Scheme {
		case "https", "http", "git":
		case "ssh":
			rem.SSH = true
		default:
			return rem, fmt.Errorf("unsupported remote %q: use https, http, ssh or git", u.Scheme)
		}
		if u.User != nil && !rem.SSH {
			rem.Username = u.User.Username()
			rem.Password, _ = u.User.Password()
			u.User = nil
		} else if u.User != nil {
			u.User = url.User(u.User.Username()) // never keep an SSH password
		}
		if strings.Trim(u.Path, "/") == "" {
			return rem, fmt.Errorf("the remote URL needs a repository path")
		}
		rem.URL, host = u.String(), u.Hostname()
	}
	if host == "" || strings.HasPrefix(host, "-") {
		return rem, fmt.Errorf("invalid host in remote URL")
	}
	if err := f.checkMirrorHost(ctx, host); err != nil {
		return rem, err
	}
	return rem, nil
}

func (f *Forge) checkMirrorHost(ctx context.Context, host string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("can't resolve %s: %v", host, err)
	}
	for _, ip := range ips {
		if ip.IP.IsUnspecified() || ip.IP.IsLinkLocalUnicast() || ip.IP.IsLinkLocalMulticast() || ip.IP.IsMulticast() {
			return fmt.Errorf("%s resolves to %s, which mirrors may not use", host, ip.IP)
		}
		if ip.IP.IsLoopback() && !f.Cfg.Mirror.AllowLoopback {
			return fmt.Errorf("%s is this machine (%s); set mirror.allow_loopback to mirror from it", host, ip.IP)
		}
	}
	return nil
}

// MirrorOptions are the settings of a new or changed mirror.
type MirrorOptions struct {
	URL      string
	Username string
	Password string // "" keeps the current one when editing
	Interval time.Duration
	RunCI    bool // pull mirrors: run pipelines for synced commits
}

func parseInterval(d time.Duration, direction string) (time.Duration, error) {
	if direction == MirrorPush && d == 0 {
		return 0, nil // only after pushes
	}
	if d == 0 {
		return DefaultMirrorInterval, nil
	}
	if d < MinMirrorInterval || d > 30*24*time.Hour {
		return 0, fmt.Errorf("sync interval must be between 10m and 30d")
	}
	return d, nil
}

// ParseInterval reads "30m", "8h", "1d" (and "" or "0").
func ParseInterval(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil {
			return 0, fmt.Errorf("invalid interval %q", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid interval %q (e.g. 30m, 8h, 1d)", s)
	}
	return d, nil
}

// FormatInterval writes an interval the way ParseInterval reads it.
func FormatInterval(d time.Duration) string {
	switch {
	case d == 0:
		return "0"
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	}
	return fmt.Sprintf("%dm", d/time.Minute)
}

func secretAD(m *store.Mirror, what string) string { return fmt.Sprintf("mirror:%d:%s", m.ID, what) }

// fill applies options to m: URL, credentials, deploy key, interval.
func (f *Forge) fillMirror(ctx context.Context, m *store.Mirror, o MirrorOptions) error {
	rem, err := f.ParseMirrorURL(ctx, o.URL)
	if err != nil {
		return err
	}
	iv, err := parseInterval(o.Interval, m.Direction)
	if err != nil {
		return err
	}
	user, pass := strings.TrimSpace(o.Username), o.Password
	if rem.Username != "" {
		user = rem.Username
	}
	if rem.Password != "" {
		pass = rem.Password
	}
	m.URL, m.Username, m.Interval, m.RunCI = rem.URL, user, iv, o.RunCI && m.Direction == MirrorPull
	if pass != "" {
		m.Secret = f.box.Seal(pass, secretAD(m, "secret"))
	}
	if rem.SSH {
		m.Secret = ""
		if m.SSHKey == "" {
			if err := f.newDeployKey(m); err != nil {
				return err
			}
		}
	} else {
		m.SSHKey, m.SSHPub = "", ""
	}
	return nil
}

func (f *Forge) newDeployKey(m *store.Mirror) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	block, err := gossh.MarshalPrivateKey(priv, "forge mirror")
	if err != nil {
		return err
	}
	sp, err := gossh.NewPublicKey(pub)
	if err != nil {
		return err
	}
	m.SSHKey = f.box.Seal(string(pem.EncodeToMemory(block)), secretAD(m, "ssh"))
	m.SSHPub = strings.TrimSpace(string(gossh.MarshalAuthorizedKey(sp))) + " forge-mirror"
	return nil
}

func (f *Forge) mirrorAdmin(ctx context.Context, actor *store.User, r *store.Repo) error {
	if actor == nil || f.Store.AccessLevel(ctx, actor, r) < store.AccessAdmin {
		return ErrForbidden
	}
	return nil
}

// CreatePullMirror creates a repository that mirrors a remote. The first
// sync runs in the background right away.
func (f *Forge) CreatePullMirror(ctx context.Context, actor *store.User, owner, name, desc string, private bool, o MirrorOptions) (*store.Repo, error) {
	if actor == nil {
		return nil, ErrForbidden
	}
	if name == "" {
		name = repoNameFromURL(o.URL)
	}
	// check the remote before creating anything
	if _, err := f.ParseMirrorURL(ctx, o.URL); err != nil {
		return nil, err
	}
	if _, err := parseInterval(o.Interval, MirrorPull); err != nil {
		return nil, err
	}
	r, err := f.CreateRepoWith(ctx, actor, owner, name, desc, RepoOptions{Private: private})
	if err != nil {
		return nil, err
	}
	m := &store.Mirror{RepoID: r.ID, Direction: MirrorPull, NextSync: time.Now()}
	if err := f.Store.CreateMirror(ctx, m); err != nil { // the id binds the encrypted values
		f.DeleteRepo(ctx, actor, r)
		return nil, err
	}
	if err := f.fillMirror(ctx, m, o); err == nil {
		err = f.Store.UpdateMirror(ctx, m)
	} else {
		f.DeleteRepo(ctx, actor, r)
		return nil, err
	}
	r.Mirror = true
	f.Store.RecordEvent(ctx, r.ID, actor.ID, "repo", fmt.Sprintf("%s created %s as a mirror of %s", actor.Name, r.FullName(), m.URL))
	f.wakeMirrors()
	return r, nil
}

func repoNameFromURL(u string) string {
	u = strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(u), "/"), ".git")
	if i := strings.LastIndexAny(u, "/:"); i >= 0 {
		u = u[i+1:]
	}
	return u
}

// AddPushMirror makes r push to a remote after every push.
func (f *Forge) AddPushMirror(ctx context.Context, actor *store.User, r *store.Repo, o MirrorOptions) (*store.Mirror, error) {
	if err := f.mirrorAdmin(ctx, actor, r); err != nil {
		return nil, err
	}
	if _, err := f.ParseMirrorURL(ctx, o.URL); err != nil {
		return nil, err
	}
	m := &store.Mirror{RepoID: r.ID, Direction: MirrorPush}
	if err := f.Store.CreateMirror(ctx, m); err != nil {
		return nil, err
	}
	if err := f.fillMirror(ctx, m, o); err != nil {
		f.Store.DeleteMirror(ctx, m.ID)
		return nil, err
	}
	if !f.Git(r).IsEmpty(ctx) && m.SSHPub == "" {
		m.NextSync = time.Now() // push what's there now; SSH waits until the key is added
	}
	if err := f.Store.UpdateMirror(ctx, m); err != nil {
		return nil, err
	}
	f.Store.RecordEvent(ctx, r.ID, actor.ID, "settings", fmt.Sprintf("%s added a push mirror to %s", actor.Name, m.URL))
	f.wakeMirrors()
	return m, nil
}

// Mirrors lists r's mirrors (admins).
func (f *Forge) Mirrors(ctx context.Context, actor *store.User, r *store.Repo) ([]*store.Mirror, error) {
	if err := f.mirrorAdmin(ctx, actor, r); err != nil {
		return nil, err
	}
	return f.Store.RepoMirrors(ctx, r.ID)
}

func (f *Forge) repoMirror(ctx context.Context, actor *store.User, r *store.Repo, id int64) (*store.Mirror, error) {
	if err := f.mirrorAdmin(ctx, actor, r); err != nil {
		return nil, err
	}
	m, err := f.Store.MirrorByID(ctx, id)
	if err != nil || m.RepoID != r.ID {
		return nil, ErrNotFound
	}
	return m, nil
}

// UpdateMirror changes a mirror's remote, credentials or interval.
func (f *Forge) UpdateMirror(ctx context.Context, actor *store.User, r *store.Repo, id int64, o MirrorOptions) error {
	m, err := f.repoMirror(ctx, actor, r, id)
	if err != nil {
		return err
	}
	if o.URL != m.URL {
		m.SSHKey, m.SSHPub = "", "" // a new remote gets a new deploy key
		if o.Password == "" {
			m.Secret = "" // don't send the old token to a new host
		}
	}
	if err := f.fillMirror(ctx, m, o); err != nil {
		return err
	}
	if m.Direction == MirrorPull || !f.Git(r).IsEmpty(ctx) {
		m.NextSync = time.Now()
	}
	if err := f.Store.UpdateMirror(ctx, m); err != nil {
		return err
	}
	f.wakeMirrors()
	return nil
}

// RemoveMirror deletes a mirror; removing the pull mirror turns the
// repository into a normal, writable one.
func (f *Forge) RemoveMirror(ctx context.Context, actor *store.User, r *store.Repo, id int64) error {
	m, err := f.repoMirror(ctx, actor, r, id)
	if err != nil {
		return err
	}
	if err := f.Store.DeleteMirror(ctx, id); err != nil {
		return err
	}
	what := "removed the push mirror to " + m.URL
	if m.Direction == MirrorPull {
		what = "stopped mirroring " + m.URL + "; the repository is writable now"
	}
	f.Store.RecordEvent(ctx, r.ID, actor.ID, "settings", actor.Name+" "+what)
	f.Broker.Publish(fmt.Sprintf("repo:%d", r.ID))
	return nil
}

// SyncMirrorNow schedules a sync right away.
func (f *Forge) SyncMirrorNow(ctx context.Context, actor *store.User, r *store.Repo, id int64) error {
	m, err := f.repoMirror(ctx, actor, r, id)
	if err != nil {
		return err
	}
	m.NextSync = time.Now()
	if err := f.Store.UpdateMirror(ctx, m); err != nil {
		return err
	}
	f.wakeMirrors()
	return nil
}

// queuePushMirrors runs r's push mirrors soon (after a push).
func (f *Forge) queuePushMirrors(ctx context.Context, r *store.Repo) {
	if n, _ := f.Store.ScheduleMirrors(ctx, r.ID, MirrorPush, time.Now()); n > 0 {
		f.wakeMirrors()
	}
}

// ------------------------------------------------------------------ syncing

type mirrorRunner struct {
	once    sync.Once
	wake    chan struct{}
	mu      sync.Mutex
	running map[int64]bool
}

func (f *Forge) mirrorState() *mirrorRunner {
	f.mirrors.once.Do(func() {
		f.mirrors.wake = make(chan struct{}, 1)
		f.mirrors.running = map[int64]bool{}
	})
	return &f.mirrors
}

func (f *Forge) wakeMirrors() {
	select {
	case f.mirrorState().wake <- struct{}{}:
	default:
	}
}

// MirrorLoop syncs due mirrors until ctx ends (two at a time).
func (f *Forge) MirrorLoop(ctx context.Context) {
	st := f.mirrorState()
	sem := make(chan struct{}, 2)
	for {
		due, err := f.Store.DueMirrors(ctx, time.Now())
		if err != nil {
			f.Log.Error().Err(err).Msg("mirrors")
		}
		for _, m := range due {
			st.mu.Lock()
			busy := st.running[m.ID]
			st.running[m.ID] = true
			st.mu.Unlock()
			if busy {
				continue
			}
			sem <- struct{}{}
			go func(id int64) {
				defer func() { <-sem; st.mu.Lock(); delete(st.running, id); st.mu.Unlock() }()
				f.SyncMirror(ctx, id)
			}(m.ID)
		}
		select {
		case <-ctx.Done():
			return
		case <-st.wake:
		case <-time.After(30 * time.Second):
		}
	}
}

// SyncMirror runs one sync now and records the outcome.
func (f *Forge) SyncMirror(ctx context.Context, id int64) error {
	m, err := f.Store.MirrorByID(ctx, id)
	if err != nil {
		return err
	}
	r, err := f.Store.RepoByID(ctx, m.RepoID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, mirrorTimeout)
	defer cancel()
	start := time.Now()
	if m.Direction == MirrorPull {
		err = f.pullMirror(ctx, r, m)
	} else {
		err = f.pushMirror(ctx, r, m)
	}
	var next time.Time
	if m.Interval > 0 {
		next = start.Add(m.Interval)
	}
	msg := ""
	if err != nil {
		msg = err.Error()
		f.Log.Warn().Str("repo", r.FullName()).Str("remote", m.URL).Str("direction", m.Direction).Err(err).Msg("mirror sync failed")
		if m.Interval == 0 {
			next = start.Add(time.Hour) // push mirrors without interval: retry hourly
		}
	} else {
		f.Log.Info().Str("repo", r.FullName()).Str("remote", m.URL).Str("direction", m.Direction).Dur("took", time.Since(start)).Msg("mirror synced")
	}
	// a push may have queued another sync while this one ran: keep it
	if cur, e := f.Store.MirrorByID(context.Background(), id); e == nil && !cur.NextSync.IsZero() && cur.NextSync.After(start) {
		next = cur.NextSync
	}
	f.Store.MirrorResult(context.Background(), id, time.Now(), msg, next)
	f.Broker.Publish(fmt.Sprintf("repo:%d", r.ID))
	return err
}

// gitEnv returns the command environment for talking to m's remote, and a
// cleanup function (it may write a temporary key file).
func (f *Forge) mirrorGitEnv(m *store.Mirror) ([]string, func(), error) {
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=https:http:ssh:git", "LC_ALL=C", "GIT_ASKPASS=true", "SSH_ASKPASS=true")
	cleanup := func() {}
	cfg := []string{"credential.helper="} // never use the server's own credentials
	if m.Secret != "" {
		pass, err := f.box.Open(m.Secret, secretAD(m, "secret"))
		if err != nil {
			return nil, cleanup, fmt.Errorf("can't decrypt the mirror's password: %v", err)
		}
		user := m.Username
		if user == "" {
			user = "git"
		}
		// in the environment, not the command line (which other users can see)
		cfg = append(cfg, "http.extraHeader=Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	}
	if m.SSHKey != "" {
		key, err := f.box.Open(m.SSHKey, secretAD(m, "ssh"))
		if err != nil {
			return nil, cleanup, fmt.Errorf("can't decrypt the mirror's deploy key: %v", err)
		}
		dir, err := os.MkdirTemp("", "forge-mirror-")
		if err != nil {
			return nil, cleanup, err
		}
		cleanup = func() { os.RemoveAll(dir) }
		keyFile := filepath.Join(dir, "key")
		if err := os.WriteFile(keyFile, []byte(key), 0o600); err != nil {
			return nil, cleanup, err
		}
		known := f.Cfg.Path("mirrors_known_hosts")
		env = append(env, fmt.Sprintf("GIT_SSH_COMMAND=ssh -i %s -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=20 -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=%s",
			shellQuote(keyFile), shellQuote(known)))
	}
	env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(cfg)))
	for i, kv := range cfg {
		k, v, _ := strings.Cut(kv, "=")
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, k), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, v))
	}
	return env, cleanup, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (f *Forge) runMirrorGit(ctx context.Context, m *store.Mirror, r *store.Repo, args ...string) (string, error) {
	// re-check the host each time: DNS may have changed since the mirror was set up
	rem, err := f.ParseMirrorURL(ctx, m.URL)
	if err != nil {
		return "", err
	}
	env, cleanup, err := f.mirrorGitEnv(m)
	defer cleanup()
	if err != nil {
		return "", err
	}
	c := exec.CommandContext(ctx, "git", append([]string{"--git-dir", f.RepoPath(r)}, append(args[:len(args):len(args)], rem.URL)...)...)
	c.Env = env
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	err = c.Run()
	text := strings.TrimSpace(out.String())
	if err != nil {
		if ctx.Err() != nil {
			return text, fmt.Errorf("timed out after %s", mirrorTimeout)
		}
		return text, fmt.Errorf("%s", lastLines(text, 3))
	}
	return text, nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := strings.Join(lines, " / ")
	if out == "" {
		return "git failed"
	}
	return out
}

var refspecs = []string{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"}

func (f *Forge) pullMirror(ctx context.Context, r *store.Repo, m *store.Mirror) error {
	g := f.Git(r)
	before, _ := g.RefMap(ctx)
	if g.IsEmpty(ctx) {
		// first sync: follow the remote's default branch
		if out, err := f.runMirrorGit(ctx, m, r, "ls-remote", "--symref"); err == nil {
			for _, l := range strings.Split(out, "\n") {
				if b, ok := strings.CutPrefix(l, "ref: refs/heads/"); ok {
					if br, _, ok := strings.Cut(b, "\tHEAD"); ok && br != "" {
						r.DefaultBranch = br
						g.SetHead(ctx, br)
						f.Store.UpdateRepo(ctx, r)
					}
				}
			}
		}
	}
	if _, err := f.runMirrorGitFetch(ctx, m, r); err != nil {
		return err
	}
	after, _ := g.RefMap(ctx)
	ups := DiffRefs(before, after)
	if len(ups) > 0 {
		f.postReceive(ctx, nil, r, ups, postOpts{noCI: !m.RunCI, who: "mirror"})
	}
	return nil
}

// runMirrorGitFetch is `git fetch --prune <url> <refspecs>`.
func (f *Forge) runMirrorGitFetch(ctx context.Context, m *store.Mirror, r *store.Repo) (string, error) {
	rem, err := f.ParseMirrorURL(ctx, m.URL)
	if err != nil {
		return "", err
	}
	env, cleanup, err := f.mirrorGitEnv(m)
	defer cleanup()
	if err != nil {
		return "", err
	}
	args := append([]string{"--git-dir", f.RepoPath(r), "fetch", "--prune", "--quiet", "--no-write-fetch-head", "--end-of-options", rem.URL}, refspecs...)
	return runGit(ctx, env, args)
}

func (f *Forge) pushMirror(ctx context.Context, r *store.Repo, m *store.Mirror) error {
	if f.Git(r).IsEmpty(ctx) {
		return nil
	}
	rem, err := f.ParseMirrorURL(ctx, m.URL)
	if err != nil {
		return err
	}
	env, cleanup, err := f.mirrorGitEnv(m)
	defer cleanup()
	if err != nil {
		return err
	}
	args := append([]string{"--git-dir", f.RepoPath(r), "push", "--prune", "--force", "--quiet", "--no-verify", "--end-of-options", rem.URL}, refspecs...)
	_, err = runGit(ctx, env, args)
	return err
}

func runGit(ctx context.Context, env, args []string) (string, error) {
	c := exec.CommandContext(ctx, "git", args...)
	c.Env = env
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	err := c.Run()
	text := strings.TrimSpace(out.String())
	if err != nil {
		if ctx.Err() != nil {
			return text, fmt.Errorf("timed out after %s", mirrorTimeout)
		}
		return text, fmt.Errorf("%s", lastLines(text, 3))
	}
	return text, nil
}

// rekeyMirrors re-encrypts mirror credentials sealed with old.
func (f *Forge) rekeyMirrors(ctx context.Context, open func(ct, ad string) (string, error), isOld func(ct string) bool) (int, error) {
	ms, err := f.Store.AllMirrors(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range ms {
		changed := false
		for _, fld := range []struct {
			v  *string
			ad string
		}{{&m.Secret, secretAD(m, "secret")}, {&m.SSHKey, secretAD(m, "ssh")}} {
			if *fld.v == "" || !isOld(*fld.v) {
				continue
			}
			pt, err := open(*fld.v, fld.ad)
			if err != nil {
				return n, fmt.Errorf("mirror %d: %w", m.ID, err)
			}
			*fld.v = f.box.Seal(pt, fld.ad)
			changed = true
		}
		if changed {
			if err := f.Store.UpdateMirror(ctx, m); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}
