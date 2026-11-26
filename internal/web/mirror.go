package web

import (
	"net/http"
	"time"

	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/store"
)

type mirrorDTO struct {
	ID        int64      `json:"id"`
	Direction string     `json:"direction"`
	URL       string     `json:"url"`
	Username  string     `json:"username,omitempty"`
	Interval  string     `json:"interval"`
	RunCI     bool       `json:"run_ci,omitempty"`
	DeployKey string     `json:"deploy_key,omitempty"` // add to the remote (ssh remotes)
	LastSync  *time.Time `json:"last_sync,omitempty"`
	LastError string     `json:"last_error,omitempty"`
	NextSync  *time.Time `json:"next_sync,omitempty"`
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func toMirror(m *store.Mirror) mirrorDTO {
	return mirrorDTO{ID: m.ID, Direction: m.Direction, URL: m.URL, Username: m.Username, Interval: forge.FormatInterval(m.Interval),
		RunCI: m.RunCI, DeployKey: m.SSHPub, LastSync: timePtr(m.LastSync), LastError: m.LastError, NextSync: timePtr(m.NextSync)}
}

type mirrorReq struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
	Interval string `json:"interval"`
	RunCI    bool   `json:"run_ci"`
}

func (q mirrorReq) options() (forge.MirrorOptions, error) {
	iv, err := forge.ParseInterval(q.Interval)
	return forge.MirrorOptions{URL: q.URL, Username: q.Username, Password: q.Password, Interval: iv, RunCI: q.RunCI}, err
}

func (s *Server) listMirrors(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	ms, err := s.F.Mirrors(r.Context(), u, rp)
	if err != nil {
		return err
	}
	out := []mirrorDTO{}
	for _, m := range ms {
		out = append(out, toMirror(m))
	}
	writeJSON(w, 200, out)
	return nil
}

// createMirror adds a push mirror.
func (s *Server) createMirror(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	var req mirrorReq
	if err := decode(r, &req); err != nil {
		return err
	}
	o, err := req.options()
	if err != nil {
		return err
	}
	m, err := s.F.AddPushMirror(r.Context(), u, rp, o)
	if err != nil {
		return err
	}
	writeJSON(w, 201, toMirror(m))
	return nil
}

func (s *Server) patchMirror(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	id, err := pathInt(r, "id")
	if err != nil {
		return err
	}
	cur, err := s.F.Mirrors(r.Context(), u, rp)
	if err != nil {
		return err
	}
	var m *store.Mirror
	for _, x := range cur {
		if x.ID == id {
			m = x
		}
	}
	if m == nil {
		return store.ErrNotFound
	}
	req := mirrorReq{URL: m.URL, Username: m.Username, Interval: forge.FormatInterval(m.Interval), RunCI: m.RunCI}
	if err := decode(r, &req); err != nil {
		return err
	}
	o, err := req.options()
	if err != nil {
		return err
	}
	if err := s.F.UpdateMirror(r.Context(), u, rp, id, o); err != nil {
		return err
	}
	m, _ = s.F.Store.MirrorByID(r.Context(), id)
	writeJSON(w, 200, toMirror(m))
	return nil
}

func (s *Server) deleteMirror(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	id, err := pathInt(r, "id")
	if err != nil {
		return err
	}
	if err := s.F.RemoveMirror(r.Context(), u, rp, id); err != nil {
		return err
	}
	w.WriteHeader(204)
	return nil
}

func (s *Server) syncMirror(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	id, err := pathInt(r, "id")
	if err != nil {
		return err
	}
	if err := s.F.SyncMirrorNow(r.Context(), u, rp, id); err != nil {
		return err
	}
	w.WriteHeader(202)
	return nil
}
