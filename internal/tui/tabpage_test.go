package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestGuardKeys: a tab's listed actions check permission when pressed;
// other keys fall through untouched.
func TestGuardKeys(t *testing.T) {
	ran := ""
	allowed := true
	tab := guardKeys(&Tab{Keys: []Key{{"d", "delete"}}, Handle: func(k string, _ *Row) (tea.Cmd, bool) {
		ran = k
		return nil, true
	}}, func() bool { return allowed })

	tab.Handle("d", nil)
	if ran != "d" {
		t.Fatal("allowed action didn't run")
	}
	ran, allowed = "", false
	if cmd, ok := tab.Handle("d", nil); !ok || cmd == nil || ran != "" {
		t.Fatalf("denied action ran (ran=%q, handled=%v)", ran, ok)
	}
	if tab.Handle("x", nil); ran != "x" {
		t.Fatal("unlisted key should fall through to the tab")
	}
}
