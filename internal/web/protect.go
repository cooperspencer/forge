package web

import (
	"net/http"

	"github.com/cooperspencer/forge/internal/protect"
	"github.com/cooperspencer/forge/internal/store"
)

func (s *Server) listProtections(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	rules, err := s.F.Protections(r.Context(), u, rp)
	if err != nil {
		return err
	}
	if rules == nil {
		rules = []protect.Rule{}
	}
	writeJSON(w, 200, rules)
	return nil
}

// putProtection creates or replaces the rule for {pattern} (repo admins).
func (s *Server) putProtection(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	rule := protect.Rule{BlockOnChanges: true}
	if err := decode(r, &rule); err != nil {
		return err
	}
	rule.Pattern = r.PathValue("pattern")
	if err := s.F.SetProtection(r.Context(), u, rp, rule); err != nil {
		return err
	}
	rule.Validate()
	writeJSON(w, 200, rule)
	return nil
}

func (s *Server) deleteProtection(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	if err := s.F.RemoveProtection(r.Context(), u, rp, r.PathValue("pattern")); err != nil {
		return err
	}
	w.WriteHeader(204)
	return nil
}
