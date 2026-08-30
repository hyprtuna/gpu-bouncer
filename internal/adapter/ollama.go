package adapter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hyprtuna/gpu-bouncer/internal/config"
)

// ollamaAdapter drives Ollama through its public HTTP API.
//
// Endpoints verified against ollama/ollama at v0.33.2:
//
//	GET  /api/version   server/routes.go, returns {"version":"0.33.2"}
//	GET  /api/ps        server/routes.go PsHandler, response type api.ProcessResponse
//	                    in api/types.go, whose per model VRAM field is size_vram
//	POST /api/generate   server/routes.go:403, the unload short circuit
//
// Ollama has no native unload endpoint at 0.33.x. The route block at
// server/routes.go:1841-1928 contains nothing of the kind, and
// unloadAllRunners() is reachable only on shutdown. The supported way to make
// a model release VRAM is the keep_alive short circuit: a generate request
// with an empty prompt and keep_alive set to the JSON number 0.
//
// Three properties of that short circuit shape this adapter:
//
//   - The branch exists only on /api/generate (routes.go:403) and /api/chat
//     (routes.go:2505). /api/embed and /api/embeddings have no such branch and
//     would load the model instead, so they are never used here.
//   - keep_alive must be the JSON number 0. The string "0" is a parse error and
//     any negative value means forever.
//   - HTTP 200 means expiry was scheduled, not that VRAM is free. expireRunner
//     in server/sched.go returns immediately and defers indefinitely while a
//     request is in flight, reporting no error. So the adapter verifies
//     done_reason and then waits for the model to leave /api/ps.
type ollamaAdapter struct {
	name    string
	base    string
	timeout time.Duration
	client  *http.Client

	// drainInterval and drainTimeout bound the wait for an unloaded model to
	// leave /api/ps. Fields rather than constants so tests need not sleep.
	drainInterval time.Duration
	drainTimeout  time.Duration
}

func newOllama(svc config.Service) *ollamaAdapter {
	return &ollamaAdapter{
		name:          svc.Name,
		base:          svc.Endpoint,
		timeout:       svc.Timeout.D(),
		client:        newHTTPClient(),
		drainInterval: 250 * time.Millisecond,
		drainTimeout:  30 * time.Second,
	}
}

func (a *ollamaAdapter) Name() string             { return a.name }
func (a *ollamaAdapter) Kind() config.AdapterKind { return config.AdapterOllama }

func (a *ollamaAdapter) Capabilities() Capabilities {
	return Capabilities{
		CanRelease: true,
		// Ollama exposes no in flight request count, so gpu-bouncer cannot
		// tell a busy server from an idle one. Claiming otherwise would let
		// the scheduler believe an unload had taken effect when it was merely
		// deferred behind a running generation.
		CanReportIdle: false,
		CanStop:       false,
	}
}

// ollamaVersion is the /api/version response.
type ollamaVersion struct {
	Version string `json:"version"`
}

// ollamaPS is the /api/ps response. Field names are from api.ProcessResponse
// in ollama/api/types.go. size_vram is the VRAM figure; size includes any part
// of the model spilled to host RAM and must not be used for a VRAM budget.
type ollamaPS struct {
	Models []ollamaPSModel `json:"models"`
}

