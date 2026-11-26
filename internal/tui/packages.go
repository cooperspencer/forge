package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/store"
)

func (ctx *Ctx) pkgActor() forge.PkgActor { return forge.PkgActor{User: ctx.User} }

// packagesTab lists packages from load (all visible ones on Home, the linked
// ones on a repository page).
func packagesTab(ctx *Ctx, empty string, load func() []*store.Package) *Tab {
	st := ctx.St
	t := &Tab{Name: "Packages", List: NewList(st, Column{Title: "TYPE", Width: 10}, Column{Title: "PACKAGE", Flex: 1},
		Column{Title: "LATEST", Flex: 1}, Column{Title: "UPDATED", Width: 10, Right: true})}
	t.List.Empty = empty
	t.Load = func() []Row {
		var rows []Row
		for _, p := range load() {
			rows = append(rows, Row{Cols: []string{st.Label.Render(p.Type), st.Bold.Render(p.FullName()), st.Subtle.Render(latestVersion(ctx, p)),
				st.Faint.Render(ago(p.UpdatedAt))}, Data: p})
		}
		return rows
	}
	t.Keys = []Key{{"enter", "open"}}
	t.Handle = func(k string, row *Row) (tea.Cmd, bool) {
		if k == "enter" && row != nil {
			return push(newPackagePage(ctx, row.Data.(*store.Package))), true
		}
		return nil, false
	}
	return t
}

func latestVersion(ctx *Ctx, p *store.Package) string {
	switch p.Type {
	case store.PkgContainer:
		tags, _ := ctx.F.Store.Tags(ctx.Ctx, p.ID)
		if len(tags) == 0 {
			return "no tags"
		}
		more := ""
		if len(tags) > 1 {
			more = fmt.Sprintf(" +%d", len(tags)-1)
		}
		return tags[0].Name + more
	default:
		files, _ := ctx.F.Store.Files(ctx.Ctx, p.ID)
		if len(files) == 0 {
			return "no files"
		}
		return files[0].Version
	}
}

type pkgItem struct {
	version, file string
}

func newPackagePage(ctx *Ctx, p *store.Package) Page {
	st, f, c := ctx.St, ctx.F, ctx.Ctx
	owner, _ := f.Store.UserByID(c, p.OwnerID)
	access := func() store.Access {
		if owner == nil {
			return store.AccessNone
		}
		return f.PackageAccess(c, ctx.pkgActor(), owner, p.Name)
	}
	var t *Tab
	if p.Type == store.PkgContainer {
		t = &Tab{Name: "Tags", List: NewList(st, Column{Title: "TAG", Flex: 1}, Column{Title: "DIGEST", Width: 14},
			Column{Title: "PLATFORMS", Width: 26}, Column{Title: "SIZE", Width: 9, Right: true}, Column{Title: "PUSHED", Width: 10, Right: true})}
		t.List.Empty = "No tags. Push one with: docker push " + f.ImageRef(p) + ":<tag>"
		t.Load = func() []Row {
			tags, _ := f.Store.Tags(c, p.ID)
			var rows []Row
			for _, tg := range tags {
				rows = append(rows, Row{Cols: []string{st.Bold.Render(tg.Name), st.Faint.Render(shortDigest(tg.Digest)), st.Subtle.Render(f.ImageKind(c, p, tg.Digest)),
					humanSize(f.ImageSize(c, p, tg.Digest)), st.Faint.Render(ago(tg.UpdatedAt))}, Data: pkgItem{version: tg.Name}})
			}
			return rows
		}
	} else {
		t = &Tab{Name: "Files", List: NewList(st, Column{Title: "VERSION", Width: 14}, Column{Title: "FILE", Flex: 1},
			Column{Title: "SHA256", Width: 14}, Column{Title: "SIZE", Width: 9, Right: true}, Column{Title: "BY", Width: 14}, Column{Title: "UPLOADED", Width: 10, Right: true})}
		t.List.Empty = "No files."
		t.Load = func() []Row {
			files, _ := f.Store.Files(c, p.ID)
			var rows []Row
			for _, fl := range files {
				rows = append(rows, Row{Cols: []string{st.Bold.Render(fl.Version), fl.Filename, st.Faint.Render(strings.TrimPrefix(fl.Digest, "sha256:")[:12]),
					humanSize(fl.Size), st.Faint.Render(fl.Creator), st.Faint.Render(ago(fl.CreatedAt))}, Data: pkgItem{fl.Version, fl.Filename}})
			}
			return rows
		}
	}
	t.Header = func() string {
		var how string
		if p.Type == store.PkgContainer {
			how = st.Subtle.Render("pull ") + st.Accent.Render("docker pull "+f.ImageRef(p)+":<tag>")
		} else {
			how = st.Subtle.Render("download ") + st.Accent.Render("curl -LO "+f.GenericURL(p, "<version>", "<file>"))
		}
		link := st.Subtle.Render("not linked to a repository (private to " + p.Owner + ")")
		if owner != nil {
			if r := f.LinkedRepo(c, owner, p.Name); r != nil {
				link = st.Subtle.Render("linked to ") + st.Bold.Render(r.FullName()) + st.Subtle.Render(" (uses its permissions)")
			}
		}
		return " " + how + "\n " + link
	}
	t.Keys = []Key{{"d", "delete tag/file"}, {"D", "delete version"}, {"X", "delete package"}}
	t.Handle = func(k string, row *Row) (tea.Cmd, bool) {
		switch k {
		case "d", "D":
			if row == nil {
				return nil, true
			}
			if access() < store.AccessWrite {
				return toastErr(fmt.Errorf("you need write access to delete")), true
			}
			it := row.Data.(pkgItem)
			file, what := it.file, it.version
			if p.Type == store.PkgContainer {
				file, what = "", "tag "+it.version
			} else if k == "D" {
				file, what = "", "version "+it.version+" (all files)"
			} else {
				what = it.version + "/" + it.file
			}
			return push(NewConfirm(ctx, "Delete "+what+" of "+p.FullName()+"?", func() tea.Cmd {
				return do(func() error { return f.DeletePackageVersion(c, ctx.pkgActor(), owner, p, it.version, file) }, "deleted", refreshCmd)
			})), true
		case "X":
			if access() < store.AccessAdmin {
				return toastErr(fmt.Errorf("only admins of %s can delete the package", p.Owner)), true
			}
			return push(NewForm(ctx, "Delete package "+p.FullName(), []*Field{{Label: "Type the package name to confirm", Placeholder: p.Name}},
				func(v map[string]string) (tea.Cmd, error) {
					if !confirmName(v["Type the package name to confirm"], p.Name, p.FullName()) {
						return nil, fmt.Errorf("name does not match")
					}
					if err := f.DeletePackage(c, ctx.pkgActor(), owner, p); err != nil {
						return nil, err
					}
					return tea.Sequence(pop, pop, toast("package deleted")), nil
				})), true
		}
		return nil, false
	}
	return NewTabPage(ctx, p.FullName()+" ("+p.Type+")", []*Tab{t})
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}
	return d
}
