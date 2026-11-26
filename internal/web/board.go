package web

import (
	"net/http"

	"github.com/cooperspencer/forge/internal/store"
)

// ---------------------------------------------------------------- kanban board

type boardColumnDTO struct {
	ID     int64      `json:"id"`
	Name   string     `json:"name"`
	Done   bool       `json:"done"` // the last column: closed issues
	Issues []issueDTO `json:"issues"`
	Hidden int        `json:"hidden_closed,omitempty"`
}

func (s *Server) getBoard(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	b, err := s.F.Board(r.Context(), u, rp)
	if err != nil {
		return err
	}
	out := []boardColumnDTO{}
	for i, l := range b.Lanes {
		c := boardColumnDTO{ID: l.Column.ID, Name: l.Column.Name, Done: b.Done(i), Issues: []issueDTO{}, Hidden: l.Hidden}
		for _, is := range l.Cards {
			c.Issues = append(c.Issues, toIssue(is))
		}
		out = append(out, c)
	}
	writeJSON(w, 200, map[string]any{"columns": out})
	return nil
}

func (s *Server) createBoardColumn(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	var req struct{ Name string }
	if err := decode(r, &req); err != nil {
		return err
	}
	c, err := s.F.AddBoardColumn(r.Context(), u, rp, req.Name)
	if err != nil {
		return err
	}
	writeJSON(w, 201, map[string]any{"id": c.ID, "name": c.Name})
	return nil
}

func (s *Server) patchBoardColumn(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	id, err := pathInt(r, "id")
	if err != nil {
		return err
	}
	var req struct {
		Name     *string `json:"name"`
		Position *int    `json:"position"` // 1-based
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	if req.Name != nil {
		if err := s.F.RenameBoardColumn(r.Context(), u, rp, id, *req.Name); err != nil {
			return err
		}
	}
	if req.Position != nil {
		b, err := s.F.Board(r.Context(), u, rp)
		if err != nil {
			return err
		}
		from, err := b.Lane(id, "")
		if err != nil {
			return store.ErrNotFound
		}
		if err := s.F.MoveBoardColumn(r.Context(), u, rp, id, *req.Position-1-from); err != nil {
			return err
		}
	}
	return s.getBoard(w, r, u)
}

func (s *Server) deleteBoardColumn(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, err := s.repo(r, u, store.AccessRead)
	if err != nil {
		return err
	}
	id, err := pathInt(r, "id")
	if err != nil {
		return err
	}
	if err := s.F.DeleteBoardColumn(r.Context(), u, rp, id); err != nil {
		return err
	}
	w.WriteHeader(204)
	return nil
}

// moveIssue: {"column": "In progress" or "2", "column_id": 7, "index": 0}
func (s *Server) moveIssue(w http.ResponseWriter, r *http.Request, u *store.User) error {
	rp, i, err := s.issue(r, u)
	if err != nil {
		return err
	}
	var req struct {
		Column   string `json:"column"` // name, or 1-based position as a string
		ColumnID int64  `json:"column_id"`
		Index    int    `json:"index"` // 0 = top
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	b, err := s.F.Board(r.Context(), u, rp)
	if err != nil {
		return err
	}
	li, err := b.Lane(req.ColumnID, req.Column)
	if err != nil {
		return err
	}
	if err := s.F.MoveCard(r.Context(), u, rp, i, b.Lanes[li].Column.ID, req.Index); err != nil {
		return err
	}
	return s.getBoard(w, r, u)
}
