package adapter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyprtuna/gpu-bouncer/internal/config"
)

// fakeOllama is a scriptable stand in for an Ollama server. Every mutating
// test in this package runs against one of these; nothing here ever contacts a
// real service.
type fakeOllama struct {
	mu sync.Mutex

	versionBody string
	versionCode int

	// psBodies is served one entry per call, the last entry repeating. That
	// lets a test show a model present on the first poll and gone on the next.
	psBodies []string
	psCalls  int
	psCode   int

	generateBody string
	generateCode int
	// generateRequests records the decoded body of every /api/generate call,
	// so a test can assert exactly what was asked of Ollama.
	generateRequests []map[string]any

	delay time.Duration
}

func (f *fakeOllama) counts() (ps, generate int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.psCalls, len(f.generateRequests)
}

func (f *fakeOllama) requests() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.generateRequests))
	copy(out, f.generateRequests)
	return out
}

func (f *fakeOllama) start(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		if f.delay > 0 {
			time.Sleep(f.delay)
		}
		code := f.versionCode
		if code == 0 {
			code = http.StatusOK
		}
		w.WriteHeader(code)
		body := f.versionBody
		if body == "" {
			body = `{"version":"0.33.2"}`
		}
		_, _ = io.WriteString(w, body)
	})

	mux.HandleFunc("/api/ps", func(w http.ResponseWriter, r *http.Request) {
		if f.delay > 0 {
			time.Sleep(f.delay)
		}
		f.mu.Lock()
		idx := f.psCalls
		f.psCalls++
		bodies := f.psBodies
		code := f.psCode
		f.mu.Unlock()

		if code == 0 {
			code = http.StatusOK
		}
		w.WriteHeader(code)
		body := `{"models":[]}`
		if len(bodies) > 0 {
			if idx >= len(bodies) {
				idx = len(bodies) - 1
			}
			body = bodies[idx]
		}
		_, _ = io.WriteString(w, body)
	})

	mux.HandleFunc("/api/generate", func(w http.ResponseWriter, r *http.Request) {
		if f.delay > 0 {
			time.Sleep(f.delay)
		}
		var decoded map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &decoded)
		f.mu.Lock()
		f.generateRequests = append(f.generateRequests, decoded)
		f.mu.Unlock()

		code := f.generateCode
		if code == 0 {
			code = http.StatusOK
		}
		w.WriteHeader(code)
		body := f.generateBody
		if body == "" {
			body = `{"model":"m","done":true,"done_reason":"unload"}`
		}
		_, _ = io.WriteString(w, body)
	})

	// Any unexpected path is a test failure, not a silent 404. An adapter
	// calling /api/embed to unload would otherwise slip through.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("adapter called unexpected endpoint %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newTestOllama builds an adapter pointed at a fake, with polling fast enough
// that the suite does not sleep.
func newTestOllama(url string, timeout time.Duration) *ollamaAdapter {
	a := newOllama(config.Service{
		Name:     "ollama",
		Adapter:  config.AdapterOllama,
		Endpoint: url,
		Timeout:  config.Duration(timeout),
	})
	a.drainInterval = time.Millisecond
	a.drainTimeout = 200 * time.Millisecond
	return a
}

const psTwoModels = `{"models":[
  {"name":"qwen3:8b","model":"qwen3:8b","size":6000000000,"size_vram":5368709120,
   "expires_at":"2026-08-30T12:00:00Z"},
  {"name":"nomic-embed:latest","model":"nomic-embed:latest","size":300000000,"size_vram":268435456,
   "expires_at":"2026-08-30T12:05:00Z"}
]}`

