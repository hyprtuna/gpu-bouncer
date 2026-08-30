package adapter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hyprtuna/gpu-bouncer/internal/config"
)

// envLlamaSwapAPIKey holds an optional llama-swap API key. It is an
// environment variable rather than a config key so that a secret never has to
// be written into a file that lives in /etc and is read by every gpu-bouncer
// invocation.
const envLlamaSwapAPIKey = "GPU_BOUNCER_LLAMA_SWAP_API_KEY"

// llamaSwapAdapter drives llama-swap through its management HTTP API.
//
// Endpoints verified against mostlygeek/llama-swap at v250, whose route
// registrations are byte identical at v251:
//
//	GET  /health              internal/server/server.go:288, plain text "OK"
//	GET  /running             server.go:301, handler internal/server/api.go:333,
//	                          returns {"running":[{"model","state","cmd","ttl"}]}
//	POST /api/models/unload   server.go:320, handler apigroup.go:137, returns
//	                          {"msg":"ok"}
//
// Three properties of that API shape this adapter:
//
//   - /health is registered with mux.HandleFunc, outside the apiChain that
//     carries CreateAuthMiddleware, so it answers whether or not an API key is
//     configured. Every other route used here sits behind that chain, which is
//     a pure pass through only while cfg.RequiredAPIKeys is empty. Set
//     GPU_BOUNCER_LLAMA_SWAP_API_KEY when the server does have keys, or every
//     inventory and release call becomes a 401.
//   - Neither /running nor /v1/models carries a memory figure of any kind, so
//     this adapter cannot attribute VRAM to llama-swap. See Probe.
//   - The unload is synchronous. baseRouter.Unload, internal/router/base.go:399,
//     blocks until every targeted child process has stopped. That is the
//     opposite of Ollama's expireRunner, which returns before the memory is
//     back and forces the Ollama adapter to poll: here a 200 means the
//     processes are already gone, so Release needs no drain loop. The flip side
//     is that Unload does not wait for in flight requests, it kills them, so a
//     release is disruptive to anything mid generation.
//
// GET /unload is a fourth route with the same effect as the POST. It is
// deliberately never used: the README does not document it, and it mutates
// state on a GET, so anything that follows links can trigger it by accident.
type llamaSwapAdapter struct {
	name    string
	base    string
	timeout time.Duration
	client  *http.Client

	// apiKey is sent as a bearer token when set. It is read once, at
	// construction, because the daemon builds its adapters at startup and a key
	// that changed under a running daemon would apply to only half its calls.
	apiKey string

	// unloadTimeout bounds the unload call on its own, because that one call
	// blocks for as long as the slowest model's configured unloadTimeout. Held
	// to the ordinary per request budget it would report a failure for an
	// unload that was in fact proceeding. A field rather than a constant so
	// tests need not wait on it.
	unloadTimeout time.Duration
}

func newLlamaSwap(svc config.Service) *llamaSwapAdapter {
	return &llamaSwapAdapter{
		name:          svc.Name,
		base:          svc.Endpoint,
		timeout:       svc.Timeout.D(),
		client:        newHTTPClient(),
		apiKey:        os.Getenv(envLlamaSwapAPIKey),
		unloadTimeout: 60 * time.Second,
	}
}

func (a *llamaSwapAdapter) Name() string             { return a.name }
func (a *llamaSwapAdapter) Kind() config.AdapterKind { return config.AdapterLlamaSwap }

func (a *llamaSwapAdapter) Capabilities() Capabilities {
	return Capabilities{
		CanRelease: true,
		// /running reports a lifecycle state, not activity. "ready" means the
		// upstream process is up and accepting work, and it reads the same
		// whether that process is halfway through a generation or has been
		// sitting untouched for an hour. An empty list would be a sound "there
		// is nothing loaded", but that is already visible as an empty item
		// list, and the case where idleness would actually change a decision is
		// exactly the case llama-swap cannot answer. Release here kills in
		// flight requests, so a wrong idle claim is not a cosmetic error.
		CanReportIdle: false,
		CanStop:       false,
	}
}

// llamaSwapRunning is the /running response. The handler preallocates the slice
// with make, so the field is always a JSON array and never null.
type llamaSwapRunning struct {
	Running []llamaSwapProcess `json:"running"`
}

// llamaSwapProcess is one entry of /running, from type runningModel in
// internal/server/api.go:313. There is no VRAM field: ttl is the configured
// auto unload delay in seconds, not a memory figure.
type llamaSwapProcess struct {
	Model string `json:"model"`
	// State is one of starting, ready or stopping. RunningModels filters out
	// stopped and shutdown before the handler sees them.
	State string `json:"state"`
	TTL   int    `json:"ttl"`
}

