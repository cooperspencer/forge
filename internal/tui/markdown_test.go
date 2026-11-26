package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

const mdSample = "# Getting started\n\nWelcome to **forge**, a _small_ forge with `ssh` and ~~no~~ web UI.\nSee [[Installation]] or [the docs](https://example.com/docs).\n\n## Steps\n\n1. Install it\n2. Run `forge serve`\n   - nested item\n- [ ] todo\n- [x] done\n\n> Quote line one\n> line two\n\n```go\nfunc main() {}\n```\n\n| Key | Meaning |\n|-----|---------|\n| `n` | new page |\n\n---\nsnake_case_name stays.\n"

func TestMarkdown(t *testing.T) {
	st := stylesFor(termenv.TrueColor)
	out := st.Markdown(mdSample, 60)
	plain := ansi.Strip(out)
	for _, want := range []string{
		"Getting started\n━", "Welcome to forge, a small forge with ssh and no web UI.",
		"Installation", "the docs (https://example.com/docs)",
		"Steps\n─", "1. Install it", "2. Run forge serve", "  ◦ nested item", "☐ todo", "☑ done",
		"│ Quote line one line two", "╭ go", "│ func main() {}",
		" Key │ Meaning", "─┼─", " n   │ new page", "snake_case_name stays.",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("missing %q in:\n%s", want, plain)
		}
	}
	for i, l := range strings.Split(out, "\n") {
		if ansi.StringWidth(l) > 60 {
			t.Errorf("line %d wider than 60: %q", i, ansi.Strip(l))
		}
	}
	// styles: bold, italic, strike and code are applied
	for _, seq := range []string{"\x1b[1m", "\x1b[3m", "\x1b[9m"} {
		if !strings.Contains(out, seq) {
			t.Errorf("missing SGR %q", seq)
		}
	}
	// a long paragraph wraps to the width
	long := st.Markdown(strings.Repeat("word ", 40), 30)
	if n := len(strings.Split(long, "\n")); n < 6 {
		t.Errorf("long paragraph not wrapped: %d lines", n)
	}
}

func TestWikiLinks(t *testing.T) {
	got := wikiLinks("See [[Install Guide]], [[Setup|the setup]], [docs](Usage.md#top), [ext](https://x.y), ![img](pic.png)\n```\n[[Not A Link]]\n```\n[[install guide]] again")
	var s []string
	for _, l := range got {
		s = append(s, l.Text+"→"+l.Target)
	}
	if strings.Join(s, ",") != "Install Guide→Install Guide,the setup→Setup,docs→Usage" {
		t.Errorf("links: %v", s)
	}
}
