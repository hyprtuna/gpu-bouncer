package adapter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyprtuna/gpu-bouncer/internal/config"
)

// fakeLlamaSwap stands in for llama-swap. Every test in this file talks only to
// an httptest server wrapping one of these; no request is ever made to a real
// service on this machine.
type fakeLlamaSwap struct {
	healthStatus  int
	healthBody    string
	runningStatus int
	runningBody   string
	unloadStatus  int
	unloadBody    string
	// delay makes every handler slow, for the timeout case.
	delay time.Duration

	mu           sync.Mutex
	requests     []string
	unloadCalls  int
	authByPath   map[string]string
	sawLegacyGET bool
}

func (f *fakeLlamaSwap) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	if f.authByPath == nil {
		f.authByPath = make(map[string]string)
	}
	f.authByPath[r.URL.Path] = r.Header.Get("Authorization")
	if r.URL.Path == "/api/models/unload" {
		f.unloadCalls++
	}
	if r.URL.Path == "/unload" {
		f.sawLegacyGET = true
	}
	f.mu.Unlock()

	if f.delay > 0 {
		time.Sleep(f.delay)
	}

	switch r.URL.Path {
	case "/health":
		writeFakeLlamaSwap(w, f.healthStatus, f.healthBody, "OK")
	case "/running":
		writeFakeLlamaSwap(w, f.runningStatus, f.runningBody, `{"running":[]}`)
	case "/api/models/unload":
		writeFakeLlamaSwap(w, f.unloadStatus, f.unloadBody, `{"msg":"ok"}`)
	case "/unload":
		// The legacy state mutating GET. The adapter must never use it, so
		// answer in a way no test could mistake for success.
		http.Error(w, "legacy GET /unload must not be used", http.StatusTeapot)
	default:
		http.NotFound(w, r)
	}
}

func writeFakeLlamaSwap(w http.ResponseWriter, status int, body, fallback string) {
	if status == 0 {
		status = http.StatusOK
	}
	if body == "" {
		body = fallback
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (f *fakeLlamaSwap) counts() (unload int, legacy bool, requests []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unloadCalls, f.sawLegacyGET, append([]string(nil), f.requests...)
}

func (f *fakeLlamaSwap) auth(path string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authByPath[path]
}

// startLlamaSwap wires a fake to an adapter with a short timeout.
func startLlamaSwap(t *testing.T, fake *fakeLlamaSwap, timeout time.Duration) *llamaSwapAdapter {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	a := newLlamaSwap(config.Service{
		Name:     "swap",
		Adapter:  config.AdapterLlamaSwap,
		Endpoint: srv.URL,
		Timeout:  config.Duration(timeout),
	})
	a.unloadTimeout = timeout
	return a
}

const twoRunning = `{"running":[
	{"model":"qwen3-30b","state":"ready","cmd":"llama-server -m qwen3.gguf","ttl":300},
	{"model":"gemma3-12b","state":"starting","cmd":"llama-server -m gemma3.gguf","ttl":0}
]}`

func TestLlamaSwapProbe(t *testing.T) {
	tests := []struct {
		name      string
		fake      *fakeLlamaSwap
		timeout   time.Duration
		wantErr   bool
		wantItems []Item
	}{
		{
			name:    "running models are listed with their state",
			fake:    &fakeLlamaSwap{runningBody: twoRunning},
			timeout: 2 * time.Second,
			wantItems: []Item{
				{Name: "qwen3-30b", Detail: "state ready"},
				{Name: "gemma3-12b", Detail: "state starting"},
			},
		},
		{
			name:      "empty running list is a live service holding nothing",
			fake:      &fakeLlamaSwap{runningBody: `{"running":[]}`},
			timeout:   2 * time.Second,
			wantItems: nil,
		},
		{
			name:    "health down fails the probe",
			fake:    &fakeLlamaSwap{healthStatus: http.StatusServiceUnavailable, healthBody: "down"},
			timeout: 2 * time.Second,
			wantErr: true,
		},
		{
			name:    "running refuses with HTTP 500",
			fake:    &fakeLlamaSwap{runningStatus: http.StatusInternalServerError, runningBody: "boom"},
			timeout: 2 * time.Second,
			wantErr: true,
		},
		{
			name:    "running answers 401 when the API key is wrong",
			fake:    &fakeLlamaSwap{runningStatus: http.StatusUnauthorized, runningBody: "unauthorized: invalid or missing API key"},
			timeout: 2 * time.Second,
			wantErr: true,
		},
		{
			name:    "running answers something that is not JSON",
			fake:    &fakeLlamaSwap{runningBody: "<html>proxy error</html>"},
			timeout: 2 * time.Second,
			wantErr: true,
		},
		{
			name:    "running answers JSON of the wrong shape",
			fake:    &fakeLlamaSwap{runningBody: `{"running":{"model":"qwen3-30b"}}`},
			timeout: 2 * time.Second,
			wantErr: true,
		},
		{
			name:    "a slow service is cut off at the timeout",
			fake:    &fakeLlamaSwap{delay: 300 * time.Millisecond},
			timeout: 40 * time.Millisecond,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := startLlamaSwap(t, tt.fake, tt.timeout)

			status, err := a.Probe(context.Background())
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Probe() succeeded, want an error; status = %+v", status)
				}
				return
			}
			if err != nil {
				t.Fatalf("Probe() failed: %v", err)
			}
			if !status.Up {
				t.Error("Up = false, want true")
			}
			if !reflect.DeepEqual(status.Items, tt.wantItems) {
				t.Errorf("Items = %+v, want %+v", status.Items, tt.wantItems)
			}
			// llama-swap publishes no VRAM figure at all, and the adapter must
			// say so rather than invent one.
			if status.HeldMiB != 0 {
				t.Errorf("HeldMiB = %d, want 0: llama-swap reports no VRAM", status.HeldMiB)
			}
			if !status.HeldEstimated {
				t.Error("HeldEstimated = false, want true: the zero is absent data, not a measurement")
			}
			if status.IdleKnown {
				t.Error("IdleKnown = true, want false: llama-swap cannot distinguish ready from busy")
			}
		})
	}
}

