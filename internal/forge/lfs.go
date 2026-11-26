package forge

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cooperspencer/forge/internal/gitx"
	"github.com/cooperspencer/forge/internal/store"
)

// Git LFS storage. Object bytes are stored once under <data>/lfs/objects by
// sha256; the database links them to the repositories allowed to serve them.

var (
	ErrLFSDisabled = errors.New("Git LFS is not enabled for this repository (enable it in the repository settings)")
	ErrLFSTooLarge = errors.New("object exceeds the server's LFS size limit")
	ErrLFSMismatch = errors.New("uploaded content does not match the object id or size")
)

func (f *Forge) lfsPath(oid string) string {
	return f.Cfg.Path("lfs", "objects", oid[:2], oid[2:4], oid)
}

// LFSMaxBytes is the largest accepted object (0 = no limit).
func (f *Forge) LFSMaxBytes() int64 { return int64(f.Cfg.LFS.MaxObjectMB) << 20 }

// LFSHas reports whether r can serve object oid.
func (f *Forge) LFSHas(ctx context.Context, r *store.Repo, oid string) (int64, bool) {
	if !gitx.ValidLFSOID(oid) {
		return 0, false
	}
	n, err := f.Store.LFSObjectSize(ctx, r.ID, oid)
	if err != nil {
		return 0, false
	}
	if _, err := os.Stat(f.lfsPath(oid)); err != nil {
		return 0, false // lost from disk: let the client upload it again
	}
	return n, true
}

// PutLFSObject stores an upload for r after checking its hash and size.
// Access checks are the caller's (write access to r).
func (f *Forge) PutLFSObject(ctx context.Context, r *store.Repo, oid string, size int64, body io.Reader) error {
	if !r.LFS {
		return ErrLFSDisabled
	}
	if !gitx.ValidLFSOID(oid) {
		return fmt.Errorf("invalid object id")
	}
	max := f.LFSMaxBytes()
	if max > 0 && size > max {
		return ErrLFSTooLarge
	}
	limit := size // size < 0: unknown (chunked upload), bounded by max
	if size < 0 {
		limit = max
		if limit <= 0 {
			limit = 1 << 62
		}
	}
	tmpDir := f.Cfg.Path("lfs", "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(tmpDir, "upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	// read at most one byte more than announced, to detect oversize bodies
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(body, limit+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if size < 0 && n > limit {
		return ErrLFSTooLarge
	}
	if (size >= 0 && n != size) || hex.EncodeToString(h.Sum(nil)) != oid {
		return ErrLFSMismatch
	}
	size = n
	f.lfsMu.Lock()
	defer f.lfsMu.Unlock()
	dst := f.lfsPath(oid)
	if _, err := os.Stat(dst); err != nil {
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if err := os.Rename(tmp.Name(), dst); err != nil {
			return err
		}
	}
	return f.Store.LinkLFSObject(ctx, r.ID, oid, size)
}

// OpenLFSObject opens an object r can serve.
func (f *Forge) OpenLFSObject(ctx context.Context, r *store.Repo, oid string) (*os.File, int64, error) {
	n, ok := f.LFSHas(ctx, r, oid)
	if !ok {
		return nil, 0, store.ErrNotFound
	}
	fh, err := os.Open(f.lfsPath(oid))
	return fh, n, err
}

// ReadLFSObject reads up to max bytes of an object r can serve (for display).
func (f *Forge) ReadLFSObject(ctx context.Context, r *store.Repo, oid string, max int64) ([]byte, bool, error) {
	fh, n, err := f.OpenLFSObject(ctx, r, oid)
	if err != nil {
		return nil, false, err
	}
	defer fh.Close()
	b, err := io.ReadAll(io.LimitReader(fh, max))
	return b, n > max, err
}

// gcLFS deletes object files no repository links to any more.
func (f *Forge) gcLFS(ctx context.Context) {
	f.lfsMu.Lock()
	defer f.lfsMu.Unlock()
	oids, err := f.Store.UnreferencedLFSObjects(ctx)
	if err != nil {
		f.Log.Warn().Err(err).Msg("lfs gc")
		return
	}
	for _, oid := range oids {
		os.Remove(f.lfsPath(oid))
	}
	if len(oids) > 0 {
		f.Log.Info().Int("objects", len(oids)).Msg("lfs gc: removed unreferenced objects")
	}
}

// linkMergedLFS makes the LFS objects that a merge brings into r servable by
// r, taking them from the pull request's head repository.
func (f *Forge) linkMergedLFS(ctx context.Context, r *store.Repo, headRepoID int64, mergeSHA string, before map[string]string, baseBranch string) {
	if headRepoID == 0 || headRepoID == r.ID {
		return
	}
	var not []string
	if old, ok := before["refs/heads/"+baseBranch]; ok {
		not = append(not, old)
	}
	ptrs, err := f.Git(r).LFSPointers(ctx, mergeSHA, not...)
	if err != nil || len(ptrs) == 0 {
		return
	}
	oids := make([]string, len(ptrs))
	for i, p := range ptrs {
		oids[i] = p.OID
	}
	if n, err := f.Store.CopyLFSLinks(ctx, headRepoID, r.ID, oids); err == nil && n > 0 {
		f.Log.Info().Str("repo", r.FullName()).Int("objects", n).Msg("lfs: linked objects from merged pull request")
	}
}

// ------------------------------------------------------------------ SSH tokens

// LFSToken is what `git-lfs-authenticate` hands out over SSH: a short-lived
// credential for one user and one repository, accepted only by the LFS API.
func (f *Forge) LFSToken(userID, repoID int64, write bool, exp time.Time) string {
	w := 0
	if write {
		w = 1
	}
	payload := fmt.Sprintf("lfs.%d.%d.%d.%d", userID, repoID, w, exp.Unix())
	m := hmac.New(sha256.New, f.secret)
	m.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(m.Sum(nil))
}

// VerifyLFSToken checks an LFS token and returns what it grants.
func (f *Forge) VerifyLFSToken(tok string) (userID, repoID int64, write, ok bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 6 || parts[0] != "lfs" {
		return 0, 0, false, false
	}
	var n [4]int64
	for i := range n {
		v, err := strconv.ParseInt(parts[i+1], 10, 64)
		if err != nil {
			return 0, 0, false, false
		}
		n[i] = v
	}
	if time.Now().Unix() > n[3] {
		return 0, 0, false, false
	}
	want := f.LFSToken(n[0], n[1], n[2] == 1, time.Unix(n[3], 0))
	if !hmac.Equal([]byte(want), []byte(tok)) {
		return 0, 0, false, false
	}
	return n[0], n[1], n[2] == 1, true
}
