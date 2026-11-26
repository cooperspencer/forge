package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// executor runs job steps in some environment.
type executor interface {
	Start(ctx context.Context, out io.Writer) error
	Exec(ctx context.Context, script string, out io.Writer) (exitCode int, err error)
	Fix(ctx context.Context) // normalise workspace ownership before reading artifacts
	Stop()
}

func envList(env map[string]string, withValues bool) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if withValues {
			out = append(out, k+"="+env[k])
		} else {
			out = append(out, k)
		}
	}
	return out
}

func exitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return -1, err
}

// ---------------------------------------------------------------- docker

// dockerExecutor starts one long-lived container per job (so state carries
// across steps) and runs each step with `docker exec`.
type dockerExecutor struct {
	bin, name, image, ws, network string
	socket                        string // host Docker socket to mount, if any
	env                           map[string]string
	started                       bool
}

func (d *dockerExecutor) cmd(ctx context.Context, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, d.bin, args...)
	// Values are passed through the docker CLI's environment (`-e NAME`),
	// so secrets never appear in process listings.
	c.Env = append(os.Environ(), envList(d.env, true)...)
	return c
}

func (d *dockerExecutor) Start(ctx context.Context, out io.Writer) error {
	// pull explicitly so progress is visible in the log
	if err := exec.CommandContext(ctx, d.bin, "image", "inspect", d.image).Run(); err != nil {
		fmt.Fprintf(out, "%spulling %s…%s\n", cDim, d.image, cReset)
		pull := exec.CommandContext(ctx, d.bin, "pull", "-q", d.image)
		pull.Stdout, pull.Stderr = out, out
		if err := pull.Run(); err != nil {
			return fmt.Errorf("pull %s: %w", d.image, err)
		}
	}
	args := []string{"run", "-d", "--name", d.name, "--label", "forge.job=1",
		"-v", d.ws + ":/workspace", "-w", "/workspace", "--entrypoint", "sh"}
	if d.network != "" {
		args = append(args, "--network", d.network)
	}
	if d.socket != "" {
		args = append(args, "-v", d.socket+":/var/run/docker.sock")
	}
	for _, k := range envList(d.env, false) {
		args = append(args, "-e", k)
	}
	args = append(args, d.image, "-c", "trap 'exit 0' TERM; while :; do sleep 3600 & wait $!; done")
	c := d.cmd(ctx, args...)
	if b, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("docker run: %v: %s", err, b)
	}
	d.started = true
	return nil
}

func (d *dockerExecutor) Exec(ctx context.Context, script string, out io.Writer) (int, error) {
	c := d.cmd(ctx, "exec", d.name, "sh", "-e", "-c", script)
	c.Stdout, c.Stderr = out, out
	// on cancellation kill the container so exec returns promptly
	c.Cancel = func() error {
		exec.Command(d.bin, "kill", d.name).Run()
		return c.Process.Kill()
	}
	c.WaitDelay = 10 * time.Second
	return exitCode(c.Run())
}

func (d *dockerExecutor) Fix(ctx context.Context) {
	if d.started {
		uid, gid := os.Getuid(), os.Getgid()
		exec.CommandContext(ctx, d.bin, "exec", d.name, "chown", "-R", fmt.Sprintf("%d:%d", uid, gid), "/workspace").Run()
	}
}

func (d *dockerExecutor) Stop() {
	if !d.started {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// files written by root inside the container: remove them from inside
	exec.CommandContext(ctx, d.bin, "exec", d.name, "sh", "-c", "rm -rf /workspace/* /workspace/.[!.]* /workspace/..?* 2>/dev/null; true").Run()
	exec.CommandContext(ctx, d.bin, "rm", "-f", d.name).Run()
}

// ---------------------------------------------------------------- shell

// shellExecutor runs steps directly on the host. No isolation: use only on
// trusted, dedicated machines (or for development).
type shellExecutor struct {
	dir string
	env map[string]string
}

func (s *shellExecutor) Start(ctx context.Context, out io.Writer) error { return nil }
func (s *shellExecutor) Fix(ctx context.Context)                        {}
func (s *shellExecutor) Stop()                                          {}

func (s *shellExecutor) Exec(ctx context.Context, script string, out io.Writer) (int, error) {
	c := exec.CommandContext(ctx, "sh", "-e", "-c", script)
	c.Dir = s.dir
	c.Env = append(hostEnv(), envList(s.env, true)...)
	c.Env = append(c.Env, "FORGE_WORKSPACE="+s.dir)
	c.Stdout, c.Stderr = out, out
	killTree(c) // cancellation also kills children (sleep, make, …)
	c.WaitDelay = 5 * time.Second
	return exitCode(c.Run())
}

// hostEnv is the runner's environment minus its own credentials and
// settings, which job steps must not see.
func hostEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "FORGE_RUNNER_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