func TestOllamaProbe(t *testing.T) {
	fake := &fakeOllama{psBodies: []string{psTwoModels}}
	srv := fake.start(t)
	a := newTestOllama(srv.URL, 2*time.Second)

	status, err := a.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !status.Up {
		t.Error("Up = false, want true")
	}
	if got, want := status.Version, "0.33.2"; got != want {
		t.Errorf("Version = %q, want %q", got, want)
	}
	// 5368709120 bytes is 5120 MiB, 268435456 is 256 MiB.
	if got, want := status.HeldMiB, uint64(5376); got != want {
		t.Errorf("HeldMiB = %d, want %d", got, want)
	}
	if status.HeldEstimated {
		t.Error("HeldEstimated = true, want false: Ollama reports size_vram directly")
	}
	if got, want := len(status.Items), 2; got != want {
		t.Fatalf("len(Items) = %d, want %d", got, want)
	}
	if got, want := status.Items[0].Name, "qwen3:8b"; got != want {
		t.Errorf("Items[0].Name = %q, want %q", got, want)
	}
	if got, want := status.Items[0].VRAMMiB, uint64(5120); got != want {
		t.Errorf("Items[0].VRAMMiB = %d, want %d", got, want)
	}
	// size exceeds size_vram, so the spill must be reported rather than
	// silently folded into the VRAM figure.
	if !strings.Contains(status.Items[0].Detail, "spilled to host RAM") {
		t.Errorf("Items[0].Detail = %q, want it to mention the host RAM spill", status.Items[0].Detail)
	}
	if !strings.Contains(status.Items[0].Detail, "expires") {
		t.Errorf("Items[0].Detail = %q, want it to mention expiry", status.Items[0].Detail)
	}
	// Ollama exposes no in flight request count.
	if status.IdleKnown {
		t.Error("IdleKnown = true, want false: Ollama cannot report whether it is busy")
	}
}

func TestOllamaProbeNothingLoaded(t *testing.T) {
	fake := &fakeOllama{psBodies: []string{`{"models":[]}`}}
	srv := fake.start(t)
	status, err := newTestOllama(srv.URL, 2*time.Second).Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !status.Up {
		t.Error("Up = false, want true: an idle Ollama is still up")
	}
	if status.HeldMiB != 0 || len(status.Items) != 0 {
		t.Errorf("HeldMiB = %d, Items = %v, want 0 and none", status.HeldMiB, status.Items)
	}
}

// A release must unload every loaded model, and the request it sends is the
// part most easily got wrong: the unload branch fires only for keep_alive as
// the JSON number 0 with no prompt, and only on /api/generate.
func TestOllamaReleaseSendsTheUnloadRequest(t *testing.T) {
	fake := &fakeOllama{psBodies: []string{psTwoModels, `{"models":[]}`}}
	srv := fake.start(t)
	a := newTestOllama(srv.URL, 2*time.Second)

	result, err := a.Release(context.Background())
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !result.Acted {
		t.Error("Acted = false, want true")
	}
	want := []string{"qwen3:8b", "nomic-embed:latest"}
	if got := result.Targets; len(got) != len(want) {
		t.Fatalf("Targets = %v, want %v", got, want)
	}
	for i := range want {
		if result.Targets[i] != want[i] {
			t.Fatalf("Targets = %v, want %v", result.Targets, want)
		}
	}

	requests := fake.requests()
	if got, want := len(requests), 2; got != want {
		t.Fatalf("generate calls = %d, want %d", got, want)
	}
	for i, req := range requests {
		keepAlive, present := req["keep_alive"]
		if !present {
			t.Fatalf("request %d has no keep_alive: without it no unload branch is taken: %v", i, req)
		}
		// encoding/json decodes every JSON number into float64. A string "0"
		// would arrive as a string here, and Ollama would reject it.
		number, isNumber := keepAlive.(float64)
		if !isNumber {
			t.Fatalf("request %d keep_alive = %#v (%T), want the JSON number 0", i, keepAlive, keepAlive)
		}
		if number != 0 {
			t.Fatalf("request %d keep_alive = %v, want 0: a negative value means forever", i, number)
		}
		if prompt, ok := req["prompt"].(string); ok && prompt != "" {
			t.Fatalf("request %d sent prompt %q: any prompt text loads the model instead of unloading it", i, prompt)
		}
		if req["model"] != want[i] {
			t.Errorf("request %d model = %v, want %q", i, req["model"], want[i])
		}
	}
}

// Nothing loaded means nothing to do, and no request should be sent at all.
func TestOllamaReleaseWithNothingLoaded(t *testing.T) {
	fake := &fakeOllama{psBodies: []string{`{"models":[]}`}}
	srv := fake.start(t)

	result, err := newTestOllama(srv.URL, 2*time.Second).Release(context.Background())
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if result.Acted {
		t.Error("Acted = true, want false: nothing was loaded")
	}
	if _, generate := fake.counts(); generate != 0 {
		t.Errorf("sent %d generate calls, want 0", generate)
	}
	if !strings.Contains(result.Detail, "no models") {
		t.Errorf("Detail = %q, want it to explain that nothing was loaded", result.Detail)
	}
}

