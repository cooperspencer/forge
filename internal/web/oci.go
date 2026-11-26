package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/cooperspencer/forge/internal/ci"
	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/store"
)

// Container registry: the OCI Distribution API (what `docker push/pull`,
// podman, buildx, skopeo, crane, … speak), served at /v2/.
//
// Image names are <owner>/<name>[/<more>]; see forge/packages.go for how
// permissions are derived. Clients authenticate with HTTP Basic: a username
// and an API token (`docker login`), or `forge-job` and $FORGE_TOKEN in CI.

const maxManifestSize = 4 << 20

var (
	ociNameRe = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)+$`)
	ociTagRe  = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)
)

// pkgActor authenticates a registry/package request. CI job tokens are only
// valid while their job runs.
func (s *Server) pkgActor(r *http.Request) (forge.PkgActor, error) {
	pr, err := s.authenticate(r)
	if err != nil {
		return forge.PkgActor{}, err
	}
	if pr.jobID == 0 {
		return forge.PkgActor{User: pr.user}, nil
	}
	ctx := r.Context()
	j, err := s.F.Store.JobByID(ctx, pr.jobID)
	if err != nil || j.Status != store.StatusRunning {
		return forge.PkgActor{}, errors.New("job token is only valid while the job runs")
	}
	repo, err := s.jobRepo(ctx, pr.jobID)
	if err != nil {
		return forge.PkgActor{}, err
	}
	var spec ci.JobSpec
	json.Unmarshal([]byte(j.Spec), &spec)
	return forge.PkgActor{JobRepo: repo, JobFork: spec.Fork}, nil
}

func anonymous(a forge.PkgActor) bool { return a.User == nil && a.JobRepo == nil }

func ociError(w http.ResponseWriter, status int, code, msg string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="forge"`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": msg}}})
}

type ociReq struct {
	actor forge.PkgActor
	owner *store.User
	name  string // package name without the owner
	full  string // owner/name as requested
	pkg   *store.Package
}

func (s *Server) oci(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	a, err := s.pkgActor(r)
	if err != nil {
		ociError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/v2/")
	switch p {
	case "":
		// Clients probe /v2/ first; a challenge makes `docker login` send
		// credentials. Anonymous pulls proceed without them.
		if anonymous(a) {
			ociError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
			return
		}
		writeJSON(w, 200, map[string]any{})
		return
	case "_catalog":
		s.ociCatalog(w, r, a)
		return
	}

	var full, kind, ref string
	for _, sep := range []string{"/blobs/uploads/", "/blobs/", "/manifests/"} {
		if i := strings.LastIndex(p, sep); i > 0 {
			full, kind, ref = p[:i], strings.Trim(sep, "/"), p[i+len(sep):]
			break
		}
	}
	if full == "" && strings.HasSuffix(p, "/tags/list") {
		full, kind = strings.TrimSuffix(p, "/tags/list"), "tags"
	}
	if full == "" && strings.HasSuffix(p, "/blobs/uploads") { // POST without trailing slash
		full, kind = strings.TrimSuffix(p, "/blobs/uploads"), "blobs/uploads"
	}
	if full == "" {
		ociError(w, 404, "NOT_FOUND", "unknown registry endpoint")
		return
	}
	if !ociNameRe.MatchString(full) {
		ociError(w, 400, "NAME_INVALID", "image names are <owner>/<name>, lower case")
		return
	}
	ownerName, name, _ := strings.Cut(full, "/")
	q := &ociReq{actor: a, full: full, name: name}
	write := r.Method != http.MethodGet && r.Method != http.MethodHead
	need := store.AccessRead
	if write {
		need = store.AccessWrite
	}
	q.owner, err = s.F.Store.UserByName(r.Context(), ownerName)
	lvl := store.AccessNone
	if err == nil {
		lvl = s.F.PackageAccess(r.Context(), a, q.owner, name)
	}
	if lvl < need {
		switch {
		case anonymous(a):
			ociError(w, 401, "UNAUTHORIZED", "authentication required")
		case lvl >= store.AccessRead || write:
			ociError(w, 403, "DENIED", fmt.Sprintf("%s may not %s %s", a.Name(), map[bool]string{true: "push to", false: "pull"}[write], full))
		default:
			ociError(w, 404, "NAME_UNKNOWN", "repository name not known to registry")
		}
		return
	}
	ctx := r.Context()
	if write && r.Method != http.MethodDelete {
		q.pkg, err = s.F.Store.EnsurePackage(ctx, q.owner.ID, store.PkgContainer, strings.ToLower(name))
	} else {
		q.pkg, err = s.F.Store.PackageByName(ctx, q.owner.ID, store.PkgContainer, name)
	}
	if err != nil {
		ociError(w, 404, "NAME_UNKNOWN", "repository name not known to registry")
		return
	}

	switch {
	case kind == "tags" && r.Method == http.MethodGet:
		s.ociTags(w, r, q)
	case kind == "manifests":
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			s.ociGetManifest(w, r, q, ref)
		case http.MethodPut:
			s.ociPutManifest(w, r, q, ref)
		case http.MethodDelete:
			s.ociDeleteManifest(w, r, q, ref)
		default:
			ociError(w, 405, "UNSUPPORTED", "method not allowed")
		}
	case kind == "blobs":
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			s.ociGetBlob(w, r, q, ref)
		default:
			ociError(w, 405, "UNSUPPORTED", "blobs are removed by garbage collection once unreferenced")
		}
	case kind == "blobs/uploads":
		if ref == "" {
			if r.Method != http.MethodPost {
				ociError(w, 405, "UNSUPPORTED", "method not allowed")
				return
			}
			s.ociStartUpload(w, r, q)
			return
		}
		s.ociUpload(w, r, q, ref)
	default:
		ociError(w, 405, "UNSUPPORTED", "method not allowed")
	}
}