func TestLlamaSwapRelease(t *testing.T) {
	tests := []struct {
		name        string
		fake        *fakeLlamaSwap
		wantErr     bool
		wantActed   bool
		wantTargets []string
		wantUnloads int
	}{
		{
			name:        "unloads every running model in one call",
			fake:        &fakeLlamaSwap{runningBody: twoRunning},
			wantActed:   true,
			wantTargets: []string{"qwen3-30b", "gemma3-12b"},
			wantUnloads: 1,
		},
		{
			name: "nothing running means nothing is sent",
			fake: &fakeLlamaSwap{runningBody: `{"running":[]}`},
			// An unload kills in flight requests, so it must not be fired
			// speculatively at a server that is holding nothing.
			wantActed:   false,
			wantUnloads: 0,
		},
		{
			name:        "unload refused with HTTP 500",
			fake:        &fakeLlamaSwap{runningBody: twoRunning, unloadStatus: http.StatusInternalServerError, unloadBody: "boom"},
			wantErr:     true,
			wantUnloads: 1,
		},
		{
			name:        "unload answers an unexpected body",
			fake:        &fakeLlamaSwap{runningBody: twoRunning, unloadBody: `{"msg":"queued"}`},
			wantErr:     true,
			wantUnloads: 1,
		},
		{
			name:        "unload answers something that is not JSON",
			fake:        &fakeLlamaSwap{runningBody: twoRunning, unloadBody: "OK"},
			wantErr:     true,
			wantUnloads: 1,
		},
		{
			name:        "listing fails before anything is unloaded",
			fake:        &fakeLlamaSwap{runningStatus: http.StatusInternalServerError, runningBody: "boom"},
			wantErr:     true,
			wantUnloads: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := startLlamaSwap(t, tt.fake, 2*time.Second)

			result, err := a.Release(context.Background())
			unloads, legacy, requests := tt.fake.counts()

			if unloads != tt.wantUnloads {
				t.Errorf("unload was called %d time(s), want %d; requests: %v", unloads, tt.wantUnloads, requests)
			}
			if legacy {
				t.Error("the adapter used the legacy mutating GET /unload")
			}
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Release() succeeded, want an error; result = %+v", result)
				}
				return
			}
			if err != nil {
				t.Fatalf("Release() failed: %v", err)
			}
			if result.Acted != tt.wantActed {
				t.Errorf("Acted = %v, want %v", result.Acted, tt.wantActed)
			}
			if !reflect.DeepEqual(result.Targets, tt.wantTargets) {
				t.Errorf("Targets = %v, want %v", result.Targets, tt.wantTargets)
			}
			if result.Detail == "" {
				t.Error("Detail is empty, want an explanation of what happened")
			}
		})
	}
}

