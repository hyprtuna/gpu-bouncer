package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyprtuna/gpu-bouncer/internal/config"
	"github.com/hyprtuna/gpu-bouncer/internal/daemon"
	"github.com/hyprtuna/gpu-bouncer/internal/gpu"
	"github.com/hyprtuna/gpu-bouncer/internal/ipc"
)

// fixedGPU is a gpu.Source with one 8 GiB device that never changes.
type fixedGPU struct{}

func (fixedGPU) Name() string { return "fake" }
func (fixedGPU) Devices(context.Context) ([]gpu.Device, error) {
	return []gpu.Device{{Index: 0, Name: "fake", TotalMiB: 8192, UsedMiB: 5000}}, nil
}
func (fixedGPU) Processes(context.Context, int) ([]gpu.Process, error) {
	return nil, gpu.ErrUnsupported
}
func (fixedGPU) Close() error { return nil }

// fakeOllama answers probes as a loaded server and answers every release the
// way the test says: an HTTP 500 for a failing service, or a clean unload.
func fakeOllama(t *testing.T, releaseStatus int) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"version":"0.33.2"}`)
	})
	var unloaded atomic.Bool
	mux.HandleFunc("/api/ps", func(w http.ResponseWriter, r *http.Request) {
		if unloaded.Load() {
			_, _ = io.WriteString(w, `{"models":[]}`)
			return
		}
		_, _ = io.WriteString(w, `{"models":[{"name":"m1","model":"m1","size":1073741824,"size_vram":1073741824}]}`)
	})
	mux.HandleFunc("/api/generate", func(w http.ResponseWriter, r *http.Request) {
		if releaseStatus != http.StatusOK {
			http.Error(w, "release refused", releaseStatus)
			return
		}
		unloaded.Store(true)
		_, _ = io.WriteString(w, `{"done":true,"done_reason":"unload"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// fakeBusyComfyUI is idle on its first queue read and busy from then on, so
// the plan names it and the release then declines with a reason. That is the
// one outcome that is neither success nor failure.
func fakeBusyComfyUI(t *testing.T) string {
	t.Helper()
	var reads atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/system_stats", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"system":{"comfyui_version":"0.34.0"},"devices":[{"name":"cuda:0","type":"cuda","index":0,"torch_vram_total":2147483648,"torch_vram_free":0}]}`)
	})
	mux.HandleFunc("/api/prompt", func(w http.ResponseWriter, r *http.Request) {
		remaining := 1
		if reads.Add(1) == 1 {
			remaining = 0
		}
		_, _ = io.WriteString(w, `{"exec_info":{"queue_remaining":`+string(rune('0'+remaining))+`}}`)
	})
	mux.HandleFunc("/api/free", func(w http.ResponseWriter, r *http.Request) {
		t.Error("/api/free was called on a busy ComfyUI")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// startDaemon runs a daemon over fakes on a temp socket and points the CLI's
// environment at it and at a matching config file.
func startDaemon(t *testing.T, services ...config.Service) {
	t.Helper()
	startDaemonWith(t, false, services...)
}

func startDaemonWith(t *testing.T, dryRun bool, services ...config.Service) {
	t.Helper()
	cfg := config.Config{
		Policy:   config.Policy{VRAMFloorMiB: 512, PollInterval: config.Duration(time.Hour)},
		Services: services,
	}
	if err := config.Validate(&cfg); err != nil {
		t.Fatalf("config: %v", err)
	}
	startDaemonFromConfig(t, cfg, dryRun)
}

// startDaemonFromFile writes body to a config file, points the CLI at it
// through GPU_BOUNCER_CONFIG, and starts a daemon that loaded that file, the
// way the real daemon does. It returns the path so a test can edit it.
func startDaemonFromFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvConfig, path)
	cfg, err := config.LoadFrom([]string{path})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	startDaemonFromConfig(t, cfg, false)
	return path
}

