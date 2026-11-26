// Package forge is the application service layer. Every frontend (SSH TUI,
// SSH CLI, HTTP API) goes through these methods so permission checks and side
// effects (events, CI triggers) live in exactly one place.
package forge

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cooperspencer/forge/internal/ci"
	"github.com/cooperspencer/forge/internal/config"
	"github.com/cooperspencer/forge/internal/gitx"
	"github.com/cooperspencer/forge/internal/secretbox"
	"github.com/cooperspencer/forge/internal/store"
	"github.com/rs/zerolog"
	gossh "golang.org/x/crypto/ssh"
)

var (
	ErrForbidden = errors.New("permission denied")
	ErrNotFound  = store.ErrNotFound
)

type Forge struct {
	Cfg    *config.Config
	Store  *store.Store
	CI     *ci.Orchestrator
	Logs   *ci.LogStore
	Broker *ci.Broker
	Log    zerolog.Logger

	secret   []byte         // HMAC key for job-scoped clone tokens
	box      *secretbox.Box // encrypts CI secrets at rest
	pkgMu    sync.RWMutex   // package GC (exclusive) vs. content being added (shared)
	sched    scheduleCache  // scheduled pipelines
	renameMu sync.Mutex     // user renames move repository directories
	lfsMu    sync.Mutex     // LFS uploads vs. LFS garbage collection
	mirrors  mirrorRunner   // mirror sync scheduling
}

func New(cfg *config.Config, log zerolog.Logger) (*Forge, error) {
	for _, d := range []string{"repos", "logs", "artifacts", "lfs"} {
		if err := os.MkdirAll(cfg.Path(d), 0o755); err != nil {
			return nil, err
		}
	}
	if fi, err := os.Stat(cfg.DataDir); err == nil && fi.Mode().Perm()&0o077 != 0 {
		log.Warn().Str("path", cfg.DataDir).Str("mode", fi.Mode().Perm().String()).
			Msg("data directory is accessible to other local users (private repositories, database); run chmod 700 on it")
	}
	st, err := store.Open(cfg.Path("forge.db"))
	if err != nil {
		return nil, err
	}
	logs, err := ci.NewLogStore(cfg.Path("logs"))
	if err != nil {
		return nil, err
	}
	secret, err := loadSecret(cfg.Path("secret.key"))
	if err != nil {
		return nil, err
	}
	box, err := loadSecretBox(cfg, secret, log)
	if err != nil {
		return nil, err
	}
	f := &Forge{Cfg: cfg, Store: st, Logs: logs, Broker: ci.NewBroker(), Log: log, secret: secret, box: box}
	f.CI = &ci.Orchestrator{Store: st, Logs: logs, Broker: f.Broker, RepoPath: f.RepoPath, Log: log,
		StaleAfter: time.Duration(cfg.CI.StaleSeconds) * time.Second}
	if err := f.bootstrap(context.Background()); err != nil {
		return nil, err
	}
	if err := f.migrateSecrets(context.Background()); err != nil {
		return nil, fmt.Errorf("migrating secrets: %w", err)
	}
	if err := f.installHooks(); err != nil {
		log.Warn().Err(err).Msg("installing the branch protection hook")
	}
	return f, nil
}

func (f *Forge) Close() error { return f.Store.Close() }

func loadSecret(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return b, nil
	}
	b := make([]byte, 32)
	rand.Read(b)
	return b, os.WriteFile(path, b, 0o600)
}

// bootstrap creates the initial admin from configured keys on an empty install.
func (f *Forge) bootstrap(ctx context.Context) error {
	if n, _ := f.Store.CountUsers(ctx); n > 0 || len(f.Cfg.InitialAdminKeys) == 0 {
		return nil
	}
	name := f.Cfg.InitialAdminName
	if name == "" {
		name = "admin"
	}
	u, err := f.Store.CreateUser(ctx, name, true, false)
	if err != nil {
		return fmt.Errorf("initial admin: %w", err)
	}
	for _, k := range f.Cfg.InitialAdminKeys {
		if err := f.AddKey(ctx, u, k); err != nil {
			return fmt.Errorf("initial admin key: %w", err)
		}
	}
	f.Log.Info().Str("name", u.Name).Msg("created initial admin user")
	return nil
}

func (f *Forge) RepoPath(r *store.Repo) string {
	return f.Cfg.Path("repos", strings.ToLower(r.Owner), strings.ToLower(r.Name)+".git")
}

func (f *Forge) Git(r *store.Repo) *gitx.Repo { return gitx.Open(f.RepoPath(r)) }

