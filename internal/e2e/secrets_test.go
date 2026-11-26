package e2e

import (
	"bytes"
	"context"
	"errors"
	"github.com/rs/zerolog"
	"os"
	"path/filepath"
	"testing"

	"github.com/cooperspencer/forge/internal/config"
	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/secretbox"
	"github.com/cooperspencer/forge/internal/store"
)

func openForge(t *testing.T, cfg *config.Config) *forge.Forge {
	t.Helper()
	f, err := forge.New(cfg, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// dbContains reports whether the raw database files contain b.
func dbContains(t *testing.T, cfg *config.Config, b string) bool {
	for _, p := range []string{"forge.db", "forge.db-wal"} {
		data, _ := os.ReadFile(cfg.Path(p))
		if bytes.Contains(data, []byte(b)) {
			return true
		}
	}
	return false
}

// TestSecretEncryption: legacy plaintext values are encrypted on startup,
// values never hit the disk in plaintext, a changed key is detected, and
// rekey recovers.
func TestSecretEncryption(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()

	f := openForge(t, cfg)
	u, _ := f.Store.CreateUser(ctx, "alice", true, false)
	r, err := f.CreateRepo(ctx, u, "alice", "app", "", true)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a secret stored by a version without encryption.
	const legacy = "legacy-plaintext-value-1234"
	f.Store.SetSecret(ctx, store.SecretScope{RepoID: r.ID}, "LEGACY", legacy, false)
	f.Close()
	if !dbContains(t, cfg, legacy) {
		t.Fatal("test setup: legacy plaintext not found in database files")
	}

	f = openForge(t, cfg) // migrates
	if dbContains(t, cfg, legacy) {
		t.Error("plaintext still present in database files after migration")
	}
	const fresh = "fresh-secret-value-5678"
	if err := f.SetRepoSecret(ctx, u, r, "fresh", fresh); err != nil {
		t.Fatal(err)
	}
	if dbContains(t, cfg, fresh) {
		t.Error("new secret written in plaintext")
	}
	vals, missing, err := f.JobSecrets(ctx, r, []string{"LEGACY", "FRESH", "NOPE"})
	if err != nil || vals["LEGACY"] != legacy || vals["FRESH"] != fresh || len(missing) != 1 || missing[0] != "NOPE" {
		t.Fatalf("JobSecrets = %v %v %v", vals, missing, err)
	}
	f.Close()

	// Switch to an external key: old values are detected as foreign.
	keyFile := filepath.Join(t.TempDir(), "key")
	os.WriteFile(keyFile, []byte("an-external-key-that-is-long-enough\n"), 0o600)
	cfg.SecretKeyFile = keyFile
	f = openForge(t, cfg)
	defer func() { f.Close() }()
	if _, _, err := f.JobSecrets(ctx, r, []string{"FRESH"}); !errors.Is(err, secretbox.ErrWrongKey) {
		t.Fatalf("expected wrong-key error, got %v", err)
	}
	old, _ := os.ReadFile(cfg.Path("secret.key"))
	if n, err := f.Rekey(ctx, old); err != nil || n != 2 {
		t.Fatalf("rekey = %d, %v", n, err)
	}
	if vals, _, err := f.JobSecrets(ctx, r, []string{"LEGACY", "FRESH"}); err != nil || vals["FRESH"] != fresh || vals["LEGACY"] != legacy {
		t.Fatalf("after rekey: %v %v", vals, err)
	}

	// A missing configured key file is a startup error, not a silent new key.
	cfg.SecretKeyFile = filepath.Join(t.TempDir(), "missing")
	if _, err := forge.New(cfg, zerolog.Nop()); err == nil {
		t.Fatal("started with a missing secret_key_file")
	}
}

func TestOwnerSecretPermissions(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := e.f
	org, err := f.CreateOrg(ctx, e.admin, "acme")
	if err != nil {
		t.Fatal(err)
	}
	owner, _ := f.Store.CreateUser(ctx, "olga", false, false)
	member, _ := f.Store.CreateUser(ctx, "mike", false, false)
	f.Store.SetOrgMember(ctx, org.ID, owner.ID, "owner")
	f.Store.SetOrgMember(ctx, org.ID, member.ID, "member")
	repo, err := f.CreateRepo(ctx, owner, "acme", "api", "", false)
	if err != nil {
		t.Fatal(err)
	}

	if err := f.SetOwnerSecret(ctx, owner, org, "DEPLOY", "x-value"); err != nil {
		t.Fatalf("org owner: %v", err)
	}
	if err := f.SetOwnerSecret(ctx, member, org, "DEPLOY", "y"); !errors.Is(err, forge.ErrForbidden) {
		t.Errorf("org member set org secret: %v", err)
	}
	if _, err := f.OwnerSecrets(ctx, member, org); !errors.Is(err, forge.ErrForbidden) {
		t.Errorf("org member listed org secrets: %v", err)
	}
	// Members have write (not admin) on org repos: they can't manage repo secrets either.
	if err := f.SetRepoSecret(ctx, member, repo, "X", "y"); !errors.Is(err, forge.ErrForbidden) {
		t.Errorf("org member set repo secret: %v", err)
	}
	if err := f.SetOwnerSecret(ctx, member, owner, "X", "y"); !errors.Is(err, forge.ErrForbidden) {
		t.Errorf("user set another user's secret: %v", err)
	}
	if err := f.SetOwnerSecret(ctx, member, member, "MINE", "y"); err != nil {
		t.Errorf("user set own secret: %v", err)
	}
	for _, bad := range []string{"1ABC", "A-B", "CI", "forge_token", "MATRIX_GO", ""} {
		if err := f.SetOwnerSecret(ctx, owner, org, bad, "v"); err == nil {
			t.Errorf("accepted secret name %q", bad)
		}
	}
	if err := f.SetOwnerSecret(ctx, owner, org, "EMPTY", ""); err == nil {
		t.Error("accepted empty value")
	}
	vals, _, err := f.JobSecrets(ctx, repo, []string{"DEPLOY"})
	if err != nil || vals["DEPLOY"] != "x-value" {
		t.Errorf("org secret not inherited: %v %v", vals, err)
	}
	// HTTP: member gets 403 on the org's secrets.
	tok, _ := f.Store.CreateAccessToken(ctx, member.ID, "t")
	mine := &env{t: t, f: f, srv: e.srv, token: tok}
	if c := mine.api("GET", "/api/v1/users/acme/secrets", nil, nil); c != 403 {
		t.Errorf("member GET org secrets: %d", c)
	}
}
