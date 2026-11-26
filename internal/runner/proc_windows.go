//go:build windows

package runner

import (
	"os/exec"
	"strconv"
)

// killTree makes cancellation kill the process and its descendants.
func killTree(c *exec.Cmd) {
	c.Cancel = func() error {
		if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(c.Process.Pid)).Run(); err != nil {
			return c.Process.Kill()
		}
		return nil
	}
}
