package forge

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/cooperspencer/forge/internal/store"
)

// Package registry
//
// Packages belong to a user or organization. A package whose name starts with
// the name of one of the owner's repositories ("acme/api", "acme/api/worker")
// is linked to that repository and uses its permissions — pull needs read,
// push needs write — and the repository's CI jobs may push to it. Packages
// without such a repository are private to the owner (the user, or the org's
// members; org owners and site admins may delete).
//
// Content (layers, manifests, files) lives in a content-addressed blob store
// shared by all packages: data/packages/blobs/sha256/<aa>/<hex>. Unreferenced
// blobs are removed by GCPackages.

// PkgActor is who is accessing the registry: a user, a running CI job, or
// nobody (anonymous).
type PkgActor struct {
	User    *store.User
	JobRepo *store.Repo // set for CI job tokens
	JobFork bool        // the job runs code from a fork's pull request
}

func (a PkgActor) Name() string {
	switch {
	case a.User != nil:
		return a.User.Name
	case a.JobRepo != nil:
		return "ci:" + a.JobRepo.FullName()
	}
	return "anonymous"
}

func (a PkgActor) UserID() int64 {
	if a.User != nil {
		return a.User.ID
	}
	return 0
}

var digestRe = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// ValidDigest: only sha256 digests are accepted.
func ValidDigest(d string) bool { return digestRe.MatchString(d) }

func (f *Forge) pkgDir(parts ...string) string {
	return f.Cfg.Path(append([]string{"packages"}, parts...)...)
}

// BlobPath is where the blob with the given (valid) digest is stored.
func (f *Forge) BlobPath(digest string) string {
	hex := strings.TrimPrefix(digest, "sha256:")
	return f.pkgDir("blobs", "sha256", hex[:2], hex)
}

// UploadPath is the temp file of an upload session.
func (f *Forge) UploadPath(id string) string { return f.pkgDir("uploads", id) }

// LinkedRepo returns the repository a package is linked to, if any: the
// owner's repository named like the first segment of the package name.
func (f *Forge) LinkedRepo(ctx context.Context, owner *store.User, name string) *store.Repo {
	first, _, _ := strings.Cut(name, "/")
	r, err := f.Store.RepoByName(ctx, owner.Name, first)
	if err != nil {
		return nil
	}
	return r
}

// PackageAccess is a's permission on package owner/name.
func (f *Forge) PackageAccess(ctx context.Context, a PkgActor, owner *store.User, name string) store.Access {
	repo := f.LinkedRepo(ctx, owner, name)
	if a.User != nil && a.User.Grant != nil {
		g := a.User.Grant
		// tokens: limited by the packages scope and their repositories, but
		// never worse than an anonymous client
		lvl := store.AccessNone
		if g.Allows(store.ScopePackages, store.AccessRead) && (repo != nil || g.CoversOwner(owner.Name)) {
			lvl = f.packageAccess(ctx, a, owner, repo)
			if !g.Allows(store.ScopePackages, store.AccessWrite) {
				lvl = min(lvl, store.AccessRead)
			}
		}
		return max(lvl, f.packageAccess(ctx, PkgActor{}, owner, repo))
	}
	return f.packageAccess(ctx, a, owner, repo)
}

func (f *Forge) packageAccess(ctx context.Context, a PkgActor, owner *store.User, repo *store.Repo) store.Access {
	if a.JobRepo != nil {
		if repo != nil && repo.ID == a.JobRepo.ID {
			if a.JobFork {
				return store.AccessRead
			}
			return store.AccessWrite
		}
		a = PkgActor{} // other packages: like an anonymous client
	}
	if repo != nil {
		if a.User == nil && !f.Cfg.AnonymousRead {
			return store.AccessNone
		}
		return f.Store.AccessLevel(ctx, a.User, repo)
	}
	u := a.User
	switch {
	case u == nil:
		return store.AccessNone
	case u.SiteAdmin(), u.ID == owner.ID:
		return store.AccessAdmin
	case owner.IsOrg:
		switch role, _ := f.Store.OrgRole(ctx, owner.ID, u.ID); role {
		case "owner":
			return store.AccessAdmin
		case "member":
			return store.AccessWrite
		}
	}
	return store.AccessNone
}

