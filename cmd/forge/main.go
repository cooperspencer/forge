// Command forge is a single binary containing the server (SSH TUI/CLI/git,
// HTTP API/git, CI orchestrator), the CI runner agent, and a local admin tool.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // schedule timezones work without system zoneinfo (e.g. Alpine images)

	"github.com/alecthomas/kong"
	"github.com/cooperspencer/forge/internal/config"
	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/logging"
	"github.com/cooperspencer/forge/internal/protect"
	"github.com/cooperspencer/forge/internal/runner"
	"github.com/cooperspencer/forge/internal/sshd"
	"github.com/cooperspencer/forge/internal/store"
	"github.com/cooperspencer/forge/internal/web"
	"github.com/rs/zerolog"
)

var version = "0.1.0-dev"

// The command line, parsed by kong. Every flag can also come from the
// environment variable named in its env tag.
type cli struct {
	Serve   serveCmd   `cmd:"" help:"Run the server (SSH + HTTP + CI orchestrator)."`
	Runner  runnerCmd  `cmd:"" help:"Run a CI runner agent."`
	Admin   adminCmd   `cmd:"" help:"Run an SSH CLI command locally as a site admin, or rekey."`
	Config  configCmd  `cmd:"" help:"Print an example configuration file."`
	Version versionCmd `cmd:"" help:"Print the version."`
	Hook    hookCmd    `cmd:"" hidden:"" help:"Run by git during pushes to protected repositories."`

	VersionFlag kong.VersionFlag `name:"version" short:"v" help:"Print the version."`
}

type serveCmd struct {
	Config string `short:"c" env:"FORGE_CONFIG" placeholder:"FILE" help:"Config file (default ./forge.yaml if present)."`
	Debug  bool   `help:"Debug logging (or set FORGE_DEBUG)."`
}

func (serveCmd) Help() string {
	return `Environment variables override config keys:
  FORGE_NAME, FORGE_DATA_DIR, FORGE_SSH_LISTEN, FORGE_SSH_PUBLIC_ADDR,
  FORGE_SSH_IDLE_TIMEOUT, FORGE_HTTP_LISTEN, FORGE_HTTP_PUBLIC_URL,
  FORGE_ALLOW_REGISTRATION, FORGE_ANONYMOUS_READ, FORGE_INITIAL_ADMIN_KEYS,
  FORGE_INITIAL_ADMIN_NAME,
  FORGE_SECRET_KEY_FILE, FORGE_SECRET_KEY, FORGE_PACKAGES_ENABLED,
  FORGE_CI_STALE_SECONDS, FORGE_CI_MAX_ARTIFACT_MB, FORGE_MIRROR_ALLOW_LOOPBACK,
  FORGE_LFS_MAX_OBJECT_MB, FORGE_LOG_FORMAT (pretty|json),
  FORGE_LOG_LEVEL (debug|info|warn|error).
"forge config" prints every option.`
}

type runnerCmd struct {
	Server       string `env:"FORGE_RUNNER_SERVER" placeholder:"URL" help:"Forge HTTP URL."`
	Token        string `env:"FORGE_RUNNER_TOKEN" help:"Runner token (fgr_…)."`
	Labels       string `env:"FORGE_RUNNER_LABELS" help:"Extra comma-separated labels; executor, os and arch are always included."`
	Concurrency  int    `env:"FORGE_RUNNER_CONCURRENCY" default:"1" help:"Number of jobs to run in parallel."`
	Workdir      string `env:"FORGE_RUNNER_WORKDIR" placeholder:"DIR" help:"Directory for job workspaces."`
	Executor     string `env:"FORGE_RUNNER_EXECUTOR" default:"docker" enum:"docker,shell" help:"docker | shell; shell runs steps on the host, no isolation."`
	Docker       string `env:"FORGE_RUNNER_DOCKER" help:"Container CLI to use: docker or podman (auto-detected)."`
	Registry     string `env:"FORGE_RUNNER_REGISTRY" placeholder:"HOST[:PORT]" help:"Registry jobs push to, if not the --server host."`
	DockerSocket string `env:"FORGE_RUNNER_DOCKER_SOCKET" placeholder:"PATH" help:"Mount this Docker socket (e.g. /var/run/docker.sock) into jobs so they can build images; gives jobs root on this host."`
	Network      string `env:"FORGE_RUNNER_NETWORK" help:"Docker network for job containers."`
	Debug        bool   `help:"Debug logging (same as --log-level debug)."`
	LogFormat    string `env:"FORGE_LOG_FORMAT" default:"pretty" help:"pretty | json."`
	LogLevel     string `env:"FORGE_LOG_LEVEL" default:"info" help:"debug | info | warn | error."`
}

