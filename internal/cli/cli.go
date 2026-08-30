// Package cli implements the gpu-bouncer command line.
//
// Commands split cleanly in two. status and plan are read only, work with or
// without a daemon, and are safe to run anywhere. request, release and evict
// change state, so they are refused unless a daemon is listening: the daemon
// is the only component allowed to act, and it is the thing holding the
// config that says which services may be touched at all.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/hyprtuna/gpu-bouncer/internal/config"
	"github.com/hyprtuna/gpu-bouncer/internal/ipc"
)

// Version is stamped at build time with -ldflags "-X ...cli.Version=v0.1.0".
// When it is not, the module version from the build info is used instead;
// see resolveVersion.
var Version = "dev"

// Env holds the process environment a command runs in, so tests can drive the
// CLI without touching the real streams.
type Env struct {
	Stdout io.Writer
	Stderr io.Writer
}

// globals are the flags accepted before the subcommand name. The output flags
// and --dry-run are also accepted after it, see addOutputFlags.
type globals struct {
	configPath string
	dryRun     bool
	asJSON     bool
	verbose    bool
}

const usage = `gpu-bouncer arbitrates one GPU between local AI services.

Usage:
  gpu-bouncer [flags] <command> [arguments]

Commands:
  daemon              Run the arbitration service in the foreground.
  status              Show GPU and per service state. Read only.
  plan                Show what would happen right now, without doing it.
  request <service>   Claim priority for a service, freeing room if needed.
  release <service>   Drop a claim previously made with request.
  evict <service>     Free one service now.
  version             Print the version.

Flags:
  --config <path>     Use this config file instead of the search path.
  --dry-run           Never change anything; report what would have happened.
  --json              Emit JSON instead of text.
  -v, --verbose       Include per service reasoning in text output.
  --version           Print the version, the same as the version command.

--dry-run, --json and --verbose are also accepted after the command name.
Run "gpu-bouncer <command> --help" for command specific flags.

Configuration is read from, in order:
  /etc/gpu-bouncer/config.toml
  $XDG_CONFIG_HOME/gpu-bouncer/config.toml (defaults to ~/.config)
`

// exitCode is returned by a command that has already reported its outcome
// and only needs the process to exit non zero. Main prints nothing for it.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit %d", int(e)) }

// usageError is a command line that names no command or an unknown one. It
// exits 2, and with --json it is reported as JSON like every other error.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// usageFail reports a usageError once and returns 2.
func usageFail(env Env, g *globals, err usageError) int {
	if g.asJSON {
		if werr := writeJSON(env, ipc.Response{OK: false, Error: err.msg}); werr != nil {
			fmt.Fprintf(env.Stderr, "gpu-bouncer: %v\n", werr)
		}
		return 2
	}
	fmt.Fprint(env.Stderr, usage)
	fmt.Fprintf(env.Stderr, "gpu-bouncer: %s\n", err.msg)
	return 2
}

// Main runs one command and returns the process exit code.
func Main(args []string, env Env) int {
	if env.Stdout == nil {
		env.Stdout = os.Stdout
	}
	if env.Stderr == nil {
		env.Stderr = os.Stderr
	}

	g := &globals{}
	fs := flag.NewFlagSet("gpu-bouncer", flag.ContinueOnError)
	// The flag package prints every parse error itself, followed by a usage
	// dump, before returning the error. Both are silenced here so that an
	// error is reported exactly once, by fail, and --help is answered once,
	// by the usage block below.
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	fs.StringVar(&g.configPath, "config", "", "path to a config file")
	fs.BoolVar(&g.dryRun, "dry-run", false, "do not change anything")
	fs.BoolVar(&g.asJSON, "json", false, "emit JSON")
	fs.BoolVar(&g.verbose, "verbose", false, "include per service reasoning")
	fs.BoolVar(&g.verbose, "v", false, "include per service reasoning")
	showVersion := fs.Bool("version", false, "print the version")

	if err := fs.Parse(args); err != nil {
		// Asking for help is not a usage error. The release workflow runs
		// "gpu-bouncer --help" under bash -e to prove the binary it is about
		// to publish actually runs, so a non zero exit here would block every
		// release.
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(env.Stdout, usage)
			return 0
		}
		return fail(env, g, err)
	}
	// Global flags are accepted before the command name only. Everything from
	// the command name onward belongs to the subcommand, which does its own
	// order independent parsing.
	rest := fs.Args()
	if *showVersion {
		if err := runVersion(nil, g, env); err != nil {
			return fail(env, g, err)
		}
		return 0
	}
	if len(rest) == 0 {
		return usageFail(env, g, usageError{"no command given"})
	}

	if g.configPath != "" {
		// The config package reads this env var, which keeps path resolution
		// in one place rather than threading an override through every call.
		if err := os.Setenv(config.EnvConfig, g.configPath); err != nil {
			return fail(env, g, err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	command, commandArgs := rest[0], rest[1:]
	err := dispatch(ctx, command, commandArgs, g, env)
	var (
		code   exitCode
		misuse usageError
	)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, context.Canceled):
		return 0
	case errors.As(err, &code):
		return int(code)
	case errors.As(err, &misuse):
		return usageFail(env, g, misuse)
	default:
		return fail(env, g, err)
	}
}

// fail reports an error once and returns the exit code. With --json the
// report is a JSON object on stdout, so a script parsing the output never
// has to fall back to reading stderr.
func fail(env Env, g *globals, err error) int {
	if g.asJSON {
		if werr := writeJSON(env, ipc.Response{OK: false, Error: err.Error()}); werr != nil {
			fmt.Fprintf(env.Stderr, "gpu-bouncer: %v\n", werr)
		}
		return 1
	}
	fmt.Fprintf(env.Stderr, "gpu-bouncer: %v\n", err)
	return 1
}

func dispatch(ctx context.Context, command string, args []string, g *globals, env Env) error {
	switch command {
	case "daemon":
		return runDaemon(ctx, args, g, env)
	case "status":
		return runStatus(ctx, args, g, env)
	case "plan":
		return runPlan(ctx, args, g, env)
	case "request":
		return runRequest(ctx, args, g, env)
	case "release":
		return runReleaseClaim(ctx, args, g, env)
	case "evict":
		return runEvict(ctx, args, g, env)
	case "version":
		return runVersion(args, g, env)
	case "help", "--help", "-h":
		fmt.Fprint(env.Stdout, usage)
		return nil
	default:
		return usageError{fmt.Sprintf("unknown command %q", command)}
	}
}

// loadConfig reads the configuration and reports where it came from, so that
// an unexpectedly inert gpu-bouncer is easy to diagnose.
func loadConfig() (config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}