// RenameUser changes the name of a user (by themselves or a site admin) or an
// organization (by its owners or a site admin). Repositories move with it;
// old clone URLs, remotes and image references stop working.
func (f *Forge) RenameUser(ctx context.Context, actor, u *store.User, name string) error {
	if !f.CanManageOwnerSecrets(ctx, actor, u) {
		return ErrForbidden
	}
	name = strings.TrimSpace(name)
	if name == u.Name {
		return nil
	}
	if err := store.ValidUserName(name); err != nil {
		return err
	}
	f.renameMu.Lock()
	defer f.renameMu.Unlock()
	oldDir := f.Cfg.Path("repos", strings.ToLower(u.Name))
	newDir := f.Cfg.Path("repos", strings.ToLower(name))
	moveDir := oldDir != newDir // a change of case only keeps the directory
	if moveDir {
		// An empty directory can be left behind by a deleted user of that name.
		os.Remove(newDir)
		if _, err := os.Stat(newDir); err == nil {
			return fmt.Errorf("cannot rename: %s already exists on disk", newDir)
		}
	}
	if err := f.Store.RenameUser(ctx, u.ID, name); err != nil {
		return err
	}
	if moveDir {
		if err := os.Rename(oldDir, newDir); err != nil && !os.IsNotExist(err) {
			if rerr := f.Store.RenameUser(ctx, u.ID, u.Name); rerr != nil {
				f.Log.Error().Err(rerr).Str("user", u.Name).Str("new", name).Msg("rename: could not restore name after failed move")
			}
			return fmt.Errorf("moving repositories: %w", err)
		}
	}
	f.Log.Info().Str("old", u.Name).Str("new", name).Str("by", actor.Name).Msg("renamed user")
	u.Name = name
	return nil
}

// ------------------------------------------------------------------ users & keys

func ParseKey(line string) (gossh.PublicKey, string, error) {
	pk, comment, _, _, err := gossh.ParseAuthorizedKey([]byte(strings.TrimSpace(line)))
	if err != nil {
		return nil, "", fmt.Errorf("invalid public key: %w", err)
	}
	return pk, comment, nil
}

func Fingerprint(pk gossh.PublicKey) string { return gossh.FingerprintSHA256(pk) }

func (f *Forge) AddKey(ctx context.Context, u *store.User, line string) error {
	pk, comment, err := ParseKey(line)
	if err != nil {
		return err
	}
	return f.Store.AddSSHKey(ctx, u.ID, comment, Fingerprint(pk), strings.TrimSpace(string(gossh.MarshalAuthorizedKey(pk))))
}

func (f *Forge) UserForKey(ctx context.Context, pk gossh.PublicKey) (*store.User, error) {
	return f.Store.UserByFingerprint(ctx, Fingerprint(pk))
}

// Register creates an account bound to pk (self-service signup).
func (f *Forge) Register(ctx context.Context, name string, pk gossh.PublicKey) (*store.User, error) {
	n, _ := f.Store.CountUsers(ctx)
	if !f.Cfg.AllowRegistration && n > 0 {
		return nil, fmt.Errorf("registration is disabled; ask an administrator to add your key")
	}
	// The very first user becomes the administrator.
	u, err := f.Store.CreateUser(ctx, name, n == 0, false)
	if err != nil {
		return nil, err
	}
	line := string(gossh.MarshalAuthorizedKey(pk))
	if err := f.AddKey(ctx, u, line); err != nil {
		f.Store.DeleteUser(ctx, u.ID)
		return nil, err
	}
	return u, nil
}

func requireAdmin(u *store.User) error {
	if u == nil || !u.SiteAdmin() {
		return ErrForbidden
	}
	return nil
}

func (f *Forge) AdminCreateUser(ctx context.Context, actor *store.User, name, key string, admin bool) (*store.User, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, err
	}
	u, err := f.Store.CreateUser(ctx, name, admin, false)
	if err != nil {
		return nil, err
	}
	if key != "" {
		if err := f.AddKey(ctx, u, key); err != nil {
			f.Store.DeleteUser(ctx, u.ID)
			return nil, err
		}
	}
	return u, nil
}

func (f *Forge) CreateOrg(ctx context.Context, actor *store.User, name string) (*store.User, error) {
	if actor == nil || actor.IsOrg || (actor.Grant != nil && len(actor.Grant.Repos) > 0) {
		return nil, ErrForbidden // tokens limited to some repositories can't create owners
	}
	org, err := f.Store.CreateUser(ctx, name, false, true)
	if err != nil {
		return nil, err
	}
	return org, f.Store.SetOrgMember(ctx, org.ID, actor.ID, "owner")
}

