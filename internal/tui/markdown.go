package tui

import (
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// A small Markdown renderer for the terminal: headings, paragraphs, lists
// (nested, numbered, task lists), block quotes, fenced code with syntax
// highlighting, tables, rules, and inline bold / italic / strike / code /
// links, including [[wiki links]]. It covers what people write in wikis and
// issues; anything it doesn't understand is shown as written.

var (
	mdHeading  = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*\s*$`)
	mdRule     = regexp.MustCompile(`^\s{0,3}(-(\s*-){2,}|\*(\s*\*){2,}|_(\s*_){2,})\s*$`)
	mdFence    = regexp.MustCompile("^\\s*(```+|~~~+)\\s*([\\w+#.-]*)")
	mdItem     = regexp.MustCompile(`^(\s*)([-*+]|\d{1,9}[.)])\s+(.*)$`)
	mdQuote    = regexp.MustCompile(`^\s{0,3}>\s?(.*)$`)
	mdTableSep = regexp.MustCompile(`^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$`)
)

// Markdown renders src to fit width w.
func (s *Styles) Markdown(src string, w int) string {
	if w < 20 {
		w = 20
	}
	lines := strings.Split(strings.ReplaceAll(expandTabs(src), "\r\n", "\n"), "\n")
	return strings.TrimRight(s.mdBlocks(lines, w), "\n")
}

func (s *Styles) mdBlocks(lines []string, w int) string {
	var out []string
	emit := func(block string) {
		if len(out) > 0 && out[len(out)-1] != "" {
			out = append(out, "")
		}
		out = append(out, block)
	}
	for i := 0; i < len(lines); {
		l := lines[i]
		t := strings.TrimSpace(l)
		switch {
		case t == "":
			i++

		case mdFence.MatchString(l):
			m := mdFence.FindStringSubmatch(l)
			fence, lang := m[1], m[2]
			var code []string
			i++
			for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), fence[:3]) {
				code = append(code, lines[i])
				i++
			}
			i++ // closing fence
			emit(s.mdCode(lang, strings.Join(code, "\n"), w))

		case mdHeading.MatchString(l):
			m := mdHeading.FindStringSubmatch(l)
			emit(s.mdHeading(len(m[1]), m[2], w))
			i++

		case mdRule.MatchString(l):
			emit(s.Faint.Render(strings.Repeat("─", w)))
			i++

		case mdQuote.MatchString(l):
			var q []string
			for i < len(lines) && mdQuote.MatchString(lines[i]) {
				q = append(q, mdQuote.FindStringSubmatch(lines[i])[1])
				i++
			}
			inner := s.mdBlocks(q, w-2)
			bar := s.r.NewStyle().Foreground(cAccent).Render("│ ")
			var b []string
			for _, ql := range strings.Split(strings.TrimRight(inner, "\n"), "\n") {
				b = append(b, bar+s.Subtle.Render(ql))
			}
			emit(strings.Join(b, "\n"))

		case strings.HasPrefix(t, "|") && i+1 < len(lines) && mdTableSep.MatchString(lines[i+1]):
			var rows []string
			rows = append(rows, l)
			i += 2
			for i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "|") {
				rows = append(rows, lines[i])
				i++
			}
			emit(s.mdTable(rows, w))

		case mdItem.MatchString(l):
			var items []string
			for i < len(lines) {
				cur := lines[i]
				if strings.TrimSpace(cur) == "" {
					// a blank line ends the list unless the next line continues it
					if i+1 < len(lines) && (mdItem.MatchString(lines[i+1]) || strings.HasPrefix(lines[i+1], "  ")) {
						i++
						continue
					}
					break
				}
				if mdItem.MatchString(cur) {
					items = append(items, cur)
				} else if len(items) > 0 && (strings.HasPrefix(cur, " ") || !s.mdBlockStart(cur)) {
					items[len(items)-1] += " " + strings.TrimSpace(cur) // continuation
				} else {
					break
				}
				i++
			}
			emit(s.mdList(items, w))

		default:
			var para []string
			for i < len(lines) && strings.TrimSpace(lines[i]) != "" && (len(para) == 0 || !s.mdBlockStart(lines[i])) {
				para = append(para, strings.TrimSpace(lines[i]))
				i++
			}
			emit(ansi.Wordwrap(s.mdInline(strings.Join(para, " "), lipgloss.NewStyle()), w, " "))
		}
	}
	return strings.Join(out, "\n") + "\n"
}

