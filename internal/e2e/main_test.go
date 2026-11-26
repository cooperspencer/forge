package e2e

import (
	"os"
	"testing"

	"github.com/cooperspencer/forge/internal/protect"
)

// TestMain lets the test binary stand in for the forge binary when git runs
// the branch protection hook (forge installs a hook that runs os.Executable).
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "hook" && os.Args[2] == "pre-receive" {
		os.Exit(protect.RunHook(os.Stdin, os.Stderr))
	}
	os.Exit(m.Run())
}
