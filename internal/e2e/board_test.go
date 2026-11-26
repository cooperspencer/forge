package e2e

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/store"
)

// TestBoard: the kanban board places issues, closes and reopens them through
// the done column, keeps an order, and checks permissions.
func TestBoard(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f, admin := e.f, e.admin
	r, err := f.CreateRepo(ctx, admin, "admin", "kanban", "", false)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(u *store.User, title string) *store.Issue {
		t.Helper()
		i, err := f.CreateIssue(ctx, u, r, title, "")
		if err != nil {
			t.Fatal(err)
		}
		return i
	}
	// lanes renders the board as "To do: 3 1 | In progress: 2 | Done: 4"
	lanes := func() string {
		t.Helper()
		b, err := f.Board(ctx, admin, r)
		if err != nil {
			t.Fatal(err)
		}
		var parts []string
		for _, l := range b.Lanes {
			s := l.Column.Name + ":"
			for _, c := range l.Cards {
				s += fmt.Sprintf(" %d", c.Number)
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, " | ")
	}
	check := func(want string) {
		t.Helper()
		if got := lanes(); got != want {
			t.Errorf("board\n got: %s\nwant: %s", got, want)
		}
	}
	col := func(name string) int64 {
		t.Helper()
		b, _ := f.Board(ctx, admin, r)
		li, err := b.Lane(0, name)
		if err != nil {
			t.Fatal(err)
		}
		return b.Lanes[li].Column.ID
	}
	get := func(n int64) *store.Issue {
		i, err := f.Store.IssueByNumber(ctx, r.ID, n)
		if err != nil {
			t.Fatal(err)
		}
		return i
	}
	move := func(n int64, column string, index int) {
		t.Helper()
		if err := f.MoveCard(ctx, admin, r, get(n), col(column), index); err != nil {
			t.Fatalf("move #%d to %s: %v", n, column, err)
		}
	}

	check("To do: | In progress: | Done:")
	issue(admin, "one")
	issue(admin, "two")
	issue(admin, "three")
	check("To do: 3 2 1 | In progress: | Done:") // newest on top

	move(1, "In progress", 0)
	move(3, "In progress", 5) // clamped to the bottom
	check("To do: 2 | In progress: 1 3 | Done:")
	move(3, "In progress", 0) // reorder
	check("To do: 2 | In progress: 3 1 | Done:")
	issue(admin, "four")
	check("To do: 4 2 | In progress: 3 1 | Done:")

	// the last column closes, leaving it reopens
	move(3, "Done", 0)
	if get(3).State != "closed" {
		t.Errorf("#3 should be closed")
	}
	check("To do: 4 2 | In progress: 1 | Done: 3")
	move(3, "To do", 1)
	if get(3).State != "open" {
		t.Errorf("#3 should be open again")
	}
	check("To do: 4 3 2 | In progress: 1 | Done:")

	// closing and reopening the issue itself: it comes back to its column
	if err := f.SetIssueState(ctx, admin, r, get(1), "closed"); err != nil {
		t.Fatal(err)
	}
	check("To do: 4 3 2 | In progress: | Done: 1")
	f.SetIssueState(ctx, admin, r, get(1), "open")
	check("To do: 4 3 2 | In progress: 1 | Done:")

	// pull requests are not cards
	pr, err := f.Store.CreatePullRequest(ctx, r.ID, admin.ID, "a pr", "", store.PullRequest{HeadRepoID: r.ID, HeadBranch: "x", BaseBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.MoveCard(ctx, admin, r, pr, col("Done"), 0); err == nil {
		t.Error("moved a pull request")
	}
	check("To do: 4 3 2 | In progress: 1 | Done:")

	// columns
	if _, err := f.AddBoardColumn(ctx, admin, r, "Review"); err != nil {
		t.Fatal(err)
	}
	check("To do: 4 3 2 | In progress: 1 | Review: | Done:")
	if _, err := f.AddBoardColumn(ctx, admin, r, "review"); err == nil {
		t.Error("duplicate column name accepted")
	}
	move(2, "Review", 0)
	if err := f.RenameBoardColumn(ctx, admin, r, col("Review"), "In review"); err != nil {
		t.Fatal(err)
	}
	if err := f.MoveBoardColumn(ctx, admin, r, col("In review"), -1); err != nil {
		t.Fatal(err)
	}
	check("To do: 4 3 | In review: 2 | In progress: 1 | Done:")
	if err := f.DeleteBoardColumn(ctx, admin, r, col("In review")); err != nil {
		t.Fatal(err)
	}
	check("To do: 2 4 3 | In progress: 1 | Done:") // returned cards go on top
	f.DeleteBoardColumn(ctx, admin, r, col("In progress"))
	if err := f.DeleteBoardColumn(ctx, admin, r, col("Done")); err == nil {
		t.Error("deleted down to one column")
	}
	check("To do: 2 1 4 3 | Done:")
	f.AddBoardColumn(ctx, admin, r, "Doing")

	// permissions: a reader can move their own issues only, and can't
	// change columns
	bob, _ := f.Store.CreateUser(ctx, "bob", false, false)
	mine := issue(bob, "bob's")
	if err := f.MoveCard(ctx, bob, r, mine, col("Doing"), 0); err != nil {
		t.Errorf("author move: %v", err)
	}
	if err := f.MoveCard(ctx, bob, r, get(1), col("Doing"), 0); !errors.Is(err, forge.ErrForbidden) {
		t.Errorf("reader moved someone else's issue: %v", err)
	}
	if _, err := f.AddBoardColumn(ctx, bob, r, "Bob"); !errors.Is(err, forge.ErrForbidden) {
		t.Errorf("reader added a column: %v", err)
	}
	if err := f.MoveCard(ctx, nil, r, mine, col("Done"), 0); !errors.Is(err, forge.ErrForbidden) {
		t.Errorf("anonymous move: %v", err)
	}
	check(fmt.Sprintf("To do: 2 1 4 3 | Doing: %d | Done:", mine.Number))

	// HTTP API
	var board struct {
		Columns []struct {
			Name   string
			Done   bool
			Issues []struct{ Number int64 }
		}
	}
	if code := e.api("POST", "/api/v1/repos/admin/kanban/issues/4/move", map[string]any{"column": "Done"}, &board); code != 200 {
		t.Fatalf("move via API: %d", code)
	}
	if last := board.Columns[len(board.Columns)-1]; !last.Done || len(last.Issues) != 1 || last.Issues[0].Number != 4 {
		t.Errorf("API board: %+v", board)
	}
	if code := e.api("POST", "/api/v1/repos/admin/kanban/issues/4/move", map[string]any{"column": "2"}, &board); code != 200 || board.Columns[1].Issues[0].Number != 4 {
		t.Errorf("move by position: %d %+v", code, board)
	}
	if code := e.api("POST", "/api/v1/repos/admin/kanban/issues/4/move", map[string]any{"column": "Nope"}, nil); code < 400 {
		t.Errorf("unknown column: %d", code)
	}
}
