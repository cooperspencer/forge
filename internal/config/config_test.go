package config

import (
	"strings"
	"testing"
)

func TestEnvOverrides(t *testing.T) {
	t.Setenv("FORGE_SSH_LISTEN", ":2222")
	t.Setenv("FORGE_HTTP_LISTEN", ":8080")
	t.Setenv("FORGE_SSH_PUBLIC_ADDR", "git.example.com:2222")
	t.Setenv("FORGE_HTTP_PUBLIC_URL", "https://git.example.com/")
	t.Setenv("FORGE_ANONYMOUS_READ", "false")
	t.Setenv("FORGE_CI_STALE_SECONDS", "30")
	t.Setenv("FORGE_SSH_IDLE_TIMEOUT", "600")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.SSH.Listen != ":2222" || c.HTTP.Listen != ":8080" || c.SSH.PublicAddr != "git.example.com:2222" ||
		c.HTTP.PublicURL != "https://git.example.com" || c.AnonymousRead || c.CI.StaleSeconds != 30 || c.SSH.IdleTimeout != 600 {
		t.Fatalf("overrides not applied: %+v", c)
	}
}

func TestEnvBadValues(t *testing.T) {
	t.Setenv("FORGE_ANONYMOUS_READ", "nope")
	t.Setenv("FORGE_CI_MAX_ARTIFACT_MB", "lots")
	_, err := Load("")
	if err == nil || !strings.Contains(err.Error(), "FORGE_ANONYMOUS_READ") || !strings.Contains(err.Error(), "FORGE_CI_MAX_ARTIFACT_MB") {
		t.Fatalf("want both bad values reported, got %v", err)
	}
}
