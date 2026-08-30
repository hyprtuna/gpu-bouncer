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
)

// Version is stamped at build time with -ldflags "-X ...cli.Version=v0.1.0".
var Version = "dev"

// Env holds the process environment a command runs in, so tests can drive the
// CLI without touching the real streams.
type Env struct {
	Stdout io.Writer
	Stderr io.Writer
}

// globals are the flags accepted before the subcommand name.
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

Run "gpu-bouncer <command> --help" for command specific flags.

Configuration is read from, in order:
  /etc/gpu-bouncer/config.toml
  $XDG_CONFIG_HOME/gpu-bouncer/config.toml (defaults to ~/.config)
`

// Main runs one command and returns the process exit code.
func Main(args []string, env Env) int {
	if env.Stdout == nil {
		env.Stdout = os.Stdout
	}
	if env.Stderr == nil {
		env.Stderr = os.Stderr
	}

	var g globals
	fs := flag.NewFlagSet("gpu-bouncer", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() { fmt.Fprint(env.Stderr, usage) }
	fs.StringVar(&g.configPath, "config", "", "path to a config file")
	fs.BoolVar(&g.dryRun, "dry-run", false, "do not change anything")
	fs.BoolVar(&g.asJSON, "json", false, "emit JSON")
	fs.BoolVar(&g.verbose, "verbose", false, "include per service reasoning")
	fs.BoolVar(&g.verbose, "v", false, "include per service reasoning")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprint(env.Stderr, usage)
		return 2
	}

	if g.configPath != "" {
		// The config package reads this env var, which keeps path resolution
		// in one place rather than threading an override through every call.
		if err := os.Setenv(config.EnvConfig, g.configPath); err != nil {
			fmt.Fprintf(env.Stderr, "gpu-bouncer: %v\n", err)
			return 1
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	command, commandArgs := rest[0], rest[1:]
	err := dispatch(ctx, command, commandArgs, g, env)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, context.Canceled):
		return 0
	default:
		fmt.Fprintf(env.Stderr, "gpu-bouncer: %v\n", err)
		return 1
	}
}

func dispatch(ctx context.Context, command string, args []string, g globals, env Env) error {
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
		fmt.Fprintf(env.Stdout, "gpu-bouncer %s\n", Version)
		return nil
	case "help", "--help", "-h":
		fmt.Fprint(env.Stdout, usage)
		return nil
	default:
		fmt.Fprint(env.Stderr, usage)
		return fmt.Errorf("unknown command %q", command)
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