// CanManageOrg: site admins and org owners.
func (f *Forge) CanManageOrg(ctx context.Context, actor, org *store.User) bool {
	if actor == nil || !actor.Grant.CoversOwner(org.Name) {
		return false
	}
	if actor.SiteAdmin() {
		return true
	}
	role, _ := f.Store.OrgRole(ctx, org.ID, actor.ID)
	return role == "owner"
}

// ------------------------------------------------------------------ repos

// ResolveRepo finds owner/name and checks that actor has at least `need`.
// Private repos the actor can't see report ErrNotFound to avoid leaking existence.
func (f *Forge) ResolveRepo(ctx context.Context, actor *store.User, full string, need store.Access) (*store.Repo, store.Access, error) {
	full = strings.Trim(strings.TrimSuffix(strings.Trim(full, "/"), ".git"), "/")
	owner, name, ok := strings.Cut(full, "/")
	if !ok || strings.Contains(name, "/") {
		return nil, 0, fmt.Errorf("repository must be owner/name")
	}
	r, err := f.Store.RepoByName(ctx, owner, name)
	if err != nil {
		return nil, 0, err
	}
	acc := f.Store.AccessLevel(ctx, actor, r)
	if actor == nil && !f.Cfg.AnonymousRead {
		acc = store.AccessNone
	}
	if acc == store.AccessNone {
		return nil, 0, ErrNotFound
	}
	if acc < need {
		return r, acc, ErrForbidden
	}
	return r, acc, nil
}

func (f *Forge) CreateRepo(ctx context.Context, actor *store.User, ownerName, name, desc string, private bool) (*store.Repo, error) {
	return f.CreateRepoWith(ctx, actor, ownerName, name, desc, RepoOptions{Private: private})
}

// RepoOptions are the settings chosen when creating a repository.
type RepoOptions struct {
	Private bool
	LFS     bool // accept Git LFS uploads
}

func (f *Forge) CreateRepoWith(ctx context.Context, actor *store.User, ownerName, name, desc string, opt RepoOptions) (*store.Repo, error) {
	private := opt.Private
	if actor == nil {
		return nil, ErrForbidden
	}
	if ownerName == "" {
		ownerName = actor.Name
	}
	owner, err := f.Store.UserByName(ctx, ownerName)
	if err != nil {
		return nil, fmt.Errorf("owner %q: %w", ownerName, err)
	}
	if !f.Store.CanCreateIn(ctx, actor, owner) {
		return nil, ErrForbidden
	}
	r, err := f.Store.CreateRepo(ctx, owner.ID, name, desc, private, 0, "main")
	if err != nil {
		return nil, err
	}
	if _, err := gitx.Init(ctx, f.RepoPath(r), r.DefaultBranch); err != nil {
		f.Store.DeleteRepo(ctx, r.ID)
		return nil, err
	}
	if opt.LFS {
		r.LFS = true
		if err := f.Store.UpdateRepo(ctx, r); err != nil {
			return nil, err
		}
	}
	f.Store.Watch(ctx, actor.ID, r.ID, true)
	f.Store.RecordEvent(ctx, r.ID, actor.ID, "repo", fmt.Sprintf("%s created %s", actor.Name, r.FullName()))
	return r, nil
}

func (f *Forge) Fork(ctx context.Context, actor *store.User, src *store.Repo, ownerName string) (*store.Repo, error) {
	if actor == nil {
		return nil, ErrForbidden
	}
	if ownerName == "" {
		ownerName = actor.Name
	}
	owner, err := f.Store.UserByName(ctx, ownerName)
	if err != nil {
		return nil, err
	}
	if !f.Store.CanCreateIn(ctx, actor, owner) {
		return nil, ErrForbidden
	}
	r, err := f.Store.CreateRepo(ctx, owner.ID, src.Name, src.Description, src.Private, src.ID, src.DefaultBranch)
	if err != nil {
		return nil, err
	}
	if _, err := gitx.Clone(ctx, f.RepoPath(src), f.RepoPath(r)); err != nil {
		f.Store.DeleteRepo(ctx, r.ID)
		return nil, err
	}
	// the fork serves the parent's LFS objects (its history points at them)
	if _, err := f.Store.CopyLFSLinks(ctx, src.ID, r.ID, nil); err != nil {
		f.Log.Warn().Err(err).Str("repo", r.FullName()).Msg("fork: copying LFS objects")
	}
	if src.LFS {
		r.LFS = true
		f.Store.UpdateRepo(ctx, r)
	}
	f.Store.Watch(ctx, actor.ID, r.ID, true)
	f.Store.RecordEvent(ctx, src.ID, actor.ID, "fork", fmt.Sprintf("%s forked %s to %s", actor.Name, src.FullName(), r.FullName()))
	return r, nil
}