// VisiblePackages lists the packages a can read, optionally of one owner.
func (f *Forge) VisiblePackages(ctx context.Context, a PkgActor, owner *store.User) ([]*store.Package, error) {
	var ownerID int64
	if owner != nil {
		ownerID = owner.ID
	}
	all, err := f.Store.Packages(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	owners := map[int64]*store.User{}
	var out []*store.Package
	for _, p := range all {
		o := owners[p.OwnerID]
		if o == nil {
			if o, err = f.Store.UserByID(ctx, p.OwnerID); err != nil {
				continue
			}
			owners[p.OwnerID] = o
		}
		if f.PackageAccess(ctx, a, o, p.Name) >= store.AccessRead {
			out = append(out, p)
		}
	}
	return out, nil
}

// RepoPackages lists the packages linked to repository r.
func (f *Forge) RepoPackages(ctx context.Context, r *store.Repo) ([]*store.Package, error) {
	all, err := f.Store.Packages(ctx, r.OwnerID)
	if err != nil {
		return nil, err
	}
	var out []*store.Package
	for _, p := range all {
		first, _, _ := strings.Cut(p.Name, "/")
		if strings.EqualFold(first, r.Name) {
			out = append(out, p)
		}
	}
	return out, nil
}

// PackageEvent records activity on the linked repository, if there is one.
func (f *Forge) PackageEvent(ctx context.Context, p *store.Package, a PkgActor, msg string) {
	owner, err := f.Store.UserByID(ctx, p.OwnerID)
	if err != nil {
		return
	}
	if r := f.LinkedRepo(ctx, owner, p.Name); r != nil {
		f.Store.RecordEvent(ctx, r.ID, a.UserID(), "package", a.Name()+" "+msg)
	}
}

// ------------------------------------------------------------------ blobs

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// StoreBlob moves the finished file at tmp into the blob store, verifying its
// digest if expect is set. tmp is consumed either way.
func (f *Forge) StoreBlob(ctx context.Context, tmp, expect string) (digest string, size int64, err error) {
	defer os.Remove(tmp)
	fh, err := os.Open(tmp)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	size, err = io.Copy(h, fh)
	fh.Close()
	if err != nil {
		return "", 0, err
	}
	digest = "sha256:" + hex.EncodeToString(h.Sum(nil))
	if expect != "" && expect != digest {
		return "", 0, fmt.Errorf("%w: content is %s, expected %s", ErrDigestMismatch, digest, expect)
	}
	dst := f.BlobPath(digest)
	if _, err := os.Stat(dst); err != nil {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", 0, err
		}
		if err := os.Rename(tmp, dst); err != nil {
			return "", 0, err
		}
	}
	return digest, size, f.Store.PutBlob(ctx, digest, size)
}

var ErrDigestMismatch = errors.New("digest mismatch")

// IngestBlob streams r into the blob store.
func (f *Forge) IngestBlob(ctx context.Context, r io.Reader, expect string) (string, int64, error) {
	tmp := f.UploadPath("put-" + newID())
	if err := os.MkdirAll(filepath.Dir(tmp), 0o755); err != nil {
		return "", 0, err
	}
	fh, err := os.Create(tmp)
	if err != nil {
		return "", 0, err
	}
	_, err = io.Copy(fh, r)
	if cerr := fh.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return "", 0, err
	}
	return f.StoreBlob(ctx, tmp, expect)
}

// BlobAvailable: the blob row exists and its file is on disk.
func (f *Forge) BlobAvailable(ctx context.Context, digest string) (int64, bool) {
	size, err := f.Store.BlobSize(ctx, digest)
	if err != nil {
		return 0, false
	}
	if _, err := os.Stat(f.BlobPath(digest)); err != nil {
		return 0, false
	}
	return size, true
}

