package forge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/cooperspencer/forge/internal/ci"
	"github.com/cooperspencer/forge/internal/config"
	"github.com/cooperspencer/forge/internal/secretbox"
	"github.com/cooperspencer/forge/internal/store"
	"github.com/rs/zerolog"
)

// CI secrets exist at two levels:
//
//   - repository secrets, managed by repository admins;
//   - owner secrets, belonging to a user or organization and inherited by all
//     of its repositories, managed by that user or the organization's owners.
//
// A repository secret overrides an inherited one with the same name. Values
// are encrypted with AES-256-GCM (see package secretbox) and can never be read
// back through the TUI, CLI or API — only jobs receive them, and only the ones
// they list under `secrets:`.

const maxSecretSize = 64 << 10

// SecretInfo describes a secret without its value.
type SecretInfo struct {
	Name      string
	UpdatedAt time.Time
	// Owner is set for a secret inherited from the repository's owner.
	Owner string
	// Overridden: an inherited secret shadowed by a repository secret.
	Overridden bool
}

// loadSecretBox picks the secret-encryption key: FORGE_SECRET_KEY, then
// secret_key_file, then the data directory's secret.key.
func loadSecretBox(cfg *config.Config, fallback []byte, log zerolog.Logger) (*secretbox.Box, error) {
	keyFile := cfg.SecretKeyFile
	switch {
	case cfg.SecretKey != "":
		return secretbox.New([]byte(cfg.SecretKey))
	case keyFile != "":
		st, err := os.Stat(keyFile)
		if err != nil {
			return nil, fmt.Errorf("secret_key_file: %w (create one with: openssl rand -base64 32 > %s && chmod 600 %s)", err, keyFile, keyFile)
		}
		if st.Mode().Perm()&0o077 != 0 {
			log.Warn().Str("path", keyFile).Str("mode", st.Mode().Perm().String()).Msg("secret key file is readable by other users; chmod 600 it")
		}
		b, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, fmt.Errorf("secret_key_file: %w", err)
		}
		return secretbox.New(b)
	default:
		return secretbox.New(fallback)
	}
}

// migrateSecrets encrypts any plaintext values (from before encryption
// existed) and reports values written with a different key.
func (f *Forge) migrateSecrets(ctx context.Context) error {
	all, err := f.Store.AllSecrets(ctx)
	if err != nil {
		return err
	}
	encrypted, foreign := 0, map[string]int{}
	for _, s := range all {
		if !s.Encrypted {
			if err := f.Store.UpdateSecretValue(ctx, s.Scope, s.Name, f.box.Seal(s.Value, s.Scope.Binding(s.Name)), true); err != nil {
				return err
			}
			encrypted++
			continue
		}
		if kid, _ := secretbox.KeyID(s.Value); kid != f.box.ID() {
			foreign[kid]++
		}
	}
	if encrypted > 0 {
		if err := f.Store.Compact(ctx); err != nil {
			f.Log.Warn().Err(err).Msg("could not compact database after encrypting secrets")
		}
		f.Log.Info().Int("count", encrypted).Str("key", f.box.ID()).Msg("encrypted plaintext CI secrets")
	}
	for kid, n := range foreign {
		f.Log.Error().Int("count", n).Str("their_key", kid).Str("configured_key", f.box.ID()).
			Msg("CI secrets were encrypted with a different key; jobs using them will fail until you restore that key or run `forge admin rekey --old-key-file <path>`")
	}
	return nil
}

// Rekey re-encrypts every secret written with the old key material under the
// configured key. Values already under the configured key are left alone.
func (f *Forge) Rekey(ctx context.Context, oldMaterial []byte) (int, error) {
	old, err := secretbox.New(oldMaterial)
	if err != nil {
		return 0, err
	}
	all, err := f.Store.AllSecrets(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, s := range all {
		if kid, _ := secretbox.KeyID(s.Value); !s.Encrypted || kid != old.ID() || kid == f.box.ID() {
			continue
		}
		pt, err := old.Open(s.Value, s.Scope.Binding(s.Name))
		if err != nil {
			return n, fmt.Errorf("%s: %w", s.Name, err)
		}
		if err := f.Store.UpdateSecretValue(ctx, s.Scope, s.Name, f.box.Seal(pt, s.Scope.Binding(s.Name)), true); err != nil {
			return n, err
		}
		n++
	}
	m, err := f.rekeyMirrors(ctx, old.Open, func(ct string) bool {
		kid, _ := secretbox.KeyID(ct)
		return kid == old.ID() && kid != f.box.ID()
	})
	n += m
	if err != nil {
		return n, err
	}
	if n > 0 {
		f.Store.Compact(ctx)
	}
	return n, nil
}

// SecretKeyID is the fingerprint of the configured encryption key.
func (f *Forge) SecretKeyID() string { return f.box.ID() }

// ------------------------------------------------------------- permissions

func (f *Forge) canManageRepoSecrets(ctx context.Context, actor *store.User, r *store.Repo) bool {
	return actor != nil && f.Store.AccessLevel(ctx, actor, r) >= store.AccessAdmin
}

// CanManageOwnerSecrets: site admins, the user themself, or an org's owners.
func (f *Forge) CanManageOwnerSecrets(ctx context.Context, actor, owner *store.User) bool {
	if actor == nil || !actor.Grant.CoversOwner(owner.Name) {
		return false
	}
	if owner.IsOrg {
		return f.CanManageOrg(ctx, actor, owner)
	}
	return actor.SiteAdmin() || actor.ID == owner.ID
}

func (f *Forge) setSecret(ctx context.Context, sc store.SecretScope, name, value string) error {
	n, err := ci.NormalizeSecretName(name)
	if err != nil {
		return err
	}
	if value == "" {
		return fmt.Errorf("secret value is empty")
	}
	if len(value) > maxSecretSize {
		return fmt.Errorf("secret value too large (max %d KiB)", maxSecretSize>>10)
	}
	return f.Store.SetSecret(ctx, sc, n, f.box.Seal(value, sc.Binding(n)), true)
}

func (f *Forge) deleteSecret(ctx context.Context, sc store.SecretScope, name string) error {
	err := f.Store.DeleteSecret(ctx, sc, strings.ToUpper(strings.TrimSpace(name)))
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: no secret named %q", ErrNotFound, name)
	}
	return err
}