func (f *Forge) DeleteRepo(ctx context.Context, actor *store.User, r *store.Repo) error {
	if f.Store.AccessLevel(ctx, actor, r) < store.AccessAdmin {
		return ErrForbidden
	}
	path, wiki := f.RepoPath(r), f.WikiPath(r)
	if err := f.Store.DeleteRepo(ctx, r.ID); err != nil {
		return err
	}
	f.gcLFS(ctx)
	os.RemoveAll(wiki)
	return os.RemoveAll(path)
}

func (f *Forge) RenameRepo(ctx context.Context, actor *store.User, r *store.Repo, name string) error {
	if f.Store.AccessLevel(ctx, actor, r) < store.AccessAdmin {
		return ErrForbidden
	}
	old, oldWiki := f.RepoPath(r), f.WikiPath(r)
	if err := f.Store.RenameRepo(ctx, r.ID, name); err != nil {
		return err
	}
	r.Name = name
	if err := os.Rename(old, f.RepoPath(r)); err != nil {
		return err
	}
	if _, err := os.Stat(oldWiki); err == nil {
		return os.Rename(oldWiki, f.WikiPath(r))
	}
	return nil
}

func (f *Forge) UpdateRepo(ctx context.Context, actor *store.User, r *store.Repo) error {
	if f.Store.AccessLevel(ctx, actor, r) < store.AccessAdmin {
		return ErrForbidden
	}
	if err := f.Git(r).SetHead(ctx, r.DefaultBranch); err != nil {
		return err
	}
	return f.Store.UpdateRepo(ctx, r)
}

// ------------------------------------------------------------------ push handling

type RefUpdate struct {
	Ref, Old, New string
}

func DiffRefs(before, after map[string]string) []RefUpdate {
	var ups []RefUpdate
	for ref, n := range after {
		if o := before[ref]; o != n {
			if o == "" {
				o = gitx.ZeroSHA
			}
			ups = append(ups, RefUpdate{ref, o, n})
		}
	}
	for ref, o := range before {
		if _, ok := after[ref]; !ok {
			ups = append(ups, RefUpdate{ref, o, gitx.ZeroSHA})
		}
	}
	sort.Slice(ups, func(i, j int) bool { return ups[i].Ref < ups[j].Ref })
	return ups
}

// PostReceive records events, updates PRs and triggers pipelines after a push.
func (f *Forge) PostReceive(ctx context.Context, actor *store.User, r *store.Repo, ups []RefUpdate) {
	f.postReceive(ctx, actor, r, ups, postOpts{})
}

// postOpts: noCI skips pipelines (mirror syncs); who names the actor in events.
type postOpts struct {
	noCI bool
	who  string
}

func (f *Forge) postReceive(ctx context.Context, actor *store.User, r *store.Repo, ups []RefUpdate, opt postOpts) {
	if len(ups) == 0 {
		return
	}
	g := f.Git(r)
	var actorID int64
	if actor != nil {
		actorID = actor.ID
	}
	f.Store.TouchRepo(ctx, r.ID)
	// First push: point HEAD at what was pushed if the default branch doesn't exist.
	if _, err := g.ResolveRef(ctx, "refs/heads/"+r.DefaultBranch); err != nil {
		for _, u := range ups {
			if strings.HasPrefix(u.Ref, "refs/heads/") && u.New != gitx.ZeroSHA {
				r.DefaultBranch = strings.TrimPrefix(u.Ref, "refs/heads/")
				g.SetHead(ctx, r.DefaultBranch)
				f.Store.UpdateRepo(ctx, r)
				break
			}
		}
	}
	for _, u := range ups {
		name := strings.TrimPrefix(strings.TrimPrefix(u.Ref, "refs/heads/"), "refs/tags/")
		isTag := strings.HasPrefix(u.Ref, "refs/tags/")
		who := "someone"
		if actor != nil {
			who = actor.Name
		} else if opt.who != "" {
			who = opt.who
		}
		switch {
		case u.New == gitx.ZeroSHA:
			f.Store.RecordEvent(ctx, r.ID, actorID, "push", fmt.Sprintf("%s deleted %s", who, name))
			continue
		case isTag:
			f.Store.RecordEvent(ctx, r.ID, actorID, "tag", fmt.Sprintf("%s pushed tag %s", who, name))
		default:
			n := 0
			if u.Old != gitx.ZeroSHA {
				cs, _ := g.CommitsBetween(ctx, u.Old, u.New)
				n = len(cs)
			} else {
				n = -1
			}
			msg := fmt.Sprintf("%s pushed %d commit(s) to %s", who, n, name)
			if n < 0 {
				msg = fmt.Sprintf("%s created branch %s", who, name)
			}
			f.Store.RecordEvent(ctx, r.ID, actorID, "push", msg)
		}

		var changed []string
		if u.Old != gitx.ZeroSHA {
			if diffs, err := g.Diff(ctx, u.Old, u.New); err == nil {
				for _, d := range diffs {
					changed = append(changed, d.NewPath)
				}
			}
		}
		kind := "push"
		if isTag {
			kind = "tag"
		}
		if opt.noCI {
			// mirrored commits don't run pipelines unless the mirror says so
		} else if _, err := f.CI.Dispatch(ctx, r, ci.Trigger{Kind: kind, Ref: u.Ref, SHA: u.New, ActorID: actorID, ChangedFiles: changed}); err != nil {
			f.Log.Error().Err(err).Str("repo", r.FullName()).Msg("dispatch")
		}

		// Update open PRs whose head is this branch.
		if !isTag {
			prs, _ := f.Store.OpenPRsForHead(ctx, r.ID, name)
			for _, pr := range prs {
				base, err := f.Store.RepoByID(ctx, pr.RepoID)
				if err != nil {
					continue
				}
				f.syncPR(ctx, base, pr, r, u.New, actorID)
			}
		}
	}
	f.queuePushMirrors(ctx, r)
	f.Broker.Publish(fmt.Sprintf("repo:%d", r.ID))
}