// ---------------------------------------------------------------- blobs

func (s *Server) ociGetBlob(w http.ResponseWriter, r *http.Request, q *ociReq, digest string) {
	ctx := r.Context()
	if !forge.ValidDigest(digest) {
		ociError(w, 400, "DIGEST_INVALID", "unsupported digest")
		return
	}
	if _, ok := s.F.BlobAvailable(ctx, digest); !ok || !s.F.Store.BlobLinked(ctx, q.pkg.ID, digest) {
		ociError(w, 404, "BLOB_UNKNOWN", "blob unknown to registry")
		return
	}
	s.serveBlob(w, r, digest, "application/octet-stream")
}

func (s *Server) serveBlob(w http.ResponseWriter, r *http.Request, digest, contentType string) {
	f, err := os.Open(s.F.BlobPath(digest))
	if err != nil {
		ociError(w, 404, "BLOB_UNKNOWN", "blob unknown to registry")
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("ETag", `"`+digest+`"`)
	w.Header().Set("Cache-Control", "max-age=31536000")
	http.ServeContent(w, r, "", st.ModTime(), f)
}

func (s *Server) ociStartUpload(w http.ResponseWriter, r *http.Request, q *ociReq) {
	ctx := r.Context()
	qs := r.URL.Query()
	// Cross-repository mount: reuse a blob the client can read elsewhere.
	if mount, from := qs.Get("mount"), qs.Get("from"); mount != "" && from != "" && forge.ValidDigest(mount) {
		if s.canMount(ctx, q.actor, from, mount) {
			if err := s.F.Store.LinkBlob(ctx, q.pkg.ID, mount); err == nil {
				w.Header().Set("Location", "/v2/"+q.full+"/blobs/"+mount)
				w.Header().Set("Docker-Content-Digest", mount)
				w.WriteHeader(http.StatusCreated)
				return
			}
		}
		// fall through: a normal upload (allowed by the spec)
	}
	if d := qs.Get("digest"); d != "" { // monolithic upload in the POST
		if !forge.ValidDigest(d) {
			ociError(w, 400, "DIGEST_INVALID", "unsupported digest")
			return
		}
		unlock := s.F.LockPackages()
		defer unlock()
		if _, _, err := s.F.IngestBlob(ctx, r.Body, d); err != nil {
			ociError(w, 400, "DIGEST_INVALID", err.Error())
			return
		}
		s.F.Store.LinkBlob(ctx, q.pkg.ID, d)
		w.Header().Set("Location", "/v2/"+q.full+"/blobs/"+d)
		w.Header().Set("Docker-Content-Digest", d)
		w.WriteHeader(http.StatusCreated)
		return
	}
	id, err := s.F.StartUpload(ctx, q.pkg)
	if err != nil {
		ociError(w, 500, "UNKNOWN", err.Error())
		return
	}
	uploadHeaders(w, q, id, 0)
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) canMount(ctx context.Context, a forge.PkgActor, from, digest string) bool {
	ownerName, name, ok := strings.Cut(from, "/")
	if !ok {
		return false
	}
	owner, err := s.F.Store.UserByName(ctx, ownerName)
	if err != nil || s.F.PackageAccess(ctx, a, owner, name) < store.AccessRead {
		return false
	}
	src, err := s.F.Store.PackageByName(ctx, owner.ID, store.PkgContainer, name)
	if err != nil || !s.F.Store.BlobLinked(ctx, src.ID, digest) {
		return false
	}
	_, ok = s.F.BlobAvailable(ctx, digest)
	return ok
}

func uploadHeaders(w http.ResponseWriter, q *ociReq, id string, size int64) {
	w.Header().Set("Location", "/v2/"+q.full+"/blobs/uploads/"+id)
	w.Header().Set("Docker-Upload-UUID", id)
	end := size - 1
	if end < 0 {
		end = 0
	}
	w.Header().Set("Range", fmt.Sprintf("0-%d", end))
	w.Header().Set("Content-Length", "0")
}

func (s *Server) ociUpload(w http.ResponseWriter, r *http.Request, q *ociReq, id string) {
	ctx := r.Context()
	u, err := s.F.Store.UploadByID(ctx, id)
	if err != nil || u.PackageID != q.pkg.ID {
		ociError(w, 404, "BLOB_UPLOAD_UNKNOWN", "upload unknown")
		return
	}
	switch r.Method {
	case http.MethodGet:
		uploadHeaders(w, q, id, u.Size)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		s.F.CancelUpload(ctx, u)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPatch:
		if cr := r.Header.Get("Content-Range"); cr != "" {
			start, _, _ := strings.Cut(strings.TrimPrefix(cr, "bytes "), "-")
			if n, err := strconv.ParseInt(start, 10, 64); err != nil || n != u.Size {
				uploadHeaders(w, q, id, u.Size)
				ociError(w, http.StatusRequestedRangeNotSatisfiable, "BLOB_UPLOAD_INVALID", "chunk out of order")
				return
			}
		}
		size, err := s.F.AppendUpload(ctx, u, r.Body)
		if err != nil {
			ociError(w, 500, "BLOB_UPLOAD_INVALID", err.Error())
			return
		}
		uploadHeaders(w, q, id, size)
		w.WriteHeader(http.StatusAccepted)
	case http.MethodPut:
		digest := r.URL.Query().Get("digest")
		if !forge.ValidDigest(digest) {
			ociError(w, 400, "DIGEST_INVALID", "digest query parameter missing or unsupported")
			return
		}
		if _, err := s.F.AppendUpload(ctx, u, r.Body); err != nil {
			ociError(w, 500, "BLOB_UPLOAD_INVALID", err.Error())
			return
		}
		if err := s.F.FinishUpload(ctx, u, digest); err != nil {
			code := 500
			if errors.Is(err, forge.ErrDigestMismatch) {
				code = 400
			}
			ociError(w, code, "DIGEST_INVALID", err.Error())
			return
		}
		w.Header().Set("Location", "/v2/"+q.full+"/blobs/"+digest)
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusCreated)
	default:
		ociError(w, 405, "UNSUPPORTED", "method not allowed")
	}
}

