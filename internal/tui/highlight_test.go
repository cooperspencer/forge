package tui

import (
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func stylesFor(p termenv.Profile) *Styles {
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(p)
	r.SetHasDarkBackground(true)
	return NewStyles(r)
}

const goSrc = "package main\n\n// hello\nfunc main() {\n\tprintln(\"hi\", 42)\n}\n"

func TestHighlightKeepsText(t *testing.T) {
	st := stylesFor(termenv.TrueColor)
	out := st.Highlight("main.go", goSrc)
	if out == goSrc || !strings.Contains(out, "\x1b[") {
		t.Fatal("Go source not highlighted")
	}
	if got := ansi.Strip(out); got != goSrc {
		t.Errorf("text changed:\n%q\n%q", got, goSrc)
	}
	// No style may continue past a line end (the viewer adds gutters and wraps).
	for i, l := range strings.Split(out, "\n") {
		if j := strings.LastIndex(l, "\x1b["); j >= 0 && !strings.HasPrefix(l[j:], sgrReset) {
			t.Errorf("line %d leaves a style open: %q", i+1, l)
		}
	}
	// Different token kinds get different colours.
	kw, str := st.Highlight("x.go", "func"), st.Highlight("x.go", `"s"`)
	if kw == str || ansi.Strip(kw) != "func" {
		t.Errorf("keyword %q vs string %q", kw, str)
	}
}

func TestHighlightDetection(t *testing.T) {
	st := stylesFor(termenv.TrueColor)
	for name, code := range map[string]string{
		"Dockerfile":    "FROM alpine\nRUN echo hi\n",
		".forge.yml":    "jobs:\n  a: {image: alpine}\n",
		"script":        "#!/bin/sh\necho hi\n",
		"main.py":       "def f():\n    return 1\n",
		"README.md":     "# Title\n\nsome *text*\n",
		"deploy/app.tf": "resource \"x\" \"y\" {}\n",
	} {
		if out := st.Highlight(name, code); out == code {
			t.Errorf("%s: not highlighted", name)
		}
	}
	if out := st.Highlight("notes.unknownext", "just text\n"); out != "just text\n" {
		t.Errorf("unknown type changed: %q", out)
	}
	big := strings.Repeat("x := 1\n", maxHighlight/7+1)
	if st.Highlight("big.go", big) != big {
		t.Error("oversized file highlighted")
	}
}

func TestHighlightProfiles(t *testing.T) {
	if out := stylesFor(termenv.Ascii).Highlight("main.go", goSrc); out != goSrc {
		t.Error("colour codes sent to a terminal without colours")
	}
	// 256 colours: still highlighted
	if out := stylesFor(termenv.ANSI256).Highlight("main.go", goSrc); out == goSrc {
		t.Error("not highlighted with 256 colours")
	}
}

const patch = "@@ -1,3 +1,3 @@\n package main\n-func old() {}\n+func new() { return }\n\\ No newline at end of file\n"

func TestHighlightDiff(t *testing.T) {
	st := stylesFor(termenv.TrueColor)
	out := st.HighlightDiff("main.go", patch)
	if got := ansi.Strip(out); got != patch {
		t.Errorf("diff text changed:\n%q\n%q", got, patch)
	}
	lines := strings.Split(out, "\n")
	if !strings.HasPrefix(lines[2], st.hl.delBg) || !strings.HasPrefix(lines[3], st.hl.addBg) {
		t.Error("changed lines not tinted")
	}
	// 16 colours: no background tints, plain diff colouring as before.
	st16 := stylesFor(termenv.ANSI)
	if got, want := st16.HighlightDiff("main.go", patch), st16.colorDiff(patch, 0); got != want {
		t.Errorf("16-colour diff differs from colorDiff:\n%q\n%q", got, want)
	}
}
