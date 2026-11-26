package web

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/store"
)

// Generic packages: arbitrary files addressed by owner/name/version/filename.
//
//	PUT    /api/packages/{owner}/generic/{name}/{version}/{file}   upload (body = content)
//	GET    /api/packages/{owner}/generic/{name}/{version}/{file}   download
//	DELETE /api/packages/{owner}/generic/{name}/{version}/{file}   delete a file
//	DELETE /api/packages/{owner}/generic/{name}/{version}          delete a version
//
// Files are immutable: uploading an existing file is a 409; delete it first.

var (
	pkgNameRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	pkgVersionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,99}$`)
	pkgFileRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,199}$`)
)

func (s *Server) packageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v2/", s.oci)
	g := "/api/packages/{owner}/generic/{name}/{version}"
	mux.HandleFunc("PUT "+g+"/{file}", s.genericHandler(s.genericPut))
	mux.HandleFunc("GET "+g+"/{file}", s.genericHandler(s.genericGet))
	mux.HandleFunc("DELETE "+g+"/{file}", s.genericHandler(s.genericDelete))
	mux.HandleFunc("DELETE "+g, s.genericHandler(s.genericDelete))

	h := func(pattern, scopes string, fn apiFunc) { mux.HandleFunc(pattern, s.api(scopes, fn)) }
	h("GET /api/v1/packages", "packages:read", s.listPackages)
	h("GET /api/v1/packages/{owner}", "packages:read", s.listPackages)
	h("GET /api/v1/packages/{owner}/{type}/{name...}", "packages:read", s.getPackage)
}

type genericReq struct {
	actor forge.PkgActor
	owner *store.User
	name  string
	ver   string
	file  string
}

type genericFunc func(w http.ResponseWriter, r *http.Request, q *genericReq) error