// ---------------------------------------------------------------- manifests

type ociDescriptor struct {
	MediaType string   `json:"mediaType"`
	Digest    string   `json:"digest"`
	Size      int64    `json:"size"`
	URLs      []string `json:"urls,omitempty"`
}

type ociManifest struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Config        *ociDescriptor  `json:"config"`
	Layers        []ociDescriptor `json:"layers"`
	Manifests     []ociDescriptor `json:"manifests"`
	Subject       *ociDescriptor  `json:"subject"`
}

var manifestTypes = map[string]bool{
	"application/vnd.oci.image.manifest.v1+json":                true,
	"application/vnd.oci.image.index.v1+json":                   true,
	"application/vnd.docker.distribution.manifest.v2+json":      true,
	"application/vnd.docker.distribution.manifest.list.v2+json": true,
}

func (s *Server) resolveRef(ctx context.Context, q *ociReq, ref string) (string, error) {
	if forge.ValidDigest(ref) {
		return ref, nil
	}
	if !ociTagRe.MatchString(ref) {
		return "", store.ErrNotFound
	}
	return s.F.Store.TagDigest(ctx, q.pkg.ID, ref)
}

func (s *Server) ociGetManifest(w http.ResponseWriter, r *http.Request, q *ociReq, ref string) {
	ctx := r.Context()
	digest, err := s.resolveRef(ctx, q, ref)
	var m *store.Manifest
	if err == nil {
		m, err = s.F.Store.ManifestByDigest(ctx, q.pkg.ID, digest)
	}
	if err != nil {
		ociError(w, 404, "MANIFEST_UNKNOWN", "manifest unknown")
		return
	}
	s.serveBlob(w, r, m.Digest, m.MediaType)
}