// ---------------------------------------------------------- repository level

func (f *Forge) SetRepoSecret(ctx context.Context, actor *store.User, r *store.Repo, name, value string) error {
	if !f.canManageRepoSecrets(ctx, actor, r) {
		return ErrForbidden
	}
	if err := f.setSecret(ctx, store.SecretScope{RepoID: r.ID}, name, value); err != nil {
		return err
	}
	f.Store.RecordEvent(ctx, r.ID, actor.ID, "settings", fmt.Sprintf("%s set secret %s", actor.Name, strings.ToUpper(strings.TrimSpace(name))))
	return nil
}

func (f *Forge) DeleteRepoSecret(ctx context.Context, actor *store.User, r *store.Repo, name string) error {
	if !f.canManageRepoSecrets(ctx, actor, r) {
		return ErrForbidden
	}
	if err := f.deleteSecret(ctx, store.SecretScope{RepoID: r.ID}, name); err != nil {
		return err
	}
	f.Store.RecordEvent(ctx, r.ID, actor.ID, "settings", fmt.Sprintf("%s removed secret %s", actor.Name, strings.ToUpper(strings.TrimSpace(name))))
	return nil
}

// RepoSecrets lists the repository's own secrets followed by those it
// inherits from its owner.
func (f *Forge) RepoSecrets(ctx context.Context, actor *store.User, r *store.Repo) ([]SecretInfo, error) {
	if !f.canManageRepoSecrets(ctx, actor, r) {
		return nil, ErrForbidden
	}
	own, err := f.Store.Secrets(ctx, store.SecretScope{RepoID: r.ID})
	if err != nil {
		return nil, err
	}
	inherited, err := f.Store.Secrets(ctx, store.SecretScope{OwnerID: r.OwnerID})
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	var out []SecretInfo
	for _, s := range own {
		have[s.Name] = true
		out = append(out, SecretInfo{Name: s.Name, UpdatedAt: s.CreatedAt})
	}
	for _, s := range inherited {
		out = append(out, SecretInfo{Name: s.Name, UpdatedAt: s.CreatedAt, Owner: r.Owner, Overridden: have[s.Name]})
	}
	return out, nil
}

// --------------------------------------------------------------- owner level

func (f *Forge) SetOwnerSecret(ctx context.Context, actor, owner *store.User, name, value string) error {
	if !f.CanManageOwnerSecrets(ctx, actor, owner) {
		return ErrForbidden
	}
	return f.setSecret(ctx, store.SecretScope{OwnerID: owner.ID}, name, value)
}

func (f *Forge) DeleteOwnerSecret(ctx context.Context, actor, owner *store.User, name string) error {
	if !f.CanManageOwnerSecrets(ctx, actor, owner) {
		return ErrForbidden
	}
	return f.deleteSecret(ctx, store.SecretScope{OwnerID: owner.ID}, name)
}

func (f *Forge) OwnerSecrets(ctx context.Context, actor, owner *store.User) ([]SecretInfo, error) {
	if !f.CanManageOwnerSecrets(ctx, actor, owner) {
		return nil, ErrForbidden
	}
	ss, err := f.Store.Secrets(ctx, store.SecretScope{OwnerID: owner.ID})
	if err != nil {
		return nil, err
	}
	out := make([]SecretInfo, 0, len(ss))
	for _, s := range ss {
		out = append(out, SecretInfo{Name: s.Name, UpdatedAt: s.CreatedAt})
	}
	return out, nil
}

// ------------------------------------------------------------------- jobs

// JobSecrets decrypts the named secrets for a job in repository r: repository
// secrets first, then the owner's. Names that are not defined anywhere are
// returned in missing; a secret that exists but cannot be decrypted is an
// error, so the job fails loudly instead of running with an empty value.
func (f *Forge) JobSecrets(ctx context.Context, r *store.Repo, names []string) (vals map[string]string, missing []string, err error) {
	vals = map[string]string{}
	if len(names) == 0 {
		return vals, nil, nil
	}
	byName := map[string]store.Secret{}
	for _, sc := range []store.SecretScope{{OwnerID: r.OwnerID}, {RepoID: r.ID}} { // repo last: it wins
		ss, err := f.Store.Secrets(ctx, sc)
		if err != nil {
			return nil, nil, err
		}
		for _, s := range ss {
			byName[s.Name] = s
		}
	}
	for _, name := range names {
		n := strings.ToUpper(name)
		s, ok := byName[n]
		if !ok {
			missing = append(missing, n)
			continue
		}
		v := s.Value
		if s.Encrypted {
			if v, err = f.box.Open(s.Value, s.Scope.Binding(s.Name)); err != nil {
				return nil, nil, fmt.Errorf("secret %s: %w", n, err)
			}
		}
		vals[n] = v
	}
	sort.Strings(missing)
	return vals, missing, nil
}