// TestLlamaSwapReleaseUsesDocumentedRoute pins the method and path, because the
// undocumented alternative is a GET that mutates state.
func TestLlamaSwapReleaseUsesDocumentedRoute(t *testing.T) {
	fake := &fakeLlamaSwap{runningBody: twoRunning}
	a := startLlamaSwap(t, fake, 2*time.Second)

	if _, err := a.Release(context.Background()); err != nil {
		t.Fatalf("Release() failed: %v", err)
	}

	_, legacy, requests := fake.counts()
	if legacy {
		t.Fatal("the adapter used the legacy mutating GET /unload")
	}
	want := "POST /api/models/unload"
	found := false
	for _, req := range requests {
		if req == want {
			found = true
		}
	}
	if !found {
		t.Errorf("requests = %v, want one to be %q", requests, want)
	}
}

func TestLlamaSwapAPIKey(t *testing.T) {
	t.Run("sent as a bearer token when the environment sets one", func(t *testing.T) {
		t.Setenv(envLlamaSwapAPIKey, "sekrit")

		fake := &fakeLlamaSwap{runningBody: twoRunning}
		a := startLlamaSwap(t, fake, 2*time.Second)

		if _, err := a.Probe(context.Background()); err != nil {
			t.Fatalf("Probe() failed: %v", err)
		}
		if _, err := a.Release(context.Background()); err != nil {
			t.Fatalf("Release() failed: %v", err)
		}

		// /running and the unload both sit behind CreateAuthMiddleware.
		for _, path := range []string{"/running", "/api/models/unload"} {
			if got := fake.auth(path); got != "Bearer sekrit" {
				t.Errorf("Authorization on %s = %q, want %q", path, got, "Bearer sekrit")
			}
		}
	})

	t.Run("absent when the environment sets none", func(t *testing.T) {
		t.Setenv(envLlamaSwapAPIKey, "")

		fake := &fakeLlamaSwap{runningBody: twoRunning}
		a := startLlamaSwap(t, fake, 2*time.Second)

		if _, err := a.Probe(context.Background()); err != nil {
			t.Fatalf("Probe() failed: %v", err)
		}
		if got := fake.auth("/running"); got != "" {
			t.Errorf("Authorization on /running = %q, want it unset", got)
		}
	})
}

func TestLlamaSwapStopIsNotSupported(t *testing.T) {
	fake := &fakeLlamaSwap{}
	a := startLlamaSwap(t, fake, 2*time.Second)

	result, err := a.Stop(context.Background())
	if err == nil {
		t.Fatalf("Stop() succeeded, want an error; result = %+v", result)
	}
	if !strings.Contains(err.Error(), ErrNotSupported.Error()) {
		t.Errorf("Stop() error = %q, want it to mention %q", err, ErrNotSupported)
	}
	if result.Acted {
		t.Error("Acted = true, want false")
	}
	if _, _, requests := fake.counts(); len(requests) != 0 {
		t.Errorf("Stop() made requests %v, want none", requests)
	}
}

func TestLlamaSwapIdentityAndCapabilities(t *testing.T) {
	a := newLlamaSwap(config.Service{
		Name:     "swap",
		Adapter:  config.AdapterLlamaSwap,
		Endpoint: "http://127.0.0.1:1",
		Timeout:  config.Duration(time.Second),
	})

	if a.Name() != "swap" {
		t.Errorf("Name() = %q, want %q", a.Name(), "swap")
	}
	if a.Kind() != config.AdapterLlamaSwap {
		t.Errorf("Kind() = %q, want %q", a.Kind(), config.AdapterLlamaSwap)
	}
	want := Capabilities{CanRelease: true, CanReportIdle: false, CanStop: false}
	if got := a.Capabilities(); got != want {
		t.Errorf("Capabilities() = %+v, want %+v", got, want)
	}
}

// TestLlamaSwapProbeHonoursCallerCancellation checks that an already cancelled
// caller context stops the adapter before it reaches the network.
func TestLlamaSwapProbeHonoursCallerCancellation(t *testing.T) {
	fake := &fakeLlamaSwap{runningBody: twoRunning}
	a := startLlamaSwap(t, fake, 2*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := a.Probe(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Probe() error = %v, want it to wrap context.Canceled", err)
	}
}