func (r *runnerCmd) Validate() error {
	if r.Concurrency < 1 {
		return fmt.Errorf("--concurrency must be 1 or more")
	}
	return nil
}

type adminCmd struct {
	Config string   `short:"c" env:"FORGE_CONFIG" placeholder:"FILE" help:"Config file (default ./forge.yaml if present)."`
	Args   []string `arg:"" optional:"" passthrough:"" help:"An SSH CLI command (forge admin help lists them), or: rekey --old-key-file PATH."`
}

func (adminCmd) Help() string {
	return `Runs commands directly against the data directory with site-admin rights,
for bootstrapping and recovery without SSH access. Examples:
  forge admin user create --admin alice "ssh-ed25519 AAAA..."
  forge admin runner create builder
  forge admin rekey --old-key-file data/secret.key   (re-encrypt secrets after changing the key)`
}

type rekeyCmd struct {
	OldKeyFile string `required:"" placeholder:"PATH" help:"File holding the previous key (- for stdin)."`
}

type configCmd struct{}
type versionCmd struct{}

type hookCmd struct {
	Name string `arg:"" optional:""`
}

// exitCode ends the program with a status but no further message.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func main() {
	sshd.Version = version
	args := compatArgs(os.Args[1:])
	var c cli
	parser, err := newParser(&c)
	if err != nil {
		panic(err)
	}
	ctx, err := parser.Parse(args)
	if err != nil {
		// a short usage line and exit status 2, like the flag package did
		var pe *kong.ParseError
		if errors.As(err, &pe) && pe.Context != nil {
			pe.Context.PrintUsage(true)
			fmt.Fprintln(os.Stderr)
		}
		parser.Errorf("%s", err)
		os.Exit(2)
	}
	if err := ctx.Run(); err != nil {
		var code exitCode
		if errors.As(err, &code) {
			os.Exit(int(code))
		}
		fmt.Fprintln(os.Stderr, "forge:", err)
		os.Exit(1)
	}
}

func newParser(c *cli, opts ...kong.Option) (*kong.Kong, error) {
	return kong.New(c, append([]kong.Option{
		kong.Name("forge"),
		kong.Description("A terminal-first git forge with integrated CI/CD."),
		kong.Vars{"version": "forge " + version},
		kong.ConfigureHelp(kong.HelpOptions{Compact: true}),
	}, opts...)...)
}

// compatArgs keeps the command lines of earlier versions working: "forge"
// and "forge help [cmd]" show help, and single-dash long flags (-debug,
// -old-key-file, -concurrency 2, as Go's flag package accepted) become
// double-dash ones. Arguments passed through to "forge admin" are left alone:
// the SSH CLI parses them itself.
func compatArgs(args []string) []string {
	if len(args) == 0 {
		return []string{"--help"}
	}
	if args[0] == "help" {
		return append(args[1:], "--help")
	}
	out := append([]string{}, args...)
	end := len(out)
	if out[0] == "admin" {
		end = 1
		for end < len(out) && strings.HasPrefix(out[end], "-") { // admin's own flags
			if out[end] == "-c" || out[end] == "--config" {
				end++
			}
			end++
		}
		if end < len(out) && out[end] == "rekey" {
			end = len(out) // rekey's flags are ours
		}
	}
	for i := 1; i < min(end, len(out)); i++ {
		a := out[i]
		if a == "--" {
			break
		}
		if len(a) > 2 && a[0] == '-' && a[1] != '-' && (a[1] >= 'a' && a[1] <= 'z') {
			out[i] = "-" + a
		}
	}
	return out
}

func (configCmd) Run() error  { fmt.Print(config.Example); return nil }
func (versionCmd) Run() error { fmt.Println("forge", version); return nil }

func (h hookCmd) Run() error {
	if h.Name == "pre-receive" {
		return exitCode(protect.RunHook(os.Stdin, os.Stderr))
	}
	return nil
}

// newLogger builds the logger; -debug / FORGE_DEBUG force the debug level.
func newLogger(format, level string, debug bool) zerolog.Logger {
	if debug || os.Getenv("FORGE_DEBUG") != "" {
		level = "debug"
	}
	return logging.New(logging.Options{Format: format, Level: level})
}

func loadForge(cfgPath string, debug bool) (*forge.Forge, zerolog.Logger, error) {
	if cfgPath == "" {
		cfgPath = os.Getenv("FORGE_CONFIG")
	}
	if cfgPath == "" {
		if _, err := os.Stat("forge.yaml"); err == nil {
			cfgPath = "forge.yaml"
		}
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, zerolog.Nop(), err
	}
	log := newLogger(cfg.Log.Format, cfg.Log.Level, debug)
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, log, err
	}
	f, err := forge.New(cfg, log)
	return f, log, err
}

