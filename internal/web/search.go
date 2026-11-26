package web

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/store"
)

type codeMatchDTO struct {
	Repo string `json:"repo"`
	Ref  string `json:"ref"`
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// searchCode: GET /api/v1/search/code?q=&repo=owner/name&limit=
func (s *Server) searchCode(w http.ResponseWriter, r *http.Request, u *store.User) error {
	q, err := forge.ParseCodeQuery(r.URL.Query().Get("q"))
	if err != nil {
		return err
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		q.Limit = min(n, 1000)
	}
	var scope *store.Repo
	if full := r.URL.Query().Get("repo"); full != "" {
		rp, _, err := s.F.ResolveRepo(r.Context(), u, full, store.AccessRead)
		if err != nil {
			return err
		}
		scope = rp
	}
	res, err := s.F.SearchCode(r.Context(), u, scope, q)
	if err != nil {
		return err
	}
	out := []codeMatchDTO{}
	for _, m := range res.Matches {
		out = append(out, codeMatchDTO{Repo: m.Repo.FullName(), Ref: m.Ref, Path: m.Path, Line: m.Line, Text: strings.TrimRight(m.Text, "\r")})
	}
	writeJSON(w, 200, map[string]any{"matches": out, "repositories": res.Repos, "repositories_matched": res.Hits,
		"truncated": res.Truncated, "took_ms": res.Took.Milliseconds()})
	return nil
}
