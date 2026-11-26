package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cooperspencer/forge/internal/protect"
)

// TestMain lets the test binary stand in for the forge binary when git runs
// the branch protection hook (forge installs a hook that runs os.Executable).
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "hook" && os.Args[2] == "pre-receive" {
		os.Exit(protect.RunHook(os.Stdin, os.Stderr))
	}
	os.Exit(run(m))
}

// run isolates git from the developer's own configuration: no credential
// helpers (an OS keychain would remember test tokens and hand them to
// clients that should be anonymous) and no global settings or hooks.
func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "forge-e2e-git-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	cfg := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(cfg, nil, 0o600); err != nil {
		panic(err)
	}
	os.Setenv("GIT_CONFIG_GLOBAL", cfg)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return m.Run()
}
