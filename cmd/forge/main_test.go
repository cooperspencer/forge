package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

// parse runs the real command line through kong without executing it.
func parse(t *testing.T, args ...string) (*cli, string, error) {
	t.Helper()
	var c cli
	var out bytes.Buffer
	p, err := newParser(&c, kong.Writers(&out, &out), kong.Exit(func(int) { panic("exit") }))
	if err != nil {
		t.Fatal(err)
	}
	var ctx *kong.Context
	func() {
		defer func() {
			if r := recover(); r != nil && r != "exit" {
				panic(r)
			}
		}()
		ctx, err = p.Parse(compatArgs(args))
	}()
	if ctx == nil {
		return &c, out.String(), err
	}
	return &c, ctx.Command(), err
}

func TestCommandLine(t *testing.T) {
	c, cmd, err := parse(t, "serve", "-c", "/etc/forge/forge.yaml", "-debug")
	if err != nil || cmd != "serve" || c.Serve.Config != "/etc/forge/forge.yaml" || !c.Serve.Debug {
		t.Errorf("serve: %v %q %+v", err, cmd, c.Serve)
	}
	// the flag package's single-dash long flags still work, as does --flag=value
	c, _, err = parse(t, "runner", "-server", "http://x", "--token=fgr_1", "-concurrency", "2", "-labels", "gpu")
	if err != nil || c.Runner.Server != "http://x" || c.Runner.Token != "fgr_1" || c.Runner.Concurrency != 2 || c.Runner.Labels != "gpu" ||
		c.Runner.Executor != "docker" || c.Runner.LogFormat != "pretty" {
		t.Errorf("runner: %v %+v", err, c.Runner)
	}
	if _, _, err := parse(t, "runner", "--executor", "vm"); err == nil {
		t.Error("unknown executor accepted")
	}
	if _, _, err := parse(t, "runner", "--concurrency", "0"); err == nil {
		t.Error("concurrency 0 accepted")
	}
	// admin passes everything after its own flags to the SSH CLI untouched
	c, cmd, err = parse(t, "admin", "-c", "f.yaml", "user", "create", "-admin", "alice", "ssh-ed25519 AAAA")
	if err != nil || cmd != "admin <args>" || c.Admin.Config != "f.yaml" || !reflect.DeepEqual(c.Admin.Args, []string{"user", "create", "-admin", "alice", "ssh-ed25519 AAAA"}) {
		t.Errorf("admin: %v %q %+v", err, cmd, c.Admin)
	}
	if c, _, err := parse(t, "admin", "rekey", "-old-key-file", "data/secret.key"); err != nil || !reflect.DeepEqual(c.Admin.Args, []string{"rekey", "--old-key-file", "data/secret.key"}) {
		t.Errorf("admin rekey: %v %+v", err, c.Admin)
	}
	if c, cmd, err := parse(t, "hook", "pre-receive"); err != nil || cmd != "hook <name>" || c.Hook.Name != "pre-receive" {
		t.Errorf("hook: %v %q", err, cmd)
	}
	for _, args := range [][]string{{}, {"help"}, {"--help"}} {
		if _, out, _ := parse(t, args...); !strings.Contains(out, "Usage: forge <command>") || !strings.Contains(out, "runner") || strings.Contains(out, "hook") {
			t.Errorf("help for %v:\n%s", args, out)
		}
	}
	if _, out, _ := parse(t, "help", "serve"); !strings.Contains(out, "Usage: forge serve") || !strings.Contains(out, "FORGE_DATA_DIR") {
		t.Errorf("help serve:\n%s", out)
	}
	if _, out, _ := parse(t, "--version"); !strings.Contains(out, "forge "+version) {
		t.Errorf("--version: %q", out)
	}
	if _, _, err := parse(t, "frobnicate"); err == nil {
		t.Error("unknown command accepted")
	}
}

func TestRunnerEnv(t *testing.T) {
	t.Setenv("FORGE_RUNNER_SERVER", "https://forge.example")
	t.Setenv("FORGE_RUNNER_CONCURRENCY", "3")
	t.Setenv("FORGE_RUNNER_EXECUTOR", "shell")
	t.Setenv("FORGE_LOG_FORMAT", "json")
	c, _, err := parse(t, "runner", "--token", "fgr_x")
	if err != nil || c.Runner.Server != "https://forge.example" || c.Runner.Concurrency != 3 || c.Runner.Executor != "shell" || c.Runner.LogFormat != "json" {
		t.Errorf("runner from env: %v %+v", err, c.Runner)
	}
	// flags win over the environment
	if c, _, _ := parse(t, "runner", "--server", "http://flag"); c.Runner.Server != "http://flag" {
		t.Errorf("flag over env: %q", c.Runner.Server)
	}
	t.Setenv("FORGE_RUNNER_CONCURRENCY", "lots")
	if _, _, err := parse(t, "runner"); err == nil {
		t.Error("FORGE_RUNNER_CONCURRENCY=lots accepted")
	}
}

func TestCompatArgs(t *testing.T) {
	for in, want := range map[string]string{
		"serve -c x -debug":                   "serve -c x --debug",
		"runner -server=http://x -labels a,b": "runner --server=http://x --labels a,b",
		"admin -c x repo create -private a/b": "admin -c x repo create -private a/b",
		"admin rekey --old-key-file k":        "admin rekey --old-key-file k",
		"runner -- -debug":                    "runner -- -debug",
		"help runner":                         "runner --help",
	} {
		if got := strings.Join(compatArgs(strings.Fields(in)), " "); got != want {
			t.Errorf("compatArgs(%q) = %q, want %q", in, got, want)
		}
	}
}
