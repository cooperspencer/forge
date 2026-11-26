package forge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cooperspencer/forge/internal/gitx"
	"github.com/cooperspencer/forge/internal/store"
)

// Wiki: every repository can have a wiki, stored as Markdown files in a
// separate bare repository next to it (owner/name.wiki.git, cloneable like
// any repo). A page "Getting Started" is the file Getting-Started.md; pages
// may live in folders ("guides/Setup"). Readers of the repository can read
// the wiki, writers can edit it.

const (
	WikiBranch = "main"
	WikiHome   = "Home"
)

var ErrWikiPageExists = errors.New("a page with that name already exists")

// WikiPath is the bare repository holding r's wiki.
func (f *Forge) WikiPath(r *store.Repo) string {
	return strings.TrimSuffix(f.RepoPath(r), ".git") + ".wiki.git"
}

func (f *Forge) wikiGit(r *store.Repo) *gitx.Repo { return gitx.Open(f.WikiPath(r)) }

func (f *Forge) wikiExists(r *store.Repo) bool {
	_, err := os.Stat(f.WikiPath(r))
	return err == nil
}

// HasWiki reports whether r's wiki has any pages (cheap: no git process).
func (f *Forge) HasWiki(r *store.Repo) bool {
	dir := f.WikiPath(r)
	if heads, err := os.ReadDir(filepath.Join(dir, "refs", "heads")); err == nil && len(heads) > 0 {
		return true
	}
	packed, err := os.ReadFile(filepath.Join(dir, "packed-refs"))
	return err == nil && strings.Contains(string(packed), " refs/heads/")
}

// EnsureWiki creates the wiki repository if it doesn't exist yet.
func (f *Forge) EnsureWiki(ctx context.Context, r *store.Repo) error {
	if f.wikiExists(r) {
		return nil
	}
	_, err := gitx.Init(ctx, f.WikiPath(r), WikiBranch)
	return err
}

// WikiFile maps a page name to its file: "Getting Started" → Getting-Started.md.
func WikiFile(name string) (string, error) {
	name = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(name), ".md"))
	if name == "" {
		return "", fmt.Errorf("page name is required")
	}
	if len(name) > 200 || strings.ContainsAny(name, "\\\x00") {
		return "", fmt.Errorf("invalid page name %q", name)
	}
	var parts []string
	for _, p := range strings.Split(name, "/") {
		p = strings.TrimSpace(p)
		if p == "" || p == "." || p == ".." || strings.HasPrefix(p, ".") || strings.HasPrefix(p, "-") {
			return "", fmt.Errorf("invalid page name %q", name)
		}
		for _, c := range p {
			if c < 0x20 || c == 0x7f {
				return "", fmt.Errorf("invalid page name %q", name)
			}
		}
		parts = append(parts, strings.Join(strings.Fields(p), "-"))
	}
	return strings.Join(parts, "/") + ".md", nil
}

// WikiName is the display name of a page file: Getting-Started.md → "Getting Started".
func WikiName(file string) string {
	return strings.ReplaceAll(strings.TrimSuffix(file, ".md"), "-", " ")
}

type WikiPage struct {
	Name    string // display name
	Path    string // file in the wiki repository
	Author  string // last editor
	Updated time.Time
	SHA     string // last commit touching the page
}

func (f *Forge) wikiRead(ctx context.Context, actor *store.User, r *store.Repo) error {
	if f.Store.AccessLevel(ctx, actor, r) < store.AccessRead {
		return ErrNotFound
	}
	return nil
}

