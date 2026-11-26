package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/gitx"
	"github.com/cooperspencer/forge/internal/store"
)

// Git LFS API (https://github.com/git-lfs/git-lfs/blob/main/docs/api):
//
//	POST <repo>.git/info/lfs/objects/batch
//	PUT  <repo>.git/info/lfs/objects/<oid>   upload (write access, LFS enabled)
//	GET  <repo>.git/info/lfs/objects/<oid>   download
//	POST <repo>.git/info/lfs/locks/verify    always "no locks"
//
// Credentials are the same as for git over HTTP (token as password, or the
// job token in CI), plus the short-lived tokens `git-lfs-authenticate` hands
// out over SSH.

const lfsMediaType = "application/vnd.git-lfs+json"

// isLFSRequest splits "/owner/repo.git/info/lfs/objects/batch".
func isLFSRequest(p string) (repo, rest string, ok bool) {
	p = strings.TrimPrefix(p, "/")
	i := strings.Index(p, "/info/lfs/")
	if i <= 0 {
		return "", "", false
	}
	return strings.TrimSuffix(p[:i], ".git"), p[i+len("/info/lfs"):], true
}

func lfsError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", lfsMediaType)
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

// lfsAuth resolves the repository and whether the caller may upload to it.
func (s *Server) lfsAuth(w http.ResponseWriter, r *http.Request, full string) (*store.Repo, bool, bool) {
	ctx := r.Context()
	deny := func(err error) (*store.Repo, bool, bool) {
		switch {
		case errors.Is(err, forge.ErrForbidden):
			lfsError(w, http.StatusForbidden, "permission denied")
		case errors.Is(err, store.ErrScope):
			lfsError(w, http.StatusForbidden, err.Error())
		case errors.Is(err, errUnauthorized):
			w.Header().Set("LFS-Authenticate", `Basic realm="forge"`)
			w.Header().Set("WWW-Authenticate", `Basic realm="forge"`)
			lfsError(w, http.StatusUnauthorized, "authentication required")
		default:
			lfsError(w, http.StatusNotFound, "repository not found")
		}
		return nil, false, false
	}
	// tokens from `git-lfs-authenticate` over SSH
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer lfs.") {
		uid, rid, write, ok := s.F.VerifyLFSToken(strings.TrimPrefix(h, "Bearer "))
		if !ok {
			return deny(errUnauthorized)
		}
		u, err := s.F.Store.UserByID(ctx, uid)
		if err != nil {
			return deny(errUnauthorized)
		}
		repo, acc, err := s.F.ResolveRepo(ctx, u, full, store.AccessRead)
		if err != nil || repo.ID != rid {
			return deny(store.ErrNotFound)
		}
		return repo, write && acc >= store.AccessWrite, true
	}
	pr, err := s.authenticate(r)
	if err != nil {
		return deny(errUnauthorized)
	}
	if pr.jobID != 0 {
		repo, err := s.jobRepo(ctx, pr.jobID)
		if err != nil || !strings.EqualFold(repo.FullName(), strings.TrimSuffix(strings.Trim(full, "/"), ".git")) {
			return deny(forge.ErrForbidden)
		}
		return repo, false, true // job tokens are read-only
	}
	u := pr.user
	if u != nil && !u.Grant.Allows(store.ScopeCode, store.AccessRead) {
		u = nil // like an anonymous client
	}
	repo, acc, err := s.F.ResolveRepo(ctx, u, full, store.AccessRead)
	if err != nil {
		if pr.user != nil && u == nil {
			return deny(pr.user.Grant.Require(store.ScopeCode, store.AccessRead))
		}
		if u == nil {
			return deny(errUnauthorized)
		}
		return deny(err)
	}
	return repo, acc >= store.AccessWrite && u != nil && u.Grant.Allows(store.ScopeCode, store.AccessWrite), true
}

// lfsDenyWrite answers a write without permission: 401 for anonymous
// clients (so git-lfs asks for credentials), 403 otherwise.
func lfsDenyWrite(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") == "" {
		w.Header().Set("LFS-Authenticate", `Basic realm="forge"`)
		w.Header().Set("WWW-Authenticate", `Basic realm="forge"`)
		lfsError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	lfsError(w, http.StatusForbidden, "permission denied")
}

func (s *Server) serveLFS(w http.ResponseWriter, r *http.Request, full, rest string) {
	repo, canWrite, ok := s.lfsAuth(w, r, full)
	if !ok {
		return
	}
	switch {
	case rest == "/objects/batch" && r.Method == http.MethodPost:
		s.lfsBatch(w, r, repo, canWrite)
	case strings.HasPrefix(rest, "/objects/") && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		s.lfsDownload(w, r, repo, strings.TrimPrefix(rest, "/objects/"))
	case strings.HasPrefix(rest, "/objects/") && r.Method == http.MethodPut:
		if !canWrite {
			lfsDenyWrite(w, r)
			return
		}
		s.lfsUpload(w, r, repo, strings.TrimPrefix(rest, "/objects/"))
	case rest == "/locks/verify" && r.Method == http.MethodPost:
		if !canWrite {
			lfsDenyWrite(w, r)
			return
		}
		w.Header().Set("Content-Type", lfsMediaType)
		w.Write([]byte(`{"ours":[],"theirs":[]}`))
	case rest == "/locks" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", lfsMediaType)
		w.Write([]byte(`{"locks":[]}`))
	case strings.HasPrefix(rest, "/locks"):
		lfsError(w, http.StatusNotImplemented, "file locking is not supported by this server")
	default:
		lfsError(w, http.StatusNotFound, "not found")
	}
}

