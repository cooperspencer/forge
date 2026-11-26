// Package config loads server configuration from a YAML file with
// environment-variable overrides (FORGE_*).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cooperspencer/forge/internal/logging"
	"gopkg.in/yaml.v3"
)

type Config struct {
	// Name is shown in the TUI header.
	Name string `yaml:"name"`
	// DataDir holds the database, repositories, logs, artifacts and host keys.
	DataDir string `yaml:"data_dir"`

	SSH struct {
		Listen string `yaml:"listen"`
		// PublicAddr is the host[:port] users clone from, e.g. git.example.com:23231
		PublicAddr string `yaml:"public_addr"`
		// IdleTimeout in seconds for interactive sessions (0 = none).
		IdleTimeout int `yaml:"idle_timeout"`
	} `yaml:"ssh"`

	HTTP struct {
		Listen string `yaml:"listen"`
		// PublicURL is the externally reachable base URL (clone URLs, runner API).
		PublicURL string `yaml:"public_url"`
	} `yaml:"http"`

	// AllowRegistration lets unknown SSH keys create an account from the TUI.
	AllowRegistration bool `yaml:"allow_registration"`
	// AnonymousRead allows unauthenticated access to public repositories and
	// packages. When false (and registration is off), unknown SSH keys are
	// refused at login and every HTTP request needs a token.
	AnonymousRead bool `yaml:"anonymous_read"`
	// InitialAdminKeys are authorized_keys lines granted an admin account
	// (InitialAdminName) on first start.
	InitialAdminKeys []string `yaml:"initial_admin_keys"`
	// InitialAdminName is the name of that account (default "admin"). It can
	// be renamed later: ssh host user rename NEW, or Account tab → R.
	InitialAdminName string `yaml:"initial_admin_name"`

	// SecretKeyFile holds the key that encrypts CI secrets in the database.
	// Keep it outside data_dir (and out of backups of it) so a copied database
	// or backup does not reveal secrets. Defaults to <data_dir>/secret.key.
	// FORGE_SECRET_KEY (the key itself) takes precedence over the file.
	SecretKeyFile string `yaml:"secret_key_file"`
	SecretKey     string `yaml:"-"`

	Log struct {
		// Format: pretty (human-readable, colored on a terminal) or json.
		Format string `yaml:"format"`
		// Level: debug, info, warn or error.
		Level string `yaml:"level"`
	} `yaml:"log"`

	Packages struct {
		// Enabled serves the package registry: container images at /v2/ and
		// generic files at /api/packages/.
		Enabled bool `yaml:"enabled"`
	} `yaml:"packages"`

	CI struct {
		// StaleSeconds: running jobs without heartbeat for this long are failed.
		StaleSeconds int `yaml:"stale_seconds"`
		// MaxArtifactMB caps a single artifact upload.
		MaxArtifactMB int `yaml:"max_artifact_mb"`
	} `yaml:"ci"`

	Mirror struct {
		// AllowLoopback lets mirrors use remotes on this machine (localhost).
		// Off by default so a mirror can't reach services bound to loopback.
		AllowLoopback bool `yaml:"allow_loopback"`
	} `yaml:"mirror"`

	LFS struct {
		// MaxObjectMB caps a single Git LFS object (0 = no limit).
		MaxObjectMB int `yaml:"max_object_mb"`
	} `yaml:"lfs"`
}

func Default() *Config {
	c := &Config{Name: "Forge", DataDir: "./data", AnonymousRead: true, InitialAdminName: "admin"}
	c.SSH.Listen = ":23231"
	c.SSH.PublicAddr = "localhost:23231"
	c.HTTP.Listen = ":23232"
	c.HTTP.PublicURL = "http://localhost:23232"
	c.CI.StaleSeconds = 90
	c.Packages.Enabled = true
	c.Log.Format, c.Log.Level = "pretty", "info"
	c.CI.MaxArtifactMB = 256
	c.LFS.MaxObjectMB = 5120
	return c
}

