package store

import (
	"context"
	"fmt"
	"strings"
)

// BoardColumn is one column of a repository's kanban board.
type BoardColumn struct {
	ID       int64
	RepoID   int64
	Name     string
	Position int
}

// DefaultBoardColumns are created the first time a repository's board is used.
var DefaultBoardColumns = []string{"To do", "In progress", "Done"}

// BoardColumns returns the repository's columns in order, creating the
// default ones if it has none yet.
func (s *Store) BoardColumns(ctx context.Context, repoID int64) ([]BoardColumn, error) {
	cols, err := s.boardColumns(ctx, repoID)
	if err != nil || len(cols) > 0 {
		return cols, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for i, name := range DefaultBoardColumns {
		// OR IGNORE: a concurrent first use may have created them already
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO board_columns(repo_id,name,position) VALUES (?,?,?)`, repoID, name, i); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.boardColumns(ctx, repoID)
}

func (s *Store) boardColumns(ctx context.Context, repoID int64) ([]BoardColumn, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, repo_id, name, position FROM board_columns WHERE repo_id=? ORDER BY position, id`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BoardColumn
	for rows.Next() {
		var c BoardColumn
		if err := rows.Scan(&c.ID, &c.RepoID, &c.Name, &c.Position); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func validColumnName(name string) (string, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return "", fmt.Errorf("column name is required")
	case len(name) > 40:
		return "", fmt.Errorf("column name is too long (max 40)")
	case strings.ContainsAny(name, "\n\r\t"):
		return "", fmt.Errorf("column name must be one line")
	}
	return name, nil
}

func dupColumn(err error, name string) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return fmt.Errorf("a column named %q already exists", name)
	}
	return err
}

// AddBoardColumn appends a column at the end, or before the last column when
// beforeLast is set (so a new column doesn't take over the "done" role).
func (s *Store) AddBoardColumn(ctx context.Context, repoID int64, name string, beforeLast bool) (*BoardColumn, error) {
	name, err := validColumnName(name)
	if err != nil {
		return nil, err
	}
	cols, err := s.BoardColumns(ctx, repoID)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	pos := len(cols)
	if beforeLast && len(cols) > 0 {
		pos = len(cols) - 1
	}
	// renumber so positions stay 0..n-1 with a gap at pos
	for i, c := range cols {
		p := i
		if i >= pos {
			p = i + 1
		}
		if _, err := tx.ExecContext(ctx, `UPDATE board_columns SET position=? WHERE id=?`, p, c.ID); err != nil {
			return nil, err
		}
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO board_columns(repo_id,name,position) VALUES (?,?,?)`, repoID, name, pos)
	if err != nil {
		return nil, dupColumn(err, name)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &BoardColumn{ID: id, RepoID: repoID, Name: name, Position: pos}, nil
}

func (s *Store) RenameBoardColumn(ctx context.Context, repoID, id int64, name string) error {
	name, err := validColumnName(name)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE board_columns SET name=? WHERE id=? AND repo_id=?`, name, id, repoID)
	if err != nil {
		return dupColumn(err, name)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteBoardColumn removes a column; its cards go back to the first column.
func (s *Store) DeleteBoardColumn(ctx context.Context, repoID, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM board_columns WHERE id=? AND repo_id=?`, id, repoID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE issues SET board_column=0, board_pos=0 WHERE repo_id=? AND board_column=?`, repoID, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ReorderBoardColumns stores a new column order (ids, first to last).
func (s *Store) ReorderBoardColumns(ctx context.Context, repoID int64, ids []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE board_columns SET position=? WHERE id=? AND repo_id=?`, i, id, repoID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PlaceCards puts the given issues into column colID in this order. It does
// not touch updated_at: moving a card is not an edit of the issue.
func (s *Store) PlaceCards(ctx context.Context, repoID, colID int64, issueIDs []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, id := range issueIDs {
		if _, err := tx.ExecContext(ctx, `UPDATE issues SET board_column=?, board_pos=? WHERE id=? AND repo_id=?`, colID, i+1, id, repoID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