// syncPR mirrors the head commit into the base repo and runs PR pipelines.
func (f *Forge) syncPR(ctx context.Context, base *store.Repo, pr *store.Issue, head *store.Repo, sha string, actorID int64) {
	ref := fmt.Sprintf("refs/pull/%d/head", pr.Number)
	bg := f.Git(base)
	if base.ID != head.ID {
		if err := bg.FetchRef(ctx, f.RepoPath(head), "refs/heads/"+pr.PR.HeadBranch, ref); err != nil {
			f.Log.Error().Err(err).Msg("fetch pr head")
			return
		}
	} else if err := bg.UpdateRef(ctx, ref, sha, ""); err != nil {
		f.Log.Error().Err(err).Msg("update pr ref")
		return
	}
	f.Store.SetPRHead(ctx, pr.ID, sha)
	f.recordRevision(ctx, base, pr, sha, actorID)
	if pr.PR.HeadSHA != "" && pr.PR.HeadSHA != sha {
		f.Store.RecordEvent(ctx, base.ID, actorID, "pr", fmt.Sprintf("PR #%d updated to %s", pr.Number, sha[:8]))
	}
	fromFork := base.ID != head.ID
	f.CI.Dispatch(ctx, base, ci.Trigger{Kind: "pull_request", Ref: ref, SHA: sha, PRNumber: pr.Number, BaseBranch: pr.PR.BaseBranch,
		FromFork: fromFork, Hold: fromFork && !f.trusted(ctx, actorID, base), ActorID: actorID})
}

// ------------------------------------------------------------------ issues & PRs

func (f *Forge) CreateIssue(ctx context.Context, actor *store.User, r *store.Repo, title, body string) (*store.Issue, error) {
	if actor == nil || f.Store.AccessLevel(ctx, actor, r) < store.AccessRead {
		return nil, ErrForbidden
	}
	i, err := f.Store.CreateIssue(ctx, r.ID, actor.ID, title, body)
	if err != nil {
		return nil, err
	}
	f.Store.RecordEvent(ctx, r.ID, actor.ID, "issue", fmt.Sprintf("%s opened issue #%d: %s", actor.Name, i.Number, i.Title))
	f.Broker.Publish(fmt.Sprintf("issues:%d", r.ID))
	return i, nil
}

