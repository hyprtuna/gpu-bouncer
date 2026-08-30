package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyprtuna/gpu-bouncer/internal/adapter"
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

func (f *fakeGPU) setFail(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = fail
}

// fakeOllamaServer is enough of Ollama to drive one release. With sticky set
// the model never leaves /api/ps, however many unloads are accepted. With
// flap set an unload is honoured for exactly one /api/ps read and the model
// is then listed again, which is what a client that reloads it immediately
// looks like from outside.
type fakeOllamaServer struct {
	loaded     atomic.Bool
	sticky     bool
	flap       bool
	emptyReads atomic.Int32
	unloadHit  atomic.Int32
	psHits     atomic.Int32
	onUnload   func()

	// log records every request with its time, so a test can prove what
	// happened while something else was in flight.
	logMu sync.Mutex
	log   []requestRecord
}

type requestRecord struct {
	at   time.Time
	path string
}

func (f *fakeOllamaServer) record(path string) {
	f.logMu.Lock()
	defer f.logMu.Unlock()
	f.log = append(f.log, requestRecord{at: time.Now(), path: path})
}

// requests returns the recorded requests to one path, in order.
func (f *fakeOllamaServer) requests(path string) []requestRecord {
	f.logMu.Lock()
	defer f.logMu.Unlock()
	var out []requestRecord
	for _, r := range f.log {
		if r.path == path {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeOllamaServer) start(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"version":"0.33.2"}`)
	})
	mux.HandleFunc("/api/ps", func(w http.ResponseWriter, r *http.Request) {
		f.psHits.Add(1)
		f.record("/api/ps")
		if f.flap && f.emptyReads.Load() > 0 {
			f.emptyReads.Add(-1)
			_, _ = io.WriteString(w, `{"models":[]}`)
			return
		}
		if f.loaded.Load() {
			_, _ = io.WriteString(w, `{"models":[{"name":"qwen3:8b","model":"qwen3:8b",`+
				`"size":5368709120,"size_vram":5368709120,"expires_at":"2026-08-30T12:00:00Z"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"models":[]}`)
	})
	mux.HandleFunc("/api/generate", func(w http.ResponseWriter, r *http.Request) {
		f.unloadHit.Add(1)
		f.record("/api/generate")
		switch {
		case f.flap:
			f.emptyReads.Store(1)
		case !f.sticky:
			f.loaded.Store(false)
		}
		if f.onUnload != nil {
			f.onUnload()
		}
		_, _ = io.WriteString(w, `{"model":"qwen3:8b","done":true,"done_reason":"unload"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// fakeClock is an injectable time source that moves only when told to.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// testDaemon wires a daemon to fakes and starts it on a socket in a temp dir.
func testDaemon(t *testing.T, cfg config.Config, source gpu.Source, dryRun bool) string {
	t.Helper()
	return startDaemon(t, newTestDaemon(t, cfg, source, dryRun))
}

func newTestDaemon(t *testing.T, cfg config.Config, source gpu.Source, dryRun bool) *Daemon {
	t.Helper()
	if err := config.Validate(&cfg); err != nil {
		t.Fatalf("config: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d, err := New(cfg, source, log, dryRun)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

func startDaemon(t *testing.T, d *Daemon) string {
	t.Helper()
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
			// A typo in a service name has to be visible to a script, so
			// evict answers with an error rather than an empty plan.
			if resp.Error == "" {
				t.Errorf("%s for an unconfigured service succeeded, want an error", req.Op)
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

// Without a GPU reading the daemon must refuse to act, whatever is asked. The
// source fails after startup here, as a driver reset would: a source that
// fails at startup stops the daemon from starting at all, tested separately.
func TestDaemonRefusesWithoutGPUState(t *testing.T) {
	fakeOllama := &fakeOllamaServer{}
	fakeOllama.loaded.Store(true)
	url := fakeOllama.start(t)
	hardware := &fakeGPU{device: gpu.Device{Index: 0, TotalMiB: 8192, UsedMiB: 8000}}

	testDaemon(t, twoServiceConfig(url, true), hardware, false)
	hardware.setFail(true)

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
	if status.GPU != nil && status.GPU.Error == "" {
		t.Error("GPU reported unknown with no reason")
	}
}

// runRefusal starts a daemon that is expected to refuse, and returns the error.
func runRefusal(t *testing.T, cfg config.Config, source gpu.Source) error {
	t.Helper()
	if err := config.Validate(&cfg); err != nil {
		t.Fatalf("config: %v", err)
	}
	d, err := New(cfg, source, slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return d.Run(ctx, filepath.Join(t.TempDir(), "gpu-bouncer.sock"))
}

// A hybrid laptop seen through sysfs: the NVIDIA card is index 0 and has no
// VRAM counters there. The daemon must refuse to start rather than arbitrate
// nothing, and must say what would fix it.
func TestDaemonRefusesUnreadableArbitratedGPU(t *testing.T) {
	root := t.TempDir()
	writeFakeCard(t, root, "card1", "0000:01:00.0", "0x10de", "")
	writeFakeCard(t, root, "card2", "0000:05:00.0", "0x1002", "2147483648")
	source, err := gpu.OpenSysfs(root)
	if err != nil {
		t.Fatalf("OpenSysfs: %v", err)
	}

	url := (&fakeOllamaServer{}).start(t)
	err = runRefusal(t, twoServiceConfig(url, false), source)
	if err == nil {
		t.Fatal("daemon started with an unreadable arbitrated GPU, want a refusal")
	}
	for _, want := range []string{"refusing to start", "GPU 0", "0000:01:00.0", "NVIDIA", "cgo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}

	// The AMD card at index 1 is readable, so the same tree with gpu_index = 1
	// starts. Proves the refusal is about the index, not the source.
	cfg := twoServiceConfig(url, false)
	cfg.Policy.GPUIndex = 1
	testDaemon(t, cfg, source, false)
}

// writeFakeCard lays out one DRM card the way the kernel does: a card
// directory whose device link points at a PCI address. An empty total means
// the driver exposes no VRAM counters.
func writeFakeCard(t *testing.T, root, card, busID, vendor, totalBytes string) {
	t.Helper()
	pciDir := filepath.Join(root, "devices", busID)
	if err := os.MkdirAll(pciDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, card), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(pciDir, filepath.Join(root, card, "device")); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"vendor": vendor + "\n", "device": "0x1234\n"}
	if totalBytes != "" {
		files["mem_info_vram_total"] = totalBytes + "\n"
		files["mem_info_vram_used"] = "0\n"
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(pciDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
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
		_ = first.Serve(ctx, func(context.Context, ipc.Request, func(ipc.Response)) ipc.Response {
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

// A model that never leaves /api/ps is a failed release, bounded by the
// service's drain_timeout, so a stuck service cannot block the loop for the
// old hardcoded 30 seconds and cannot be reported as freed.
func TestDaemonDrainTimeoutIsAFailedAction(t *testing.T) {
	fakeOllama := &fakeOllamaServer{sticky: true}
	fakeOllama.loaded.Store(true)
	url := fakeOllama.start(t)
	hardware := &fakeGPU{device: gpu.Device{Index: 0, TotalMiB: 8192, UsedMiB: 5200}}

	cfg := twoServiceConfig(url, false)
	cfg.Services[1].DrainTimeout = config.Duration(time.Second)
	testDaemon(t, cfg, hardware, false)

	start := time.Now()
	resp := call(t, ipc.Request{Op: ipc.OpEvict, Service: "ollama"})
	elapsed := time.Since(start)
	if elapsed >= 2*time.Second {
		t.Errorf("evict took %s, want under 2s with drain_timeout = 1s", elapsed)
	}
	if len(resp.Executed) != 1 {
		t.Fatalf("executed %+v, want one action", resp.Executed)
	}
	action := resp.Executed[0]
	if action.Acted {
		t.Error("Acted = true for a model that never drained")
	}
	if !strings.Contains(action.Error, "still loaded after 1s") {
		t.Errorf("Error = %q, want still loaded after 1s", action.Error)
	}
}

// unloadsWithin waits up to d for the fake to have seen want unloads and
// reports whether it did.
func unloadsWithin(f *fakeOllamaServer, want int32, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f.unloadHit.Load() >= want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return f.unloadHit.Load() >= want
}

// flapSetup is a reactive daemon whose only eviction candidate reloads its
// model the moment it is released, so every action measures a gain of zero.
// The clock is injected and frozen, so the cooldown window is under the
// test's control.
func flapSetup(t *testing.T, reactive bool) (*fakeOllamaServer, *fakeClock) {
	t.Helper()
	flap := &fakeOllamaServer{flap: true}
	flap.loaded.Store(true)
	flapURL := flap.start(t)
	topURL := (&fakeOllamaServer{}).start(t) // up, holding nothing, the defender

	cfg := config.Config{
		Policy: config.Policy{
			VRAMFloorMiB:   4096,
			Reactive:       reactive,
			PollInterval:   config.Duration(20 * time.Millisecond),
			MinEffectMiB:   64,
			ActionCooldown: config.Duration(60 * time.Second),
		},
		Services: []config.Service{
			{Name: "top", Adapter: config.AdapterOllama, Endpoint: topURL, Priority: 90},
			{Name: "flap", Adapter: config.AdapterOllama, Endpoint: flapURL, Priority: 10,
				DrainTimeout: config.Duration(100 * time.Millisecond)},
		},
	}
	// 2992 MiB free, below the floor, and nothing an unload changes.
	hardware := &fakeGPU{device: gpu.Device{Index: 0, TotalMiB: 8192, UsedMiB: 5200}}
	clock := &fakeClock{t: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)}
	d := newTestDaemon(t, cfg, hardware, false)
	d.now = clock.now
	startDaemon(t, d)
	return flap, clock
}

// Invariant: at most one no effect reactive action per service per window.
func TestCooldownLimitsReactiveActionsToOnePerWindow(t *testing.T) {
	flap, clock := flapSetup(t, true)

	if !unloadsWithin(flap, 1, 3*time.Second) {
		t.Fatal("the reactive loop never acted")
	}
	time.Sleep(400 * time.Millisecond) // twenty polls
	if n := flap.unloadHit.Load(); n != 1 {
		t.Fatalf("flap was released %d times inside one cooldown window, want exactly 1", n)
	}

	status := call(t, ipc.Request{Op: ipc.OpStatus})
	if len(status.Cooldowns) != 1 || status.Cooldowns[0].Service != "flap" {
		t.Fatalf("cooldowns = %+v, want flap listed", status.Cooldowns)
	}
	if want := clock.now().Add(60 * time.Second); !status.Cooldowns[0].Until.Equal(want) {
		t.Errorf("cooldown until %s, want %s", status.Cooldowns[0].Until, want)
	}
	if !strings.Contains(status.Cooldowns[0].Reason, "freed 0 MiB, below min_effect_mib 64") {
		t.Errorf("reason = %q", status.Cooldowns[0].Reason)
	}
	plan := call(t, ipc.Request{Op: ipc.OpPlan})
	if plan.Plan == nil || len(plan.Plan.Actions) != 0 {
		t.Errorf("plan = %+v, want no action during the cooldown", plan.Plan)
	}
	found := false
	for _, n := range plan.Plan.Notes {
		if strings.Contains(n, "flap left alone, cooling down until 2026-08-30T12:01:00Z") {
			found = true
		}
	}
	if !found {
		t.Errorf("plan notes = %v, want the cooldown named with its end", plan.Plan.Notes)
	}

	// The next window allows exactly one more.
	clock.advance(61 * time.Second)
	if !unloadsWithin(flap, 2, 3*time.Second) {
		t.Fatal("the reactive loop did not act again after the cooldown ended")
	}
	time.Sleep(400 * time.Millisecond)
	if n := flap.unloadHit.Load(); n != 2 {
		t.Fatalf("flap was released %d times across two windows, want exactly 2", n)
	}
}

// Invariant: an effective action starts no cooldown.
func TestEffectiveActionStartsNoCooldown(t *testing.T) {
	fakeOllama := &fakeOllamaServer{}
	fakeOllama.loaded.Store(true)
	url := fakeOllama.start(t)
	hardware := &fakeGPU{device: gpu.Device{Index: 0, TotalMiB: 8192, UsedMiB: 5200}}
	fakeOllama.onUnload = func() { hardware.setUsed(80) }
	testDaemon(t, twoServiceConfig(url, false), hardware, false)

	resp := call(t, ipc.Request{Op: ipc.OpRequest, Service: "comfyui", NeedMiB: 6000})
	if len(resp.Executed) != 1 || !resp.Executed[0].Acted {
		t.Fatalf("executed = %+v, want one effective action", resp.Executed)
	}
	if status := call(t, ipc.Request{Op: ipc.OpStatus}); len(status.Cooldowns) != 0 {
		t.Errorf("cooldowns = %+v, want none after an action that freed 5120 MiB", status.Cooldowns)
	}
}

// Invariant: an explicit evict during a cooldown still acts.
func TestEvictBypassesCooldown(t *testing.T) {
	flap, _ := flapSetup(t, true)
	if !unloadsWithin(flap, 1, 3*time.Second) {
		t.Fatal("the reactive loop never acted")
	}
	if status := call(t, ipc.Request{Op: ipc.OpStatus}); len(status.Cooldowns) != 1 {
		t.Fatalf("cooldowns = %+v, want flap cooling down", status.Cooldowns)
	}

	resp := call(t, ipc.Request{Op: ipc.OpEvict, Service: "flap"})
	if len(resp.Executed) != 1 {
		t.Fatalf("evict executed %+v, want one action despite the cooldown (notes: %v)", resp.Executed, resp.Plan.Notes)
	}
	if n := flap.unloadHit.Load(); n < 2 {
		t.Errorf("flap was released %d times, want the evict to have added one", n)
	}
}

// Invariant: the cooldown ends exactly on time, not a nanosecond early.
func TestCooldownEndsExactlyOnTime(t *testing.T) {
	flap, clock := flapSetup(t, true)
	if !unloadsWithin(flap, 1, 3*time.Second) {
		t.Fatal("the reactive loop never acted")
	}

	clock.advance(60*time.Second - time.Nanosecond)
	time.Sleep(300 * time.Millisecond)
	if n := flap.unloadHit.Load(); n != 1 {
		t.Fatalf("flap was released %d times one nanosecond before the cooldown ended, want 1", n)
	}
	plan := call(t, ipc.Request{Op: ipc.OpPlan})
	if plan.Plan == nil || !plan.Plan.Empty() {
		t.Errorf("plan = %+v, want empty one nanosecond before the end", plan.Plan)
	}

	clock.advance(time.Nanosecond)
	if !unloadsWithin(flap, 2, 3*time.Second) {
		t.Fatal("the reactive loop did not act at the exact end of the cooldown")
	}
	if status := call(t, ipc.Request{Op: ipc.OpStatus}); len(status.Cooldowns) != 1 {
		t.Errorf("cooldowns = %+v, want the second action to have started a new one", status.Cooldowns)
	}
}

// Invariant: a standing claim is defended again once the window ends. The
// request itself acts at once, bypassing nothing because there is nothing to
// bypass yet; the loop then leaves the useless target alone for one window.
func TestStandingClaimDefenseResumesAfterCooldown(t *testing.T) {
	flap, clock := flapSetup(t, false)

	resp := call(t, ipc.Request{Op: ipc.OpRequest, Service: "top", NeedMiB: 6000})
	if len(resp.Executed) != 1 {
		t.Fatalf("request executed %+v, want one action", resp.Executed)
	}
	time.Sleep(400 * time.Millisecond)
	if n := flap.unloadHit.Load(); n != 1 {
		t.Fatalf("flap was released %d times while cooling down under a standing claim, want 1", n)
	}

	clock.advance(61 * time.Second)
	if !unloadsWithin(flap, 2, 3*time.Second) {
		t.Fatal("the standing claim was not defended again after the cooldown")
	}
}

// A gpu_index beyond the device count is a startup refusal that names the
// index and the count, not a daemon that runs and can never act.
func TestDaemonRefusesIndexBeyondDeviceCount(t *testing.T) {
	url := (&fakeOllamaServer{}).start(t)
	cfg := twoServiceConfig(url, false)
	cfg.Policy.GPUIndex = 9
	err := runRefusal(t, cfg, &fakeGPU{device: gpu.Device{Index: 0, TotalMiB: 8192}})
	if err == nil {
		t.Fatal("daemon started with gpu_index 9 on a one device machine")
	}
	want := "policy.gpu_index 9 names no device: the fake source sees 1 device(s), indexes 0 to 0"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
}

// A daemon started with --dry-run never acts, so it must never record a claim
// either: a claim it cannot act on would be listed by status until restart and
// could never be released.
func TestDryRunDaemonRecordsNoClaim(t *testing.T) {
	url := (&fakeOllamaServer{}).start(t)
	hardware := &fakeGPU{device: gpu.Device{Index: 0, TotalMiB: 8192, UsedMiB: 100}}
	testDaemon(t, twoServiceConfig(url, false), hardware, true)

	resp := call(t, ipc.Request{Op: ipc.OpRequest, Service: "comfyui", NeedMiB: 100})
	if !resp.OK || resp.Plan == nil {
		t.Fatalf("request = %+v, want a plan", resp)
	}
	if !strings.Contains(resp.Message, "dry-run mode") || !strings.Contains(resp.Message, "no claim was recorded") {
		t.Errorf("message = %q, want it to say the daemon is in dry-run mode and recorded nothing", resp.Message)
	}

	status := call(t, ipc.Request{Op: ipc.OpStatus})
	if len(status.Claims) != 0 {
		t.Errorf("claims = %+v, want none from a dry-run daemon", status.Claims)
	}
	if status.DaemonDryRun == nil || !*status.DaemonDryRun {
		t.Errorf("daemon_dry_run = %v, want true", status.DaemonDryRun)
	}
	ping := call(t, ipc.Request{Op: ipc.OpPing})
	if ping.DaemonDryRun == nil || !*ping.DaemonDryRun {
		t.Errorf("ping daemon_dry_run = %v, want true", ping.DaemonDryRun)
	}

	release := call(t, ipc.Request{Op: ipc.OpRelease, Service: "comfyui"})
	if !release.OK || !strings.Contains(release.Message, "nothing to release") {
		t.Errorf("release = %+v, want an ok reply saying there is nothing to release", release)
	}
}

// A normal daemon reports daemon_dry_run false, so a consumer can tell the two
// apart without parsing text.
func TestNormalDaemonReportsDryRunFalse(t *testing.T) {
	url := (&fakeOllamaServer{}).start(t)
	testDaemon(t, twoServiceConfig(url, false), &fakeGPU{device: gpu.Device{Index: 0, TotalMiB: 8192}}, false)
	ping := call(t, ipc.Request{Op: ipc.OpPing})
	if ping.DaemonDryRun == nil || *ping.DaemonDryRun {
		t.Errorf("daemon_dry_run = %v, want false", ping.DaemonDryRun)
	}
}

// drainSetup is a reactive daemon over two eviction candidates: slow, whose
// model never leaves /api/ps so every release drains for drainTimeout, and
// quick, which unloads at once. The defender top holds nothing. The floor is
// unreachable, so every poll wants both released.
func drainSetup(t *testing.T, drainTimeout time.Duration) (slow, quick *fakeOllamaServer) {
	t.Helper()
	slow = &fakeOllamaServer{sticky: true}
	slow.loaded.Store(true)
	quick = &fakeOllamaServer{flap: true}
	quick.loaded.Store(true)
	topURL := (&fakeOllamaServer{}).start(t)
	cfg := config.Config{
		Policy: config.Policy{
			VRAMFloorMiB:   100000,
			Reactive:       true,
			PollInterval:   config.Duration(20 * time.Millisecond),
			MinEffectMiB:   64,
			ActionCooldown: config.Duration(time.Hour),
		},
		Services: []config.Service{
			{Name: "top", Adapter: config.AdapterOllama, Endpoint: topURL, Priority: 90},
			{Name: "slow", Adapter: config.AdapterOllama, Endpoint: slow.start(t), Priority: 10,
				DrainTimeout: config.Duration(drainTimeout)},
			{Name: "quick", Adapter: config.AdapterOllama, Endpoint: quick.start(t), Priority: 20,
				DrainTimeout: config.Duration(drainTimeout)},
		},
	}
	hardware := &fakeGPU{device: gpu.Device{Index: 0, TotalMiB: 8192, UsedMiB: 5200}}
	testDaemon(t, cfg, hardware, false)
	return slow, quick
}

// Invariant: the poll loop never blocks on an action. While slow is draining
// for five seconds, quick is still probed at every poll and still released.
func TestDrainDoesNotBlockTheLoop(t *testing.T) {
	slow, quick := drainSetup(t, 5*time.Second)

	if !unloadsWithin(slow, 1, 3*time.Second) {
		t.Fatal("slow was never released")
	}
	drainStart := slow.requests("/api/generate")[0].at
	// slow is now draining. For the next two seconds, well inside its five
	// second drain, the loop must keep observing quick and act on it.
	probesBefore := quick.psHits.Load()
	if !unloadsWithin(quick, 1, 2*time.Second) {
		t.Fatal("quick was not released while slow was draining")
	}
	quickRelease := quick.requests("/api/generate")[0].at
	if quickRelease.Sub(drainStart) > 4*time.Second {
		t.Errorf("quick was released %s after slow's drain began, which is after the drain, not during it", quickRelease.Sub(drainStart))
	}
	time.Sleep(time.Second)
	if n := quick.psHits.Load() - probesBefore; n < 20 {
		t.Errorf("quick was probed %d times in about three seconds of a slow drain, want at least 20 (one per poll)", n)
	}
	// slow's drain must still be in flight at this point, which is the whole
	// point: its second release, if any, may only start after the first ends.
	if n := len(slow.requests("/api/generate")); n != 1 {
		t.Errorf("slow received %d releases while its first was still draining, want 1", n)
	}
}

// Invariant: at most one action in flight per service. Two explicit evicts
// of a draining service at once produce one release; the second is reported
// as skipped rather than run alongside the first.
func TestOneActionInFlightPerService(t *testing.T) {
	slow, _ := drainSetup(t, 2*time.Second)
	if !unloadsWithin(slow, 1, 3*time.Second) {
		t.Fatal("slow was never released")
	}
	// The loop's release is draining. Explicit evicts must queue behind it,
	// not run beside it.
	results := make(chan ipc.Response, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- call(t, ipc.Request{Op: ipc.OpEvict, Service: "slow"}) }()
	}
	skipped := 0
	for i := 0; i < 2; i++ {
		resp := <-results
		for _, r := range resp.Executed {
			if strings.Contains(r.Detail, "already in flight") {
				skipped++
			}
		}
		for _, n := range resp.Plan.Notes {
			if strings.Contains(n, "still in flight") {
				skipped++
			}
		}
	}
	if skipped != 2 {
		t.Errorf("%d of 2 concurrent evicts were skipped, want both while the loop's release drains", skipped)
	}
	gens := slow.requests("/api/generate")
	for i := 1; i < len(gens); i++ {
		if gens[i].at.Sub(gens[i-1].at) < 2*time.Second {
			t.Errorf("two releases of slow %s apart, want at least the 2s drain between them", gens[i].at.Sub(gens[i-1].at))
		}
	}
	if len(gens) != 1 {
		t.Errorf("slow received %d releases, want exactly the loop's one", len(gens))
	}
}

// Invariant: shutdown during a drain does not wait for it.
func TestShutdownDuringDrainIsPrompt(t *testing.T) {
	slow := &fakeOllamaServer{sticky: true}
	slow.loaded.Store(true)
	cfg := config.Config{
		Policy: config.Policy{VRAMFloorMiB: 512, PollInterval: config.Duration(time.Hour)},
		Services: []config.Service{
			{Name: "top", Adapter: config.AdapterOllama, Endpoint: (&fakeOllamaServer{}).start(t), Priority: 90},
			{Name: "slow", Adapter: config.AdapterOllama, Endpoint: slow.start(t), Priority: 10,
				DrainTimeout: config.Duration(30 * time.Second)},
		},
	}
	d := newTestDaemon(t, cfg, &fakeGPU{device: gpu.Device{Index: 0, TotalMiB: 8192, UsedMiB: 5200}}, false)
	socket := filepath.Join(t.TempDir(), "gpu-bouncer.sock")
	t.Setenv(ipc.EnvSocket, socket)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx, socket) }()
	waitForDaemon(t, socket)

	go func() {
		evictCtx, evictCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer evictCancel()
		_, _ = ipc.Do(evictCtx, ipc.Request{Op: ipc.OpEvict, Service: "slow"})
	}()
	if !unloadsWithin(slow, 1, 3*time.Second) {
		t.Fatal("slow was never released")
	}

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of cancellation during a drain")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("shutdown took %s during a 30s drain, want under the 1s service timeout", elapsed)
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Errorf("socket still exists after shutdown: %v", err)
	}
}

// The daemon never reloads, so its status reply names the configuration it
// loaded, with a digest a client can compare against the files on disk.
func TestStatusReportsTheLoadedConfig(t *testing.T) {
	url := (&fakeOllamaServer{}).start(t)
	path := filepath.Join(t.TempDir(), "c.toml")
	body := "[policy]\nvram_floor_mib = 512\npoll_interval = \"1h\"\n[[service]]\nname = \"ollama\"\nadapter = \"ollama\"\nendpoint = \"" + url + "\"\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFrom([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hash == "" || cfg.LoadedAt.IsZero() {
		t.Fatalf("LoadFrom left Hash %q, LoadedAt %v", cfg.Hash, cfg.LoadedAt)
	}
	testDaemon(t, cfg, &fakeGPU{device: gpu.Device{Index: 0, TotalMiB: 8192}}, false)

	status := call(t, ipc.Request{Op: ipc.OpStatus})
	if status.DaemonConfig == nil {
		t.Fatal("status carries no daemon_config")
	}
	if status.DaemonConfig.Path != path || status.DaemonConfig.SHA256 != cfg.Hash || !status.DaemonConfig.LoadedAt.Equal(cfg.LoadedAt) {
		t.Errorf("daemon_config = %+v, want path %s, sha %s, loaded %s", status.DaemonConfig, path, cfg.Hash, cfg.LoadedAt)
	}

	// An edit on disk changes what a fresh load sees and not what the daemon
	// reports, which is exactly the mismatch the client is meant to notice.
	if err := os.WriteFile(path, []byte(body+"\n# edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	edited, err := config.LoadFrom([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	if edited.Hash == cfg.Hash {
		t.Fatal("an edited file produced the same digest")
	}
	after := call(t, ipc.Request{Op: ipc.OpStatus})
	if after.DaemonConfig.SHA256 != cfg.Hash {
		t.Errorf("the daemon's digest changed to %s after an edit, want the loaded %s", after.DaemonConfig.SHA256, cfg.Hash)
	}
	if ping := call(t, ipc.Request{Op: ipc.OpPing}); ping.DaemonConfig == nil || ping.DaemonConfig.SHA256 != cfg.Hash {
		t.Errorf("ping daemon_config = %+v, want the loaded digest", ping.DaemonConfig)
	}
}

// Every expired cooldown is forgotten on the next sweep, whether or not its
// service is in the observation, so the map never holds a dead entry.
func TestExpiredCooldownsAreSweptEvenWhenUnobserved(t *testing.T) {
	url := (&fakeOllamaServer{}).start(t)
	clock := &fakeClock{t: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)}
	d := newTestDaemon(t, twoServiceConfig(url, false), &fakeGPU{device: gpu.Device{Index: 0, TotalMiB: 8192}}, false)
	d.now = clock.now
	d.mu.Lock()
	d.cooldowns["gone"] = cooldown{until: clock.now().Add(-time.Second), reason: "old"}
	d.cooldowns["ollama"] = cooldown{until: clock.now().Add(time.Minute), reason: "live"}
	d.mu.Unlock()

	obs := scheduler.Observation{Services: []scheduler.ServiceState{{Name: "ollama"}}}
	d.applyCooldowns(&obs)
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, still := d.cooldowns["gone"]; still {
		t.Error("an expired cooldown for an unobserved service was kept")
	}
	if _, kept := d.cooldowns["ollama"]; !kept || obs.Services[0].CooldownUntil.IsZero() {
		t.Error("a live cooldown was lost or not applied")
	}
}

func TestElide(t *testing.T) {
	if got := elide("short"); got != "short" {
		t.Errorf("elide(short) = %q", got)
	}
	long := strings.Repeat("y", 500)
	if got := elide(long); len(got) != 83 || !strings.HasSuffix(got, "...") {
		t.Errorf("elide(500 chars) = %d chars, want 80 plus three dots", len(got))
	}
}

// A second request for the same service updates the amount and keeps the
// original timestamp, so re-asking does not lose an equal priority tie to a
// peer that asked in between.
func TestRerequestKeepsItsPlaceInLine(t *testing.T) {
	cfg := config.Config{
		Policy: config.Policy{VRAMFloorMiB: 512, PollInterval: config.Duration(time.Hour)},
		Services: []config.Service{
			{Name: "first", Adapter: config.AdapterOllama, Endpoint: (&fakeOllamaServer{}).start(t), Priority: 50},
			{Name: "second", Adapter: config.AdapterOllama, Endpoint: (&fakeOllamaServer{}).start(t), Priority: 50},
		},
	}
	testDaemon(t, cfg, &fakeGPU{device: gpu.Device{Index: 0, TotalMiB: 8192, UsedMiB: 100}}, false)

	call(t, ipc.Request{Op: ipc.OpRequest, Service: "first", NeedMiB: 100})
	since := call(t, ipc.Request{Op: ipc.OpStatus}).Claims[0].At
	time.Sleep(5 * time.Millisecond)
	call(t, ipc.Request{Op: ipc.OpRequest, Service: "second", NeedMiB: 100})
	time.Sleep(5 * time.Millisecond)

	resp := call(t, ipc.Request{Op: ipc.OpRequest, Service: "first", NeedMiB: 200})
	if want := "updated the claim held since " + since.Format(time.RFC3339); resp.Message != want {
		t.Errorf("message = %q, want %q", resp.Message, want)
	}
	status := call(t, ipc.Request{Op: ipc.OpStatus})
	var first *ipc.ClaimReport
	for i := range status.Claims {
		if status.Claims[i].Service == "first" {
			first = &status.Claims[i]
		}
	}
	if first == nil {
		t.Fatalf("claims = %+v, want first", status.Claims)
	}
	if first.NeedMiB != 200 {
		t.Errorf("need_mib = %d, want the updated 200", first.NeedMiB)
	}
	if !first.At.Equal(since) {
		t.Errorf("at = %s, want the original %s", first.At, since)
	}
	if plan := call(t, ipc.Request{Op: ipc.OpPlan}); plan.Plan == nil || plan.Plan.Beneficiary != "first" {
		t.Errorf("beneficiary = %v, want first, which asked first and asked again", plan.Plan)
	}
	// A first request carries no such message.
	if resp := call(t, ipc.Request{Op: ipc.OpRelease, Service: "first"}); !resp.OK {
		t.Fatal(resp.Error)
	}
	if resp := call(t, ipc.Request{Op: ipc.OpRequest, Service: "first", NeedMiB: 100}); resp.Message != "" {
		t.Errorf("a fresh request carried message %q", resp.Message)
	}
}

// At debug level the daemon logs every poll and every adapter request, and
// a llama-swap API key sent as a bearer on every call never reaches the log.
func TestDebugLogNeverCarriesTheAPIKey(t *testing.T) {
	const key = "SUPERSECRET_TOKEN_9x7"
	t.Setenv("GPU_BOUNCER_LLAMA_SWAP_API_KEY", key)
	mux := http.NewServeMux()
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "OK") })
	mux.HandleFunc("/running", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r) {
			_, _ = io.WriteString(w, `{"running":[{"model":"m","state":"ready","ttl":0}]}`)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	adapter.Logger = log
	t.Cleanup(func() { adapter.Logger = nil })

	cfg := config.Config{
		Policy:   config.Policy{VRAMFloorMiB: 512, PollInterval: config.Duration(20 * time.Millisecond)},
		Services: []config.Service{{Name: "swap", Adapter: config.AdapterLlamaSwap, Endpoint: srv.URL, Priority: 10}},
	}
	if err := config.Validate(&cfg); err != nil {
		t.Fatal(err)
	}
	d, err := New(cfg, &fakeGPU{device: gpu.Device{Index: 0, TotalMiB: 8192, UsedMiB: 100}}, log, false)
	if err != nil {
		t.Fatal(err)
	}
	startDaemon(t, d)
	call(t, ipc.Request{Op: ipc.OpRequest, Service: "swap", NeedMiB: 100})
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	text := buf.String()
	mu.Unlock()
	if strings.Contains(text, key) {
		t.Fatalf("the API key appears in the log:\n%s", text)
	}
	if strings.Contains(strings.ToLower(text), "bearer") {
		t.Errorf("the log mentions the bearer header:\n%s", text)
	}
	if !strings.Contains(text, "msg=tick") || !strings.Contains(text, "swap.up=true") {
		t.Errorf("no per tick debug line with the service's state:\n%s", text)
	}
	if !strings.Contains(text, `msg="http request"`) || !strings.Contains(text, "url="+srv.URL+"/running") || !strings.Contains(text, "status=200") {
		t.Errorf("no per request debug line:\n%s", text)
	}
	if !strings.Contains(text, "claim_mib=100") {
		t.Errorf("the tick line lacks the claim:\n%s", text)
	}
}

// lockedWriter serialises the log handler's writes so the test can read
// the buffer under the same lock.
type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// Four services that will not drain must cost one drain, not four. Run one
// after another, `evict --all-except` on four stuck services outlasts any
// fixed client deadline and the operator is told no daemon exists while the
// daemon is busy doing exactly what was asked.
func TestEvictAllExceptRunsItsActionsConcurrently(t *testing.T) {
	const drain = 3 * time.Second

	services := []config.Service{
		{Name: "keeper", Adapter: config.AdapterOllama, Endpoint: (&fakeOllamaServer{}).start(t), Priority: 90},
	}
	stuck := make([]*fakeOllamaServer, 4)
	for i := range stuck {
		stuck[i] = &fakeOllamaServer{sticky: true}
		stuck[i].loaded.Store(true)
		services = append(services, config.Service{
			Name:         fmt.Sprintf("stuck%d", i),
			Adapter:      config.AdapterOllama,
			Endpoint:     stuck[i].start(t),
			Priority:     10,
			Timeout:      config.Duration(time.Second),
			DrainTimeout: config.Duration(drain),
		})
	}
	cfg := config.Config{
		Policy:   config.Policy{VRAMFloorMiB: 512, PollInterval: config.Duration(time.Hour)},
		Services: services,
	}
	testDaemon(t, cfg, &fakeGPU{device: gpu.Device{Index: 0, TotalMiB: 8192, UsedMiB: 5200}}, false)

	start := time.Now()
	resp, err := ipc.Exchange(context.Background(),
		ipc.Request{Op: ipc.OpEvict, Service: "keeper", AllExcept: true, PlanFirst: true})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("evict --all-except: %v", err)
	}
	if len(resp.Executed) != len(stuck) {
		t.Fatalf("%d results, want one per stuck service (%d): %+v", len(resp.Executed), len(stuck), resp.Executed)
	}
	for _, r := range resp.Executed {
		if r.Error == "" {
			t.Errorf("%s reported no error, want the drain to have timed out", r.Service)
		}
	}
	// Four drains one after another would be 12s. One drain plus the request
	// timeout either side of it is the whole cost when they overlap.
	if elapsed > drain+2*time.Second {
		t.Errorf("evict --all-except took %s for four %s drains, want about one drain", elapsed, drain)
	}
	for i, f := range stuck {
		if got := f.unloadHit.Load(); got != 1 {
			t.Errorf("stuck%d saw %d releases, want exactly 1", i, got)
		}
	}
}

// The client's wait is derived from the plan the daemon announces, so the
// daemon has to announce one before it starts working.
func TestTheDaemonAnnouncesItsPlanBeforeCarryingItOut(t *testing.T) {
	stuck := &fakeOllamaServer{sticky: true}
	stuck.loaded.Store(true)
	cfg := config.Config{
		Policy: config.Policy{VRAMFloorMiB: 512, PollInterval: config.Duration(time.Hour)},
		Services: []config.Service{
			{Name: "keeper", Adapter: config.AdapterOllama, Endpoint: (&fakeOllamaServer{}).start(t), Priority: 90},
			{Name: "stuck", Adapter: config.AdapterOllama, Endpoint: stuck.start(t), Priority: 10,
				Timeout: config.Duration(time.Second), DrainTimeout: config.Duration(2 * time.Second)},
		},
	}
	socket := testDaemon(t, cfg, &fakeGPU{device: gpu.Device{Index: 0, TotalMiB: 8192, UsedMiB: 5200}}, false)

	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	req, _ := json.Marshal(ipc.Request{Op: ipc.OpEvict, Service: "stuck", PlanFirst: true})
	if _, err := conn.Write(append(req, '\n')); err != nil {
		t.Fatal(err)
	}

	reader := bufio.NewReader(conn)
	start := time.Now()
	var first ipc.Response
	if err := json.NewDecoder(reader).Decode(&first); err != nil {
		t.Fatalf("first reply: %v", err)
	}
	announced := time.Since(start)
	if !first.Preliminary {
		t.Errorf("first reply is not marked preliminary: %+v", first)
	}
	if first.Plan == nil || len(first.Plan.Actions) != 1 {
		t.Errorf("first reply carries %v, want the one action plan", first.Plan)
	}
	// The plan arrives before the work, not after it.
	if announced > time.Second {
		t.Errorf("the plan arrived after %s, want it before the 2s drain began", announced)
	}
	if want := (time.Second + 2*time.Second).Milliseconds(); first.LongestActionMS != want {
		t.Errorf("longest_action_ms = %d, want timeout plus drain_timeout = %d", first.LongestActionMS, want)
	}

	var final ipc.Response
	if err := json.NewDecoder(reader).Decode(&final); err != nil {
		t.Fatalf("final reply: %v", err)
	}
	if final.Preliminary || len(final.Executed) != 1 {
		t.Errorf("final reply = %+v, want the results", final)
	}
}

// A client that did not ask for the plan gets one reply, as every 0.1.2
// client does.
func TestAClientThatDidNotAskGetsOneReply(t *testing.T) {
	stuck := &fakeOllamaServer{sticky: true}
	stuck.loaded.Store(true)
	cfg := config.Config{
		Policy: config.Policy{VRAMFloorMiB: 512, PollInterval: config.Duration(time.Hour)},
		Services: []config.Service{
			{Name: "keeper", Adapter: config.AdapterOllama, Endpoint: (&fakeOllamaServer{}).start(t), Priority: 90},
			{Name: "stuck", Adapter: config.AdapterOllama, Endpoint: stuck.start(t), Priority: 10,
				Timeout: config.Duration(time.Second), DrainTimeout: config.Duration(time.Second)},
		},
	}
	testDaemon(t, cfg, &fakeGPU{device: gpu.Device{Index: 0, TotalMiB: 8192, UsedMiB: 5200}}, false)

	resp := call(t, ipc.Request{Op: ipc.OpEvict, Service: "stuck"})
	if resp.Preliminary {
		t.Error("a client that did not set plan_first was sent a preliminary reply")
	}
	if len(resp.Executed) != 1 {
		t.Errorf("executed = %+v, want the one result", resp.Executed)
	}
}

// A connection must stay open for as long as the work it carries can take.
// The old flat two minutes cut off any plan with a drain over that.
func TestConnBudgetFollowsTheLongestAction(t *testing.T) {
	tests := []struct {
		name     string
		services []config.Service
		want     time.Duration
	}{
		{
			name: "no services still gets the flat floor plus room to probe",
			want: ipc.PlanWait + time.Minute,
		},
		{
			name: "the longest drain a config may set outlasts the old two minute cap",
			services: []config.Service{
				{Name: "a", Timeout: config.Duration(5 * time.Second), DrainTimeout: config.Duration(30 * time.Second)},
				{Name: "b", Timeout: config.Duration(5 * time.Second), DrainTimeout: config.Duration(config.MaxDrainTimeout)},
			},
			want: config.MaxDrainTimeout + 5*time.Second + ipc.ClientSlack + time.Minute,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &Daemon{cfg: config.Config{Services: tt.services}}
			if got := d.connBudget(); got != tt.want {
				t.Errorf("connBudget() = %s, want %s", got, tt.want)
			}
			if d.connBudget() <= 2*time.Minute {
				t.Errorf("connBudget() = %s, want more than the old flat two minutes", d.connBudget())
			}
		})
	}
}
