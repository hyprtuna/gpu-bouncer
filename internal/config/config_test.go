package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

const systemLayer = `
[policy]
vram_floor_mib = 1024
reactive = true
poll_interval = "10s"

[[service]]
name = "ollama"
adapter = "ollama"
endpoint = "http://127.0.0.1:11434"
priority = 50
allow_stop = true

[[service]]
name = "comfyui"
adapter = "comfyui"
endpoint = "http://127.0.0.1:8188"
priority = 20
`

// The user layer must be able to retune one key of one service without
// restating the rest, and must leave sibling blocks alone. This is a
// regression test: toml.MetaData reports [[service]] keys without their index,
// so a merge driven by IsDefined silently applied nothing at all.
func TestLoadFromMergesUserLayerPerKey(t *testing.T) {
	dir := t.TempDir()
	sys := writeFile(t, dir, "system.toml", systemLayer)
	user := writeFile(t, dir, "user.toml", `
[policy]
vram_floor_mib = 2048

[[service]]
name = "ollama"
allow_stop = false

[[service]]
name = "sdnext"
adapter = "systemd-unit"
unit = "sdnext.service"
priority = 10
`)

	cfg, err := LoadFrom([]string{sys, user})
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}

	if got, want := cfg.Policy.VRAMFloorMiB, uint64(2048); got != want {
		t.Errorf("vram_floor_mib = %d, want %d (user layer overrides)", got, want)
	}
	if !cfg.Policy.Reactive {
		t.Error("reactive = false, want true (system layer survives)")
	}
	if got, want := cfg.Policy.PollInterval.D(), 10*time.Second; got != want {
		t.Errorf("poll_interval = %s, want %s (system layer survives)", got, want)
	}

	ollama, ok := cfg.Service("ollama")
	if !ok {
		t.Fatal("service ollama missing")
	}
	if ollama.AllowStop {
		t.Error("ollama.allow_stop = true, want false (user layer overrides)")
	}
	if got, want := ollama.Priority, 50; got != want {
		t.Errorf("ollama.priority = %d, want %d (unset in user layer, must survive)", got, want)
	}
	if got, want := ollama.Endpoint, "http://127.0.0.1:11434"; got != want {
		t.Errorf("ollama.endpoint = %q, want %q", got, want)
	}

	comfy, ok := cfg.Service("comfyui")
	if !ok {
		t.Fatal("service comfyui missing")
	}
	if comfy.Priority != 20 || comfy.AllowStop {
		t.Errorf("comfyui untouched block changed: %+v", comfy)
	}

	sdnext, ok := cfg.Service("sdnext")
	if !ok {
		t.Fatal("service sdnext missing, user layer must be able to add services")
	}
	if got, want := sdnext.Unit, "sdnext.service"; got != want {
		t.Errorf("sdnext.unit = %q, want %q", got, want)
	}

	if got, want := len(cfg.Sources), 2; got != want {
		t.Errorf("Sources = %v, want %d entries", cfg.Sources, want)
	}
}

func TestLoadFromSkipsMissingFiles(t *testing.T) {
	dir := t.TempDir()
	sys := writeFile(t, dir, "system.toml", systemLayer)
	cfg, err := LoadFrom([]string{filepath.Join(dir, "nope.toml"), sys})
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got, want := len(cfg.Services), 2; got != want {
		t.Fatalf("len(Services) = %d, want %d", got, want)
	}
	if got, want := len(cfg.Sources), 1; got != want {
		t.Errorf("Sources = %v, want %d entry", cfg.Sources, want)
	}
}

func TestLoadFromNoFilesIsInert(t *testing.T) {
	cfg, err := LoadFrom([]string{filepath.Join(t.TempDir(), "absent.toml")})
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if len(cfg.Services) != 0 {
		t.Errorf("Services = %v, want none: with no config nothing may be acted on", cfg.Services)
	}
	if cfg.Policy.Reactive {
		t.Error("reactive defaults to true, want false: acting must be opt in")
	}
}