// Load reads path (if it exists) on top of defaults, then applies env overrides.
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err == nil {
			if err := yaml.Unmarshal(b, c); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
		}
	}
	env := func(k string, dst *string) {
		if v := os.Getenv(k); v != "" {
			*dst = v
		}
	}
	env("FORGE_NAME", &c.Name)
	env("FORGE_DATA_DIR", &c.DataDir)
	env("FORGE_SSH_LISTEN", &c.SSH.Listen)
	env("FORGE_SSH_PUBLIC_ADDR", &c.SSH.PublicAddr)
	env("FORGE_HTTP_LISTEN", &c.HTTP.Listen)
	env("FORGE_HTTP_PUBLIC_URL", &c.HTTP.PublicURL)
	env("FORGE_LOG_FORMAT", &c.Log.Format)
	env("FORGE_LOG_LEVEL", &c.Log.Level)
	env("FORGE_SECRET_KEY_FILE", &c.SecretKeyFile)
	env("FORGE_SECRET_KEY", &c.SecretKey)
	env("FORGE_INITIAL_ADMIN_NAME", &c.InitialAdminName)
	var bad []string
	envBool := func(k string, dst *bool) {
		if v := os.Getenv(k); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				bad = append(bad, fmt.Sprintf("%s=%q: want true or false", k, v))
				return
			}
			*dst = b
		}
	}
	envInt := func(k string, dst *int) {
		if v := os.Getenv(k); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				bad = append(bad, fmt.Sprintf("%s=%q: want a non-negative number", k, v))
				return
			}
			*dst = n
		}
	}
	envBool("FORGE_ALLOW_REGISTRATION", &c.AllowRegistration)
	envBool("FORGE_ANONYMOUS_READ", &c.AnonymousRead)
	envBool("FORGE_PACKAGES_ENABLED", &c.Packages.Enabled)
	envInt("FORGE_SSH_IDLE_TIMEOUT", &c.SSH.IdleTimeout)
	envInt("FORGE_CI_STALE_SECONDS", &c.CI.StaleSeconds)
	envInt("FORGE_CI_MAX_ARTIFACT_MB", &c.CI.MaxArtifactMB)
	envInt("FORGE_LFS_MAX_OBJECT_MB", &c.LFS.MaxObjectMB)
	envBool("FORGE_MIRROR_ALLOW_LOOPBACK", &c.Mirror.AllowLoopback)
	if len(bad) > 0 {
		return nil, fmt.Errorf("environment: %s", strings.Join(bad, "; "))
	}
	if v := os.Getenv("FORGE_INITIAL_ADMIN_KEYS"); v != "" {
		for _, k := range strings.Split(v, "\n") {
			if k = strings.TrimSpace(k); k != "" {
				c.InitialAdminKeys = append(c.InitialAdminKeys, k)
			}
		}
	}
	c.HTTP.PublicURL = strings.TrimRight(c.HTTP.PublicURL, "/")
	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		return nil, err
	}
	c.DataDir = abs
	if err := (logging.Options{Format: c.Log.Format, Level: c.Log.Level}).Validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return c, nil
}

func (c *Config) Path(parts ...string) string {
	return filepath.Join(append([]string{c.DataDir}, parts...)...)
}

// SSHCloneURL returns the clone URL for owner/name over SSH.
func (c *Config) SSHCloneURL(full string) string {
	host, port, ok := strings.Cut(c.SSH.PublicAddr, ":")
	if !ok || port == "22" {
		return fmt.Sprintf("ssh://%s/%s.git", host, full)
	}
	return fmt.Sprintf("ssh://%s:%s/%s.git", host, port, full)
}

func (c *Config) HTTPCloneURL(full string) string {
	return c.HTTP.PublicURL + "/" + full + ".git"
}

// SSHCommand returns the ssh invocation for the TUI / CLI.
func (c *Config) SSHCommand() string {
	host, port, ok := strings.Cut(c.SSH.PublicAddr, ":")
	if !ok || port == "22" {
		return "ssh " + host
	}
	return fmt.Sprintf("ssh -p %s %s", port, host)
}

const Example = `# forge.yaml — all keys optional
name: Forge
data_dir: ./data
ssh:
  listen: ":23231"
  public_addr: "git.example.com:23231"
http:
  listen: ":23232"
  public_url: "https://git.example.com"
# Let unknown SSH keys create an account from the TUI.
allow_registration: false
# Let clients without a registered key / token read public repositories and
# packages. With false (and registration off) the SSH server refuses unknown
# keys at login, and HTTP requires a token for everything.
anonymous_read: true
initial_admin_keys:
  - "ssh-ed25519 AAAA... you@laptop"
initial_admin_name: admin   # rename later with: ssh host user rename NEW
# Key that encrypts CI secrets at rest; keep it outside data_dir.
# Create with: openssl rand -base64 32 > /etc/forge/secret.key && chmod 600 /etc/forge/secret.key
# Default: <data_dir>/secret.key. FORGE_SECRET_KEY overrides with the key itself.
# secret_key_file: /etc/forge/secret.key
packages:
  enabled: true          # container registry at /v2/, generic files at /api/packages/
log:
  format: pretty         # pretty (human-readable) or json (one object per line)
  level: info            # debug, info, warn or error
ci:
  stale_seconds: 90
  max_artifact_mb: 256
mirror:
  allow_loopback: false  # let mirrors use remotes on this machine (localhost)
lfs:
  max_object_mb: 5120    # largest Git LFS object accepted (0 = no limit)
`

// RegistryHost is the host[:port] of the container registry (the HTTP public
// URL without scheme), as used in image references: <host>/owner/name:tag.
func (c *Config) RegistryHost() string {
	h := c.HTTP.PublicURL
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	return strings.TrimRight(h, "/")
}
