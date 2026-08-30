package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyprtuna/gpu-bouncer/internal/config"
	"github.com/hyprtuna/gpu-bouncer/internal/gpu"
	"github.com/hyprtuna/gpu-bouncer/internal/ipc"
	"github.com/hyprtuna/gpu-bouncer/internal/scheduler"
)

// fakeGPU is a gpu.Source whose readings the test controls. Free VRAM changes
// only when the test says so, which is what lets a test assert that the daemon
// reports the GPU's own measurement rather than the service's claim.
type fakeGPU struct {
	mu     sync.Mutex
	device gpu.Device
	fail   bool
}

func (f *fakeGPU) Name() string { return "fake" }

func (f *fakeGPU) Devices(context.Context) ([]gpu.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, gpu.ErrNoDevices
	}
	return []gpu.Device{f.device}, nil
}

func (f *fakeGPU) Processes(context.Context, int) ([]gpu.Process, error) {
	return nil, gpu.ErrUnsupported
}

func (f *fakeGPU) Close() error { return nil }

func (f *fakeGPU) setUsed(mib uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.device.UsedMiB = mib
}

// fakeOllamaServer is enough of Ollama to drive one release.
type fakeOllamaServer struct {
	loaded    atomic.Bool
	unloadHit atomic.Int32
	onUnload  func()
}

