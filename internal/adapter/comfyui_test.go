package adapter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyprtuna/gpu-bouncer/internal/config"
)

// comfyFake is a stand in ComfyUI. Every handler is optional; an unset one
// answers 404 so that a test which does not expect a route to be called finds
// out rather than silently passing.
type comfyFake struct {
	systemStats http.HandlerFunc
	prompt      http.HandlerFunc
	free        http.HandlerFunc

	// freeCalls counts requests that reached /api/free. The busy path asserts
	// on it, so it is the reason this fake exists at all.
	freeCalls atomic.Int32
	// freeBody is the last body /api/free received, verbatim.
	freeBody atomic.Value
}

func (f *comfyFake) start(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/system_stats", func(w http.ResponseWriter, r *http.Request) {
		if f.systemStats == nil {
			http.NotFound(w, r)
			return
		}
		f.systemStats(w, r)
	})
	mux.HandleFunc("/api/prompt", func(w http.ResponseWriter, r *http.Request) {
		if f.prompt == nil {
			http.NotFound(w, r)
			return
		}
		f.prompt(w, r)
	})
	mux.HandleFunc("/api/free", func(w http.ResponseWriter, r *http.Request) {
		f.freeCalls.Add(1)
		body, _ := io.ReadAll(r.Body)
		f.freeBody.Store(string(body))
		if f.free == nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		f.free(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// jsonHandler answers with a fixed body. The body is written as a string rather
// than marshalled from a struct so that malformed and wrongly shaped payloads
// can be expressed as literally as valid ones.
func jsonHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

func statusHandler(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

// sleepHandler answers only after d, to drive a request past its timeout.
func sleepHandler(d time.Duration, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(d):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

func newTestComfyUI(t *testing.T, base string, timeout time.Duration) *comfyUIAdapter {
	t.Helper()
	return newComfyUI(config.Service{
		Name:     "comfy",
		Adapter:  config.AdapterComfyUI,
		Endpoint: base,
		Timeout:  config.Duration(timeout),
	})
}

// A realistic /api/system_stats body, trimmed to the fields the adapter reads
// plus enough neighbours to prove the extras are ignored. The cuda device holds
// 8 GiB reserved with 2 GiB of that cached free, so 6 GiB is active: 6144 MiB.
const comfyStatsTwoDevices = `{
  "system": {
    "os": "linux",
    "ram_total": 67108864000,
    "ram_free": 41000000000,
    "comfyui_version": "0.34.0",
    "python_version": "3.12.3",
    "pytorch_version": "2.5.1+cu124"
  },
  "devices": [
    {
      "name": "cuda:0 NVIDIA GeForce RTX 4090 : cudaMallocAsync",
      "type": "cuda",
      "index": 0,
      "vram_total": 25757220864,
      "vram_free": 17179869184,
      "torch_vram_total": 8589934592,
      "torch_vram_free": 2147483648
    },
    {
      "name": "cpu",
      "type": "cpu",
      "index": null,
      "vram_total": 67108864000,
      "vram_free": 41000000000,
      "torch_vram_total": 0,
      "torch_vram_free": 0
    }
  ]
}`

const comfyIdle = `{"exec_info":{"queue_remaining":0}}`
const comfyBusy = `{"exec_info":{"queue_remaining":3}}`

func TestComfyUIProbe(t *testing.T) {
	// A device list whose only entry has a null index, which must not be read
	// as device 0.
	const nullIndexOnly = `{
	  "system": {"comfyui_version": "0.34.0"},
	  "devices": [
	    {"name": "cpu", "type": "cpu", "index": null,
	     "vram_total": 1, "vram_free": 1,
	     "torch_vram_total": 4294967296, "torch_vram_free": 0}
	  ]
	}`
	// Only device 1 is present, so the adapter's device 0 is absent.
	const otherIndexOnly = `{
	  "system": {"comfyui_version": "0.34.0"},
	  "devices": [
	    {"name": "cuda:1 NVIDIA GeForce RTX 4090 : native", "type": "cuda", "index": 1,
	     "vram_total": 25757220864, "vram_free": 17179869184,
	     "torch_vram_total": 8589934592, "torch_vram_free": 2147483648}
	  ]
	}`
	// A null indexed device listed before the matching one, to prove the null
	// is skipped rather than ending the search.
	const nullThenMatch = `{
	  "system": {"comfyui_version": "0.34.0"},
	  "devices": [
	    {"name": "cpu", "type": "cpu", "index": null,
	     "torch_vram_total": 999999999999, "torch_vram_free": 0},
	    {"name": "cuda:0 NVIDIA GeForce RTX 4090 : native", "type": "cuda", "index": 0,
	     "torch_vram_total": 2147483648, "torch_vram_free": 1073741824}
	  ]
	}`
	// torch_vram_free above torch_vram_total, which must clamp rather than
	// underflow the unsigned subtraction.
	const invertedTorchFigures = `{
	  "system": {"comfyui_version": "0.34.0"},
	  "devices": [
	    {"name": "cuda:0", "type": "cuda", "index": 0,
	     "torch_vram_total": 1048576, "torch_vram_free": 8388608}
	  ]
	}`

	tests := []struct {
		name        string
		stats       string
		prompt      string
		wantVersion string
		wantHeldMiB uint64
		wantItems   int
		wantIdle    bool
	}{
		{
			name:        "held is torch reserved minus torch cached free",
			stats:       comfyStatsTwoDevices,
			prompt:      comfyIdle,
			wantVersion: "0.34.0",
			wantHeldMiB: 6144,
			wantItems:   1,
			wantIdle:    true,
		},
		{
			name:        "null index never matches device 0",
			stats:       nullIndexOnly,
			prompt:      comfyIdle,
			wantVersion: "0.34.0",
			wantHeldMiB: 0,
			wantItems:   0,
			wantIdle:    true,
		},
		{
			name:        "requested device absent from the list",
			stats:       otherIndexOnly,
			prompt:      comfyIdle,
			wantVersion: "0.34.0",
			wantHeldMiB: 0,
			wantItems:   0,
			wantIdle:    true,
		},
		{
			name:        "null index is skipped, not fatal to the search",
			stats:       nullThenMatch,
			prompt:      comfyIdle,
			wantVersion: "0.34.0",
			wantHeldMiB: 1024,
			wantItems:   1,
			wantIdle:    true,
		},
		{
			name:        "inverted torch figures clamp to zero",
			stats:       invertedTorchFigures,
			prompt:      comfyIdle,
			wantVersion: "0.34.0",
			wantHeldMiB: 0,
			wantItems:   1,
			wantIdle:    true,
		},
		{
			name:        "queue_remaining 3 is not idle",
			stats:       comfyStatsTwoDevices,
			prompt:      comfyBusy,
			wantVersion: "0.34.0",
			wantHeldMiB: 6144,
			wantItems:   1,
			wantIdle:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &comfyFake{
				systemStats: jsonHandler(tt.stats),
				prompt:      jsonHandler(tt.prompt),
			}
			a := newTestComfyUI(t, fake.start(t), 2*time.Second)

			got, err := a.Probe(context.Background())
			if err != nil {
				t.Fatalf("Probe: unexpected error: %v", err)
			}
			if !got.Up {
				t.Error("Up = false, want true")
			}
			if got.Version != tt.wantVersion {
				t.Errorf("Version = %q, want %q", got.Version, tt.wantVersion)
			}
			if got.HeldMiB != tt.wantHeldMiB {
				t.Errorf("HeldMiB = %d, want %d", got.HeldMiB, tt.wantHeldMiB)
			}
			if !got.HeldEstimated {
				t.Error("HeldEstimated = false, want true: the held figure is derived from torch counters")
			}
			if len(got.Items) != tt.wantItems {
				t.Errorf("len(Items) = %d, want %d (%v)", len(got.Items), tt.wantItems, got.Items)
			}
			if !got.IdleKnown {
				t.Error("IdleKnown = false, want true")
			}
			if got.Idle != tt.wantIdle {
				t.Errorf("Idle = %v, want %v", got.Idle, tt.wantIdle)
			}
		})
	}
}

// TestComfyUIProbeItemMatchesHeld guards the one thing the count check above
// cannot: that the emitted Item describes the same bytes as HeldMiB.
func TestComfyUIProbeItemMatchesHeld(t *testing.T) {
	fake := &comfyFake{
		systemStats: jsonHandler(comfyStatsTwoDevices),
		prompt:      jsonHandler(comfyIdle),
	}
	a := newTestComfyUI(t, fake.start(t), 2*time.Second)

	got, err := a.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: unexpected error: %v", err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("len(Items) = %d, want 1", len(got.Items))
	}
	item := got.Items[0]
	if item.VRAMMiB != got.HeldMiB {
		t.Errorf("Items[0].VRAMMiB = %d, want %d", item.VRAMMiB, got.HeldMiB)
	}
	if !strings.Contains(item.Name, "cuda:0") {
		t.Errorf("Items[0].Name = %q, want the matched device name", item.Name)
	}
	// 2 GiB of the 8 GiB reserved is allocator cache.
	if !strings.Contains(item.Detail, "2048 MiB") {
		t.Errorf("Items[0].Detail = %q, want the cache-empty-reclaimable figure", item.Detail)
	}
}

func TestComfyUIProbeErrors(t *testing.T) {
	tests := []struct {
		name   string
		stats  http.HandlerFunc
		prompt http.HandlerFunc
	}{
		{
			name:   "system_stats refuses",
			stats:  statusHandler(http.StatusInternalServerError, "boom"),
			prompt: jsonHandler(comfyIdle),
		},
		{
			name:   "prompt refuses",
			stats:  jsonHandler(comfyStatsTwoDevices),
			prompt: statusHandler(http.StatusInternalServerError, "boom"),
		},
		{
			name:   "system_stats is not JSON",
			stats:  jsonHandler("<!doctype html><html>the frontend</html>"),
			prompt: jsonHandler(comfyIdle),
		},
		{
			name:   "prompt is not JSON",
			stats:  jsonHandler(comfyStatsTwoDevices),
			prompt: jsonHandler("not json at all"),
		},
		{
			// Valid JSON, wrong shape. An empty device list must not decode to
			// "up and holding nothing".
			name:   "system_stats has no devices",
			stats:  jsonHandler(`{"system":{"comfyui_version":"0.34.0"},"devices":[]}`),
			prompt: jsonHandler(comfyIdle),
		},
		{
			name:   "system_stats is a JSON array",
			stats:  jsonHandler(`["system_stats"]`),
			prompt: jsonHandler(comfyIdle),
		},
		{
			// queue_remaining as a string rather than a number: decoding must
			// fail rather than leave the count at a zero that reads as idle.
			name:   "queue_remaining is not a number",
			stats:  jsonHandler(comfyStatsTwoDevices),
			prompt: jsonHandler(`{"exec_info":{"queue_remaining":"three"}}`),
		},
		{
			name:   "system_stats route missing",
			stats:  nil,
			prompt: jsonHandler(comfyIdle),
		},
		{
			name:   "prompt route missing",
			stats:  jsonHandler(comfyStatsTwoDevices),
			prompt: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &comfyFake{systemStats: tt.stats, prompt: tt.prompt}
			a := newTestComfyUI(t, fake.start(t), 2*time.Second)

			got, err := a.Probe(context.Background())
			if err == nil {
				t.Fatalf("Probe: want error, got status %+v", got)
			}
			if got.Up {
				t.Error("Up = true on a failed probe, want the zero Status")
			}
		})
	}
}

func TestComfyUIProbeTimeout(t *testing.T) {
	fake := &comfyFake{
		systemStats: sleepHandler(1*time.Second, comfyStatsTwoDevices),
		prompt:      jsonHandler(comfyIdle),
	}
	a := newTestComfyUI(t, fake.start(t), 50*time.Millisecond)

	if _, err := a.Probe(context.Background()); err == nil {
		t.Fatal("Probe: want a timeout error, got nil")
	}
}

// TestComfyUIReleaseRefusesWhenBusy is the load bearing test in this file.
// ComfyUI answers 200 to a free it will not act on until the running prompt
// ends, so the adapter must not send one at all while work is in flight.
func TestComfyUIReleaseRefusesWhenBusy(t *testing.T) {
	fake := &comfyFake{
		systemStats: jsonHandler(comfyStatsTwoDevices),
		prompt:      jsonHandler(comfyBusy),
	}
	a := newTestComfyUI(t, fake.start(t), 2*time.Second)

	got, err := a.Release(context.Background())
	if err != nil {
		t.Fatalf("Release: unexpected error: %v", err)
	}
	if n := fake.freeCalls.Load(); n != 0 {
		t.Fatalf("/api/free was called %d time(s) on a busy server, want 0", n)
	}
	if got.Acted {
		t.Error("Acted = true, want false: nothing was released")
	}
	if len(got.Targets) != 0 {
		t.Errorf("Targets = %v, want none", got.Targets)
	}
	if got.Detail == "" {
		t.Fatal("Detail is empty, want an explanation of the refusal")
	}
	if !strings.Contains(got.Detail, "3") {
		t.Errorf("Detail = %q, want the in-flight count", got.Detail)
	}
}

func TestComfyUIReleaseWhenIdle(t *testing.T) {
	fake := &comfyFake{
		systemStats: jsonHandler(comfyStatsTwoDevices),
		prompt:      jsonHandler(comfyIdle),
		// The real handler answers a bare 200 with an empty body.
		free: statusHandler(http.StatusOK, ""),
	}
	a := newTestComfyUI(t, fake.start(t), 2*time.Second)

	got, err := a.Release(context.Background())
	if err != nil {
		t.Fatalf("Release: unexpected error: %v", err)
	}
	if !got.Acted {
		t.Error("Acted = false, want true")
	}
	if n := fake.freeCalls.Load(); n != 1 {
		t.Fatalf("/api/free was called %d time(s), want 1", n)
	}

	raw, _ := fake.freeBody.Load().(string)
	var sent map[string]any
	if err := json.Unmarshal([]byte(raw), &sent); err != nil {
		t.Fatalf("/api/free body %q is not JSON: %v", raw, err)
	}
	want := map[string]any{"unload_models": true, "free_memory": true}
	if len(sent) != len(want) {
		t.Errorf("/api/free body = %q, want exactly %v", raw, want)
	}
	for k, v := range want {
		if sent[k] != v {
			t.Errorf("/api/free body key %q = %v, want %v (body: %q)", k, sent[k], v, raw)
		}
	}
}

func TestComfyUIReleaseErrors(t *testing.T) {
	tests := []struct {
		name   string
		prompt http.HandlerFunc
		free   http.HandlerFunc
		// wantFreeCalls records whether the free was reached at all.
		wantFreeCalls int32
	}{
		{
			name:          "idleness check refuses",
			prompt:        statusHandler(http.StatusInternalServerError, "boom"),
			free:          statusHandler(http.StatusOK, ""),
			wantFreeCalls: 0,
		},
		{
			name:          "idleness check is garbage",
			prompt:        jsonHandler("<html>frontend</html>"),
			free:          statusHandler(http.StatusOK, ""),
			wantFreeCalls: 0,
		},
		{
			name:   "idleness check is valid JSON of the wrong shape",
			prompt: jsonHandler(`{"exec_info":[]}`),
			free:   statusHandler(http.StatusOK, ""),
			// A wrongly shaped queue report must not be read as idle and let a
			// free through on a possibly busy server.
			wantFreeCalls: 0,
		},
		{
			name:          "free refuses",
			prompt:        jsonHandler(comfyIdle),
			free:          statusHandler(http.StatusInternalServerError, "boom"),
			wantFreeCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &comfyFake{
				systemStats: jsonHandler(comfyStatsTwoDevices),
				prompt:      tt.prompt,
				free:        tt.free,
			}
			a := newTestComfyUI(t, fake.start(t), 2*time.Second)

			got, err := a.Release(context.Background())
			if err == nil {
				t.Fatalf("Release: want error, got result %+v", got)
			}
			if got.Acted {
				t.Error("Acted = true on a failed release, want false")
			}
			if n := fake.freeCalls.Load(); n != tt.wantFreeCalls {
				t.Errorf("/api/free was called %d time(s), want %d", n, tt.wantFreeCalls)
			}
		})
	}
}

func TestComfyUIReleaseTimeout(t *testing.T) {
	fake := &comfyFake{
		systemStats: jsonHandler(comfyStatsTwoDevices),
		prompt:      sleepHandler(1*time.Second, comfyIdle),
		free:        statusHandler(http.StatusOK, ""),
	}
	a := newTestComfyUI(t, fake.start(t), 50*time.Millisecond)

	if _, err := a.Release(context.Background()); err == nil {
		t.Fatal("Release: want a timeout error, got nil")
	}
	if n := fake.freeCalls.Load(); n != 0 {
		t.Errorf("/api/free was called %d time(s) after a timed out idleness check, want 0", n)
	}
}

func TestComfyUIStop(t *testing.T) {
	fake := &comfyFake{}
	a := newTestComfyUI(t, fake.start(t), 2*time.Second)

	got, err := a.Stop(context.Background())
	if err == nil {
		t.Fatalf("Stop: want an error, got result %+v", got)
	}
	if got.Acted {
		t.Error("Acted = true, want false")
	}
	if !strings.Contains(err.Error(), ErrNotSupported.Error()) {
		t.Errorf("Stop error = %q, want it to carry ErrNotSupported's text", err)
	}
}

func TestComfyUIMetadata(t *testing.T) {
	fake := &comfyFake{}
	a := newTestComfyUI(t, fake.start(t), 2*time.Second)

	if a.Name() != "comfy" {
		t.Errorf("Name = %q, want %q", a.Name(), "comfy")
	}
	if a.Kind() != config.AdapterComfyUI {
		t.Errorf("Kind = %q, want %q", a.Kind(), config.AdapterComfyUI)
	}
	want := Capabilities{CanRelease: true, CanReportIdle: true, CanStop: false}
	if got := a.Capabilities(); got != want {
		t.Errorf("Capabilities = %+v, want %+v", got, want)
	}
}

// TestComfyUIUsesAPIPrefixedRoutes pins the route forms. The bare paths share a
// namespace with the static frontend handler, so a regression to them would
// still work against some deployments and fail against others.
func TestComfyUIUsesAPIPrefixedRoutes(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/api/system_stats":
			_, _ = io.WriteString(w, comfyStatsTwoDevices)
		case "/api/prompt":
			_, _ = io.WriteString(w, comfyIdle)
		case "/api/free":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	a := newTestComfyUI(t, srv.URL, 2*time.Second)
	if _, err := a.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: unexpected error: %v", err)
	}
	if _, err := a.Release(context.Background()); err != nil {
		t.Fatalf("Release: unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"GET /api/system_stats",
		"GET /api/prompt",
		"GET /api/prompt",
		"POST /api/free",
	}
	if len(paths) != len(want) {
		t.Fatalf("requests = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("request %d = %q, want %q", i, paths[i], want[i])
		}
	}
}
