package tui

import (
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/gitx"
	"github.com/cooperspencer/forge/internal/store"
)

// ---------------------------------------------------------------- wiki tab

func (s *repoState) wikiTab() *Tab {
	st, f, c := s.ctx.St, s.ctx.F, s.ctx.Ctx
	t := &Tab{Name: "Wiki", List: NewList(st, Column{Title: "PAGE", Flex: 1}, Column{Title: "LAST EDIT", Width: 16}, Column{Title: "UPDATED", Width: 10, Right: true})}
	t.Header = func() string {
		return st.Faint.Render(" clone: ") + st.Accent.Render(strings.TrimSuffix(f.Cfg.SSHCloneURL(s.repo.FullName()), ".git")+".wiki.git")
	}
	t.Load = func() []Row {
		pages, err := f.WikiPages(c, s.ctx.User, s.repo)
		if err != nil {
			t.List.Empty = err.Error()
			return nil
		}
		var rows []Row
		for _, p := range pages {
			name := st.Bold.Render(p.Name)
			if strings.EqualFold(p.Name, forge.WikiHome) {
				name = st.Label.Render("⌂ " + p.Name)
			}
			rows = append(rows, Row{Cols: []string{name, st.Subtle.Render(p.Author), st.Faint.Render(ago(p.Updated))}, Filter: p.Name, Data: p})
		}
		t.List.Empty = "This wiki has no pages yet."
		if s.ctx.Access(s.repo) >= store.AccessWrite {
			t.List.Empty = "This wiki has no pages yet. Press n to write the Home page."
		}
		return rows
	}
	t.Keys = []Key{{"enter", "read"}, {"n", "new page"}, {"e", "edit"}, {"d", "delete"}}
	t.Handle = func(k string, row *Row) (tea.Cmd, bool) {
		switch k {
		case "enter":
			if row != nil {
				return push(newWikiPage(s.ctx, s.repo, row.Data.(forge.WikiPage).Name)), true
			}
		case "n":
			name := ""
			if len(t.List.rows) == 0 {
				name = forge.WikiHome
			}
			return push(wikiEditForm(s.ctx, s.repo, "", name, "", true)), true
		case "e":
			if row != nil {
				p := row.Data.(forge.WikiPage)
				_, content, err := f.WikiPageContent(c, s.ctx.User, s.repo, p.Name, "")
				if err != nil {
					return toastErr(err), true
				}
				return push(wikiEditForm(s.ctx, s.repo, p.Name, p.Name, content, false)), true
			}
		case "d":
			if row != nil {
				return push(wikiDeleteConfirm(s.ctx, s.repo, row.Data.(forge.WikiPage).Name, false)), true
			}
		}
		return nil, false
	}
	return t
}

// wikiEditForm creates (oldName "") or edits a page. open: show the page
// after saving.
func wikiEditForm(ctx *Ctx, r *store.Repo, oldName, name, content string, open bool) Page {
	if ctx.User == nil {
		return NewConfirm(ctx, "Sign in to edit the wiki.", func() tea.Cmd { return nil })
	}
	if ctx.Access(r) < store.AccessWrite {
		return NewConfirm(ctx, "You need write access to "+r.FullName()+" to edit its wiki.", func() tea.Cmd { return nil })
	}
	head := ctx.F.WikiHead(ctx.Ctx, r)
	title := "New wiki page"
	if oldName != "" {
		title = "Edit " + oldName
	}
	hint := "Markdown · link pages with [[Page Name]]"
	return NewForm(ctx, title, []*Field{
		{Label: "Title", Value: name, Placeholder: "Page name, e.g. Getting Started or guides/Setup"},
		{Label: "Content", Value: content, Multiline: true, Tall: true, Hint: hint},
		{Label: "Message", Placeholder: "optional: what changed (commit message)"},
	}, func(v map[string]string) (tea.Cmd, error) {
		file, err := ctx.F.SaveWikiPage(ctx.Ctx, ctx.User, r, forge.WikiEdit{OldName: oldName, Name: v["Title"], Content: v["Content"], Message: v["Message"], Expect: head})
		if err != nil {
			return nil, err
		}
		saved := forge.WikiName(file)
		if open {
			return tea.Sequence(pop, toast("saved "+saved), push(newWikiPage(ctx, r, saved))), nil
		}
		return tea.Sequence(pop, toast("saved "+saved)), nil
	})
}

func wikiDeleteConfirm(ctx *Ctx, r *store.Repo, name string, thenPop bool) Page {
	return NewConfirm(ctx, fmt.Sprintf("Delete the wiki page %q? (It stays in the wiki's git history.)", name), func() tea.Cmd {
		then := []tea.Cmd{refreshCmd}
		if thenPop {
			then = []tea.Cmd{pop}
		}
		return do(func() error { return ctx.F.DeleteWikiPage(ctx.Ctx, ctx.User, r, name) }, "deleted "+name, then...)
	})
}

// ---------------------------------------------------------------- page view