// WikiPages lists the pages, Home first, then alphabetically.
func (f *Forge) WikiPages(ctx context.Context, actor *store.User, r *store.Repo) ([]WikiPage, error) {
	if err := f.wikiRead(ctx, actor, r); err != nil {
		return nil, err
	}
	if !f.wikiExists(r) {
		return nil, nil
	}
	g := f.wikiGit(r)
	if g.IsEmpty(ctx) {
		return nil, nil
	}
	files, err := g.ListFiles(ctx, "refs/heads/"+WikiBranch, ".")
	if err != nil {
		if errors.Is(err, gitx.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	changes, _ := g.LastChanges(ctx, "refs/heads/"+WikiBranch)
	var out []WikiPage
	for _, p := range files {
		if !strings.HasSuffix(strings.ToLower(p), ".md") {
			continue
		}
		ch := changes[p]
		out = append(out, WikiPage{Name: WikiName(p), Path: p, Author: ch.Author, Updated: ch.When, SHA: ch.SHA})
	}
	sort.SliceStable(out, func(i, j int) bool {
		hi, hj := strings.EqualFold(out[i].Name, WikiHome), strings.EqualFold(out[j].Name, WikiHome)
		if hi != hj {
			return hi
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// findWikiPage resolves a page name (case- and dash/space-insensitive).
func (f *Forge) findWikiPage(ctx context.Context, actor *store.User, r *store.Repo, name string) (*WikiPage, error) {
	file, err := WikiFile(name)
	if err != nil {
		return nil, err
	}
	pages, err := f.WikiPages(ctx, actor, r)
	if err != nil {
		return nil, err
	}
	for i := range pages {
		if pages[i].Path == file {
			return &pages[i], nil
		}
	}
	for i := range pages {
		if strings.EqualFold(pages[i].Path, file) {
			return &pages[i], nil
		}
	}
	return nil, ErrNotFound
}

// WikiPageContent returns a page and its Markdown. rev "" is the current
// version; otherwise a commit from the page's history.
func (f *Forge) WikiPageContent(ctx context.Context, actor *store.User, r *store.Repo, name, rev string) (*WikiPage, string, error) {
	p, err := f.findWikiPage(ctx, actor, r, name)
	if err != nil {
		return nil, "", err
	}
	if rev == "" {
		rev = "refs/heads/" + WikiBranch
	}
	b, _, err := f.wikiGit(r).Blob(ctx, rev, p.Path, 4<<20)
	if err != nil {
		return nil, "", err
	}
	return p, string(b), nil
}

// WikiHead is the wiki's current commit ("" if empty), for optimistic locking.
func (f *Forge) WikiHead(ctx context.Context, r *store.Repo) string {
	if !f.wikiExists(r) {
		return ""
	}
	sha, _ := f.wikiGit(r).ResolveRef(ctx, "refs/heads/"+WikiBranch)
	return sha
}

// WikiEdit creates, edits or renames a page.
type WikiEdit struct {
	OldName string // "" to create a page
	Name    string
	Content string
	Message string // commit message (default: "Update <name>")
	Expect  string // WikiHead when editing started: refuse if someone saved since
}

func (f *Forge) signature(actor *store.User) gitx.Signature {
	email := actor.Email
	if email == "" {
		email = actor.Name + "@" + strings.Split(f.Cfg.SSH.PublicAddr, ":")[0]
	}
	name := actor.FullName
	if name == "" {
		name = actor.Name
	}
	return gitx.Signature{Name: name, Email: email}
}

func (f *Forge) wikiWrite(ctx context.Context, actor *store.User, r *store.Repo) error {
	if actor == nil || f.Store.AccessLevel(ctx, actor, r) < store.AccessWrite {
		return ErrForbidden
	}
	return nil
}

// SaveWikiPage stores a page and returns its file name.
func (f *Forge) SaveWikiPage(ctx context.Context, actor *store.User, r *store.Repo, e WikiEdit) (string, error) {
	if err := f.wikiWrite(ctx, actor, r); err != nil {
		return "", err
	}
	file, err := WikiFile(e.Name)
	if err != nil {
		return "", err
	}
	if err := f.EnsureWiki(ctx, r); err != nil {
		return "", err
	}
	files := map[string][]byte{file: []byte(strings.TrimRight(strings.ReplaceAll(e.Content, "\r\n", "\n"), "\n") + "\n")}
	verb := "Create"
	var old *WikiPage
	if e.OldName != "" {
		if old, err = f.findWikiPage(ctx, actor, r, e.OldName); err != nil {
			return "", err
		}
		verb = "Update"
		if old.Path != file {
			files[old.Path] = nil
			verb = "Rename"
		}
	}
	if old == nil || old.Path != file {
		// creating, or renaming onto another page's name
		if existing, err := f.findWikiPage(ctx, actor, r, e.Name); err == nil && (old == nil || existing.Path != old.Path) {
			return "", ErrWikiPageExists
		}
	}
	name := WikiName(file)
	msg := strings.TrimSpace(e.Message)
	if msg == "" {
		msg = verb + " " + name
		if verb == "Rename" {
			msg = fmt.Sprintf("Rename %s to %s", old.Name, name)
		}
	}
	if _, err := f.wikiGit(r).WriteFiles(ctx, WikiBranch, files, msg, f.signature(actor), e.Expect); err != nil {
		if errors.Is(err, gitx.ErrStale) {
			return "", fmt.Errorf("someone else saved the wiki while you were editing; reopen the page and try again")
		}
		return "", err
	}
	what := map[string]string{"Create": "created", "Update": "edited", "Rename": "renamed"}[verb]
	f.Store.RecordEvent(ctx, r.ID, actor.ID, "wiki", fmt.Sprintf("%s %s wiki page %s", actor.Name, what, name))
	f.Broker.Publish(fmt.Sprintf("wiki:%d", r.ID))
	return file, nil
}

func (f *Forge) DeleteWikiPage(ctx context.Context, actor *store.User, r *store.Repo, name string) error {
	if err := f.wikiWrite(ctx, actor, r); err != nil {
		return err
	}
	p, err := f.findWikiPage(ctx, actor, r, name)
	if err != nil {
		return err
	}
	if _, err := f.wikiGit(r).WriteFiles(ctx, WikiBranch, map[string][]byte{p.Path: nil}, "Delete "+p.Name, f.signature(actor), ""); err != nil {
		return err
	}
	f.Store.RecordEvent(ctx, r.ID, actor.ID, "wiki", fmt.Sprintf("%s deleted wiki page %s", actor.Name, p.Name))
	f.Broker.Publish(fmt.Sprintf("wiki:%d", r.ID))
	return nil
}

// WikiHistory lists the commits that changed a page, newest first.
func (f *Forge) WikiHistory(ctx context.Context, actor *store.User, r *store.Repo, name string) ([]gitx.Commit, error) {
	p, err := f.findWikiPage(ctx, actor, r, name)
	if err != nil {
		return nil, err
	}
	return f.wikiGit(r).Log(ctx, "refs/heads/"+WikiBranch, 0, 200, p.Path)
}

// WikiPostPush runs after a git push to the wiki (no CI, just an event).
func (f *Forge) WikiPostPush(ctx context.Context, actor *store.User, r *store.Repo) {
	if actor != nil {
		f.Store.RecordEvent(ctx, r.ID, actor.ID, "wiki", fmt.Sprintf("%s pushed to the wiki", actor.Name))
	}
	f.Broker.Publish(fmt.Sprintf("wiki:%d", r.ID))
}
