package tui

import (
	"path"
	"strings"

	"github.com/cooperspencer/forge/internal/gitx"
)

// readmeCache holds a directory's README and its rendering for one width.
type readmeCache struct {
	name, path string // file name, path in the repository
	src        string
	w          int
	rendered   string
}

// readmeNames in order of preference (matched case-insensitively).
var readmeNames = []string{"readme.md", "readme.markdown", "readme", "readme.txt", "readme.rst", "readme.org", "readme.adoc"}

// pickReadme returns the README among a directory's entries, if any.
func pickReadme(entries []gitx.TreeEntry) string {
	best, rank := "", len(readmeNames)
	for _, e := range entries {
		if e.Type != "blob" {
			continue
		}
		for i, n := range readmeNames {
			if strings.EqualFold(e.Name, n) && i < rank {
				best, rank = e.Name, i
			}
		}
	}
	return best
}

func (s *repoState) loadReadme(entries []gitx.TreeEntry) {
	name := pickReadme(entries)
	if name == "" {
		s.readme = readmeCache{}
		return
	}
	p := path.Join(s.path, name)
	data, trunc, err := s.git.Blob(s.ctx.Ctx, s.ref, p, 512<<10)
	if err != nil {
		s.readme = readmeCache{}
		return
	}
	if ptr, ok := gitx.ParseLFSPointer(data); ok {
		if obj, otrunc, err := s.ctx.F.ReadLFSObject(s.ctx.Ctx, s.repo, ptr.OID, 512<<10); err == nil {
			data, trunc = obj, otrunc
		}
	}
	src := strings.ToValidUTF8(string(data), "�")
	if trunc {
		src += "\n\n…"
	}
	if s.readme.path == p && s.readme.src == src {
		return // unchanged: keep the rendering
	}
	s.readme = readmeCache{name: name, path: p, src: src}
}

// readmeView renders the README for width w ("" without one).
func (s *repoState) readmeView(w int) string {
	r := &s.readme
	if r.name == "" {
		return ""
	}
	if r.w != w || r.rendered == "" {
		st := s.ctx.St
		cw := min(w-4, 110)
		lower := strings.ToLower(r.name)
		var body string
		if strings.HasSuffix(lower, ".md") || strings.HasSuffix(lower, ".markdown") {
			body = st.Markdown(r.src, cw)
		} else {
			body = wrap(strings.TrimRight(r.src, "\n"), cw)
		}
		var b strings.Builder
		b.WriteString("  " + st.Label.Render("📖 "+r.name) + "\n\n")
		for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
			b.WriteString("  " + line + "\n")
		}
		r.w, r.rendered = w, b.String()
	}
	return r.rendered
}
