package forge

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/cooperspencer/forge/internal/store"
)

// Kanban board: every repository has ordered columns (default "To do",
// "In progress", "Done") holding its issues as cards.
//
//   - Open issues sit in the column they were moved to; new issues and issues
//     whose column was deleted land at the top of the first column.
//   - The last column is the "done" column: it shows the closed issues, most
//     recently closed first. Moving a card there closes the issue, moving a
//     card out of it reopens the issue.

// BoardDoneLimit caps the closed issues shown in the last column.
const BoardDoneLimit = 30

type BoardLane struct {
	Column store.BoardColumn
	Cards  []*store.Issue
	Hidden int // closed issues not shown (last column only)
}

type Board struct {
	Lanes []BoardLane
}

// Done reports whether lane i is the done column.
func (b *Board) Done(i int) bool { return i == len(b.Lanes)-1 }

// Find locates an issue's card: lane and index, or -1, -1.
func (b *Board) Find(number int64) (int, int) {
	for li, l := range b.Lanes {
		for ci, c := range l.Cards {
			if c.Number == number {
				return li, ci
			}
		}
	}
	return -1, -1
}

// Lane finds a column by id, or by name (case-insensitive) or 1-based
// position when id is 0.
func (b *Board) Lane(id int64, name string) (int, error) {
	for i, l := range b.Lanes {
		if id != 0 && l.Column.ID == id {
			return i, nil
		}
		if id == 0 && strings.EqualFold(l.Column.Name, strings.TrimSpace(name)) {
			return i, nil
		}
	}
	if id == 0 {
		var n int
		if _, err := fmt.Sscanf(name, "%d", &n); err == nil && fmt.Sprint(n) == strings.TrimSpace(name) && n >= 1 && n <= len(b.Lanes) {
			return n - 1, nil
		}
		return -1, fmt.Errorf("no column %q", name)
	}
	return -1, fmt.Errorf("no column %d", id)
}