// The same endpoint returns 200 for real generation, so a 200 alone proves
// nothing. If done_reason is not "unload" the request did something else, and
// reporting success would be a lie.
func TestOllamaReleaseRejectsWrongDoneReason(t *testing.T) {
	fake := &fakeOllama{
		psBodies:     []string{psTwoModels},
		generateBody: `{"model":"qwen3:8b","done":true,"done_reason":"stop","response":"hello"}`,
	}
	srv := fake.start(t)

	_, err := newTestOllama(srv.URL, 2*time.Second).Release(context.Background())
	if err == nil {
		t.Fatal("Release reported success for a response that was not an unload")
	}
	if !strings.Contains(err.Error(), "done_reason") {
		t.Errorf("error = %q, want it to name done_reason", err)
	}
}

// When a model will not drain, the adapter must say so rather than claim the
// VRAM is back.
func TestOllamaReleaseReportsUndrainedModels(t *testing.T) {
	fake := &fakeOllama{psBodies: []string{psTwoModels}} // never empties
	srv := fake.start(t)

	result, err := newTestOllama(srv.URL, 2*time.Second).Release(context.Background())
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !result.Acted {
		t.Error("Acted = false, want true: the unload requests were accepted")
	}
	if !strings.Contains(result.Detail, "still listed") {
		t.Errorf("Detail = %q, want it to admit the models were still listed", result.Detail)
	}
}

func TestOllamaFailures(t *testing.T) {
	tests := []struct {
		name    string
		fake    *fakeOllama
		timeout time.Duration
		wantErr string
	}{
		{
			name:    "version refuses",
			fake:    &fakeOllama{versionCode: http.StatusInternalServerError, versionBody: "boom"},
			wantErr: "HTTP 500",
		},
		{
			name:    "ps refuses",
			fake:    &fakeOllama{psCode: http.StatusInternalServerError, psBodies: []string{"boom"}},
			wantErr: "HTTP 500",
		},
		{
			name:    "version returns garbage",
			fake:    &fakeOllama{versionBody: "<html>not json</html>"},
			wantErr: "not the expected JSON",
		},
		{
			name:    "ps returns garbage",
			fake:    &fakeOllama{psBodies: []string{"}{"}},
			wantErr: "not the expected JSON",
		},
		{
			name:    "ps returns the wrong shape",
			fake:    &fakeOllama{psBodies: []string{`{"models":"lots"}`}},
			wantErr: "not the expected JSON",
		},
		{
			name:    "server is too slow",
			fake:    &fakeOllama{delay: 300 * time.Millisecond},
			timeout: 30 * time.Millisecond,
			wantErr: "context deadline exceeded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := tt.fake.start(t)
			timeout := tt.timeout
			if timeout == 0 {
				timeout = 2 * time.Second
			}
			_, err := newTestOllama(srv.URL, timeout).Probe(context.Background())
			if err == nil {
				t.Fatalf("Probe succeeded, want an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
			// Every error must name the service, so a multi service log line
			// is attributable.
			if !strings.Contains(err.Error(), "ollama") {
				t.Errorf("error = %q, want it to name the service", err)
			}
		})
	}
}

// An unreachable service is a plain error, not a zero valued success.
func TestOllamaProbeUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	url := srv.URL
	srv.Close() // nothing is listening now

	status, err := newTestOllama(url, 200*time.Millisecond).Probe(context.Background())
	if err == nil {
		t.Fatal("Probe succeeded against a closed server")
	}
	if status.Up {
		t.Error("Up = true alongside an error")
	}
}

func TestOllamaCapabilitiesAndStop(t *testing.T) {
	a := newTestOllama("http://127.0.0.1:1", time.Second)
	caps := a.Capabilities()
	if !caps.CanRelease {
		t.Error("CanRelease = false, want true")
	}
	if caps.CanReportIdle {
		t.Error("CanReportIdle = true, want false")
	}
	if caps.CanStop {
		t.Error("CanStop = true, want false")
	}
	if got, want := a.Kind(), config.AdapterOllama; got != want {
		t.Errorf("Kind = %q, want %q", got, want)
	}
	if _, err := a.Stop(context.Background()); err == nil {
		t.Error("Stop succeeded, want ErrNotSupported")
	}
}

func TestBytesToMiB(t *testing.T) {
	cases := []struct {
		in   uint64
		want uint64
	}{
		{0, 0},
		{1, 1}, // a small holding must not display as nothing
		{1024, 1},
		{1024 * 1024, 1},
		{5368709120, 5120},
	}
	for _, c := range cases {
		if got := bytesToMiB(c.in); got != c.want {
			t.Errorf("bytesToMiB(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
