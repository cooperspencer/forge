// Package gitx wraps the git CLI for operations on bare repositories.
// Shelling out to git keeps repositories 100% standard and lets us rely on
// git's own battle-tested implementations of merge, diff and the wire protocol.
package gitx

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var ErrNotFound = errors.New("not found")

// ErrBadArg: a ref, revision or path that could be mistaken for a git option
// (leading "-") or contains control characters. Every value that can come
// from a user goes through safe() before it reaches a git command line.
var ErrBadArg = errors.New("invalid ref or path")

func safe(vals ...string) error {
	for _, v := range vals {
		if strings.HasPrefix(v, "-") || len(v) > 1024 {
			return ErrBadArg
		}
		for _, c := range v {
			if c < 0x20 || c == 0x7f {
				return ErrBadArg
			}
		}
	}
	return nil
}

const ZeroSHA = "0000000000000000000000000000000000000000"

type Repo struct {
	Path string
}

func Open(path string) *Repo { return &Repo{Path: path} }

func (r *Repo) cmd(ctx context.Context, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, "git", append([]string{"--git-dir", r.Path}, args...)...)
	c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	return c
}

func (r *Repo) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	c := r.cmd(ctx, args...)
	if stdin != nil {
		c.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "unknown revision") || strings.Contains(msg, "bad revision") ||
			strings.Contains(msg, "does not exist") || strings.Contains(msg, "Not a valid object") || strings.Contains(msg, "not a tree object") {
			return nil, ErrNotFound
		}
		return out, fmt.Errorf("git %s: %v: %s", args[0], err, msg)
	}
	return out, nil
}

// Init creates a new bare repository with the given default branch.
func Init(ctx context.Context, path, defaultBranch string) (*Repo, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	c := exec.CommandContext(ctx, "git", "init", "--bare", "--quiet", "--initial-branch="+defaultBranch, path)
	if out, err := c.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git init: %v: %s", err, out)
	}
	r := Open(path)
	// Allow uploadpack to serve arbitrary reachable SHAs (needed for runners
	// fetching PR heads) and enable the http backend.
	r.run(ctx, nil, "config", "uploadpack.allowReachableSHA1InWant", "true")
	r.run(ctx, nil, "config", "http.receivepack", "true")
	return r, nil
}

// Clone creates a bare copy (used for forks).
func Clone(ctx context.Context, src, dst string) (*Repo, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return nil, err
	}
	c := exec.CommandContext(ctx, "git", "clone", "--bare", "--quiet", "--no-local", src, dst)
	if out, err := c.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git clone: %v: %s", err, out)
	}
	r := Open(dst)
	r.run(ctx, nil, "config", "uploadpack.allowReachableSHA1InWant", "true")
	r.run(ctx, nil, "config", "http.receivepack", "true")
	return r, nil
}

func (r *Repo) SetHead(ctx context.Context, branch string) error {
	if err := safe(branch); err != nil {
		return err
	}
	_, err := r.run(ctx, nil, "symbolic-ref", "HEAD", "refs/heads/"+branch)
	return err
}

func (r *Repo) IsEmpty(ctx context.Context) bool {
	out, err := r.run(ctx, nil, "for-each-ref", "--count=1", "refs/heads")
	return err != nil || len(bytes.TrimSpace(out)) == 0
}

// ------------------------------------------------------------------ refs

type Ref struct {
	Name    string // short name
	Full    string // refs/heads/x
	SHA     string
	Subject string
	When    time.Time
	IsTag   bool
}

// Refs returns all refs keyed by full name (used to diff before/after a push).
func (r *Repo) RefMap(ctx context.Context) (map[string]string, error) {
	out, err := r.run(ctx, nil, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads", "refs/tags")
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if sha, name, ok := strings.Cut(l, " "); ok {
			m[name] = sha
		}
	}
	return m, nil
}

func (r *Repo) refs(ctx context.Context, pattern string, tags bool) ([]Ref, error) {
	out, err := r.run(ctx, nil, "for-each-ref", "--sort=-creatordate",
		"--format=%(refname)%00%(refname:short)%00%(*objectname)%00%(objectname)%00%(creatordate:unix)%00%(contents:subject)", pattern)
	if err != nil {
		return nil, err
	}
	var refs []Ref
	for _, l := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		f := strings.Split(l, "\x00")
		if len(f) < 6 {
			continue
		}
		sha := f[3]
		if f[2] != "" { // annotated tag: peel to commit
			sha = f[2]
		}
		ts, _ := strconv.ParseInt(f[4], 10, 64)
		refs = append(refs, Ref{Full: f[0], Name: f[1], SHA: sha, When: time.Unix(ts, 0), Subject: f[5], IsTag: tags})
	}
	return refs, nil
}