func (s *Server) ociPutManifest(w http.ResponseWriter, r *http.Request, q *ociReq, ref string) {
	ctx := r.Context()
	isDigest := forge.ValidDigest(ref)
	if !isDigest && !ociTagRe.MatchString(ref) {
		ociError(w, 400, "TAG_INVALID", "invalid tag")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxManifestSize+1))
	if err != nil || len(body) > maxManifestSize {
		ociError(w, 400, "MANIFEST_INVALID", "manifest too large (max 4 MiB)")
		return
	}
	var m ociManifest
	if err := json.Unmarshal(body, &m); err != nil || m.SchemaVersion != 2 {
		ociError(w, 400, "MANIFEST_INVALID", "not a schema version 2 manifest")
		return
	}
	mt := r.Header.Get("Content-Type")
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = mt[:i]
	}
	if m.MediaType != "" {
		mt = m.MediaType
	}
	if !manifestTypes[mt] {
		if len(m.Manifests) > 0 {
			mt = "application/vnd.oci.image.index.v1+json"
		} else {
			mt = "application/vnd.oci.image.manifest.v1+json"
		}
	}

	unlock := s.F.LockPackages()
	defer unlock()
	// Everything referenced must already be in this package: blobs uploaded
	// or mounted here, child manifests pushed here.
	var refs []string
	var missing []string
	if m.Config != nil {
		refs = append(refs, m.Config.Digest)
		if !s.blobHere(ctx, q, m.Config.Digest) {
			missing = append(missing, m.Config.Digest)
		}
	}
	for _, l := range m.Layers {
		if len(l.URLs) > 0 { // non-distributable layer hosted elsewhere
			continue
		}
		refs = append(refs, l.Digest)
		if !s.blobHere(ctx, q, l.Digest) {
			missing = append(missing, l.Digest)
		}
	}
	for _, c := range m.Manifests {
		refs = append(refs, c.Digest)
		if _, err := s.F.Store.ManifestByDigest(ctx, q.pkg.ID, c.Digest); err != nil {
			missing = append(missing, c.Digest)
		}
	}
	if len(missing) > 0 {
		code := "BLOB_UNKNOWN"
		if len(m.Manifests) > 0 {
			code = "MANIFEST_BLOB_UNKNOWN"
		}
		ociError(w, 400, code, "referenced content not uploaded to this repository: "+strings.Join(missing, ", "))
		return
	}
	digest, size, err := s.F.IngestBlob(ctx, bytes.NewReader(body), "")
	if err != nil {
		ociError(w, 500, "UNKNOWN", err.Error())
		return
	}
	if isDigest && digest != ref {
		ociError(w, 400, "DIGEST_INVALID", "manifest digest is "+digest)
		return
	}
	if err := s.F.Store.PutManifest(ctx, &store.Manifest{PackageID: q.pkg.ID, Digest: digest, MediaType: mt, Size: size, Refs: refs}); err != nil {
		ociError(w, 500, "UNKNOWN", err.Error())
		return
	}
	s.F.Store.LinkBlob(ctx, q.pkg.ID, digest)
	if !isDigest {
		if err := s.F.Store.SetTag(ctx, q.pkg.ID, ref, digest); err != nil {
			ociError(w, 500, "UNKNOWN", err.Error())
			return
		}
		s.F.PackageEvent(ctx, q.pkg, q.actor, fmt.Sprintf("pushed image %s:%s", q.pkg.FullName(), ref))
	}
	if m.Subject != nil {
		w.Header().Set("OCI-Subject", m.Subject.Digest)
	}
	w.Header().Set("Location", "/v2/"+q.full+"/manifests/"+digest)
	w.Header().Set("Docker-Content-Digest", digest)
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) blobHere(ctx context.Context, q *ociReq, digest string) bool {
	if !forge.ValidDigest(digest) || !s.F.Store.BlobLinked(ctx, q.pkg.ID, digest) {
		return false
	}
	_, ok := s.F.BlobAvailable(ctx, digest)
	return ok
}

