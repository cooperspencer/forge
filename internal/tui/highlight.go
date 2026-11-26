package tui

import (
	"path"
	"strings"
	"sync"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Syntax highlighting: chroma tokenizes, and tokens are coloured through the
// session's own renderer, so each SSH client gets colours for its terminal
// (true colour, 256, 16 or none; light or dark background).

// maxHighlight bounds the work per file: bigger files are shown plain.
const maxHighlight = 512 << 10

// syntax colours, GitHub-like.
var (
	synKeyword  = lipgloss.AdaptiveColor{Light: "#CF222E", Dark: "#FF7B72"}
	synString   = lipgloss.AdaptiveColor{Light: "#0A3069", Dark: "#A5D6FF"}
	synComment  = lipgloss.AdaptiveColor{Light: "#6E7781", Dark: "#8B949E"}
	synConstant = lipgloss.AdaptiveColor{Light: "#0550AE", Dark: "#79C0FF"}
	synFunction = lipgloss.AdaptiveColor{Light: "#8250DF", Dark: "#D2A8FF"}
	synType     = lipgloss.AdaptiveColor{Light: "#953800", Dark: "#FFA657"}
	synTag      = lipgloss.AdaptiveColor{Light: "#116329", Dark: "#7EE787"}

	diffAddBg = lipgloss.AdaptiveColor{Light: "#E6FFEC", Dark: "#12261E"}
	diffDelBg = lipgloss.AdaptiveColor{Light: "#FFEBE9", Dark: "#2D1215"}
)

// synStyle holds the escape sequences that switch a token style on; they are
// computed once per session (rendering every token through lipgloss would be
// far too slow for large files).
type synStyle struct{ on string }

type highlighter struct {
	enabled bool
	styles  map[chroma.TokenType]synStyle
	addBg   string // background for added / removed diff lines ("" = no tint)
	delBg   string
}

const sgrReset = "\x1b[0m"

// onSeq returns the escape sequence that style s emits before its text.
func onSeq(s lipgloss.Style) string {
	out := s.Render("\x00")
	if i := strings.IndexByte(out, 0); i > 0 {
		return out[:i]
	}
	return ""
}

func newHighlighter(r *lipgloss.Renderer) *highlighter {
	h := &highlighter{styles: map[chroma.TokenType]synStyle{}}
	prof := r.ColorProfile()
	if prof == termenv.Ascii {
		return h
	}
	h.enabled = true
	fg := func(c lipgloss.TerminalColor) lipgloss.Style { return r.NewStyle().Foreground(c) }
	set := func(s lipgloss.Style, types ...chroma.TokenType) {
		for _, t := range types {
			h.styles[t] = synStyle{on: onSeq(s)}
		}
	}
	set(fg(synKeyword), chroma.Keyword, chroma.KeywordNamespace, chroma.KeywordDeclaration, chroma.KeywordReserved,
		chroma.CommentPreproc, chroma.CommentPreprocFile, chroma.OperatorWord, chroma.NameException)
	set(fg(synType), chroma.KeywordType, chroma.NameClass, chroma.NameBuiltin, chroma.NameBuiltinPseudo,
		chroma.NameNamespace, chroma.NameEntity)
	set(fg(synConstant), chroma.KeywordConstant, chroma.NameConstant, chroma.LiteralNumber, chroma.Literal,
		chroma.NameAttribute, chroma.NameVariableMagic, chroma.LiteralStringSymbol)
	set(fg(synString), chroma.LiteralString, chroma.LiteralStringEscape, chroma.LiteralStringRegex)
	set(fg(synComment).Italic(true), chroma.Comment, chroma.CommentHashbang, chroma.CommentSpecial)
	set(fg(synFunction), chroma.NameFunction, chroma.NameFunctionMagic, chroma.NameDecorator, chroma.NameLabel)
	set(fg(synTag), chroma.NameTag)
	set(fg(synConstant).Bold(true), chroma.GenericHeading, chroma.GenericSubheading)
	set(r.NewStyle().Bold(true), chroma.GenericStrong)
	set(r.NewStyle().Italic(true), chroma.GenericEmph)
	set(fg(cGreen), chroma.GenericInserted)
	set(fg(cRed), chroma.GenericDeleted)
	// Background tints only where the terminal can show subtle shades;
	// with 16 colours they would become solid green/red blocks.
	if prof == termenv.TrueColor || prof == termenv.ANSI256 {
		h.addBg = onSeq(r.NewStyle().Background(diffAddBg))
		h.delBg = onSeq(r.NewStyle().Background(diffDelBg))
	}
	return h
}

// style finds the colour for t, falling back to its sub-category and category
// (e.g. LiteralStringDouble → LiteralString).
func (h *highlighter) style(t chroma.TokenType) (synStyle, bool) {
	if s, ok := h.styles[t]; ok {
		return s, true
	}
	if s, ok := h.styles[t.SubCategory()]; ok {
		return s, true
	}
	s, ok := h.styles[t.Category()]
	return s, ok
}

var lexerCache sync.Map // file name pattern → chroma.Lexer (nil if none)

// lexerFor picks a lexer by file name, then by content (shebangs, modelines).
func lexerFor(name, code string) chroma.Lexer {
	base := path.Base(name)
	if l, ok := lexerCache.Load(base); ok {
		if l == nil {
			return nil
		}
		return l.(chroma.Lexer)
	}
	l := lexers.Match(base)
	if l == nil && code != "" {
		sample := code
		if len(sample) > 4096 {
			sample = sample[:4096]
		}
		l = lexers.Analyse(sample)
		if l != nil {
			return chroma.Coalesce(l) // content-based: don't cache by name
		}
	}
	if l != nil {
		l = chroma.Coalesce(l)
	}
	lexerCache.Store(base, l)
	return l
}

// Highlight colours code according to the language of file name. It returns
// code unchanged if the language is unknown, the file is too big or the
// terminal has no colours.
func (s *Styles) Highlight(name, code string) string {
	h := s.hl
	if !h.enabled || len(code) > maxHighlight {
		return code
	}
	lx := lexerFor(name, code)
	if lx == nil {
		return code
	}
	it, err := lx.Tokenise(nil, code)
	if err != nil {
		return code
	}
	var b strings.Builder
	b.Grow(len(code) * 2)
	for tok := it(); tok != chroma.EOF; tok = it() {
		h.write(&b, tok, "")
	}
	return b.String()
}

// write emits one token; bg (an escape sequence or "") is re-applied after
// every reset so diff line tints stay continuous.
func (h *highlighter) write(b *strings.Builder, tok chroma.Token, bg string) {
	st, ok := h.style(tok.Type)
	if !ok && bg == "" {
		b.WriteString(tok.Value)
		return
	}
	// Styles must not span lines: the viewer wraps and prefixes lines.
	for i, part := range strings.Split(tok.Value, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		if part == "" {
			continue
		}
		b.WriteString(bg)
		b.WriteString(st.on)
		b.WriteString(part)
		b.WriteString(sgrReset)
	}
}

// highlightLine colours one line of code (no newline) on background bg.
func (h *highlighter) highlightLine(lx chroma.Lexer, line, bg string) string {
	var b strings.Builder
	if lx != nil {
		if it, err := lx.Tokenise(nil, line); err == nil {
			for tok := it(); tok != chroma.EOF; tok = it() {
				h.write(&b, chroma.Token{Type: tok.Type, Value: strings.TrimSuffix(tok.Value, "\n")}, bg)
			}
			return b.String()
		}
	}
	if bg == "" {
		return line
	}
	return bg + line + sgrReset
}

// HighlightDiff renders a unified diff of file name: hunk headers in blue,
// +/- markers in green/red with a tinted background, and the code itself
// syntax-highlighted. Without colour support it falls back to colorDiff.
func (s *Styles) HighlightDiff(name, patch string) string {
	h := s.hl
	if !h.enabled || h.addBg == "" || len(patch) > maxHighlight {
		return s.colorDiff(patch, 0)
	}
	lx := lexerFor(name, "")
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimRight(patch, "\n"), "\n") {
		l = expandTabs(l)
		switch {
		case strings.HasPrefix(l, "@@"):
			b.WriteString(s.Hunk.Render(l))
		case strings.HasPrefix(l, `\`): // "\ No newline at end of file"
			b.WriteString(s.Faint.Render(l))
		case strings.HasPrefix(l, "+"):
			b.WriteString(h.addBg + s.Add.Render("+") + h.highlightLine(lx, l[1:], h.addBg))
		case strings.HasPrefix(l, "-"):
			b.WriteString(h.delBg + s.Del.Render("-") + h.highlightLine(lx, l[1:], h.delBg))
		case strings.HasPrefix(l, " "):
			b.WriteString(" " + h.highlightLine(lx, l[1:], ""))
		default:
			b.WriteString(l)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// HighlightLang colours code in a named language (a Markdown fence's info
// string such as "go" or "yaml"); unknown languages are shown plain.
func (s *Styles) HighlightLang(lang, code string) string {
	h := s.hl
	if !h.enabled || lang == "" || len(code) > maxHighlight {
		return code
	}
	l := lexers.Get(strings.ToLower(lang))
	if l == nil {
		return code
	}
	it, err := chroma.Coalesce(l).Tokenise(nil, code)
	if err != nil {
		return code
	}
	var b strings.Builder
	for tok := it(); tok != chroma.EOF; tok = it() {
		h.write(&b, tok, "")
	}
	return b.String()
}
