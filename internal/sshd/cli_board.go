package sshd

import (
	"fmt"
	"strings"

	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/store"
)

type boardCmds struct {
	View   boardViewCmd   `cmd:"" help:"Show the kanban board."`
	Add    boardAddCmd    `cmd:"" help:"Add a column (before the last one)."`
	Rename boardRenameCmd `cmd:"" help:"Rename a column."`
	Rm     boardRmCmd     `cmd:"" help:"Delete a column; its cards go to the first column."`
}

func (c *CLI) board(full string) (*store.Repo, *forge.Board, error) {
	r, err := c.repo(full, store.AccessRead)
	if err != nil {
		return nil, nil, err
	}
	b, err := c.F.Board(c.Ctx, c.User, r)
	return r, b, err
}

type issueMoveCmd struct {
	Repo   string   `arg:"" help:"owner/name"`
	Number string   `arg:""`
	Column []string `arg:"" help:"Column name or 1-based position."`
}

func (x *issueMoveCmd) Run(c *CLI) error {
	r, i, err := c.issue(x.Repo, x.Number, store.AccessRead)
	if err != nil {
		return err
	}
	_, b, err := c.board(x.Repo)
	if err != nil {
		return err
	}
	li, err := b.Lane(0, strings.Join(x.Column, " "))
	if err != nil {
		return err
	}
	col := b.Lanes[li].Column
	if err := c.F.MoveCard(c.Ctx, c.User, r, i, col.ID, 0); err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "#%d → %s\n", i.Number, col.Name)
	return nil
}

type boardViewCmd struct {
	Repo string `arg:"" help:"owner/name"`
}

func (x *boardViewCmd) Run(c *CLI) error {
	_, b, err := c.board(x.Repo)
	if err != nil {
		return err
	}
	for li, l := range b.Lanes {
		if li > 0 {
			fmt.Fprintln(c.Out)
		}
		suffix := ""
		if b.Done(li) {
			suffix = " (closed)"
		}
		fmt.Fprintf(c.Out, "%d. %s%s — %d\n", li+1, l.Column.Name, suffix, len(l.Cards)+l.Hidden)
		for _, i := range l.Cards {
			lbl := ""
			if len(i.Labels) > 0 {
				lbl = "  [" + strings.Join(i.Labels, ", ") + "]"
			}
			fmt.Fprintf(c.Out, "   #%-4d %s%s\n", i.Number, i.Title, lbl)
		}
		if l.Hidden > 0 {
			fmt.Fprintf(c.Out, "   … %d older\n", l.Hidden)
		}
	}
	return nil
}

type boardAddCmd struct {
	Repo   string   `arg:"" help:"owner/name"`
	Column []string `arg:"" help:"Column name."`
}

func (x *boardAddCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessRead)
	if err != nil {
		return err
	}
	col, err := c.F.AddBoardColumn(c.Ctx, c.User, r, strings.Join(x.Column, " "))
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "added column %q\n", col.Name)
	return nil
}

type boardRenameCmd struct {
	Repo   string `arg:"" help:"owner/name"`
	Column string `arg:"" help:"Column name or 1-based position."`
	New    string `arg:"" name:"new-name"`
}

func (x *boardRenameCmd) Run(c *CLI) error {
	r, b, err := c.board(x.Repo)
	if err != nil {
		return err
	}
	li, err := b.Lane(0, x.Column)
	if err != nil {
		return err
	}
	return c.F.RenameBoardColumn(c.Ctx, c.User, r, b.Lanes[li].Column.ID, x.New)
}

type boardRmCmd struct {
	Repo   string   `arg:"" help:"owner/name"`
	Column []string `arg:"" help:"Column name or 1-based position."`
}

func (x *boardRmCmd) Run(c *CLI) error {
	r, b, err := c.board(x.Repo)
	if err != nil {
		return err
	}
	li, err := b.Lane(0, strings.Join(x.Column, " "))
	if err != nil {
		return err
	}
	return c.F.DeleteBoardColumn(c.Ctx, c.User, r, b.Lanes[li].Column.ID)
}
