//go:build !windows

package runner

import (
	"os/exec"
	"syscall"
)

// killTree runs the command in its own process group and makes cancellation
// kill the whole group, not just the shell.
func killTree(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
}