// StartUpload opens a chunked upload session for package p.
func (f *Forge) StartUpload(ctx context.Context, p *store.Package) (string, error) {
	id := newID()
	if err := os.MkdirAll(f.pkgDir("uploads"), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(f.UploadPath(id), nil, 0o644); err != nil {
		return "", err
	}
	return id, f.Store.CreateUpload(ctx, id, p.ID)
}

// AppendUpload appends r to the session and returns the new total size.
func (f *Forge) AppendUpload(ctx context.Context, u *store.Upload, r io.Reader) (int64, error) {
	fh, err := os.OpenFile(f.UploadPath(u.ID), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(fh, r)
	if cerr := fh.Close(); err == nil {
		err = cerr
	}
	u.Size += n
	f.Store.SetUploadSize(ctx, u.ID, u.Size)
	return u.Size, err
}

// FinishUpload verifies and stores the upload, and links it to its package.
func (f *Forge) FinishUpload(ctx context.Context, u *store.Upload, digest string) error {
	f.pkgMu.RLock()
	defer f.pkgMu.RUnlock()
	_, _, err := f.StoreBlob(ctx, f.UploadPath(u.ID), digest)
	f.Store.DeleteUpload(ctx, u.ID)
	if err != nil {
		return err
	}
	return f.Store.LinkBlob(ctx, u.PackageID, digest)
}

func (f *Forge) CancelUpload(ctx context.Context, u *store.Upload) {
	os.Remove(f.UploadPath(u.ID))
	f.Store.DeleteUpload(ctx, u.ID)
}

// LockPackages is held (shared) while content is being added, so garbage
// collection never deletes a blob between its upload and its reference.
func (f *Forge) LockPackages() func() {
	f.pkgMu.RLock()
	return f.pkgMu.RUnlock
}

// ------------------------------------------------------------------ deletes

func (f *Forge) DeletePackage(ctx context.Context, a PkgActor, owner *store.User, p *store.Package) error {
	if f.PackageAccess(ctx, a, owner, p.Name) < store.AccessAdmin {
		return ErrForbidden
	}
	if err := f.Store.DeletePackage(ctx, p.ID); err != nil {
		return err
	}
	f.PackageEvent(ctx, p, a, "deleted package "+p.FullName())
	return nil
}

// DeletePackageVersion removes a container tag, or a generic version (or one
// of its files if file is set).
func (f *Forge) DeletePackageVersion(ctx context.Context, a PkgActor, owner *store.User, p *store.Package, version, file string) error {
	if f.PackageAccess(ctx, a, owner, p.Name) < store.AccessWrite {
		return ErrForbidden
	}
	var err error
	what := p.FullName() + ":" + version
	switch p.Type {
	case store.PkgContainer:
		if ValidDigest(version) {
			err = f.Store.DeleteManifest(ctx, p.ID, version)
			what = p.FullName() + "@" + version
		} else {
			err = f.Store.DeleteTag(ctx, p.ID, version)
		}
	default:
		err = f.Store.DeleteFiles(ctx, p.ID, version, file)
		if file != "" {
			what += "/" + file
		}
	}
	if err != nil {
		return err
	}
	f.PackageEvent(ctx, p, a, "deleted "+what)
	return nil
}

// ------------------------------------------------------------------ GC

type GCStats struct {
	Blobs, Bytes, Links, Uploads, Packages int64
}

// PackageGCGrace protects content that is still being pushed: blobs and links
// younger than this are never collected.
var PackageGCGrace = time.Hour

// GCPackages deletes blobs no package references, abandoned uploads and
// empty packages. Untagged manifests are kept (pullable by digest) until
// deleted explicitly.
func (f *Forge) GCPackages(ctx context.Context) (GCStats, error) {
	f.pkgMu.Lock()
	defer f.pkgMu.Unlock()
	var st GCStats
	cutoff := time.Now().Add(-PackageGCGrace)

	// abandoned upload sessions
	stale, err := f.Store.StaleUploads(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		return st, err
	}
	for _, id := range stale {
		os.Remove(f.UploadPath(id))
		f.Store.DeleteUpload(ctx, id)
		st.Uploads++
	}
	if ents, err := os.ReadDir(f.pkgDir("uploads")); err == nil {
		for _, e := range ents {
			info, err := e.Info()
			if err != nil || info.ModTime().After(cutoff) || f.Store.UploadExists(ctx, e.Name()) {
				continue
			}
			os.Remove(f.UploadPath(e.Name()))
		}
	}

	gs, err := f.Store.LoadGCState(ctx)
	if err != nil {
		return st, err
	}
	live := map[string]bool{}
	for d := range gs.Referenced {
		live[d] = true
	}
	for _, l := range gs.Links {
		if !gs.Referenced[l.Digest] && l.CreatedAt.Before(cutoff) {
			f.Store.UnlinkBlob(ctx, l.PackageID, l.Digest)
			st.Links++
			continue
		}
		live[l.Digest] = true
	}
	known := map[string]bool{}
	for _, b := range gs.Blobs {
		known[b.Digest] = true
		if live[b.Digest] || b.CreatedAt.After(cutoff) {
			continue
		}
		if info, err := os.Stat(f.BlobPath(b.Digest)); err == nil {
			st.Bytes += info.Size()
		}
		os.Remove(f.BlobPath(b.Digest))
		f.Store.DeleteBlobRow(ctx, b.Digest)
		st.Blobs++
	}
	// files without a row (crash between write and insert)
	root := f.pkgDir("blobs", "sha256")
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		digest := "sha256:" + d.Name()
		if known[digest] || live[digest] {
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().Before(cutoff) {
			st.Bytes += info.Size()
			os.Remove(path)
			st.Blobs++
		}
		return nil
	})
	st.Packages, _ = f.Store.DeleteEmptyPackages(ctx, cutoff)
	return st, nil
}

// PackageGCLoop collects garbage hourly until ctx is done.
func (f *Forge) PackageGCLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			st, err := f.GCPackages(ctx)
			if err != nil {
				f.Log.Warn().Err(err).Msg("package gc failed")
			} else if st.Blobs+st.Uploads+st.Packages > 0 {
				f.Log.Info().Int64("blobs", st.Blobs).Int64("bytes", st.Bytes).Int64("uploads", st.Uploads).Int64("packages", st.Packages).Msg("package gc")
			}
		}
	}
}