func (s *Server) ociDeleteManifest(w http.ResponseWriter, r *http.Request, q *ociReq, ref string) {
	if err := s.F.DeletePackageVersion(r.Context(), q.actor, q.owner, q.pkg, ref, ""); err != nil {
		if errors.Is(err, forge.ErrForbidden) {
			ociError(w, 403, "DENIED", "permission denied")
			return
		}
		ociError(w, 404, "MANIFEST_UNKNOWN", "manifest unknown")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// ---------------------------------------------------------------- listings

// paginate applies the ?n=&last= parameters to a sorted list.
func paginate(w http.ResponseWriter, r *http.Request, base string, items []string) []string {
	sort.Strings(items)
	if last := r.URL.Query().Get("last"); last != "" {
		i := sort.SearchStrings(items, last)
		if i < len(items) && items[i] == last {
			i++
		}
		items = items[i:]
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("n")); err == nil && n >= 0 && n < len(items) {
		items = items[:n]
		if n > 0 {
			w.Header().Set("Link", fmt.Sprintf(`<%s?n=%d&last=%s>; rel="next"`, base, n, items[n-1]))
		}
	}
	if items == nil {
		items = []string{}
	}
	return items
}

func (s *Server) ociTags(w http.ResponseWriter, r *http.Request, q *ociReq) {
	tags, err := s.F.Store.Tags(r.Context(), q.pkg.ID)
	if err != nil {
		ociError(w, 500, "UNKNOWN", err.Error())
		return
	}
	names := make([]string, 0, len(tags))
	for _, t := range tags {
		names = append(names, t.Name)
	}
	writeJSON(w, 200, map[string]any{"name": q.full, "tags": paginate(w, r, "/v2/"+q.full+"/tags/list", names)})
}

func (s *Server) ociCatalog(w http.ResponseWriter, r *http.Request, a forge.PkgActor) {
	if anonymous(a) && !s.F.Cfg.AnonymousRead {
		ociError(w, 401, "UNAUTHORIZED", "authentication required")
		return
	}
	pkgs, err := s.F.VisiblePackages(r.Context(), a, nil)
	if err != nil {
		ociError(w, 500, "UNKNOWN", err.Error())
		return
	}
	var names []string
	for _, p := range pkgs {
		if p.Type == store.PkgContainer {
			names = append(names, strings.ToLower(p.FullName()))
		}
	}
	writeJSON(w, 200, map[string]any{"repositories": paginate(w, r, "/v2/_catalog", names)})
}