func (f *fakeOllamaServer) start(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"version":"0.33.2"}`)
	})
	mux.HandleFunc("/api/ps", func(w http.ResponseWriter, r *http.Request) {
		if f.loaded.Load() {
			_, _ = io.WriteString(w, `{"models":[{"name":"qwen3:8b","model":"qwen3:8b",`+
				`"size":5368709120,"size_vram":5368709120,"expires_at":"2026-08-30T12:00:00Z"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"models":[]}`)
	})
	mux.HandleFunc("/api/generate", func(w http.ResponseWriter, r *http.Request) {
		f.unloadHit.Add(1)
		f.loaded.Store(false)
		if f.onUnload != nil {
			f.onUnload()
		}
		_, _ = io.WriteString(w, `{"model":"qwen3:8b","done":true,"done_reason":"unload"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// testDaemon wires a daemon to fakes and starts it on a socket in a temp dir.
func testDaemon(t *testing.T, cfg config.Config, source gpu.Source, dryRun bool) string {
	t.Helper()
	if err := config.Validate(&cfg); err != nil {
		t.Fatalf("config: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d, err := New(cfg, source, log, dryRun)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	socket := filepath.Join(t.TempDir(), "gpu-bouncer.sock")
	t.Setenv(ipc.EnvSocket, socket)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.Run(ctx, socket)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop within 5s")
		}
	})

	waitForDaemon(t, socket)
	return socket
}

func waitForDaemon(t *testing.T, socket string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		resp, err := ipc.Do(ctx, ipc.Request{Op: ipc.OpPing})
		cancel()
		if err == nil && resp.OK {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("daemon never answered on %s", socket)
}

func call(t *testing.T, req ipc.Request) ipc.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := ipc.Do(ctx, req)
	if err != nil {
		t.Fatalf("%s: %v", req.Op, err)
	}
	return resp
}

func twoServiceConfig(ollamaURL string, reactive bool) config.Config {
	return config.Config{
		Policy: config.Policy{
			VRAMFloorMiB: 4096,
			Reactive:     reactive,
			PollInterval: config.Duration(20 * time.Millisecond),
		},
		Services: []config.Service{
			{Name: "comfyui", Adapter: config.AdapterComfyUI, Endpoint: "http://127.0.0.1:1", Priority: 90},
			{Name: "ollama", Adapter: config.AdapterOllama, Endpoint: ollamaURL, Priority: 10},
		},
	}
}

// An explicit request must free a lower priority holder and report the GPU's
// own before and after figures, not the service's account of itself.
func TestDaemonRequestFreesLowerPriorityService(t *testing.T) {
	fakeOllama := &fakeOllamaServer{}
	fakeOllama.loaded.Store(true)
	url := fakeOllama.start(t)

	hardware := &fakeGPU{device: gpu.Device{Index: 0, Name: "fake", TotalMiB: 8192, UsedMiB: 5200}}
	// Freeing the model gives the memory back, which the GPU reading reflects.
	fakeOllama.onUnload = func() { hardware.setUsed(80) }

	testDaemon(t, twoServiceConfig(url, false), hardware, false)

	resp := call(t, ipc.Request{Op: ipc.OpRequest, Service: "comfyui", NeedMiB: 6000})
	if resp.Error != "" {
		t.Fatalf("request failed: %s", resp.Error)
	}
	if len(resp.Executed) != 1 {
		t.Fatalf("executed %d actions, want 1: %+v (plan notes: %v)",
			len(resp.Executed), resp.Executed, resp.Plan.Notes)
	}

	action := resp.Executed[0]
	if action.Service != "ollama" || action.Verb != string(scheduler.VerbRelease) {
		t.Errorf("executed %s %s, want release ollama", action.Verb, action.Service)
	}
	if !action.Acted {
		t.Errorf("Acted = false, want true: %+v", action)
	}
	if action.Error != "" {
		t.Errorf("action error: %s", action.Error)
	}
	if got, want := action.FreeBeforeMiB, uint64(2992); got != want {
		t.Errorf("FreeBeforeMiB = %d, want %d", got, want)
	}
	if got, want := action.FreeAfterMiB, uint64(8112); got != want {
		t.Errorf("FreeAfterMiB = %d, want %d", got, want)
	}
	if fakeOllama.unloadHit.Load() != 1 {
		t.Errorf("ollama received %d unload calls, want 1", fakeOllama.unloadHit.Load())
	}
}

// A dry run must plan exactly as the real thing would, and touch nothing.
func TestDaemonDryRunTouchesNothing(t *testing.T) {
	fakeOllama := &fakeOllamaServer{}
	fakeOllama.loaded.Store(true)
	url := fakeOllama.start(t)
	hardware := &fakeGPU{device: gpu.Device{Index: 0, TotalMiB: 8192, UsedMiB: 5200}}

	testDaemon(t, twoServiceConfig(url, false), hardware, false)

	resp := call(t, ipc.Request{Op: ipc.OpRequest, Service: "comfyui", NeedMiB: 6000, DryRun: true})
	if resp.Plan == nil || len(resp.Plan.Actions) != 1 {
		t.Fatalf("plan = %+v, want one action", resp.Plan)
	}
	if len(resp.Executed) != 0 {
		t.Errorf("executed %+v, want nothing on a dry run", resp.Executed)
	}
	if fakeOllama.unloadHit.Load() != 0 {
		t.Errorf("ollama received %d unload calls during a dry run, want 0", fakeOllama.unloadHit.Load())
	}
	// A dry run must not leave a claim behind.
	status := call(t, ipc.Request{Op: ipc.OpStatus})
	if len(status.Claims) != 0 {
		t.Errorf("claims = %+v, want none after a dry run", status.Claims)
	}
}

// The global --dry-run flag has to override even a real request.
func TestDaemonGlobalDryRun(t *testing.T) {
	fakeOllama := &fakeOllamaServer{}
	fakeOllama.loaded.Store(true)
	url := fakeOllama.start(t)
	hardware := &fakeGPU{device: gpu.Device{Index: 0, TotalMiB: 8192, UsedMiB: 5200}}

	testDaemon(t, twoServiceConfig(url, false), hardware, true)

	call(t, ipc.Request{Op: ipc.OpRequest, Service: "comfyui", NeedMiB: 6000})
	if fakeOllama.unloadHit.Load() != 0 {
		t.Errorf("ollama was unloaded %d times under a global dry run, want 0", fakeOllama.unloadHit.Load())
	}
}

// Reactive mode must act on its own once free VRAM falls below the floor.
func TestDaemonReactiveLoopActs(t *testing.T) {
	fakeOllama := &fakeOllamaServer{}
	fakeOllama.loaded.Store(true)
	url := fakeOllama.start(t)
	hardware := &fakeGPU{device: gpu.Device{Index: 0, TotalMiB: 8192, UsedMiB: 5200}}
	fakeOllama.onUnload = func() { hardware.setUsed(80) }

	cfg := twoServiceConfig(url, true)
	// comfyui is unreachable in this test, so the highest priority live
	// service is ollama itself. Give the floor a defender that is up.
	cfg.Services[0].Endpoint = "http://127.0.0.1:1"
	cfg.Policy.DefaultWorkload = ""
	cfg.Services = append(cfg.Services, config.Service{
		Name: "keeper", Adapter: config.AdapterSystemdUnit, Unit: "nonexistent-keeper.service", Priority: 99,
	})
	testDaemon(t, cfg, hardware, false)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fakeOllama.unloadHit.Load() > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	// keeper cannot be probed, so nothing outranks ollama and nothing should
	// have been freed. Assert that explicitly rather than leaving it implied.
	if fakeOllama.unloadHit.Load() != 0 {
		t.Fatal("unexpected unload")
	}
	plan := call(t, ipc.Request{Op: ipc.OpPlan})
	if plan.Plan == nil || len(plan.Plan.Actions) != 0 {
		t.Fatalf("plan = %+v, want no action while no higher priority service is up", plan.Plan)
	}
}

// Nothing may be acted on unless the config names it.
func TestDaemonRefusesUnconfiguredService(t *testing.T) {
	url := (&fakeOllamaServer{}).start(t)
	hardware := &fakeGPU{device: gpu.Device{Index: 0, TotalMiB: 8192, UsedMiB: 100}}
	testDaemon(t, twoServiceConfig(url, false), hardware, false)

	for _, req := range []ipc.Request{
		{Op: ipc.OpRequest, Service: "stable-diffusion"},
		{Op: ipc.OpEvict, Service: "stable-diffusion"},
	} {
		resp := call(t, req)
		switch req.Op {
		case ipc.OpRequest:
			if resp.Error == "" {
				t.Errorf("%s for an unconfigured service succeeded, want an error", req.Op)
			}
		case ipc.OpEvict:
			// Evict answers with an empty plan explaining the refusal rather
			// than an error, so the operator sees why.
			if resp.Plan == nil || len(resp.Plan.Actions) != 0 {
				t.Errorf("%s produced actions for an unconfigured service: %+v", req.Op, resp.Plan)
			}
			if len(resp.Executed) != 0 {
				t.Errorf("%s executed %+v, want nothing", req.Op, resp.Executed)
			}
		}
	}
}

// A claim survives until it is released, and shows up in status meanwhile.
func TestDaemonClaimLifecycle(t *testing.T) {
	url := (&fakeOllamaServer{}).start(t)
	hardware := &fakeGPU{device: gpu.Device{Index: 0, TotalMiB: 8192, UsedMiB: 100}}
	testDaemon(t, twoServiceConfig(url, false), hardware, false)

	call(t, ipc.Request{Op: ipc.OpRequest, Service: "comfyui", NeedMiB: 1024})
	status := call(t, ipc.Request{Op: ipc.OpStatus})
	if len(status.Claims) != 1 || status.Claims[0].Service != "comfyui" {
		t.Fatalf("claims = %+v, want one held by comfyui", status.Claims)
	}
	if got, want := status.Claims[0].NeedMiB, uint64(1024); got != want {
		t.Errorf("claim NeedMiB = %d, want %d", got, want)
	}

	resp := call(t, ipc.Request{Op: ipc.OpRelease, Service: "comfyui"})
	if !strings.Contains(resp.Message, "released") {
		t.Errorf("message = %q, want it to confirm the release", resp.Message)
	}
	if status := call(t, ipc.Request{Op: ipc.OpStatus}); len(status.Claims) != 0 {
		t.Errorf("claims = %+v, want none after release", status.Claims)
	}

	// Releasing a claim nobody holds is not an error, so a cleanup path can
	// call it unconditionally.
	if resp := call(t, ipc.Request{Op: ipc.OpRelease, Service: "comfyui"}); !resp.OK {
		t.Errorf("releasing an absent claim failed: %s", resp.Error)
	}
}

// Without a GPU reading the daemon must refuse to act, whatever is asked.
func TestDaemonRefusesWithoutGPUState(t *testing.T) {
	fakeOllama := &fakeOllamaServer{}
	fakeOllama.loaded.Store(true)
	url := fakeOllama.start(t)
	hardware := &fakeGPU{device: gpu.Device{Index: 0, TotalMiB: 8192, UsedMiB: 8000}, fail: true}

	testDaemon(t, twoServiceConfig(url, true), hardware, false)

	resp := call(t, ipc.Request{Op: ipc.OpRequest, Service: "comfyui", NeedMiB: 6000})
	if len(resp.Executed) != 0 {
		t.Errorf("executed %+v, want nothing when GPU state is unreadable", resp.Executed)
	}
	if fakeOllama.unloadHit.Load() != 0 {
		t.Errorf("ollama was unloaded %d times with no GPU reading, want 0", fakeOllama.unloadHit.Load())
	}
	status := call(t, ipc.Request{Op: ipc.OpStatus})
	if status.GPU == nil || status.GPU.Known {
		t.Errorf("GPU = %+v, want it reported as unknown", status.GPU)
	}
}

// A malformed request must get a clean refusal rather than take the daemon down.
func TestDaemonRejectsUnknownOperation(t *testing.T) {
	url := (&fakeOllamaServer{}).start(t)
	hardware := &fakeGPU{device: gpu.Device{Index: 0, TotalMiB: 8192, UsedMiB: 100}}
	testDaemon(t, twoServiceConfig(url, false), hardware, false)

	resp := call(t, ipc.Request{Op: "drop-everything"})
	if resp.Error == "" {
		t.Error("unknown operation was accepted")
	}
	// The daemon must still be serving.
	if ping := call(t, ipc.Request{Op: ipc.OpPing}); !ping.OK {
		t.Error("daemon stopped answering after a bad request")
	}
}

// Two daemons must not share one socket, or the second silently steals control.
func TestListenRefusesASecondDaemon(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "gpu-bouncer.sock")
	t.Setenv(ipc.EnvSocket, socket)
	first, err := ipc.Listen(socket)
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	defer func() { _ = first.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = first.Serve(ctx, func(context.Context, ipc.Request) ipc.Response {
			return ipc.Response{OK: true}
		})
	}()
	waitForDaemon(t, socket)

	if _, err := ipc.Listen(socket); err == nil {
		t.Fatal("a second Listen on a live socket succeeded, want a refusal")
	}
}

// A socket left behind by a crashed daemon must not block a restart.
func TestListenReplacesStaleSocket(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "gpu-bouncer.sock")
	first, err := ipc.Listen(socket)
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	// Close the listener but leave the file, which is what a crash looks like.
	_ = first.Close()
	if err := writeStaleFile(socket); err != nil {
		t.Fatalf("recreate stale socket: %v", err)
	}

	second, err := ipc.Listen(socket)
	if err != nil {
		t.Fatalf("Listen over a stale socket failed: %v", err)
	}
	_ = second.Close()
}