// OpenPR creates a pull request from headRepo:headBranch into base:baseBranch.
func (f *Forge) OpenPR(ctx context.Context, actor *store.User, base *store.Repo, baseBranch string, head *store.Repo, headBranch, title, body string) (*store.Issue, error) {
	if actor == nil || f.Store.AccessLevel(ctx, actor, base) < store.AccessRead || f.Store.AccessLevel(ctx, actor, head) < store.AccessRead {
		return nil, ErrForbidden
	}
	if head.ID != base.ID && head.ForkOf != base.ID {
		return nil, fmt.Errorf("%s is not a fork of %s", head.FullName(), base.FullName())
	}
	if baseBranch == "" {
		baseBranch = base.DefaultBranch
	}
	if head.ID == base.ID && headBranch == baseBranch {
		return nil, fmt.Errorf("head and base branch are the same")
	}
	sha, err := f.Git(head).ResolveRef(ctx, "refs/heads/"+headBranch)
	if err != nil {
		return nil, fmt.Errorf("branch %q not found in %s", headBranch, head.FullName())
	}
	if _, err := f.Git(base).ResolveRef(ctx, "refs/heads/"+baseBranch); err != nil {
		return nil, fmt.Errorf("base branch %q not found", baseBranch)
	}
	if title == "" {
		if c, err := f.Git(head).Commit(ctx, sha); err == nil {
			title = c.Subject
		}
	}
	i, err := f.Store.CreatePullRequest(ctx, base.ID, actor.ID, title, body, store.PullRequest{
		HeadRepoID: head.ID, HeadBranch: headBranch, BaseBranch: baseBranch})
	if err != nil {
		return nil, err
	}
	f.Store.RecordEvent(ctx, base.ID, actor.ID, "pr", fmt.Sprintf("%s opened PR #%d: %s", actor.Name, i.Number, i.Title))
	f.syncPR(ctx, base, i, head, sha, actor.ID)
	f.Broker.Publish(fmt.Sprintf("issues:%d", base.ID))
	return f.Store.IssueByNumber(ctx, base.ID, i.Number)
}

func (f *Forge) Comment(ctx context.Context, actor *store.User, r *store.Repo, i *store.Issue, c *store.Comment) error {
	if actor == nil || f.Store.AccessLevel(ctx, actor, r) < store.AccessRead {
		return ErrForbidden
	}
	if c.Review == "approve" || c.Review == "changes" {
		if !i.IsPR {
			return fmt.Errorf("reviews apply to pull requests only")
		}
		if actor.ID == i.AuthorID {
			return fmt.Errorf("you cannot review your own pull request")
		}
	}
	c.IssueID, c.AuthorID = i.ID, actor.ID
	if err := f.Store.AddComment(ctx, c); err != nil {
		return err
	}
	kind := "commented on"
	switch c.Review {
	case "approve":
		kind = "approved"
	case "changes":
		kind = "requested changes on"
	}
	what := "issue"
	if i.IsPR {
		what = "PR"
	}
	f.Store.RecordEvent(ctx, r.ID, actor.ID, "comment", fmt.Sprintf("%s %s %s #%d", actor.Name, kind, what, i.Number))
	f.Broker.Publish(fmt.Sprintf("issue:%d", i.ID))
	return nil
}

// SetIssueState closes/reopens. Authors may close their own issues; others need write.
func (f *Forge) SetIssueState(ctx context.Context, actor *store.User, r *store.Repo, i *store.Issue, state string) error {
	if actor == nil {
		return ErrForbidden
	}
	if actor.ID != i.AuthorID && f.Store.AccessLevel(ctx, actor, r) < store.AccessWrite {
		return ErrForbidden
	}
	if i.State == "merged" {
		return fmt.Errorf("pull request is already merged")
	}
	i.State = state
	if err := f.Store.UpdateIssue(ctx, i); err != nil {
		return err
	}
	verb := "closed"
	if state == "open" {
		verb = "reopened"
	}
	f.Store.RecordEvent(ctx, r.ID, actor.ID, "issue", fmt.Sprintf("%s %s #%d", actor.Name, verb, i.Number))
	f.Broker.Publish(fmt.Sprintf("issue:%d", i.ID))
	return nil
}

func (f *Forge) EditIssue(ctx context.Context, actor *store.User, r *store.Repo, i *store.Issue) error {
	if actor == nil || (actor.ID != i.AuthorID && f.Store.AccessLevel(ctx, actor, r) < store.AccessWrite) {
		return ErrForbidden
	}
	return f.Store.UpdateIssue(ctx, i)
}

// PRStatus summarises mergeability for display.
type PRStatus struct {
	Commits    []gitx.Commit
	Files      []gitx.FileDiff
	Conflicts  []string
	Mergeable  bool
	Approvals  []string
	Changes    []string // users whose latest review requests changes
	Checks     []*store.Run
	ChecksOK   bool
	ChecksDone bool
}