// Board loads the repository's board.
func (f *Forge) Board(ctx context.Context, actor *store.User, r *store.Repo) (*Board, error) {
	if f.Store.AccessLevel(ctx, actor, r) < store.AccessRead {
		return nil, ErrNotFound
	}
	cols, err := f.Store.BoardColumns(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	open, err := f.Store.ListIssues(ctx, r.ID, store.IssueFilter{State: "open"})
	if err != nil {
		return nil, err
	}
	closed, err := f.Store.ListIssues(ctx, r.ID, store.IssueFilter{State: "closed"})
	if err != nil {
		return nil, err
	}
	return layoutBoard(cols, open, closed), nil
}

func layoutBoard(cols []store.BoardColumn, open, closed []*store.Issue) *Board {
	b := &Board{}
	idx := map[int64]int{}
	for i, c := range cols {
		b.Lanes = append(b.Lanes, BoardLane{Column: c})
		idx[c.ID] = i
	}
	if len(b.Lanes) == 0 {
		return b
	}
	last := len(b.Lanes) - 1
	for _, i := range open {
		li, ok := idx[i.BoardColumn]
		if !ok || (li == last && last > 0) {
			li = 0 // never placed, column deleted, or reopened while in done
		}
		b.Lanes[li].Cards = append(b.Lanes[li].Cards, i)
	}
	for li := range b.Lanes {
		cs := b.Lanes[li].Cards
		// unplaced cards (pos 0) first, newest on top; then by position
		sort.SliceStable(cs, func(x, y int) bool {
			if cs[x].BoardPos != cs[y].BoardPos {
				return cs[x].BoardPos < cs[y].BoardPos
			}
			return cs[x].Number > cs[y].Number
		})
	}
	done := append([]*store.Issue(nil), closed...)
	sort.SliceStable(done, func(x, y int) bool { return done[x].UpdatedAt.After(done[y].UpdatedAt) })
	if len(done) > BoardDoneLimit {
		b.Lanes[last].Hidden = len(done) - BoardDoneLimit
		done = done[:BoardDoneLimit]
	}
	b.Lanes[last].Cards = append(b.Lanes[last].Cards, done...)
	return b
}

// canTriage: issue authors may move their own issues, others need write.
func (f *Forge) canTriage(ctx context.Context, actor *store.User, r *store.Repo, i *store.Issue) bool {
	return actor != nil && (actor.ID == i.AuthorID || f.Store.AccessLevel(ctx, actor, r) >= store.AccessWrite)
}

// MoveCard moves issue i to column colID at index (clamped; 0 = top).
// Moving into the done column closes the issue, out of it reopens it.
func (f *Forge) MoveCard(ctx context.Context, actor *store.User, r *store.Repo, i *store.Issue, colID int64, index int) error {
	if i.IsPR {
		return fmt.Errorf("pull requests are not on the board")
	}
	if !f.canTriage(ctx, actor, r, i) {
		return ErrForbidden
	}
	b, err := f.Board(ctx, actor, r)
	if err != nil {
		return err
	}
	li, err := b.Lane(colID, "")
	if err != nil {
		return err
	}
	defer f.Broker.Publish(fmt.Sprintf("issues:%d", r.ID))
	if b.Done(li) {
		if i.State == "open" {
			return f.SetIssueState(ctx, actor, r, i, "closed")
		}
		return nil // the done column is ordered by close time
	}
	if i.State != "open" {
		if err := f.SetIssueState(ctx, actor, r, i, "open"); err != nil {
			return err
		}
	}
	var ids []int64
	for _, c := range b.Lanes[li].Cards {
		if c.ID != i.ID {
			ids = append(ids, c.ID)
		}
	}
	if index < 0 {
		index = 0
	}
	if index > len(ids) {
		index = len(ids)
	}
	ids = append(ids[:index], append([]int64{i.ID}, ids[index:]...)...)
	return f.Store.PlaceCards(ctx, r.ID, b.Lanes[li].Column.ID, ids)
}

func (f *Forge) canEditBoard(ctx context.Context, actor *store.User, r *store.Repo) error {
	if actor == nil || f.Store.AccessLevel(ctx, actor, r) < store.AccessWrite {
		return ErrForbidden
	}
	return nil
}

// AddBoardColumn adds a column just before the done column.
func (f *Forge) AddBoardColumn(ctx context.Context, actor *store.User, r *store.Repo, name string) (*store.BoardColumn, error) {
	if err := f.canEditBoard(ctx, actor, r); err != nil {
		return nil, err
	}
	c, err := f.Store.AddBoardColumn(ctx, r.ID, name, true)
	if err == nil {
		f.Broker.Publish(fmt.Sprintf("issues:%d", r.ID))
	}
	return c, err
}

func (f *Forge) RenameBoardColumn(ctx context.Context, actor *store.User, r *store.Repo, id int64, name string) error {
	if err := f.canEditBoard(ctx, actor, r); err != nil {
		return err
	}
	err := f.Store.RenameBoardColumn(ctx, r.ID, id, name)
	if err == nil {
		f.Broker.Publish(fmt.Sprintf("issues:%d", r.ID))
	}
	return err
}

// DeleteBoardColumn removes a column; its open cards return to the first
// column. A board keeps at least two columns (work and done).
func (f *Forge) DeleteBoardColumn(ctx context.Context, actor *store.User, r *store.Repo, id int64) error {
	if err := f.canEditBoard(ctx, actor, r); err != nil {
		return err
	}
	cols, err := f.Store.BoardColumns(ctx, r.ID)
	if err != nil {
		return err
	}
	if len(cols) <= 2 {
		return fmt.Errorf("a board needs at least two columns")
	}
	if err := f.Store.DeleteBoardColumn(ctx, r.ID, id); err != nil {
		return err
	}
	f.Broker.Publish(fmt.Sprintf("issues:%d", r.ID))
	return nil
}

// MoveBoardColumn shifts a column by delta places (-1 = left).
func (f *Forge) MoveBoardColumn(ctx context.Context, actor *store.User, r *store.Repo, id int64, delta int) error {
	if err := f.canEditBoard(ctx, actor, r); err != nil {
		return err
	}
	cols, err := f.Store.BoardColumns(ctx, r.ID)
	if err != nil {
		return err
	}
	from := -1
	for i, c := range cols {
		if c.ID == id {
			from = i
		}
	}
	if from < 0 {
		return ErrNotFound
	}
	to := from + delta
	if to < 0 || to >= len(cols) || to == from {
		return nil
	}
	c := cols[from]
	cols = append(cols[:from], cols[from+1:]...)
	cols = append(cols[:to], append([]store.BoardColumn{c}, cols[to:]...)...)
	ids := make([]int64, len(cols))
	for i, c := range cols {
		ids[i] = c.ID
	}
	if err := f.Store.ReorderBoardColumns(ctx, r.ID, ids); err != nil {
		return err
	}
	f.Broker.Publish(fmt.Sprintf("issues:%d", r.ID))
	return nil
}