// llamaSwapUnloadResponse is the POST /api/models/unload response.
type llamaSwapUnloadResponse struct {
	Msg string `json:"msg"`
}

func (a *llamaSwapAdapter) Probe(ctx context.Context) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	// /health rather than /api/version as the liveness check: it is the only
	// route outside the auth chain, which keeps "the service is down" and "the
	// API key is wrong" distinguishable. The first fails here, the second gets
	// past here and fails on /running with a 401.
	if _, err := doText(ctx, a.client, http.MethodGet, a.base+"/health", nil, a.authHeader()); err != nil {
		return Status{}, fmt.Errorf("%s: %w", a.name, err)
	}

	// Version is left empty on purpose. /api/version is behind the auth chain,
	// and a display only field is not worth giving a probe another way to fail.
	status := Status{Up: true}

	running, err := a.running(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("%s: %w", a.name, err)
	}
	for _, p := range running {
		// VRAMMiB stays 0 for the same reason HeldMiB does, below.
		status.Items = append(status.Items, Item{Name: p.Model, Detail: "state " + p.State})
	}

	// HeldMiB is 0 and HeldEstimated says that the 0 is an absence of data
	// rather than a measurement. llama-swap reports no VRAM figure anywhere:
	// not in /running, not in /v1/models, not at all. Attributing VRAM to it
	// would mean matching the cmd string of each upstream against the driver's
	// per process accounting through NVML, which v0.1 does not do. A wrong
	// guess here would feed straight into an eviction decision, so the adapter
	// says nothing instead.
	status.HeldEstimated = true
	return status, nil
}

func (a *llamaSwapAdapter) running(ctx context.Context) ([]llamaSwapProcess, error) {
	var out llamaSwapRunning
	if err := doJSON(ctx, a.client, http.MethodGet, a.base+"/running", nil, &out, a.authHeader()); err != nil {
		return nil, err
	}
	return out.Running, nil
}

// Release stops every local upstream process in one call.
//
// Unlike the Ollama adapter there is no poll afterwards: baseRouter.Unload
// blocks until each process has stopped, so the response is the confirmation.
// Peer models are remote and are unaffected by any unload.
func (a *llamaSwapAdapter) Release(ctx context.Context) (Result, error) {
	listCtx, cancel := context.WithTimeout(ctx, a.timeout)
	running, err := a.running(listCtx)
	cancel()
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", a.name, err)
	}
	if len(running) == 0 {
		// Not merely pointless. An unload kills in flight requests, so firing
		// one at a server that is holding nothing is all disruption and no
		// reclaim.
		return Result{Acted: false, Detail: "no models were running"}, nil
	}

	targets := make([]string, 0, len(running))
	for _, p := range running {
		targets = append(targets, p.Model)
	}

	// The bulk route rather than one POST per model: releasing means handing
	// back the whole allocation, and Unload buckets its targets by configured
	// timeout smallest first, so one slow model cannot hold up the quick ones.
	unloadCtx, cancel := context.WithTimeout(ctx, a.unloadTimeout)
	defer cancel()

	var resp llamaSwapUnloadResponse
	if err := doJSON(unloadCtx, a.client, http.MethodPost, a.base+"/api/models/unload", nil, &resp, a.authHeader()); err != nil {
		return Result{}, fmt.Errorf("%s: unload all: %w", a.name, err)
	}
	// The handler writes {"msg":"ok"} unconditionally on its success path, so
	// anything else means the response came from somewhere other than the
	// handler this adapter thinks it called: a proxy in front, or a version
	// that moved the route.
	if resp.Msg != "ok" {
		return Result{}, fmt.Errorf("%s: unload all: expected msg \"ok\", got %q: the request did not reach llama-swap's unload handler", a.name, resp.Msg)
	}

	return Result{
		Acted:   true,
		Targets: targets,
		Detail:  "unloaded " + strings.Join(targets, ", "),
	}, nil
}

// authHeader returns the credential for a management call, or nil when no key
// is configured. Bearer is used because it is the form llama-swap's own doc
// comment states plainly; x-api-key would also be accepted.
func (a *llamaSwapAdapter) authHeader() map[string]string {
	if a.apiKey == "" {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + a.apiKey}
}

// Stop is not available: llama-swap is not managed at the process level by this
// adapter. Configure a separate systemd-unit service if stopping is wanted.
func (a *llamaSwapAdapter) Stop(context.Context) (Result, error) {
	return Result{}, errors.New("llama-swap adapter cannot stop the process: " + ErrNotSupported.Error())
}
