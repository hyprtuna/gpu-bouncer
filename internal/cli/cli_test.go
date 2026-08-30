package cli

import (
	"bytes"
	"flag"
	"strings"
	"testing"
)

// Flags must be accepted before or after the positional arguments, because
// "gpu-bouncer request assistant --need-mib 6144" is the order people type and
// the flag package alone stops at the first positional.
func TestParseArgsAcceptsFlagsInAnyPosition(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		wantPositional []string
		wantNeed       uint64
		wantDry        bool
	}{
		{
			name:           "flags after the positional",
			args:           []string{"assistant", "--need-mib", "6144"},
			wantPositional: []string{"assistant"},
			wantNeed:       6144,
		},
		{
			name:           "flags before the positional",
			args:           []string{"--need-mib", "6144", "assistant"},
			wantPositional: []string{"assistant"},
			wantNeed:       6144,
		},
		{
			name:           "flags on both sides",
			args:           []string{"--need-mib=2048", "assistant", "--dry-run"},
			wantPositional: []string{"assistant"},
			wantNeed:       2048,
			wantDry:        true,
		},
		{
			name:           "no flags at all",
			args:           []string{"assistant"},
			wantPositional: []string{"assistant"},
		},
		{
			name:           "nothing at all",
			args:           nil,
			wantPositional: nil,
		},
		{
			// A flag value that looks like a service name must stay with its
			// flag rather than being collected as a positional.
			name:           "a value that looks positional",
			args:           []string{"--need-mib", "6144", "assistant", "extra"},
			wantPositional: []string{"assistant", "extra"},
			wantNeed:       6144,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			fs := newFlagSet("request", Env{Stderr: &stderr})
			need := fs.Uint64("need-mib", 0, "")
			dry := fs.Bool("dry-run", false, "")

			positional, err := parseArgs(fs, tt.args)
			if err != nil {
				t.Fatalf("parseArgs: %v", err)
			}
			if len(positional) != len(tt.wantPositional) {
				t.Fatalf("positional = %v, want %v", positional, tt.wantPositional)
			}
			for i := range tt.wantPositional {
				if positional[i] != tt.wantPositional[i] {
					t.Fatalf("positional = %v, want %v", positional, tt.wantPositional)
				}
			}
			if *need != tt.wantNeed {
				t.Errorf("need-mib = %d, want %d", *need, tt.wantNeed)
			}
			if *dry != tt.wantDry {
				t.Errorf("dry-run = %v, want %v", *dry, tt.wantDry)
			}
		})
	}
}

func TestParseArgsReportsBadFlags(t *testing.T) {
	var stderr bytes.Buffer
	fs := newFlagSet("request", Env{Stderr: &stderr})
	fs.Uint64("need-mib", 0, "")
	if _, err := parseArgs(fs, []string{"assistant", "--nonsense"}); err == nil {
		t.Fatal("parseArgs accepted an unknown flag")
	}
}

func TestParseArgsHelpIsNotAnError(t *testing.T) {
	var stderr bytes.Buffer
	fs := newFlagSet("request", Env{Stderr: &stderr})
	_, err := parseArgs(fs, []string{"--help"})
	if err != flag.ErrHelp {
		t.Fatalf("parseArgs(--help) = %v, want flag.ErrHelp so the command exits 0", err)
	}
}

func TestOneService(t *testing.T) {
	if _, err := oneService(nil, "request"); err == nil {
		t.Error("oneService accepted no arguments")
	}
	if _, err := oneService([]string{"a", "b"}, "request"); err == nil {
		t.Error("oneService accepted two service names")
	}
	got, err := oneService([]string{"ollama"}, "request")
	if err != nil || got != "ollama" {
		t.Errorf("oneService = %q, %v; want ollama, nil", got, err)
	}
}

func TestMainVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Main([]string{"version"}, Env{Stdout: &stdout, Stderr: &stderr}); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "gpu-bouncer") {
		t.Errorf("stdout = %q, want the version line", stdout.String())
	}
}

func TestMainUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Main([]string{"obliterate"}, Env{Stdout: &stdout, Stderr: &stderr}); code == 0 {
		t.Error("an unknown command exited 0")
	}
	if !strings.Contains(stderr.String(), "Usage") {
		t.Errorf("stderr = %q, want the usage text", stderr.String())
	}
}

func TestMainNoArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Main(nil, Env{Stdout: &stdout, Stderr: &stderr}); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

// The mutating commands must refuse rather than act when no daemon answers,
// and must say how to start one.
func TestMutatingCommandsNeedADaemon(t *testing.T) {
	t.Setenv("GPU_BOUNCER_SOCKET", "/nonexistent/gpu-bouncer.sock")
	for _, args := range [][]string{
		{"request", "ollama"},
		{"release", "ollama"},
		{"evict", "ollama"},
	} {
		var stdout, stderr bytes.Buffer
		code := Main(args, Env{Stdout: &stdout, Stderr: &stderr})
		if code == 0 {
			t.Errorf("%v exited 0 with no daemon running", args)
		}
		if !strings.Contains(stderr.String(), "daemon") {
			t.Errorf("%v stderr = %q, want it to mention the daemon", args, stderr.String())
		}
	}
}

// The release workflow runs "gpu-bouncer --help" under bash -e to prove the
// binary it is about to publish actually runs. A non zero exit there would
// block every release, so the exit code is pinned.
func TestHelpExitsZero(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		var stdout, stderr bytes.Buffer
		code := Main(args, Env{Stdout: &stdout, Stderr: &stderr})
		if code != 0 {
			t.Errorf("%v exit code = %d, want 0 (stderr: %s)", args, code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "Usage") {
			t.Errorf("%v printed no usage to stdout", args)
		}
	}
}

// --json and --verbose are global, but people type them after the command.
func TestOutputFlagsAcceptedAfterTheCommand(t *testing.T) {
	t.Setenv("GPU_BOUNCER_CONFIG", "/nonexistent/gpu-bouncer-does-not-exist.toml")
	for _, args := range [][]string{
		{"status", "--json"},
		{"plan", "--json"},
		{"status", "--verbose"},
		{"plan", "-v"},
	} {
		var stdout, stderr bytes.Buffer
		Main(args, Env{Stdout: &stdout, Stderr: &stderr})
		// The config is missing so the command fails, but it must fail on the
		// config rather than on the flag.
		if strings.Contains(stderr.String(), "flag provided but not defined") {
			t.Errorf("%v rejected the flag: %s", args, stderr.String())
		}
	}
}

func TestStatusRejectsStrayArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Main([]string{"status", "ollama"}, Env{Stdout: &stdout, Stderr: &stderr}); code == 0 {
		t.Error("status accepted a stray argument")
	}
	if !strings.Contains(stderr.String(), "takes no arguments") {
		t.Errorf("stderr = %q, want it to say status takes no arguments", stderr.String())
	}
}