// newWikiPage shows a rendered page. e edit, o follow a link, h history.
func newWikiPage(ctx *Ctx, r *store.Repo, name string) Page {
	st, f, c := ctx.St, ctx.F, ctx.Ctx
	var content string
	var page *forge.WikiPage
	load := func() error {
		p, body, err := f.WikiPageContent(c, ctx.User, r, name, "")
		if err != nil {
			return err
		}
		page, content, name = p, body, p.Name
		return nil
	}
	loadErr := load()
	tp := NewTextPage(ctx, "wiki: "+name, "")
	tp.renderW = func(w int) string {
		if err := load(); err != nil {
			loadErr = err
		}
		if loadErr != nil {
			if errors.Is(loadErr, store.ErrNotFound) {
				return st.Faint.Render(fmt.Sprintf(" The page %q doesn't exist (any more).", name))
			}
			return st.Error.Render(" " + loadErr.Error())
		}
		cw := min(w-2, 100)
		head := " " + st.Title.Render(page.Name) + "  " + st.Faint.Render(fmt.Sprintf("edited by %s %s · %s", page.Author, ago(page.Updated), shortSHA(page.SHA)))
		return head + "\n" + st.Faint.Render(strings.Repeat("─", w)) + "\n\n" + indent(st.Markdown(content, cw), " ")
	}
	tp.keys = []Key{{"e", "edit"}, {"o", "open link"}, {"h", "history"}, {"n", "new page"}, {"d", "delete"}}
	tp.handle = func(k string) (tea.Cmd, bool) {
		switch k {
		case "e":
			if page == nil {
				return nil, true
			}
			return push(wikiEditForm(ctx, r, page.Name, page.Name, content, false)), true
		case "n":
			return push(wikiEditForm(ctx, r, "", "", "", true)), true
		case "d":
			if page == nil {
				return nil, true
			}
			return push(wikiDeleteConfirm(ctx, r, page.Name, true)), true
		case "h":
			if page == nil {
				return nil, true
			}
			return push(wikiHistory(ctx, r, page.Name)), true
		case "o":
			links := wikiLinks(content)
			if len(links) == 0 {
				return toast("this page links to no other wiki pages"), true
			}
			return push(wikiLinkPicker(ctx, r, links)), true
		}
		return nil, false
	}
	return tp
}

// wikiLinkPicker lists the page's links; missing pages can be created.
func wikiLinkPicker(ctx *Ctx, r *store.Repo, links []mdLink) Page {
	st, f, c := ctx.St, ctx.F, ctx.Ctx
	t := &Tab{Name: "Links", List: NewList(st, Column{Title: "LINK", Flex: 1}, Column{Title: "PAGE", Flex: 1}, Column{Title: "", Width: 10})}
	t.Load = func() []Row {
		pages, _ := f.WikiPages(c, ctx.User, r)
		exists := map[string]bool{}
		for _, p := range pages {
			exists[strings.ToLower(p.Path)] = true
		}
		var rows []Row
		for _, l := range links {
			state := st.StatusOK.Render("✓")
			if file, err := forge.WikiFile(l.Target); err != nil || !exists[strings.ToLower(file)] {
				state = st.Faint.Render("missing")
			}
			rows = append(rows, Row{Cols: []string{st.Bold.Render(l.Text), st.Accent.Render(l.Target), state}, Filter: l.Text + " " + l.Target, Data: l})
		}
		return rows
	}
	t.Keys = []Key{{"enter", "open (or create)"}}
	t.Handle = func(k string, row *Row) (tea.Cmd, bool) {
		if k != "enter" || row == nil {
			return nil, false
		}
		l := row.Data.(mdLink)
		if _, _, err := f.WikiPageContent(c, ctx.User, r, l.Target, ""); errors.Is(err, store.ErrNotFound) {
			return replace(wikiEditForm(ctx, r, "", l.Target, "", true)), true
		}
		return replace(newWikiPage(ctx, r, l.Target)), true
	}
	return NewTabPage(ctx, "links", []*Tab{t})
}

// wikiHistory lists a page's revisions; enter shows that version.
func wikiHistory(ctx *Ctx, r *store.Repo, name string) Page {
	st, f, c := ctx.St, ctx.F, ctx.Ctx
	t := &Tab{Name: "History of " + name, List: NewList(st, Column{Title: "SHA", Width: 8}, Column{Title: "MESSAGE", Flex: 1}, Column{Title: "AUTHOR", Width: 16}, Column{Title: "WHEN", Width: 10, Right: true})}
	t.Load = func() []Row {
		cs, err := f.WikiHistory(c, ctx.User, r, name)
		if err != nil {
			t.List.Empty = err.Error()
		}
		var rows []Row
		for _, cm := range cs {
			rows = append(rows, Row{Cols: []string{st.Faint.Render(cm.Short()), cm.Subject, st.Subtle.Render(cm.Author), st.Faint.Render(ago(cm.When))}, Filter: cm.Subject + " " + cm.Author, Data: cm})
		}
		return rows
	}
	t.Keys = []Key{{"enter", "show this version"}}
	t.Handle = func(k string, row *Row) (tea.Cmd, bool) {
		if k != "enter" || row == nil {
			return nil, false
		}
		cm := row.Data.(gitx.Commit)
		_, body, err := f.WikiPageContent(c, ctx.User, r, name, cm.SHA)
		if err != nil {
			return toastErr(err), true
		}
		tp := NewTextPage(ctx, fmt.Sprintf("%s @ %s", name, cm.Short()), "")
		tp.renderW = func(w int) string {
			head := " " + st.Title.Render(name) + "  " + st.StatusRun.Render("old version") + "  " +
				st.Faint.Render(fmt.Sprintf("%s · %s · %s", cm.Short(), cm.Author, ago(cm.When)))
			return head + "\n" + st.Faint.Render(strings.Repeat("─", w)) + "\n\n" + indent(st.Markdown(body, min(w-2, 100)), " ")
		}
		return push(tp), true
	}
	return NewTabPage(ctx, "wiki history", []*Tab{t})
}