func TestValidateRejects(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "unknown adapter",
			body:    "[[service]]\nname = \"x\"\nadapter = \"telepathy\"\n",
			wantErr: "unknown adapter",
		},
		{
			name:    "unknown key",
			body:    "[policy]\nvram_floor_mb = 512\n",
			wantErr: "unknown key",
		},
		{
			name:    "http adapter without endpoint",
			body:    "[[service]]\nname = \"x\"\nadapter = \"ollama\"\n",
			wantErr: "requires an endpoint",
		},
		{
			name:    "http adapter with a unit",
			body:    "[[service]]\nname = \"x\"\nadapter = \"ollama\"\nendpoint = \"http://h:1\"\nunit = \"u.service\"\n",
			wantErr: "does not take a unit",
		},
		{
			name:    "systemd adapter without unit",
			body:    "[[service]]\nname = \"x\"\nadapter = \"systemd-unit\"\n",
			wantErr: "requires a unit",
		},
		{
			name:    "endpoint with no scheme",
			body:    "[[service]]\nname = \"x\"\nadapter = \"ollama\"\nendpoint = \"127.0.0.1:11434\"\n",
			wantErr: "must use http or https",
		},
		{
			name:    "duplicate service name",
			body:    "[[service]]\nname = \"x\"\nadapter = \"systemd-unit\"\nunit = \"u\"\n\n[[service]]\nname = \"x\"\nadapter = \"systemd-unit\"\nunit = \"v\"\n",
			wantErr: "defined twice",
		},
		{
			name:    "bad service name",
			body:    "[[service]]\nname = \"has space\"\nadapter = \"systemd-unit\"\nunit = \"u\"\n",
			wantErr: "name must match",
		},
		{
			name:    "default_workload naming nothing",
			body:    "[policy]\ndefault_workload = \"ghost\"\n",
			wantErr: "names no configured service",
		},
		{
			name:    "bad duration",
			body:    "[policy]\npoll_interval = \"soon\"\n",
			wantErr: "invalid duration",
		},
		{
			// A password in the URL would be sent as Basic auth and echoed in
			// every error string. The message points at the supported way.
			name:    "endpoint with userinfo",
			body:    "[[service]]\nname = \"x\"\nadapter = \"llama-swap\"\nendpoint = \"http://u:p@127.0.0.1:9292\"\n",
			wantErr: "GPU_BOUNCER_LLAMA_SWAP_API_KEY",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeFile(t, dir, "c.toml", tt.body)
			_, err := LoadFrom([]string{path})
			if err == nil {
				t.Fatalf("LoadFrom accepted invalid config, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateFillsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "c.toml", "[[service]]\nname = \"x\"\nadapter = \"ollama\"\nendpoint = \"http://127.0.0.1:11434/\"\n")
	cfg, err := LoadFrom([]string{path})
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	svc := cfg.Services[0]
	if got, want := svc.Timeout.D(), DefaultServiceTimeout; got != want {
		t.Errorf("timeout = %s, want %s", got, want)
	}
	if got, want := svc.DrainTimeout.D(), DefaultDrainTimeout; got != want {
		t.Errorf("drain_timeout = %s, want %s", got, want)
	}
	if got, want := svc.Endpoint, "http://127.0.0.1:11434"; got != want {
		t.Errorf("endpoint = %q, want %q (trailing slash trimmed)", got, want)
	}
	if svc.AllowStop {
		t.Error("allow_stop defaults to true, want false: process level action must be opt in")
	}
}

func TestSearchPathsHonoursOverride(t *testing.T) {
	t.Setenv(EnvConfig, "/somewhere/only.toml")
	got := SearchPaths()
	if len(got) != 1 || got[0] != "/somewhere/only.toml" {
		t.Fatalf("SearchPaths = %v, want the override alone", got)
	}
}

func TestSearchPathsUsesXDG(t *testing.T) {
	t.Setenv(EnvConfig, "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	got := SearchPaths()
	want := []string{"/etc/gpu-bouncer/config.toml", "/xdg/gpu-bouncer/config.toml"}
	if len(got) != len(want) {
		t.Fatalf("SearchPaths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SearchPaths = %v, want %v", got, want)
		}
	}
}

// An explicitly named config file that does not exist is an error, unlike a
// missing file found by search.
func TestLoadExplicitMissingFileErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.toml")
	t.Setenv(EnvConfig, missing)
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a missing explicit config file, want error")
	}
}