func (s *Server) genericHandler(fn genericFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, err := s.pkgActor(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Basic realm="forge"`)
			writeErr(w, fmt.Errorf("%w: %v", errUnauthorized, err))
			return
		}
		q := &genericReq{actor: a, name: r.PathValue("name"), ver: r.PathValue("version"), file: r.PathValue("file")}
		if !pkgNameRe.MatchString(q.name) || !pkgVersionRe.MatchString(q.ver) || (q.file != "" && !pkgFileRe.MatchString(q.file)) {
			writeErr(w, errors.New("invalid package name, version or file name (letters, digits, . _ - +)"))
			return
		}
		need := store.AccessRead
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			need = store.AccessWrite
		}
		lvl := store.AccessNone
		if q.owner, err = s.F.Store.UserByName(r.Context(), r.PathValue("owner")); err == nil {
			lvl = s.F.PackageAccess(r.Context(), a, q.owner, q.name)
		}
		if lvl < need {
			switch {
			case anonymous(a):
				w.Header().Set("WWW-Authenticate", `Basic realm="forge"`)
				writeErr(w, errUnauthorized)
			case lvl >= store.AccessRead || need == store.AccessWrite:
				writeErr(w, forge.ErrForbidden)
			default:
				writeErr(w, store.ErrNotFound)
			}
			return
		}
		if err := fn(w, r, q); err != nil {
			if errors.Is(err, store.ErrExists) {
				writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
				return
			}
			writeErr(w, err)
		}
	}
}

func (s *Server) genericPut(w http.ResponseWriter, r *http.Request, q *genericReq) error {
	ctx := r.Context()
	p, err := s.F.Store.EnsurePackage(ctx, q.owner.ID, store.PkgGeneric, q.name)
	if err != nil {
		return err
	}
	if _, err := s.F.Store.File(ctx, p.ID, q.ver, q.file); err == nil {
		return fmt.Errorf("%w: %s %s/%s; delete it first", store.ErrExists, p.FullName(), q.ver, q.file)
	}
	unlock := s.F.LockPackages()
	defer unlock()
	digest, size, err := s.F.IngestBlob(ctx, r.Body, "")
	if err != nil {
		return err
	}
	if err := s.F.Store.AddFile(ctx, &store.PkgFile{PackageID: p.ID, Version: q.ver, Filename: q.file, Digest: digest, Size: size, Creator: q.actor.Name()}); err != nil {
		if errors.Is(err, store.ErrExists) {
			return fmt.Errorf("%w: %s %s/%s; delete it first", store.ErrExists, p.FullName(), q.ver, q.file)
		}
		return err
	}
	s.F.PackageEvent(ctx, p, q.actor, fmt.Sprintf("published %s %s/%s", p.FullName(), q.ver, q.file))
	writeJSON(w, http.StatusCreated, map[string]any{
		"package": p.FullName(), "version": q.ver, "file": q.file, "size": size,
		"sha256": strings.TrimPrefix(digest, "sha256:"),
		"url":    s.F.Cfg.HTTP.PublicURL + r.URL.Path,
	})
	return nil
}

func (s *Server) genericGet(w http.ResponseWriter, r *http.Request, q *genericReq) error {
	ctx := r.Context()
	p, err := s.F.Store.PackageByName(ctx, q.owner.ID, store.PkgGeneric, q.name)
	if err != nil {
		return err
	}
	f, err := s.F.Store.File(ctx, p.ID, q.ver, q.file)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", f.Filename))
	w.Header().Set("X-Checksum-Sha256", strings.TrimPrefix(f.Digest, "sha256:"))
	s.serveBlob(w, r, f.Digest, "application/octet-stream")
	return nil
}

func (s *Server) genericDelete(w http.ResponseWriter, r *http.Request, q *genericReq) error {
	ctx := r.Context()
	p, err := s.F.Store.PackageByName(ctx, q.owner.ID, store.PkgGeneric, q.name)
	if err != nil {
		return err
	}
	if err := s.F.DeletePackageVersion(ctx, q.actor, q.owner, p, q.ver, q.file); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ---------------------------------------------------------------- JSON API

type packageJSON struct {
	Owner     string    `json:"owner"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	Repo      string    `json:"repository,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *Server) toPackageJSON(r *http.Request, p *store.Package) packageJSON {
	out := packageJSON{Owner: p.Owner, Name: p.Name, Type: p.Type, UpdatedAt: p.UpdatedAt}
	if o, err := s.F.Store.UserByID(r.Context(), p.OwnerID); err == nil {
		if repo := s.F.LinkedRepo(r.Context(), o, p.Name); repo != nil {
			out.Repo = repo.FullName()
		}
	}
	return out
}

func (s *Server) listPackages(w http.ResponseWriter, r *http.Request, u *store.User) error {
	var owner *store.User
	if n := r.PathValue("owner"); n != "" {
		var err error
		if owner, err = s.F.Store.UserByName(r.Context(), n); err != nil {
			return err
		}
	}
	pkgs, err := s.F.VisiblePackages(r.Context(), forge.PkgActor{User: u}, owner)
	if err != nil {
		return err
	}
	out := []packageJSON{}
	for _, p := range pkgs {
		out = append(out, s.toPackageJSON(r, p))
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) getPackage(w http.ResponseWriter, r *http.Request, u *store.User) error {
	ctx := r.Context()
	owner, err := s.F.Store.UserByName(ctx, r.PathValue("owner"))
	if err != nil {
		return err
	}
	name := r.PathValue("name")
	if s.F.PackageAccess(ctx, forge.PkgActor{User: u}, owner, name) < store.AccessRead {
		return store.ErrNotFound
	}
	p, err := s.F.Store.PackageByName(ctx, owner.ID, r.PathValue("type"), name)
	if err != nil {
		return err
	}
	out := map[string]any{"package": s.toPackageJSON(r, p)}
	switch p.Type {
	case store.PkgContainer:
		tags, _ := s.F.Store.Tags(ctx, p.ID)
		ts := []map[string]any{}
		for _, t := range tags {
			ts = append(ts, map[string]any{"tag": t.Name, "digest": t.Digest, "updated_at": t.UpdatedAt,
				"size": s.F.ImageSize(ctx, p, t.Digest)})
		}
		out["tags"] = ts
		out["pull"] = "docker pull " + s.F.ImageRef(p) + ":<tag>"
	default:
		files, _ := s.F.Store.Files(ctx, p.ID)
		fs := []map[string]any{}
		for _, f := range files {
			fs = append(fs, map[string]any{"version": f.Version, "file": f.Filename, "size": f.Size,
				"sha256": strings.TrimPrefix(f.Digest, "sha256:"), "created_at": f.CreatedAt, "by": f.Creator,
				"url": s.F.GenericURL(p, f.Version, f.Filename)})
		}
		out["files"] = fs
	}
	writeJSON(w, 200, out)
	return nil
}