type ollamaPSModel struct {
	Name      string    `json:"name"`
	Model     string    `json:"model"`
	Size      uint64    `json:"size"`
	SizeVRAM  uint64    `json:"size_vram"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ollamaGenerateResponse is the part of the generate response this adapter
// reads. done_reason is "unload" when the keep_alive short circuit fired; any
// other value means the request was treated as real work.
type ollamaGenerateResponse struct {
	Done       bool   `json:"done"`
	DoneReason string `json:"done_reason"`
}

func (a *ollamaAdapter) Probe(ctx context.Context) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	var version ollamaVersion
	if err := doJSON(ctx, a.client, http.MethodGet, a.base+"/api/version", nil, &version, nil); err != nil {
		return Status{}, fmt.Errorf("%s: %w", a.name, err)
	}

	status := Status{Up: true, Version: version.Version}
	loaded, err := a.loaded(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("%s: %w", a.name, err)
	}
	for _, m := range loaded {
		item := Item{Name: m.Name, VRAMMiB: bytesToMiB(m.SizeVRAM)}
		if spill := m.Size - m.SizeVRAM; m.Size > m.SizeVRAM {
			item.Detail = fmt.Sprintf("%d MiB spilled to host RAM", bytesToMiB(spill))
		}
		if !m.ExpiresAt.IsZero() {
			if item.Detail != "" {
				item.Detail += ", "
			}
			item.Detail += "expires " + m.ExpiresAt.Format(time.RFC3339)
		}
		status.Items = append(status.Items, item)
		status.HeldMiB += item.VRAMMiB
	}
	return status, nil
}

func (a *ollamaAdapter) loaded(ctx context.Context) ([]ollamaPSModel, error) {
	var ps ollamaPS
	if err := doJSON(ctx, a.client, http.MethodGet, a.base+"/api/ps", nil, &ps, nil); err != nil {
		return nil, err
	}
	return ps.Models, nil
}

// Release unloads every loaded model, one at a time. Sequential rather than
// concurrent, because each unload triggers Ollama's own VRAM recovery wait and
// overlapping them makes its scheduler's accounting worse.
func (a *ollamaAdapter) Release(ctx context.Context) (Result, error) {
	listCtx, cancel := context.WithTimeout(ctx, a.timeout)
	models, err := a.loaded(listCtx)
	cancel()
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", a.name, err)
	}
	if len(models) == 0 {
		return Result{Acted: false, Detail: "no models were loaded"}, nil
	}

	result := Result{}
	for _, m := range models {
		if err := a.unload(ctx, m.Name); err != nil {
			// Report the models already unloaded alongside the failure, so a
			// partial success is never presented as a total one.
			return result, fmt.Errorf("%s: unload %s: %w", a.name, m.Name, err)
		}
		result.Acted = true
		result.Targets = append(result.Targets, m.Name)
	}

	drained, err := a.waitDrained(ctx, result.Targets)
	switch {
	case err != nil:
		return result, fmt.Errorf("%s: %w", a.name, err)
	case !drained:
		result.Detail = fmt.Sprintf("unloaded %s, but they were still listed after %s",
			strings.Join(result.Targets, ", "), a.drainTimeout)
	default:
		result.Detail = "unloaded " + strings.Join(result.Targets, ", ")
	}
	return result, nil
}

// unload asks Ollama to expire one model now.
func (a *ollamaAdapter) unload(ctx context.Context, model string) error {
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	// prompt is deliberately absent. Any prompt text would fall through to
	// real inference and load the model rather than unload it. keep_alive is
	// the JSON number 0, which is the only value that means "expire now".
	body := map[string]any{"model": model, "keep_alive": 0}

	var resp ollamaGenerateResponse
	if err := doJSON(ctx, a.client, http.MethodPost, a.base+"/api/generate", body, &resp, nil); err != nil {
		return err
	}
	// A 200 alone is not enough. The same endpoint returns 200 for real
	// generation, so the unload branch has to be confirmed by done_reason.
	if resp.DoneReason != "unload" {
		return fmt.Errorf(`expected done_reason "unload", got %q: the request was not treated as an unload`, resp.DoneReason)
	}
	return nil
}

// waitDrained polls /api/ps until none of the named models is listed. It
// reports whether they drained, and errors only if the polling itself failed.
//
// Draining is necessary but not sufficient evidence that VRAM is free: Ollama
// removes a model from its loaded set before waitForVRAMRecovery finishes. The
// daemon therefore reports the driver's measured before and after figures and
// does not treat this as proof.
func (a *ollamaAdapter) waitDrained(ctx context.Context, names []string) (bool, error) {
	want := make(map[string]struct{}, len(names))
	for _, n := range names {
		want[n] = struct{}{}
	}

	deadline := time.Now().Add(a.drainTimeout)
	ticker := time.NewTicker(a.drainInterval)
	defer ticker.Stop()

	for {
		listCtx, cancel := context.WithTimeout(ctx, a.timeout)
		models, err := a.loaded(listCtx)
		cancel()
		if err != nil {
			return false, fmt.Errorf("polling for unload: %w", err)
		}
		stillThere := false
		for _, m := range models {
			if _, ok := want[m.Name]; ok {
				stillThere = true
				break
			}
		}
		if !stillThere {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-ticker.C:
		}
	}
}

// Stop is not available: Ollama is not managed at the process level by this
// adapter. Configure a separate systemd-unit service if stopping is wanted.
func (a *ollamaAdapter) Stop(context.Context) (Result, error) {
	return Result{}, errors.New("ollama adapter cannot stop the process: " + ErrNotSupported.Error())
}
