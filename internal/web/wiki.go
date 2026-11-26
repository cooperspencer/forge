package web

import (
	"net/http"
	"time"

	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/store"
)

// ---------------------------------------------------------------- wiki

type wikiPageDTO struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Author  string    `json:"author"`
	Updated time.Time `json:"updated_at"`
	SHA     string    `json:"sha"`
	Content *string   `json:"content,omitempty"`
	Head    string    `json:"wiki_head,omitempty"` // pass back as "expect" to detect concurrent edits
}

func toWikiPage(p *forge.WikiPage) wikiPageDTO {
	return wikiPageDTO{Name: p.Name, Path: p.Path, Author: p.Author, Updated: p.Updated, SHA: p.SHA}
}

func (s *Server) listWiki(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	pages, err := s.F.WikiPages(r.Context(), u, rp)
	if err != nil {
		return err
	}
	out := []wikiPageDTO{}
	for i := range pages {
		out = append(out, toWikiPage(&pages[i]))
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) getWikiPage(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	p, content, err := s.F.WikiPageContent(r.Context(), u, rp, r.PathValue("page"), r.URL.Query().Get("rev"))
	if err != nil {
		return err
	}
	d := toWikiPage(p)
	d.Content, d.Head = &content, s.F.WikiHead(r.Context(), rp)
	writeJSON(w, 200, d)
	return nil
}

// putWikiPage creates or replaces a page: {content, message?, name? (rename), expect?}.
func (s *Server) putWikiPage(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	var req struct {
		Content string `json:"content"`
		Message string `json:"message"`
		Name    string `json:"name"`
		Expect  string `json:"expect"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	page := r.PathValue("page")
	e := forge.WikiEdit{Name: page, Content: req.Content, Message: req.Message, Expect: req.Expect}
	code := 201
	if _, _, err := s.F.WikiPageContent(r.Context(), u, rp, page, ""); err == nil {
		e.OldName, code = page, 200
		if req.Name != "" {
			e.Name = req.Name
		}
	}
	file, err := s.F.SaveWikiPage(r.Context(), u, rp, e)
	if err != nil {
		return err
	}
	p, content, err := s.F.WikiPageContent(r.Context(), u, rp, forge.WikiName(file), "")
	if err != nil {
		return err
	}
	d := toWikiPage(p)
	d.Content, d.Head = &content, s.F.WikiHead(r.Context(), rp)
	writeJSON(w, code, d)
	return nil
}

func (s *Server) deleteWikiPage(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	if err := s.F.DeleteWikiPage(r.Context(), u, rp, r.PathValue("page")); err != nil {
		return err
	}
	w.WriteHeader(204)
	return nil
}
