package sshd

import (
	"fmt"
	"io"
	"strings"

	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/store"
)

type wikiCmds struct {
	List wikiListCmd `cmd:"" help:"List wiki pages."`
	View wikiViewCmd `cmd:"" help:"Print a page's Markdown."`
	Edit wikiEditCmd `cmd:"" help:"Create or replace a page from stdin (wiki edit owner/name Page < page.md)."`
	Rm   wikiPageCmd `cmd:"" help:"Delete a page."`
	Log  wikiLogCmd  `cmd:"" help:"Show a page's history."`
}

type wikiListCmd struct {
	Repo string `arg:"" help:"owner/name"`
}

func (x *wikiListCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessRead)
	if err != nil {
		return err
	}
	pages, err := c.F.WikiPages(c.Ctx, c.User, r)
	if err != nil {
		return err
	}
	if len(pages) == 0 {
		fmt.Fprintln(c.Out, "no wiki pages")
		return nil
	}
	w := c.table()
	for _, p := range pages {
		fmt.Fprintf(w, "%s\t%s\t%s\n", p.Name, p.Author, ts(p.Updated))
	}
	return w.Flush()
}

type wikiViewCmd struct {
	Rev  string   `help:"Show the page as of this commit."`
	Repo string   `arg:"" help:"owner/name"`
	Page []string `arg:""`
}

func (x *wikiViewCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessRead)
	if err != nil {
		return err
	}
	_, content, err := c.F.WikiPageContent(c.Ctx, c.User, r, strings.Join(x.Page, " "), x.Rev)
	if err != nil {
		return err
	}
	_, err = io.WriteString(c.Out, content)
	return err
}

type wikiEditCmd struct {
	Message string   `short:"m" help:"Commit message."`
	Rename  string   `placeholder:"new-name" help:"Also rename the page."`
	Repo    string   `arg:"" help:"owner/name"`
	Page    []string `arg:""`
}

func (x *wikiEditCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessWrite)
	if err != nil {
		return err
	}
	name := strings.Join(x.Page, " ")
	content, err := c.bodyArg("-")
	if err != nil {
		return err
	}
	e := forge.WikiEdit{Name: name, Content: content, Message: x.Message}
	if _, _, err := c.F.WikiPageContent(c.Ctx, c.User, r, name, ""); err == nil {
		e.OldName = name
		if x.Rename != "" {
			e.Name = x.Rename
		}
	} else if x.Rename != "" {
		return fmt.Errorf("no page %q to rename", name)
	}
	file, err := c.F.SaveWikiPage(c.Ctx, c.User, r, e)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "saved %s\n", forge.WikiName(file))
	return nil
}

type wikiPageCmd struct {
	Repo string   `arg:"" help:"owner/name"`
	Page []string `arg:""`
}

func (x *wikiPageCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessWrite)
	if err != nil {
		return err
	}
	return c.F.DeleteWikiPage(c.Ctx, c.User, r, strings.Join(x.Page, " "))
}

type wikiLogCmd struct {
	Repo string   `arg:"" help:"owner/name"`
	Page []string `arg:""`
}

func (x *wikiLogCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessRead)
	if err != nil {
		return err
	}
	cs, err := c.F.WikiHistory(c.Ctx, c.User, r, strings.Join(x.Page, " "))
	if err != nil {
		return err
	}
	w := c.table()
	for _, cm := range cs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", cm.Short(), ts(cm.When), cm.Author, cm.Subject)
	}
	return w.Flush()
}