func (r *Repo) Branches(ctx context.Context) ([]Ref, error) { return r.refs(ctx, "refs/heads", false) }
func (r *Repo) Tags(ctx context.Context) ([]Ref, error)     { return r.refs(ctx, "refs/tags", true) }

func (r *Repo) ResolveRef(ctx context.Context, rev string) (string, error) {
	if err := safe(rev); err != nil {
		return "", err
	}
	out, err := r.run(ctx, nil, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	if err != nil || len(out) == 0 {
		return "", ErrNotFound
	}
	return strings.TrimSpace(string(out)), nil
}

func (r *Repo) UpdateRef(ctx context.Context, ref, newSHA, oldSHA string) error {
	if err := safe(ref, newSHA, oldSHA); err != nil {
		return err
	}
	args := []string{"update-ref", ref, newSHA}
	if oldSHA != "" {
		args = append(args, oldSHA)
	}
	_, err := r.run(ctx, nil, args...)
	return err
}

func (r *Repo) DeleteRef(ctx context.Context, ref string) error {
	if err := safe(ref); err != nil {
		return err
	}
	_, err := r.run(ctx, nil, "update-ref", "-d", ref)
	return err
}

func (r *Repo) CreateTag(ctx context.Context, name, target, message, tagger string) error {
	if err := safe(name, target); err != nil {
		return err
	}
	sha, err := r.ResolveRef(ctx, target)
	if err != nil {
		return err
	}
	if message == "" {
		return r.UpdateRef(ctx, "refs/tags/"+name, sha, ZeroSHA)
	}
	c := r.cmd(ctx, "tag", "-a", name, sha, "-m", message)
	c.Env = append(c.Env, "GIT_COMMITTER_NAME="+tagger, "GIT_COMMITTER_EMAIL="+tagger+"@forge")
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("git tag: %v: %s", err, out)
	}
	return nil
}

// FetchRef copies a ref from another local repository (used for fork PRs).
func (r *Repo) FetchRef(ctx context.Context, srcPath, srcRef, dstRef string) error {
	if err := safe(srcPath, srcRef, dstRef); err != nil {
		return err
	}
	_, err := r.run(ctx, nil, "fetch", "--quiet", "--no-tags", srcPath, "+"+srcRef+":"+dstRef)
	return err
}

// ------------------------------------------------------------------ commits

type Commit struct {
	SHA         string
	Parents     []string
	Author      string
	AuthorEmail string
	When        time.Time
	Subject     string
	Body        string
	ChangeID    string // jj change ID, if the commit has one
}

func (c *Commit) Short() string {
	if len(c.SHA) > 8 {
		return c.SHA[:8]
	}
	return c.SHA
}

const logFormat = "%H%x00%P%x00%an%x00%ae%x00%at%x00%s%x00%b%x1e"

func parseCommits(out []byte) []Commit {
	var cs []Commit
	for _, rec := range strings.Split(string(out), "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\x00", 7)
		if len(f) < 7 {
			continue
		}
		ts, _ := strconv.ParseInt(f[4], 10, 64)
		c := Commit{SHA: f[0], Author: f[2], AuthorEmail: f[3], When: time.Unix(ts, 0), Subject: f[5], Body: strings.TrimSpace(f[6])}
		if f[1] != "" {
			c.Parents = strings.Fields(f[1])
		}
		cs = append(cs, c)
	}
	return cs
}