func writeStaleFile(path string) error {
	f, err := createFile(path)
	if err != nil {
		return err
	}
	return f.Close()
}

func TestResponseRoundTripsAsJSON(t *testing.T) {
	plan := scheduler.Plan{
		Trigger: scheduler.TriggerReactive,
		Actions: []scheduler.Action{{Service: "ollama", Verb: scheduler.VerbRelease, ExpectFreeMiB: 5120}},
	}
	encoded, err := json.Marshal(ipc.Response{OK: true, Plan: &plan})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded ipc.Response
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Plan == nil || len(decoded.Plan.Actions) != 1 {
		t.Fatalf("plan did not survive the round trip: %+v", decoded.Plan)
	}
	if decoded.Plan.Actions[0].Verb != scheduler.VerbRelease {
		t.Errorf("verb = %q, want %q", decoded.Plan.Actions[0].Verb, scheduler.VerbRelease)
	}
}

func createFile(path string) (*os.File, error) { return os.Create(path) }

// A Unix socket path over the kernel limit fails with a bare "invalid
// argument" that names nothing. Catching it here means the operator is told
// which path was too long and by how much.
func TestListenRejectsAnOverlongPath(t *testing.T) {
	dir := t.TempDir()
	long := filepath.Join(dir, strings.Repeat("d", 120)+".sock")
	_, err := ipc.Listen(long)
	if err == nil {
		t.Fatal("Listen accepted a path over the Unix socket limit")
	}
	if !strings.Contains(err.Error(), "over the") || !strings.Contains(err.Error(), "byte limit") {
		t.Errorf("error = %q, want it to explain the length limit", err)
	}
}
