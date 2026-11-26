package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrStale: the branch moved since the caller read it (someone else saved).
var ErrStale = errors.New("the branch changed in the meantime")

// WriteFiles commits changes to branch of a bare repository without a work
// tree: files maps a path to its new content, nil deletes it. If expect is
// set, the branch must still point at that commit (optimistic locking). It
// returns the new commit, or the old one if nothing changed.
func (r *Repo) WriteFiles(ctx context.Context, branch string, files map[string][]byte, message string, sig Signature, expect string) (string, error) {
	if err := safe(branch, expect); err != nil {
		return "", err
	}
	for p := range files {
		if err := safe(p); err != nil || p == "" || strings.HasPrefix(p, "/") {
			return "", ErrBadArg
		}
	}
	ref := "refs/heads/" + branch
	old, err := r.ResolveRef(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		old = ""
	} else if err != nil {
		return "", err
	}
	if expect != "" && old != expect {
		return "", ErrStale
	}
	dir, err := os.MkdirTemp("", "forge-index-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "index"),
		"GIT_AUTHOR_NAME=" + sig.Name, "GIT_AUTHOR_EMAIL=" + sig.Email,
		"GIT_COMMITTER_NAME=" + sig.Name, "GIT_COMMITTER_EMAIL=" + sig.Email}
	run := func(stdin []byte, args ...string) (string, error) {
		c := r.cmd(ctx, args...)
		c.Env = append(c.Env, env...)
		if stdin != nil {
			c.Stdin = bytes.NewReader(stdin)
		}
		var stderr bytes.Buffer
		c.Stderr = &stderr
		out, err := c.Output()
		if err != nil {
			return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
		}
		return strings.TrimSpace(string(out)), nil
	}
	if old != "" {
		if _, err := run(nil, "read-tree", old); err != nil {
			return "", err
		}
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		content := files[p]
		if content == nil {
			// mode 0 removes the entry (works without a work tree)
			if _, err := run([]byte("0 "+ZeroSHA+"\t"+p+"\n"), "update-index", "--index-info"); err != nil {
				return "", err
			}
			continue
		}
		blob, err := run(content, "hash-object", "-w", "--stdin")
		if err != nil {
			return "", err
		}
		if _, err := run(nil, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+p); err != nil {
			return "", err
		}
	}
	tree, err := run(nil, "write-tree")
	if err != nil {
		return "", err
	}
	if old != "" {
		if prev, err := run(nil, "rev-parse", old+"^{tree}"); err == nil && prev == tree {
			return old, nil
		}
	}
	args := []string{"commit-tree", tree, "-m", message}
	if old != "" {
		args = append(args, "-p", old)
	}
	sha, err := run(nil, args...)
	if err != nil {
		return "", err
	}
	want := old
	if want == "" {
		want = ZeroSHA // must still not exist
	}
	if err := r.UpdateRef(ctx, ref, sha, want); err != nil {
		return "", ErrStale
	}
	return sha, nil
}

// FileChange is the last commit that touched a path.
type FileChange struct {
	SHA    string
	Author string
	When   time.Time
}

// LastChanges walks the history of rev once and returns, for every path that
// still exists, the latest commit that touched it.
func (r *Repo) LastChanges(ctx context.Context, rev string) (map[string]FileChange, error) {
	if err := safe(rev); err != nil {
		return nil, err
	}
	out, err := r.run(ctx, nil, "log", "--format=%x00%H%x1f%an%x1f%at", "--name-only", "--no-renames", "--end-of-options", rev, "--")
	if err != nil {
		return nil, err
	}
	res := map[string]FileChange{}
	for _, rec := range strings.Split(string(out), "\x00") {
		lines := strings.Split(strings.TrimSpace(rec), "\n")
		f := strings.Split(lines[0], "\x1f")
		if len(f) != 3 {
			continue
		}
		ts, _ := strconv.ParseInt(f[2], 10, 64)
		ch := FileChange{SHA: f[0], Author: f[1], When: time.Unix(ts, 0)}
		for _, p := range lines[1:] {
			if p = strings.TrimSpace(p); p != "" {
				if _, ok := res[p]; !ok {
					res[p] = ch
				}
			}
		}
	}
	return res, nil
}
