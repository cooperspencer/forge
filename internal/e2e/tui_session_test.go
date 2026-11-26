package e2e

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/cooperspencer/forge/internal/store"
	"github.com/cooperspencer/forge/internal/tui"
	gossh "golang.org/x/crypto/ssh"
)

// TestTUISessionReauth: an open TUI session follows changes to its account
// at the next key press: lost admin rights apply at once, and a removed key
// or deleted account ends the session.
func TestTUISessionReauth(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := e.f

	session := func(u *store.User) (*tui.Ctx, func(string) bool) {
		t.Helper()
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		pk, _ := gossh.NewPublicKey(pub)
		if err := f.AddKey(ctx, u, string(gossh.MarshalAuthorizedKey(pk))); err != nil {
			t.Fatal(err)
		}
		cp := *u
		c := &tui.Ctx{F: f, User: &cp, PubKey: pk, St: tui.NewStyles(lipgloss.NewRenderer(io.Discard)), Ctx: ctx}
		app := tui.New(c, tui.NewHome(c))
		t.Cleanup(app.Close)
		// press reports whether the session is still alive after a key
		press := func(k string) bool {
			_, cmd := app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
			if cmd == nil {
				return true
			}
			_, quit := cmd().(tea.QuitMsg)
			return !quit
		}
		return c, press
	}

	// demoted while the session is open
	carol, _ := f.Store.CreateUser(ctx, "carol", true, false)
	c, press := session(carol)
	if !press("j") || !c.User.SiteAdmin() {
		t.Fatal("admin session should run as admin")
	}
	carol.IsAdmin = false
	f.Store.UpdateUser(ctx, carol)
	if !press("j") {
		t.Fatal("a demoted admin keeps their session")
	}
	if c.User.SiteAdmin() {
		t.Error("the session still has admin rights after the demotion")
	}

	// key removed
	dave, _ := f.Store.CreateUser(ctx, "dave", false, false)
	_, press = session(dave)
	keys, _ := f.Store.SSHKeys(ctx, dave.ID)
	f.Store.DeleteSSHKey(ctx, dave.ID, keys[0].ID)
	if press("j") {
		t.Error("the session survived the removal of its key")
	}

	// account deleted
	erin, _ := f.Store.CreateUser(ctx, "erin", false, false)
	_, press = session(erin)
	f.Store.DeleteUser(ctx, erin.ID)
	if press("j") {
		t.Error("the session survived the deletion of its account")
	}
}