func (s *Styles) mdBlockStart(l string) bool {
	return mdFence.MatchString(l) || mdHeading.MatchString(l) || mdRule.MatchString(l) || mdQuote.MatchString(l) || mdItem.MatchString(l)
}

func (s *Styles) mdHeading(level int, text string, w int) string {
	st := s.r.NewStyle().Bold(true)
	switch level {
	case 1:
		st = st.Foreground(cAccent)
	case 2:
		st = st.Foreground(cAccent2)
	default:
		st = st.Foreground(cText)
	}
	h := ansi.Wordwrap(s.mdInline(text, st), w, " ")
	switch level {
	case 1:
		return h + "\n" + s.r.NewStyle().Foreground(cAccent).Render(strings.Repeat("━", min(w, ansi.StringWidth(h)+2)))
	case 2:
		return h + "\n" + s.Faint.Render(strings.Repeat("─", min(w, ansi.StringWidth(h)+2)))
	}
	return h
}

func (s *Styles) mdCode(lang, code string, w int) string {
	body := s.HighlightLang(lang, code)
	bar := s.Faint.Render("│ ")
	var b []string
	if lang != "" {
		b = append(b, s.Faint.Render("╭ "+lang))
	}
	for _, l := range strings.Split(body, "\n") {
		if ansi.StringWidth(l) > w-2 {
			l = ansi.Truncate(l, w-3, "…")
		}
		b = append(b, bar+l)
	}
	return strings.Join(b, "\n")
}

func (s *Styles) mdList(items []string, w int) string {
	var b []string
	n := map[int]int{} // numbering per depth
	for _, it := range items {
		m := mdItem.FindStringSubmatch(it)
		depth := len(m[1]) / 2
		for d := range n {
			if d > depth {
				delete(n, d)
			}
		}
		marker := s.r.NewStyle().Foreground(cAccent).Render([]string{"•", "◦", "▪"}[depth%3])
		if m[2][0] >= '0' && m[2][0] <= '9' {
			n[depth]++
			if n[depth] == 1 {
				// keep the author's starting number
				var start int
				for _, c := range m[2] {
					if c >= '0' && c <= '9' {
						start = start*10 + int(c-'0')
					}
				}
				n[depth] = start
			}
			marker = s.r.NewStyle().Foreground(cAccent).Render(itoa(n[depth]) + ".")
		}
		text := m[3]
		switch {
		case strings.HasPrefix(text, "[ ] "):
			marker, text = s.Faint.Render("☐"), text[4:]
		case strings.HasPrefix(text, "[x] ") || strings.HasPrefix(text, "[X] "):
			marker, text = s.StatusOK.Render("☑"), text[4:]
		}
		indent := strings.Repeat("  ", depth)
		prefixW := len(indent) + ansi.StringWidth(marker) + 1
		body := ansi.Wordwrap(s.mdInline(text, lipgloss.NewStyle()), w-prefixW, " ")
		for k, bl := range strings.Split(body, "\n") {
			if k == 0 {
				b = append(b, indent+marker+" "+bl)
			} else {
				b = append(b, strings.Repeat(" ", prefixW)+bl)
			}
		}
	}
	return strings.Join(b, "\n")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for ; n > 0; n /= 10 {
		d = append([]byte{byte('0' + n%10)}, d...)
	}
	return string(d)
}