// Log lists commits reachable from rev (optionally limited to path).
func (r *Repo) Log(ctx context.Context, rev string, skip, limit int, path string) ([]Commit, error) {
	if err := safe(rev, path); err != nil {
		return nil, err
	}
	args := []string{"log", "--format=" + logFormat, fmt.Sprintf("--skip=%d", skip), fmt.Sprintf("--max-count=%d", limit), "--end-of-options", rev, "--"}
	if path != "" {
		args = append(args, path)
	}
	out, err := r.run(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	cs := parseCommits(out)
	r.fillChangeIDs(ctx, cs)
	return cs, nil
}

// CommitsBetween lists commits in head not in base (PR commits).
func (r *Repo) CommitsBetween(ctx context.Context, base, head string) ([]Commit, error) {
	if err := safe(base, head); err != nil {
		return nil, err
	}
	out, err := r.run(ctx, nil, "log", "--format="+logFormat, "--end-of-options", base+".."+head, "--")
	if err != nil {
		return nil, err
	}
	cs := parseCommits(out)
	r.fillChangeIDs(ctx, cs)
	return cs, nil
}

func (r *Repo) Commit(ctx context.Context, rev string) (*Commit, error) {
	if err := safe(rev); err != nil {
		return nil, err
	}
	out, err := r.run(ctx, nil, "log", "-1", "--format="+logFormat, "--end-of-options", rev, "--")
	if err != nil {
		return nil, err
	}
	cs := parseCommits(out)
	if len(cs) == 0 {
		return nil, ErrNotFound
	}
	r.fillChangeIDs(ctx, cs[:1])
	return &cs[0], nil
}

func (r *Repo) MergeBase(ctx context.Context, a, b string) (string, error) {
	if err := safe(a, b); err != nil {
		return "", err
	}
	out, err := r.run(ctx, nil, "merge-base", a, b)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (r *Repo) IsAncestor(ctx context.Context, a, b string) bool {
	if safe(a, b) != nil {
		return false
	}
	c := r.cmd(ctx, "merge-base", "--is-ancestor", a, b)
	return c.Run() == nil
}

// ------------------------------------------------------------------ trees

type TreeEntry struct {
	Mode string
	Type string // blob | tree | commit
	SHA  string
	Size int64
	Name string
}

func (e TreeEntry) IsDir() bool { return e.Type == "tree" }

func (r *Repo) Tree(ctx context.Context, rev, path string) ([]TreeEntry, error) {
	if err := safe(rev); err != nil {
		return nil, err
	}
	spec := rev + "^{tree}"
	if path != "" {
		spec = rev + ":" + strings.Trim(path, "/")
	}
	out, err := r.run(ctx, nil, "ls-tree", "-l", "-z", spec)
	if err != nil {
		return nil, err
	}
	var entries []TreeEntry
	for _, rec := range strings.Split(string(out), "\x00") {
		meta, name, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta)
		if len(f) < 4 {
			continue
		}
		size, _ := strconv.ParseInt(f[3], 10, 64)
		entries = append(entries, TreeEntry{Mode: f[0], Type: f[1], SHA: f[2], Size: size, Name: name})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	return entries, nil
}

// Blob returns file content at rev:path, truncated to max bytes (0 = unlimited).
func (r *Repo) Blob(ctx context.Context, rev, path string, max int64) ([]byte, bool, error) {
	if err := safe(rev); err != nil {
		return nil, false, err
	}
	out, err := r.run(ctx, nil, "cat-file", "blob", rev+":"+strings.Trim(path, "/"))
	if err != nil {
		return nil, false, err
	}
	truncated := false
	if max > 0 && int64(len(out)) > max {
		out, truncated = out[:max], true
	}
	return out, truncated, nil
}

func IsBinary(b []byte) bool {
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	return bytes.IndexByte(b[:n], 0) >= 0
}

// ListFiles lists all paths under dir at rev matching any of the suffixes.
func (r *Repo) ListFiles(ctx context.Context, rev, dir string) ([]string, error) {
	if err := safe(rev, dir); err != nil {
		return nil, err
	}
	out, err := r.run(ctx, nil, "ls-tree", "-r", "--name-only", "-z", "--end-of-options", rev, "--", dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// ------------------------------------------------------------------ diffs

type FileDiff struct {
	OldPath, NewPath string
	Status           string // A M D R
	Additions        int
	Deletions        int
	Binary           bool
	Patch            string
}

// Diff returns per-file diffs between two revisions. If from is empty, the
// commit `to` is diffed against its first parent (or the empty tree).
func (r *Repo) Diff(ctx context.Context, from, to string) ([]FileDiff, error) {
	if err := safe(from, to); err != nil {
		return nil, err
	}
	if from == "" {
		c, err := r.Commit(ctx, to)
		if err != nil {
			return nil, err
		}
		if len(c.Parents) > 0 {
			from = c.Parents[0]
		} else {
			from = "4b825dc642cb6eb9a060e54bf8d69288fbee4904" // empty tree
		}
	}
	out, err := r.run(ctx, nil, "diff", "--no-color", "--find-renames", "--patch", "--no-ext-diff", "--end-of-options", from, to, "--")
	if err != nil {
		return nil, err
	}
	return ParsePatch(string(out)), nil
}

// ParsePatch splits unified diff output into files.
func ParsePatch(p string) []FileDiff {
	var files []FileDiff
	var cur *FileDiff
	var buf strings.Builder
	flush := func() {
		if cur != nil {
			cur.Patch = buf.String()
			files = append(files, *cur)
		}
		buf.Reset()
	}
	sc := bufio.NewScanner(strings.NewReader(p))
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	inHunk := false
	for sc.Scan() {
		l := sc.Text()
		if strings.HasPrefix(l, "diff --git ") {
			flush()
			cur = &FileDiff{Status: "M"}
			inHunk = false
			rest := strings.TrimPrefix(l, "diff --git ")
			// "a/x b/x" — split at " b/" is good enough for non-pathological names
			if i := strings.Index(rest, " b/"); i >= 0 {
				cur.OldPath = strings.TrimPrefix(rest[:i], "a/")
				cur.NewPath = rest[i+3:]
			}
			continue
		}
		if cur == nil {
			continue
		}
		switch {
		case !inHunk && strings.HasPrefix(l, "new file"):
			cur.Status = "A"
		case !inHunk && strings.HasPrefix(l, "deleted file"):
			cur.Status = "D"
		case !inHunk && strings.HasPrefix(l, "rename from "):
			cur.Status, cur.OldPath = "R", strings.TrimPrefix(l, "rename from ")
		case !inHunk && strings.HasPrefix(l, "rename to "):
			cur.NewPath = strings.TrimPrefix(l, "rename to ")
		case !inHunk && strings.HasPrefix(l, "Binary files"):
			cur.Binary = true
		case strings.HasPrefix(l, "@@"):
			inHunk = true
			buf.WriteString(l + "\n")
		case inHunk:
			if strings.HasPrefix(l, "+") {
				cur.Additions++
			} else if strings.HasPrefix(l, "-") {
				cur.Deletions++
			}
			buf.WriteString(l + "\n")
		}
	}
	flush()
	return files
}

// ------------------------------------------------------------------ merge

var ErrConflict = errors.New("merge conflict")

// MergeCheck reports whether head merges cleanly into base without touching refs.
func (r *Repo) MergeCheck(ctx context.Context, base, head string) (conflicts []string, err error) {
	_, conflicts, err = r.mergeTree(ctx, base, head)
	return conflicts, err
}

func (r *Repo) mergeTree(ctx context.Context, base, head string) (tree string, conflicts []string, err error) {
	if err := safe(base, head); err != nil {
		return "", nil, err
	}
	return r.runMergeTree(ctx, "merge-tree", "--write-tree", "--name-only", "--no-messages", base, head)
}

func (r *Repo) runMergeTree(ctx context.Context, args ...string) (tree string, conflicts []string, err error) {
	c := r.cmd(ctx, args...)
	out, err := c.Output()
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		if len(lines) > 1 {
			conflicts = lines[1:]
		}
		return "", conflicts, ErrConflict
	}
	if err != nil {
		return "", nil, fmt.Errorf("git merge-tree: %w", err)
	}
	return lines[0], nil, nil
}

type Signature struct{ Name, Email string }

// Merge creates a merge commit of head into baseBranch and advances the branch.
// method: "merge" (always a merge commit), "squash", or "ff" (fast-forward only).
func (r *Repo) Merge(ctx context.Context, baseBranch, head, method, message string, sig Signature) (string, error) {
	if err := safe(baseBranch, head); err != nil {
		return "", err
	}
	ref := "refs/heads/" + baseBranch
	base, err := r.ResolveRef(ctx, ref)
	if err != nil {
		return "", err
	}
	if method == "ff" {
		if !r.IsAncestor(ctx, base, head) {
			return "", fmt.Errorf("cannot fast-forward: %s has diverged", baseBranch)
		}
		return head, r.UpdateRef(ctx, ref, head, base)
	}
	tree, _, err := r.mergeTree(ctx, base, head)
	if err != nil {
		return "", err
	}
	args := []string{"commit-tree", tree, "-p", base}
	if method != "squash" {
		args = append(args, "-p", head)
	}
	args = append(args, "-m", message)
	c := r.cmd(ctx, args...)
	c.Env = append(c.Env,
		"GIT_AUTHOR_NAME="+sig.Name, "GIT_AUTHOR_EMAIL="+sig.Email,
		"GIT_COMMITTER_NAME="+sig.Name, "GIT_COMMITTER_EMAIL="+sig.Email)
	out, err := c.Output()
	if err != nil {
		return "", fmt.Errorf("git commit-tree: %w", err)
	}
	sha := strings.TrimSpace(string(out))
	return sha, r.UpdateRef(ctx, ref, sha, base)
}

// CheckRefName validates a full ref name ("refs/heads/x") with git's rules.
func CheckRefName(ctx context.Context, full string) error {
	if err := safe(full); err != nil || strings.HasSuffix(full, "/HEAD") || strings.Contains(full, "@{") {
		return ErrBadArg
	}
	if out, err := exec.CommandContext(ctx, "git", "check-ref-format", full).CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", ErrBadArg, strings.TrimSpace(string(out)))
	}
	return nil
}

// ErrRootCommit: the first commit of a history can't be reverted.
var ErrRootCommit = errors.New("the first commit of a repository can't be reverted")

// RevertTree is the tree of onto with the changes of commit sha undone (for
// a merge commit: the changes it brought relative to its first parent).
// ErrConflict lists the paths when later changes touch the same lines.
func (r *Repo) RevertTree(ctx context.Context, onto, sha string) (tree string, conflicts []string, err error) {
	if err := safe(onto, sha); err != nil {
		return "", nil, err
	}
	c, err := r.Commit(ctx, sha)
	if err != nil {
		return "", nil, err
	}
	if len(c.Parents) == 0 {
		return "", nil, ErrRootCommit
	}
	// a three-way merge of onto and the parent, with the commit as base
	return r.runMergeTree(ctx, "merge-tree", "--write-tree", "--name-only", "--no-messages", "--merge-base="+c.SHA, onto, c.Parents[0])
}

// EmptyTree is git's empty tree.
const EmptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// PickTree is the tree of onto with the changes of commit sha applied (for a
// merge commit: the changes it brought relative to its first parent).
func (r *Repo) PickTree(ctx context.Context, onto, sha string) (tree string, conflicts []string, err error) {
	if err := safe(onto, sha); err != nil {
		return "", nil, err
	}
	c, err := r.Commit(ctx, sha)
	if err != nil {
		return "", nil, err
	}
	base := EmptyTree
	if len(c.Parents) > 0 {
		base = c.Parents[0]
	}
	return r.runMergeTree(ctx, "merge-tree", "--write-tree", "--name-only", "--no-messages", "--merge-base="+base, onto, c.SHA)
}

// TreeOf returns the tree id of a commit.
func (r *Repo) TreeOf(ctx context.Context, rev string) (string, error) {
	if err := safe(rev); err != nil {
		return "", err
	}
	out, err := r.run(ctx, nil, "rev-parse", "--verify", "--end-of-options", rev+"^{tree}")
	return strings.TrimSpace(string(out)), err
}

// CommitTree writes a commit (without moving any ref).
func (r *Repo) CommitTree(ctx context.Context, tree string, parents []string, message string, sig Signature) (string, error) {
	return r.CommitTreeAs(ctx, tree, parents, message, sig, sig, time.Time{})
}

// CommitTreeAs is CommitTree with a separate author (and author date, if set).
func (r *Repo) CommitTreeAs(ctx context.Context, tree string, parents []string, message string, author, committer Signature, authored time.Time) (string, error) {
	if err := safe(append([]string{tree}, parents...)...); err != nil {
		return "", err
	}
	args := []string{"commit-tree", tree}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	c := r.cmd(ctx, append(args, "-m", message)...)
	c.Env = append(c.Env,
		"GIT_AUTHOR_NAME="+author.Name, "GIT_AUTHOR_EMAIL="+author.Email,
		"GIT_COMMITTER_NAME="+committer.Name, "GIT_COMMITTER_EMAIL="+committer.Email)
	if !authored.IsZero() {
		c.Env = append(c.Env, fmt.Sprintf("GIT_AUTHOR_DATE=@%d +0000", authored.Unix()))
	}
	out, err := c.Output()
	if err != nil {
		return "", fmt.Errorf("git commit-tree: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