func (cmd *serveCmd) Run() error {
	f, log, err := loadForge(cmd.Config, cmd.Debug)
	if err != nil {
		return err
	}
	defer f.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sshSrv, err := sshd.New(f)
	if err != nil {
		return err
	}
	httpSrv := web.New(f)
	go f.CI.Loop(ctx)
	go f.ScheduleLoop(ctx)
	go f.MirrorLoop(ctx)
	if cfg := f.Cfg; cfg.Packages.Enabled {
		go f.PackageGCLoop(ctx)
	}

	errc := make(chan error, 2)
	go func() { errc <- sshSrv.ListenAndServe() }()
	go func() { errc <- httpSrv.ListenAndServe() }()
	log.Info().Str("version", version).Str("data", f.Cfg.DataDir).Str("ssh", f.Cfg.SSH.Listen).
		Str("http", f.Cfg.HTTP.Listen).Str("connect", f.Cfg.SSHCommand()).Msg("forge started")
	if n, _ := f.Store.CountUsers(ctx); n == 0 {
		log.Warn().Msg("no users yet: the first SSH key to connect and register becomes the administrator")
	}

	select {
	case <-ctx.Done():
	case err := <-errc:
		if err != nil {
			return err
		}
	}
	log.Info().Msg("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sshSrv.Shutdown(sctx)
	httpSrv.Shutdown(sctx)
	return nil
}

func (cmd *runnerCmd) Run() error {
	if err := (logging.Options{Format: cmd.LogFormat, Level: cmd.LogLevel}).Validate(); err != nil {
		return err
	}
	var ls []string
	for _, l := range strings.Split(cmd.Labels, ",") {
		if l = strings.TrimSpace(l); l != "" {
			ls = append(ls, l)
		}
	}
	r, err := runner.New(runner.Config{Server: cmd.Server, Token: cmd.Token, Labels: ls, Concurrency: cmd.Concurrency, WorkDir: cmd.Workdir,
		Executor: cmd.Executor, Docker: cmd.Docker, Registry: cmd.Registry, Network: cmd.Network, DockerSocket: cmd.DockerSocket, Version: version},
		newLogger(cmd.LogFormat, cmd.LogLevel, cmd.Debug))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return r.Run(ctx)
}

// admin runs CLI commands directly against the data directory with site-admin
// rights — for bootstrapping and recovery without SSH access.
func (cmd *adminCmd) Run() error {
	args := cmd.Args
	var rk rekeyCmd
	if len(args) > 0 && args[0] == "rekey" {
		p, err := kong.New(&rk, kong.Name("forge admin rekey"), kong.Description("Re-encrypt CI secrets and mirror credentials after changing the secret key."))
		if err != nil {
			return err
		}
		if _, err := p.Parse(args[1:]); err != nil {
			p.Errorf("%s", err)
			return exitCode(2)
		}
	}
	f, _, err := loadForge(cmd.Config, false)
	if err != nil {
		return err
	}
	defer f.Close()
	ctx := context.Background()
	if len(args) > 0 && args[0] == "rekey" {
		return exitCode(rekey(ctx, f, rk.OldKeyFile))
	}
	u := &store.User{Name: "local-admin", IsAdmin: true}
	// Use a real admin account if one exists so actions are attributed.
	if users, err := f.Store.ListUsers(ctx); err == nil {
		for _, x := range users {
			if x.IsAdmin && !x.IsOrg {
				u = x
				break
			}
		}
	}
	if u.ID == 0 && len(args) > 0 && args[0] != "user" && args[0] != "runner" && args[0] != "help" {
		return errors.New("no admin user exists yet; create one first: forge admin user create --admin <name> <ssh-key>")
	}
	c := &sshd.CLI{F: f, User: u, In: os.Stdin, Out: os.Stdout, Err: os.Stderr, Ctx: ctx, Prog: "forge admin"}
	if code := c.Run(args); code != 0 {
		return exitCode(code)
	}
	return nil
}

// rekey re-encrypts CI secrets written with an old key (e.g. the default
// data/secret.key before secret_key_file was configured) under the current key.
func rekey(ctx context.Context, f *forge.Forge, oldFile string) int {
	var old []byte
	var err error
	if oldFile == "-" {
		old, err = io.ReadAll(os.Stdin)
	} else {
		old, err = os.ReadFile(oldFile)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "forge:", err)
		return 1
	}
	n, err := f.Rekey(ctx, old)
	if err != nil {
		fmt.Fprintln(os.Stderr, "forge: rekey:", err)
		return 1
	}
	fmt.Printf("re-encrypted %d secret(s) under key %s\n", n, f.SecretKeyID())
	return 0
}