func (s *Styles) mdTable(rows []string, w int) string {
	split := func(r string) []string {
		r = strings.TrimSpace(r)
		r = strings.TrimSuffix(strings.TrimPrefix(r, "|"), "|")
		cells := strings.Split(r, "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		return cells
	}
	var grid [][]string
	cols := 0
	for i, r := range rows {
		st := lipgloss.NewStyle()
		if i == 0 {
			st = st.Bold(true)
		}
		cells := split(r)
		for k := range cells {
			cells[k] = s.mdInline(cells[k], st)
		}
		grid = append(grid, cells)
		cols = max(cols, len(cells))
	}
	widths := make([]int, cols)
	for _, r := range grid {
		for k, c := range r {
			widths[k] = max(widths[k], ansi.StringWidth(c))
		}
	}
	// shrink the widest columns until the table fits
	for total := func() int {
		t := 0
		for _, x := range widths {
			t += x + 3
		}
		return t
	}; total() > w; {
		wi := 0
		for k := range widths {
			if widths[k] > widths[wi] {
				wi = k
			}
		}
		if widths[wi] <= 3 {
			break
		}
		widths[wi]--
	}
	sep := s.Faint.Render(" │ ")
	var b []string
	for i, r := range grid {
		var cells []string
		for k := 0; k < cols; k++ {
			c := ""
			if k < len(r) {
				c = r[k]
			}
			if ansi.StringWidth(c) > widths[k] {
				c = ansi.Truncate(c, widths[k], "…")
			}
			cells = append(cells, c+strings.Repeat(" ", widths[k]-ansi.StringWidth(c)))
		}
		b = append(b, " "+strings.Join(cells, sep))
		if i == 0 {
			var dash []string
			for _, x := range widths {
				dash = append(dash, strings.Repeat("─", x))
			}
			b = append(b, s.Faint.Render("─"+strings.Join(dash, "─┼─")+"─"))
		}
	}
	return strings.Join(b, "\n")
}

// ---------------------------------------------------------------- inline

type mdSpan struct {
	text                       string
	bold, italic, strike, code bool
	link, wiki, faint          bool
}

// mdInline renders inline markup on top of base.
func (s *Styles) mdInline(text string, base lipgloss.Style) string {
	var b strings.Builder
	for _, sp := range parseInline(text, mdSpan{}) {
		st := base.Renderer(s.r)
		switch {
		case sp.code:
			st = st.Foreground(cAccent2)
		case sp.wiki:
			st = st.Foreground(cAccent).Underline(true)
		case sp.link:
			st = st.Foreground(cBlue).Underline(true)
		case sp.faint:
			st = st.Foreground(cFaint)
		}
		if sp.bold {
			st = st.Bold(true)
		}
		if sp.italic {
			st = st.Italic(true)
		}
		if sp.strike {
			st = st.Strikethrough(true)
		}
		b.WriteString(st.Render(sp.text))
	}
	return b.String()
}

var (
	mdWikiLink = regexp.MustCompile(`^\[\[([^\]|]+)(?:\|([^\]]+))?\]\]`)
	mdLinkRe   = regexp.MustCompile(`^(!?)\[([^\]]*)\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	mdAutoLink = regexp.MustCompile(`^<((?:https?|mailto):[^>\s]+)>`)
)

func parseInline(text string, cur mdSpan) []mdSpan {
	var out []mdSpan
	var plain strings.Builder
	flush := func() {
		if plain.Len() > 0 {
			sp := cur
			sp.text = plain.String()
			out = append(out, sp)
			plain.Reset()
		}
	}
	for i := 0; i < len(text); {
		rest := text[i:]
		c := text[i]
		switch {
		case c == '\\' && i+1 < len(text) && strings.ContainsRune("\\`*_{}[]()#+-.!|~<>", rune(text[i+1])):
			plain.WriteByte(text[i+1])
			i += 2
			continue
		case c == '`':
			n := 1
			for n < len(rest) && rest[n] == '`' {
				n++
			}
			if end := strings.Index(rest[n:], rest[:n]); end >= 0 {
				flush()
				sp := cur
				sp.code, sp.text = true, strings.TrimSpace(rest[n:n+end])
				out = append(out, sp)
				i += n + end + n
				continue
			}
		case c == '[' && mdWikiLink.MatchString(rest):
			m := mdWikiLink.FindStringSubmatch(rest)
			flush()
			sp := cur
			sp.wiki, sp.text = true, strings.TrimSpace(m[1])
			if m[2] != "" {
				sp.text = strings.TrimSpace(m[2])
			}
			out = append(out, sp)
			i += len(m[0])
			continue
		case (c == '[' || c == '!') && mdLinkRe.MatchString(rest):
			m := mdLinkRe.FindStringSubmatch(rest)
			flush()
			if m[1] == "!" {
				sp := cur
				sp.faint, sp.text = true, "🖼 "+m[2]
				out = append(out, sp)
			} else {
				inner := cur
				if isExternal(m[3]) {
					inner.link = true
				} else {
					inner.wiki = true
				}
				out = append(out, parseInline(m[2], inner)...)
				if isExternal(m[3]) && m[2] != m[3] {
					sp := cur
					sp.faint, sp.text = true, " ("+m[3]+")"
					out = append(out, sp)
				}
			}
			i += len(m[0])
			continue
		case c == '<' && mdAutoLink.MatchString(rest):
			m := mdAutoLink.FindStringSubmatch(rest)
			flush()
			sp := cur
			sp.link, sp.text = true, m[1]
			out = append(out, sp)
			i += len(m[0])
			continue
		case strings.HasPrefix(rest, "**") || strings.HasPrefix(rest, "__"):
			if end := strings.Index(rest[2:], rest[:2]); end > 0 {
				flush()
				inner := cur
				inner.bold = true
				out = append(out, parseInline(rest[2:2+end], inner)...)
				i += end + 4
				continue
			}
		case strings.HasPrefix(rest, "~~"):
			if end := strings.Index(rest[2:], "~~"); end > 0 {
				flush()
				inner := cur
				inner.strike = true
				out = append(out, parseInline(rest[2:2+end], inner)...)
				i += end + 4
				continue
			}
		case c == '*' || c == '_':
			// _x_ only at word boundaries, so snake_case stays intact
			prevOK := i == 0 || !isWordByte(text[i-1]) || c == '*'
			if prevOK && len(rest) > 1 && rest[1] != ' ' {
				if end := strings.IndexByte(rest[1:], c); end > 0 && rest[end] != ' ' {
					after := i + end + 2
					if c == '*' || after >= len(text) || !isWordByte(text[after]) {
						flush()
						inner := cur
						inner.italic = true
						out = append(out, parseInline(rest[1:1+end], inner)...)
						i = after
						continue
					}
				}
			}
		}
		plain.WriteByte(c)
		i++
	}
	flush()
	return out
}

func isWordByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func isExternal(target string) bool {
	return strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "#")
}

// mdLink is a link from one wiki page to another.
type mdLink struct {
	Text   string
	Target string // page name
}

// wikiLinks finds the links to other wiki pages: [[Page]], [[Page|text]]
// and relative Markdown links [text](Page) or [text](Page.md).
func wikiLinks(src string) []mdLink {
	var out []mdLink
	seen := map[string]bool{}
	add := func(text, target string) {
		target = strings.TrimSpace(target)
		if i := strings.IndexByte(target, '#'); i >= 0 {
			target = target[:i]
		}
		target = strings.TrimSuffix(strings.TrimPrefix(target, "./"), ".md")
		if target == "" || seen[strings.ToLower(target)] {
			return
		}
		seen[strings.ToLower(target)] = true
		out = append(out, mdLink{Text: text, Target: target})
	}
	inFence := false
	for _, l := range strings.Split(src, "\n") {
		if mdFence.MatchString(l) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		for i := 0; i < len(l); i++ {
			rest := l[i:]
			if m := mdWikiLink.FindStringSubmatch(rest); m != nil {
				text := m[1]
				if m[2] != "" {
					text = m[2]
				}
				add(strings.TrimSpace(text), m[1])
				i += len(m[0]) - 1
			} else if m := mdLinkRe.FindStringSubmatch(rest); m != nil {
				if m[1] == "" && !isExternal(m[3]) {
					add(m[2], strings.ReplaceAll(m[3], "%20", " "))
				}
				i += len(m[0]) - 1
			}
		}
	}
	return out
}