func (f *Forge) PRStatus(ctx context.Context, r *store.Repo, i *store.Issue) (*PRStatus, error) {
	g := f.Git(r)
	st := &PRStatus{}
	head := fmt.Sprintf("refs/pull/%d/head", i.Number)
	if i.State == "merged" && i.PR.MergeSHA != "" {
		head = i.PR.HeadSHA
	}
	baseRef := "refs/heads/" + i.PR.BaseBranch
	mb, err := g.MergeBase(ctx, baseRef, head)
	if err != nil {
		return st, err
	}
	st.Commits, _ = g.CommitsBetween(ctx, mb, head)
	st.Files, _ = g.Diff(ctx, mb, head)
	if i.State == "open" {
		baseSHA, _ := g.ResolveRef(ctx, baseRef)
		headSHA, _ := g.ResolveRef(ctx, head)
		st.Conflicts, err = g.MergeCheck(ctx, baseSHA, headSHA)
		st.Mergeable = err == nil
	}
	comments, _ := f.Store.Comments(ctx, i.ID)
	latest := map[string]string{}
	var order []string
	for _, c := range comments {
		if c.Review == "approve" || c.Review == "changes" {
			if _, ok := latest[c.Author]; !ok {
				order = append(order, c.Author)
			}
			latest[c.Author] = c.Review
		}
	}
	for _, u := range order {
		if latest[u] == "approve" {
			st.Approvals = append(st.Approvals, u)
		} else {
			st.Changes = append(st.Changes, u)
		}
	}
	st.Checks, _ = f.Store.RunsForSHA(ctx, r.ID, i.PR.HeadSHA)
	// keep only the latest run per pipeline
	seen := map[string]bool{}
	var latestRuns []*store.Run
	for _, run := range st.Checks {
		if !seen[run.Pipeline] {
			seen[run.Pipeline] = true
			latestRuns = append(latestRuns, run)
		}
	}
	st.Checks = latestRuns
	st.ChecksOK, st.ChecksDone = true, true
	for _, run := range st.Checks {
		if !store.IsTerminal(run.Status) {
			st.ChecksDone = false
		}
		if run.Status != store.StatusSuccess {
			st.ChecksOK = false
		}
	}
	return st, nil
}

func (f *Forge) MergePR(ctx context.Context, actor *store.User, r *store.Repo, i *store.Issue, method string) (string, error) {
	if actor == nil || f.Store.AccessLevel(ctx, actor, r) < store.AccessWrite {
		return "", ErrForbidden
	}
	if !i.IsPR || i.State != "open" {
		return "", fmt.Errorf("pull request is not open")
	}
	if r.Mirror {
		return "", f.ReadOnlyError(ctx, r)
	}
	if st, err := f.PRStatus(ctx, r, i); err == nil {
		if b := f.MergeBlockers(ctx, actor, r, i, st); len(b) > 0 {
			return "", fmt.Errorf("%s is protected: %s", i.PR.BaseBranch, strings.Join(b, "; "))
		}
	}
	if method == "" {
		method = "merge"
	}
	g := f.Git(r)
	head := fmt.Sprintf("refs/pull/%d/head", i.Number)
	headSHA, err := g.ResolveRef(ctx, head)
	if err != nil {
		return "", err
	}
	msg := fmt.Sprintf("Merge pull request #%d from %s:%s\n\n%s", i.Number, i.PR.HeadRepo, i.PR.HeadBranch, i.Title)
	if method == "squash" {
		msg = fmt.Sprintf("%s (#%d)\n\n%s", i.Title, i.Number, i.Body)
	}
	email := actor.Email
	if email == "" {
		email = actor.Name + "@" + strings.Split(f.Cfg.SSH.PublicAddr, ":")[0]
	}
	name := actor.FullName
	if name == "" {
		name = actor.Name
	}
	before, _ := g.RefMap(ctx)
	sha, err := g.Merge(ctx, i.PR.BaseBranch, headSHA, method, msg, gitx.Signature{Name: name, Email: email})
	if err != nil {
		if errors.Is(err, gitx.ErrConflict) {
			return "", fmt.Errorf("merge conflict — resolve locally and push")
		}
		return "", err
	}
	if err := f.Store.MarkMerged(ctx, i.ID, actor.ID, sha); err != nil {
		return "", err
	}
	f.linkMergedLFS(ctx, r, i.PR.HeadRepoID, sha, before, i.PR.BaseBranch)
	f.Store.RecordEvent(ctx, r.ID, actor.ID, "pr", fmt.Sprintf("%s merged PR #%d into %s", actor.Name, i.Number, i.PR.BaseBranch))
	f.retargetStack(ctx, actor, r, i)
	after, _ := g.RefMap(ctx)
	f.PostReceive(ctx, actor, r, DiffRefs(before, after))
	f.Broker.Publish(fmt.Sprintf("issue:%d", i.ID))
	return sha, nil
}

// ------------------------------------------------------------------ releases