func startDaemonFromConfig(t *testing.T, cfg config.Config, dryRun bool) {
	t.Helper()
	d, err := daemon.New(cfg, fixedGPU{}, slog.New(slog.NewTextHandler(io.Discard, nil)), dryRun)
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	socket := filepath.Join(shortDir(t), "gb.sock")
	t.Setenv(ipc.EnvSocket, socket)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	runErr := make(chan error, 1)
	go func() {
		defer close(done)
		runErr <- d.Run(ctx, socket)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// A daemon that gave up says why. Waiting out the full deadline and
		// reporting only that nothing answered hides the reason.
		select {
		case err := <-runErr:
			t.Fatalf("the daemon exited instead of answering on %s: %v", socket, err)
		default:
		}
		pingCtx, pingCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		resp, err := ipc.Do(pingCtx, ipc.Request{Op: ipc.OpPing})
		pingCancel()
		if err == nil && resp.OK {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("daemon never answered on %s", socket)
}

// shortDir is a temporary directory for a Unix socket. t.TempDir folds the
// test's name and TMPDIR into its path, and a socket path is capped at 107
// bytes, so a long subtest name on a machine with a long TMPDIR produces a
// socket that cannot be bound and a daemon that never answers. This keeps
// the path short whatever the test is called.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func run(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = Main(args, Env{Stdout: &out, Stderr: &errOut})
	return code, out.String(), errOut.String()
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}

// TestExitCodesAndFirstLines pins, per invocation, the exit code and the
// first line of what the user sees, because those are what a script and a
// human respectively read first.
func TestExitCodesAndFirstLines(t *testing.T) {
	missingConfig := filepath.Join(t.TempDir(), "absent.toml")
	emptyConfig := filepath.Join(t.TempDir(), "empty.toml")
	if err := os.WriteFile(emptyConfig, []byte("[policy]\nvram_floor_mib = 512\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	noSocket := filepath.Join(shortDir(t), "none.sock")

	tests := []struct {
		name       string
		env        map[string]string
		args       []string
		wantCode   int
		wantStdout string // first line of stdout, or "" for no stdout at all
		wantStderr string // first line of stderr, or "" for no stderr at all
		wantJSONOK *bool  // when set, stdout must be JSON with this ok
		wantLast   string // last line of stdout, when set
	}{
		{
			name:       "--help prints the usage once, on stdout",
			args:       []string{"--help"},
			wantCode:   0,
			wantStdout: "gpu-bouncer arbitrates one GPU between local AI services.",
		},
		{
			name:       "a flag parse error is one line on stderr, no usage",
			args:       []string{"request", "x", "--need-mib", "abc"},
			wantCode:   1,
			wantStderr: `gpu-bouncer: invalid value "abc" for flag -need-mib: parse error`,
		},
		{
			name:       "an unknown global flag is one line on stderr",
			args:       []string{"--bogus", "status"},
			wantCode:   1,
			wantStderr: "gpu-bouncer: flag provided but not defined: -bogus",
		},
		{
			name:       "version rejects positional arguments",
			args:       []string{"version", "foo"},
			wantCode:   1,
			wantStderr: `gpu-bouncer: version takes no arguments, got "foo"`,
		},
		{
			name:       "version --help explains itself",
			args:       []string{"version", "--help"},
			wantCode:   0,
			wantStdout: "Usage of gpu-bouncer version:",
		},
		{
			name:       "version prints the version",
			args:       []string{"version"},
			wantCode:   0,
			wantStdout: "gpu-bouncer " + version(),
		},
		{
			name:       "--json version is JSON",
			args:       []string{"--json", "version"},
			wantCode:   0,
			wantStdout: "{",
			wantJSONOK: nil,
		},
		{
			name:       "--json turns a config error into JSON on stdout",
			env:        map[string]string{config.EnvConfig: missingConfig},
			args:       []string{"--json", "status"},
			wantCode:   1,
			wantJSONOK: ptr(false),
		},
		{
			name:       "--json after the command still turns the error into JSON",
			env:        map[string]string{config.EnvConfig: missingConfig},
			args:       []string{"status", "--json"},
			wantCode:   1,
			wantJSONOK: ptr(false),
		},
		{
			name:       "request --json with no daemon is JSON",
			env:        map[string]string{ipc.EnvSocket: noSocket},
			args:       []string{"request", "x", "--json"},
			wantCode:   1,
			wantJSONOK: ptr(false),
		},
		{
			name:       "release --json with no daemon is JSON",
			env:        map[string]string{ipc.EnvSocket: noSocket},
			args:       []string{"release", "x", "--json"},
			wantCode:   1,
			wantJSONOK: ptr(false),
		},
		{
			name:       "evict --json with no daemon is JSON",
			env:        map[string]string{ipc.EnvSocket: noSocket},
			args:       []string{"evict", "x", "--json"},
			wantCode:   1,
			wantJSONOK: ptr(false),
		},
		{
			name:     "status --dry-run is accepted as a no op",
			env:      map[string]string{config.EnvConfig: emptyConfig, ipc.EnvSocket: noSocket},
			args:     []string{"status", "--dry-run"},
			wantCode: 0,
		},
		{
			name:       "plan --dry-run is accepted as a no op",
			env:        map[string]string{config.EnvConfig: emptyConfig, ipc.EnvSocket: noSocket},
			args:       []string{"plan", "--dry-run"},
			wantCode:   0,
			wantStdout: "Trigger: none",
		},
		{
			name:       "plan --verbose --json after the command",
			env:        map[string]string{config.EnvConfig: emptyConfig, ipc.EnvSocket: noSocket},
			args:       []string{"plan", "--verbose", "--json"},
			wantCode:   0,
			wantJSONOK: ptr(true),
		},
		{
			name:       "an unknown command prints the usage and one error line",
			args:       []string{"frobnicate"},
			wantCode:   2,
			wantStderr: "gpu-bouncer arbitrates one GPU between local AI services.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			code, stdout, stderr := run(tt.args...)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d\nstdout: %s\nstderr: %s", code, tt.wantCode, stdout, stderr)
			}
			if tt.wantStdout != "" && firstLine(stdout) != tt.wantStdout {
				t.Errorf("first stdout line = %q, want %q", firstLine(stdout), tt.wantStdout)
			}
			if tt.wantStderr != "" && firstLine(stderr) != tt.wantStderr {
				t.Errorf("first stderr line = %q, want %q", firstLine(stderr), tt.wantStderr)
			}
			if tt.wantStderr == "" && tt.wantJSONOK != nil && stderr != "" {
				t.Errorf("stderr = %q, want nothing alongside JSON output", stderr)
			}
			if tt.wantLast != "" && lastLine(stdout) != tt.wantLast {
				t.Errorf("last stdout line = %q, want %q", lastLine(stdout), tt.wantLast)
			}
			if tt.wantJSONOK != nil {
				var decoded struct {
					OK    bool   `json:"ok"`
					Error string `json:"error"`
				}
				if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
					t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
				}
				if decoded.OK != *tt.wantJSONOK {
					t.Errorf("ok = %v, want %v", decoded.OK, *tt.wantJSONOK)
				}
				if !decoded.OK && decoded.Error == "" {
					t.Error("ok is false with an empty error")
				}
			}
			// Whatever else happened, the usage block is never printed twice
			// and a flag error is never printed twice.
			all := stdout + stderr
			if n := strings.Count(all, "Usage:"); n > 1 {
				t.Errorf("usage printed %d times", n)
			}
			if n := strings.Count(all, "flag provided but not defined"); n > 1 {
				t.Errorf("flag error printed %d times", n)
			}
			if n := strings.Count(all, "invalid value"); n > 1 {
				t.Errorf("flag error printed %d times", n)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestJSONVersion(t *testing.T) {
	code, stdout, _ := run("--json", "version")
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if decoded["version"] != version() {
		t.Errorf("version = %q, want %q", decoded["version"], version())
	}
}

// The status JSON must say whether a daemon is running and which config
// produced it, both of which the text mode already prints.
func TestJSONStatusCarriesDaemonAndConfig(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(cfgPath, []byte("[policy]\nvram_floor_mib = 512\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvConfig, cfgPath)
	t.Setenv(ipc.EnvSocket, filepath.Join(shortDir(t), "none.sock"))

	code, stdout, stderr := run("--json", "status")
	if code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	var decoded struct {
		DaemonRunning *bool   `json:"daemon_running"`
		Config        *string `json:"config"`
	}
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if decoded.DaemonRunning == nil || *decoded.DaemonRunning {
		t.Errorf("daemon_running = %v, want false", decoded.DaemonRunning)
	}
	if decoded.Config == nil || *decoded.Config != cfgPath {
		t.Errorf("config = %v, want %q", decoded.Config, cfgPath)
	}

	// With no config file at all, config is JSON null rather than absent.
	t.Setenv(config.EnvConfig, "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, stdout, _ = run("--json", "status")
	if !strings.Contains(stdout, `"config": null`) {
		t.Errorf("stdout lacks \"config\": null:\n%s", stdout)
	}
}

// TestExitCodesAgainstADaemon covers the outcomes that need a daemon: a
// failed action, a declined action, and a service the config does not name.
func TestExitCodesAgainstADaemon(t *testing.T) {
	startDaemon(t,
		config.Service{Name: "r500", Adapter: config.AdapterOllama, Endpoint: fakeOllama(t, http.StatusInternalServerError), Priority: 10},
		config.Service{Name: "good", Adapter: config.AdapterOllama, Endpoint: fakeOllama(t, http.StatusOK), Priority: 10},
		config.Service{Name: "busy", Adapter: config.AdapterComfyUI, Endpoint: fakeBusyComfyUI(t), Priority: 10},
		config.Service{Name: "top", Adapter: config.AdapterOllama, Endpoint: fakeOllama(t, http.StatusOK), Priority: 90},
	)

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
		wantLast   string
		wantJSONOK *bool
	}{
		{
			name:     "evict of a failing release exits 1 and says so last",
			args:     []string{"evict", "r500"},
			wantCode: 1,
			wantLast: "1 of 1 actions failed",
		},
		{
			name:       "evict --json of a failing release has ok false",
			args:       []string{"--json", "evict", "r500"},
			wantCode:   1,
			wantJSONOK: ptr(false),
		},
		{
			name:       "evict of a clean release exits 0",
			args:       []string{"evict", "good"},
			wantCode:   0,
			wantStdout: "Done:",
		},
		{
			name:     "a declined action is not a failure",
			args:     []string{"evict", "busy"},
			wantCode: 0,
		},
		{
			name:       "evict of an unconfigured service exits 1",
			args:       []string{"evict", "nosuch"},
			wantCode:   1,
			wantStderr: `gpu-bouncer: service "nosuch" is not in the config`,
		},
		{
			name:       "release of an unconfigured service exits 1",
			args:       []string{"release", "nosuch"},
			wantCode:   1,
			wantStderr: `gpu-bouncer: service "nosuch" is not in the config`,
		},
		{
			name:       "request of an unconfigured service exits 1",
			args:       []string{"request", "nosuch"},
			wantCode:   1,
			wantStderr: `gpu-bouncer: service "nosuch" is not in the config`,
		},
		{
			name:       "evict --all-except an unconfigured service refuses and exits 1",
			args:       []string{"evict", "--all-except", "nosuch"},
			wantCode:   1,
			wantStderr: `gpu-bouncer: service "nosuch" is not in the config`,
		},
		{
			name:     "request that needs a failing release exits 1",
			args:     []string{"request", "top", "--need-mib", "8000"},
			wantCode: 1,
		},
		{
			name:       "release --json after the command is accepted",
			args:       []string{"release", "top", "--json"},
			wantCode:   0,
			wantJSONOK: ptr(true),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := run(tt.args...)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d\nstdout: %s\nstderr: %s", code, tt.wantCode, stdout, stderr)
			}
			if tt.wantStdout != "" && firstLine(stdout) != tt.wantStdout {
				t.Errorf("first stdout line = %q, want %q", firstLine(stdout), tt.wantStdout)
			}
			if tt.wantStderr != "" && firstLine(stderr) != tt.wantStderr {
				t.Errorf("first stderr line = %q, want %q", firstLine(stderr), tt.wantStderr)
			}
			if tt.wantLast != "" && lastLine(stdout) != tt.wantLast {
				t.Errorf("last stdout line = %q, want %q\n%s", lastLine(stdout), tt.wantLast, stdout)
			}
			if tt.wantCode == 0 && strings.Contains(stdout, "actions failed") {
				t.Errorf("exit 0 but the output reports a failure:\n%s", stdout)
			}
			if tt.wantJSONOK != nil {
				var decoded struct {
					OK bool `json:"ok"`
				}
				if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
					t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
				}
				if decoded.OK != *tt.wantJSONOK {
					t.Errorf("ok = %v, want %v\n%s", decoded.OK, *tt.wantJSONOK, stdout)
				}
			}
		})
	}
}

// The plan object uses the same snake_case keys as every sibling object.
func TestPlanJSONKeysAreSnakeCase(t *testing.T) {
	t.Setenv(config.EnvConfig, "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(ipc.EnvSocket, filepath.Join(shortDir(t), "none.sock"))
	code, stdout, stderr := run("--json", "plan")
	if code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	var decoded struct {
		Plan map[string]json.RawMessage `json:"plan"`
	}
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	for _, key := range []string{"trigger", "beneficiary", "current_free_mib", "target_free_mib", "actions", "notes"} {
		if _, ok := decoded.Plan[key]; !ok {
			t.Errorf("plan lacks key %q: %v", key, stdout)
		}
	}
	for _, key := range []string{"Trigger", "CurrentFreeMiB"} {
		if _, ok := decoded.Plan[key]; ok {
			t.Errorf("plan still has Go spelled key %q", key)
		}
	}
}

// status ends with the daemon line and the config path even when no service
// is configured, so an empty list can be traced to the file that produced it.
func TestStatusTrailerWithoutServices(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "empty.toml")
	if err := os.WriteFile(cfgPath, []byte("[policy]\nvram_floor_mib = 512\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvConfig, cfgPath)
	t.Setenv(ipc.EnvSocket, filepath.Join(shortDir(t), "none.sock"))

	code, stdout, stderr := run("status")
	if code != 0 {
		t.Fatalf("exit code = %d: %s", code, stderr)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("stdout too short:\n%s", stdout)
	}
	if got, want := lines[len(lines)-1], "Config: "+cfgPath; got != want {
		t.Errorf("last line = %q, want %q", got, want)
	}
	if got, want := lines[len(lines)-2], "No daemon is running: gpu-bouncer is observing only, and will not act."; got != want {
		t.Errorf("second to last line = %q, want %q", got, want)
	}
	if !strings.Contains(stdout, "No services are configured") {
		t.Errorf("stdout lacks the no services line:\n%s", stdout)
	}
}

// A dry-run daemon says so in the status trailer and in JSON, and a request
// against it leaves no claim behind.
func TestStatusNamesADryRunDaemon(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(cfgPath, []byte("[policy]\nvram_floor_mib = 512\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvConfig, cfgPath)
	startDaemonWith(t, true, config.Service{Name: "good", Adapter: config.AdapterOllama, Endpoint: fakeOllama(t, http.StatusOK), Priority: 10})

	code, stdout, stderr := run("request", "good", "--need-mib", "100")
	if code != 0 {
		t.Fatalf("request: exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "dry-run mode") || !strings.Contains(stdout, "no claim was recorded") {
		t.Errorf("request output = %q, want it to say the daemon is in dry-run mode and recorded nothing", stdout)
	}

	code, stdout, _ = run("status")
	if code != 0 {
		t.Fatalf("status: exit %d", code)
	}
	if !strings.Contains(stdout, "A daemon is running in dry-run mode: it plans and never acts.") {
		t.Errorf("status lacks the dry-run trailer:\n%s", stdout)
	}
	if strings.Contains(stdout, "Outstanding claims") {
		t.Errorf("status lists a claim a dry-run daemon can never act on:\n%s", stdout)
	}

	_, stdout, _ = run("--json", "status")
	var decoded struct {
		DaemonRunning bool `json:"daemon_running"`
		DaemonDryRun  bool `json:"daemon_dry_run"`
	}
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if !decoded.DaemonRunning || !decoded.DaemonDryRun {
		t.Errorf("daemon_running = %v, daemon_dry_run = %v; want both true", decoded.DaemonRunning, decoded.DaemonDryRun)
	}
}

// After an edit to the config file, status says the daemon is still running
// on the file it loaded, in text and in JSON, until the daemon restarts.
func TestStatusNoticesAStaleDaemonConfig(t *testing.T) {
	url := fakeOllama(t, http.StatusOK)
	path := startDaemonFromFile(t, "[policy]\nvram_floor_mib = 512\npoll_interval = \"1h\"\n[[service]]\nname = \"good\"\nadapter = \"ollama\"\nendpoint = \""+url+"\"\n")

	code, stdout, stderr := run("status")
	if code != 0 {
		t.Fatalf("status: exit %d: %s", code, stderr)
	}
	if strings.Contains(stdout, "loaded a different config") {
		t.Errorf("status reports a stale config before any edit:\n%s", stdout)
	}
	_, stdout, _ = run("--json", "status")
	var decoded struct {
		ConfigStale  *bool `json:"config_stale"`
		DaemonConfig *struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
		} `json:"daemon_config"`
	}
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if decoded.ConfigStale == nil || *decoded.ConfigStale || decoded.DaemonConfig == nil || decoded.DaemonConfig.Path != path {
		t.Errorf("before the edit: config_stale = %v, daemon_config = %+v", decoded.ConfigStale, decoded.DaemonConfig)
	}

	// Rename the service in the file; the daemon still holds the old name.
	if err := os.WriteFile(path, []byte("[policy]\nvram_floor_mib = 512\npoll_interval = \"1h\"\n[[service]]\nname = \"renamed\"\nadapter = \"ollama\"\nendpoint = \""+url+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ = run("status")
	if code != 0 {
		t.Fatalf("status after the edit: exit %d", code)
	}
	want := "the daemon loaded a different config (" + path + ", loaded "
	if !strings.Contains(stdout, want) || !strings.Contains(stdout, "); restart it to apply your edit") {
		t.Errorf("status after the edit lacks the stale config line:\n%s", stdout)
	}
	_, stdout, _ = run("--json", "status")
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if decoded.ConfigStale == nil || !*decoded.ConfigStale {
		t.Errorf("after the edit: config_stale = %v, want true", decoded.ConfigStale)
	}
}

// Usage errors exit 2 and, with --json, are one JSON object on stdout with
// nothing on stderr, like every other error.
func TestUsageErrorsExitTwo(t *testing.T) {
	for _, tt := range []struct {
		args     []string
		wantJSON bool
	}{
		{[]string{}, false},
		{[]string{"--json"}, true},
		{[]string{"frobnicate"}, false},
		{[]string{"--json", "frobnicate"}, true},
	} {
		code, stdout, stderr := run(tt.args...)
		if code != 2 {
			t.Errorf("%v: exit %d, want 2", tt.args, code)
		}
		if tt.wantJSON {
			var decoded struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal([]byte(stdout), &decoded); err != nil || decoded.OK || decoded.Error == "" {
				t.Errorf("%v: stdout = %q, want {ok:false, error}", tt.args, stdout)
			}
			if stderr != "" {
				t.Errorf("%v: stderr = %q, want nothing with --json", tt.args, stderr)
			}
			continue
		}
		if stdout != "" || !strings.Contains(stderr, "Usage:") {
			t.Errorf("%v: stdout = %q, stderr = %q; want the usage on stderr only", tt.args, stdout, stderr)
		}
	}
}

// Every list in the JSON output is present, empty as [], never null or
// absent, and the keys a consumer may read are always there.
func TestJSONShapesHaveEveryKey(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "empty.toml")
	if err := os.WriteFile(cfgPath, []byte("[policy]\nvram_floor_mib = 512\ngpu_index = 9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvConfig, cfgPath)
	t.Setenv(ipc.EnvSocket, filepath.Join(shortDir(t), "none.sock"))

	_, stdout, _ := run("--json", "status")
	var status map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &status); err != nil {
		t.Fatalf("status is not JSON: %v", err)
	}
	for _, key := range []string{"ok", "gpu", "devices", "services", "claims", "cooldowns", "daemon_running", "daemon_dry_run", "daemon_config", "config_stale", "config"} {
		if _, ok := status[key]; !ok {
			t.Errorf("status lacks %q: %s", key, stdout)
		}
	}
	for _, key := range []string{"services", "claims", "cooldowns"} {
		if v := string(status[key]); v != "[]" {
			t.Errorf("status %s = %s, want [] with nothing configured and no daemon", key, v)
		}
	}
	// devices depends on the machine's GPU source; it must be a list either way.
	if v := string(status["devices"]); !strings.HasPrefix(v, "[") {
		t.Errorf("status devices = %.40s, want a list", v)
	}
	var gpuField struct {
		Index int `json:"index"`
	}
	if err := json.Unmarshal(status["gpu"], &gpuField); err != nil || gpuField.Index != 9 {
		t.Errorf("gpu.index = %d (%v), want the configured 9", gpuField.Index, err)
	}

	_, stdout, _ = run("--json", "plan")
	var plan struct {
		Plan map[string]json.RawMessage `json:"plan"`
	}
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil {
		t.Fatalf("plan is not JSON: %v", err)
	}
	if string(plan.Plan["actions"]) != "[]" {
		t.Errorf("plan.actions = %s, want []", plan.Plan["actions"])
	}
	if strings.HasPrefix(string(plan.Plan["notes"]), "null") {
		t.Errorf("plan.notes = %s, want a list", plan.Plan["notes"])
	}
}

// A request reports whether it got the room it asked for, on the last text
// line and as target_met in JSON, and exits 0 either way.
func TestRequestReportsTheShortfall(t *testing.T) {
	startDaemon(t,
		config.Service{Name: "top", Adapter: config.AdapterOllama, Endpoint: fakeOllama(t, http.StatusOK), Priority: 90},
		config.Service{Name: "low", Adapter: config.AdapterOllama, Endpoint: fakeOllama(t, http.StatusOK), Priority: 10},
	)
	// The fixed GPU never changes, so nothing is ever freed and the target
	// is out of reach: 8192 asked on a card with 3192 free.
	code, stdout, _ := run("request", "top", "--need-mib", "8192")
	if code != 0 {
		t.Fatalf("exit %d, want 0: a shortfall is not a failure", code)
	}
	if got := lastLine(stdout); got != "freed 0 MiB of the 5000 MiB asked for, target not met" {
		t.Errorf("last line = %q", got)
	}
	code, stdout, _ = run("--json", "request", "top", "--need-mib", "8192")
	var decoded struct {
		OK        bool  `json:"ok"`
		TargetMet *bool `json:"target_met"`
		Executed  []any `json:"executed"`
	}
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if code != 0 || !decoded.OK || decoded.TargetMet == nil || *decoded.TargetMet || decoded.Executed == nil {
		t.Errorf("exit %d, ok %v, target_met %v, executed %v; want 0, true, false, a list", code, decoded.OK, decoded.TargetMet, decoded.Executed)
	}

	// A request already satisfied says so, and target_met is true.
	code, stdout, _ = run("request", "top", "--need-mib", "1000")
	if code != 0 || lastLine(stdout) != "the 1000 MiB asked for were already free" {
		t.Errorf("exit %d, last line %q", code, lastLine(stdout))
	}
	_, stdout, _ = run("--json", "request", "top", "--need-mib", "1000")
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil || decoded.TargetMet == nil || !*decoded.TargetMet {
		t.Errorf("target_met = %v (%v), want true", decoded.TargetMet, err)
	}

	// release --json has its own small shape.
	_, stdout, _ = run("--json", "release", "top")
	var rel map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &rel); err != nil || len(rel) != 2 || string(rel["ok"]) != "true" {
		t.Errorf("release --json = %s, want {ok, message}", stdout)
	}
}

// A service name of twenty thousand characters is echoed back elided.
func TestLongNamesAreElided(t *testing.T) {
	startDaemon(t, config.Service{Name: "good", Adapter: config.AdapterOllama, Endpoint: fakeOllama(t, http.StatusOK), Priority: 10})
	long := strings.Repeat("x", 20000)
	for _, args := range [][]string{{"request", long}, {"evict", long}, {"release", long}, {"evict", "--all-except", long}} {
		code, _, stderr := run(args...)
		if code != 1 {
			t.Errorf("%s: exit %d, want 1", args[0], code)
		}
		if len(stderr) > 200 || !strings.Contains(stderr, "...") {
			t.Errorf("%s: stderr is %d bytes: %.120s", args[0], len(stderr), stderr)
		}
	}
}

// Every list is present and empty as [], at every level. The top level was
// guaranteed in 0.1.2; services[].items is a list too and was absent.
func TestNestedListsArePresentAndEmpty(t *testing.T) {
	startDaemonFromFile(t, "[policy]\npoll_interval = \"1h\"\n[[service]]\nname = \"empty\"\nadapter = \"ollama\"\nendpoint = \""+
		fakeOllamaEmpty(t)+"\"\n")
	_, stdout, _ := run("--json", "status")

	var decoded struct {
		Services []map[string]json.RawMessage `json:"services"`
	}
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if len(decoded.Services) != 1 {
		t.Fatalf("services = %v, want one", decoded.Services)
	}
	items, present := decoded.Services[0]["items"]
	if !present {
		t.Fatalf("services[0] has no items key: %v", decoded.Services[0])
	}
	if string(items) != "[]" {
		t.Errorf("services[0].items = %s, want []", items)
	}
	// The nested strings are the documented exception: absent when unset.
	for _, key := range []string{"error"} {
		if _, unexpected := decoded.Services[0][key]; unexpected {
			t.Errorf("services[0] carries %s on a healthy service: %v", key, decoded.Services[0][key])
		}
	}
}

// fakeOllamaEmpty is an Ollama that is up and holding nothing, so its items
// list is empty rather than merely short.
func fakeOllamaEmpty(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"version":"0.33.2"}`)
	})
	mux.HandleFunc("/api/ps", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"models":[]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}