// ImageSize is the total size of the content of manifest digest: the
// manifest, its config and layers, and for an index those of each platform
// (shared layers counted once).
func (f *Forge) ImageSize(ctx context.Context, p *store.Package, digest string) int64 {
	seen := map[string]bool{}
	var total int64
	var walk func(d string, depth int)
	walk = func(d string, depth int) {
		if seen[d] || depth > 3 {
			return
		}
		seen[d] = true
		m, err := f.Store.ManifestByDigest(ctx, p.ID, d)
		if err != nil {
			if n, err := f.Store.BlobSize(ctx, d); err == nil {
				total += n
			}
			return
		}
		total += m.Size
		for _, r := range m.Refs {
			walk(r, depth+1)
		}
	}
	walk(digest, 0)
	return total
}

// ImageKind describes a manifest: "image", or the platforms of a
// multi-platform index ("linux/amd64, linux/arm64"). Attestation manifests
// (platform unknown/unknown) are not platforms and are left out.
func (f *Forge) ImageKind(ctx context.Context, p *store.Package, digest string) string {
	m, err := f.Store.ManifestByDigest(ctx, p.ID, digest)
	if err != nil {
		return ""
	}
	if !strings.Contains(m.MediaType, "index") && !strings.Contains(m.MediaType, "list") {
		return "image"
	}
	b, err := os.ReadFile(f.BlobPath(digest))
	if err != nil {
		return "multi-platform"
	}
	var idx struct {
		Manifests []struct {
			Platform *struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
				Variant      string `json:"variant"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if json.Unmarshal(b, &idx) != nil {
		return "multi-platform"
	}
	var plats []string
	for _, c := range idx.Manifests {
		if c.Platform == nil || c.Platform.OS == "unknown" {
			continue
		}
		pl := c.Platform.OS + "/" + c.Platform.Architecture
		if c.Platform.Variant != "" {
			pl += "/" + c.Platform.Variant
		}
		plats = append(plats, pl)
	}
	if len(plats) == 0 {
		return "index"
	}
	return strings.Join(plats, ", ")
}

// ImageRef is how clients address a container package.
func (f *Forge) ImageRef(p *store.Package) string {
	return f.Cfg.RegistryHost() + "/" + strings.ToLower(p.FullName())
}

// GenericURL is the download URL of a generic package file.
func (f *Forge) GenericURL(p *store.Package, version, file string) string {
	return fmt.Sprintf("%s/api/packages/%s/generic/%s/%s/%s", f.Cfg.HTTP.PublicURL, p.Owner, p.Name, version, file)
}

// ResolvePackage finds owner/name for a, of type typ ("" = whichever exists;
// ambiguous if both do). Packages a can't read report ErrNotFound.
func (f *Forge) ResolvePackage(ctx context.Context, a PkgActor, full, typ string) (*store.Package, *store.User, store.Access, error) {
	ownerName, name, ok := strings.Cut(strings.Trim(full, "/"), "/")
	if !ok || name == "" {
		return nil, nil, 0, fmt.Errorf("package must be owner/name")
	}
	owner, err := f.Store.UserByName(ctx, ownerName)
	if err != nil {
		return nil, nil, 0, ErrNotFound
	}
	acc := f.PackageAccess(ctx, a, owner, name)
	if acc < store.AccessRead {
		return nil, nil, 0, ErrNotFound
	}
	types := []string{store.PkgContainer, store.PkgGeneric}
	if typ != "" {
		types = []string{typ}
	}
	var found []*store.Package
	for _, t := range types {
		if p, err := f.Store.PackageByName(ctx, owner.ID, t, name); err == nil {
			found = append(found, p)
		}
	}
	switch len(found) {
	case 0:
		return nil, nil, 0, ErrNotFound
	case 1:
		return found[0], owner, acc, nil
	}
	return nil, nil, 0, fmt.Errorf("both a container and a generic package are named %s; pass --type", full)
}