func (f *Forge) CreateRelease(ctx context.Context, actor *store.User, r *store.Repo, tag, target, title, notes string) (*store.Release, error) {
	if actor == nil || f.Store.AccessLevel(ctx, actor, r) < store.AccessWrite {
		return nil, ErrForbidden
	}
	g := f.Git(r)
	if _, err := g.ResolveRef(ctx, "refs/tags/"+tag); err != nil {
		if r.Mirror {
			return nil, f.ReadOnlyError(ctx, r) // releases of mirrored tags are fine, new tags aren't
		}
		if target == "" {
			target = "refs/heads/" + r.DefaultBranch
		}
		before, _ := g.RefMap(ctx)
		msg := title
		if msg == "" {
			msg = tag
		}
		if err := g.CreateTag(ctx, tag, target, msg, actor.Name); err != nil {
			return nil, err
		}
		after, _ := g.RefMap(ctx)
		defer f.PostReceive(ctx, actor, r, DiffRefs(before, after))
	}
	rel, err := f.Store.CreateRelease(ctx, r.ID, actor.ID, tag, title, notes)
	if err != nil {
		return nil, err
	}
	f.Store.RecordEvent(ctx, r.ID, actor.ID, "release", fmt.Sprintf("%s published release %s", actor.Name, rel.Title))
	return rel, nil
}

// ------------------------------------------------------------------ CI

func (f *Forge) TriggerManual(ctx context.Context, actor *store.User, r *store.Repo, ref, pipeline string) ([]*store.Run, error) {
	if actor == nil || f.Store.AccessLevel(ctx, actor, r) < store.AccessWrite {
		return nil, ErrForbidden
	}
	if ref == "" {
		ref = r.DefaultBranch
	}
	full := ref
	if !strings.HasPrefix(ref, "refs/") {
		full = "refs/heads/" + ref
	}
	sha, err := f.Git(r).ResolveRef(ctx, full)
	if err != nil {
		if sha, err = f.Git(r).ResolveRef(ctx, ref); err != nil {
			return nil, fmt.Errorf("unknown ref %q", ref)
		}
		full = ref
	}
	runs, err := f.CI.Dispatch(ctx, r, ci.Trigger{Kind: "manual", Ref: full, SHA: sha, ActorID: actor.ID, Only: pipeline})
	if err == nil && len(runs) == 0 {
		return nil, fmt.Errorf("no pipeline accepts manual triggers (add `on: {manual: true}` or name one explicitly)")
	}
	return runs, err
}

// trusted: the user may run code in r's CI without approval, i.e. has write
// access to it (and so could push a pipeline to it anyway).
func (f *Forge) trusted(ctx context.Context, userID int64, r *store.Repo) bool {
	if userID == 0 {
		return false
	}
	u, err := f.Store.UserByID(ctx, userID)
	return err == nil && f.Store.AccessLevel(ctx, u, r) >= store.AccessWrite
}

// ApproveRun lets a run that waits for approval proceed. It needs write
// access: the approver vouches for running the code on the repository's
// runners. Approved fork runs still get no secrets.
func (f *Forge) ApproveRun(ctx context.Context, actor *store.User, r *store.Repo, run *store.Run) error {
	if actor == nil || f.Store.AccessLevel(ctx, actor, r) < store.AccessWrite {
		return ErrForbidden
	}
	if err := f.CI.Approve(ctx, run.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("run #%d is not waiting for approval", run.Number)
		}
		return err
	}
	f.Store.RecordEvent(ctx, r.ID, actor.ID, "pipeline", fmt.Sprintf("%s approved pipeline %s #%d (%s)", actor.Name, run.Pipeline, run.Number, shortSHA(run.SHA)))
	return nil
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func (f *Forge) CancelRun(ctx context.Context, actor *store.User, r *store.Repo, run *store.Run) error {
	if f.Store.AccessLevel(ctx, actor, r) < store.AccessWrite {
		return ErrForbidden
	}
	return f.CI.Cancel(ctx, run.ID)
}

func (f *Forge) RerunRun(ctx context.Context, actor *store.User, r *store.Repo, run *store.Run, failedOnly bool) error {
	if f.Store.AccessLevel(ctx, actor, r) < store.AccessWrite {
		return ErrForbidden
	}
	return f.CI.Rerun(ctx, run.ID, failedOnly)
}

// JobToken returns a credential that lets a runner clone the job's repository
// over HTTP while the job is running.
func (f *Forge) JobToken(jobID int64) string {
	m := hmac.New(sha256.New, f.secret)
	fmt.Fprintf(m, "job:%d", jobID)
	return fmt.Sprintf("%d.%s", jobID, hex.EncodeToString(m.Sum(nil)))
}

// VerifyJobToken returns the job id if tok is valid.
func (f *Forge) VerifyJobToken(tok string) (int64, bool) {
	idStr, _, ok := strings.Cut(tok, ".")
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, hmac.Equal([]byte(f.JobToken(id)), []byte(tok))
}

func (f *Forge) ArtifactDir(jobID int64) string {
	return filepath.Join(f.Cfg.Path("artifacts"), strconv.FormatInt(jobID, 10))
}