type lfsObject struct {
	OID           string                `json:"oid"`
	Size          int64                 `json:"size"`
	Authenticated bool                  `json:"authenticated,omitempty"`
	Actions       map[string]*lfsAction `json:"actions,omitempty"`
	Error         *lfsObjError          `json:"error,omitempty"`
}

type lfsAction struct {
	Href      string            `json:"href"`
	Header    map[string]string `json:"header,omitempty"`
	ExpiresIn int               `json:"expires_in,omitempty"`
}

type lfsObjError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// lfsBase is the URL the client reached us at, so object links work for
// runners and LAN clients as well as through the public URL.
func (s *Server) lfsBase(r *http.Request) string {
	pub := s.F.Cfg.HTTP.PublicURL
	if u, err := url.Parse(pub); err == nil && strings.EqualFold(u.Host, r.Host) {
		return pub
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p == "https" || p == "http" {
		scheme = p
	}
	return scheme + "://" + r.Host
}

func (s *Server) lfsBatch(w http.ResponseWriter, r *http.Request, repo *store.Repo, canWrite bool) {
	var req struct {
		Operation string      `json:"operation"`
		Transfers []string    `json:"transfers"`
		Objects   []lfsObject `json:"objects"`
		HashAlgo  string      `json:"hash_algo"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 10<<20)).Decode(&req); err != nil {
		lfsError(w, http.StatusUnprocessableEntity, "invalid JSON")
		return
	}
	if req.HashAlgo != "" && req.HashAlgo != "sha256" {
		lfsError(w, http.StatusConflict, "only sha256 is supported")
		return
	}
	if len(req.Transfers) > 0 && !contains(req.Transfers, "basic") {
		lfsError(w, http.StatusUnprocessableEntity, "only the basic transfer adapter is supported")
		return
	}
	switch req.Operation {
	case "download":
	case "upload":
		if !canWrite {
			lfsDenyWrite(w, r)
			return
		}
		if !repo.LFS {
			lfsError(w, http.StatusForbidden, fmt.Sprintf("Git LFS is not enabled for %s. Enable it in the repository settings (TUI: Settings › e) or with: %s repo edit --lfs true %s",
				repo.FullName(), s.F.Cfg.SSHCommand(), repo.FullName()))
			return
		}
	default:
		lfsError(w, http.StatusUnprocessableEntity, "operation must be download or upload")
		return
	}
	header := map[string]string{}
	if h := r.Header.Get("Authorization"); h != "" {
		header["Authorization"] = h
	}
	base := s.lfsBase(r) + "/" + repo.FullName() + ".git/info/lfs/objects/"
	max := s.F.LFSMaxBytes()
	out := make([]lfsObject, 0, len(req.Objects))
	for _, o := range req.Objects {
		res := lfsObject{OID: o.OID, Size: o.Size, Authenticated: true}
		size, have := s.F.LFSHas(r.Context(), repo, o.OID)
		switch {
		case !gitx.ValidLFSOID(o.OID) || o.Size < 0:
			res.Error = &lfsObjError{422, "invalid object id or size"}
		case req.Operation == "download" && !have:
			res.Error = &lfsObjError{404, "object not found on the server"}
		case req.Operation == "download":
			res.Size = size
			res.Actions = map[string]*lfsAction{"download": {Href: base + o.OID, Header: header, ExpiresIn: 3600}}
		case have:
			// already stored for this repository: nothing to upload
		case max > 0 && o.Size > max:
			res.Error = &lfsObjError{422, fmt.Sprintf("object is larger than the server limit of %d MB", s.F.Cfg.LFS.MaxObjectMB)}
		default:
			res.Actions = map[string]*lfsAction{"upload": {Href: base + o.OID, Header: header, ExpiresIn: 3600}}
		}
		out = append(out, res)
	}
	w.Header().Set("Content-Type", lfsMediaType)
	json.NewEncoder(w).Encode(map[string]any{"transfer": "basic", "objects": out, "hash_algo": "sha256"})
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func (s *Server) lfsDownload(w http.ResponseWriter, r *http.Request, repo *store.Repo, oid string) {
	fh, _, err := s.F.OpenLFSObject(r.Context(), repo, oid)
	if err != nil {
		lfsError(w, http.StatusNotFound, "object not found")
		return
	}
	defer fh.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, oid, time.Time{}, fh) // supports Range for resumed downloads
}

func (s *Server) lfsUpload(w http.ResponseWriter, r *http.Request, repo *store.Repo, oid string) {
	err := s.F.PutLFSObject(r.Context(), repo, oid, r.ContentLength, r.Body)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusOK)
	case errors.Is(err, forge.ErrLFSDisabled):
		lfsError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, forge.ErrLFSTooLarge):
		lfsError(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, forge.ErrLFSMismatch):
		lfsError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		s.F.Log.Error().Err(err).Str("repo", repo.FullName()).Msg("lfs upload")
		lfsError(w, http.StatusInternalServerError, "upload failed")
	}
}
