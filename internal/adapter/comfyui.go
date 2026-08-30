package adapter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/hyprtuna/gpu-bouncer/internal/config"
)

// comfyUIAdapter drives ComfyUI through its public HTTP API.
//
// Endpoints verified against comfyanonymous/ComfyUI at 0.34.0:
//
//	GET  /api/system_stats  server.py:686-737, the only VRAM accounting exposed
//	GET  /api/prompt        server.py:747, returns exec_info.queue_remaining
//	POST /api/free          server.py:1192-1201, sets two queue flags
//
// Every non static route is registered twice, once bare and once under /api
// (server.py:1233-1240). The /api form is used throughout because the bare
// paths share a namespace with the static frontend handler mounted at /
// (server.py:1279-1281).
//
// Two properties of ComfyUI shape this adapter, and both cost accuracy:
//
//   - There is no model level inventory over HTTP. current_loaded_models lives
//     entirely inside comfy/model_management.py and is never serialised, so the
//     adapter can report bytes and nothing else. Ollama's per model Items have
//     no equivalent here.
//   - POST /free does not free anything. The handler only sets flags on the
//     PromptQueue; the executor thread reads them at main.py:421, which is
//     reached only after the blocking e.execute() call at main.py:393 returns.
//     On a busy server the request returns 200 and frees nothing until the
//     running prompt finishes. The 200 carries no information at all, so
//     Release refuses rather than reporting a release that did not happen.
//
// Stock ComfyUI has no authentication on any of these routes (server.py:230-248
// registers no authenticator), so no credentials are sent. In particular no
// Origin header is set: create_origin_only_middleware (server.py:159-197) is a
// pass through for a client that sends none, but answers 403 to one whose
// Origin does not match Host.
type comfyUIAdapter struct {
	name    string
	base    string
	timeout time.Duration
	client  *http.Client

	// gpuIndex is the torch device ordinal whose memory is attributed to this
	// service. It is policy.gpu_index: v0.1 assumes the torch device ordinal
	// ComfyUI reports equals the NVML index gpu-bouncer arbitrates, which
	// holds on a single NVIDIA card and on a multi card machine where
	// CUDA_VISIBLE_DEVICES has not reordered the devices for ComfyUI.
	gpuIndex int
}

func newComfyUI(svc config.Service, gpuIndex int) *comfyUIAdapter {
	return &comfyUIAdapter{
		name:     svc.Name,
		base:     svc.Endpoint,
		timeout:  svc.Timeout.D(),
		client:   newHTTPClient(),
		gpuIndex: gpuIndex,
	}
}

func (a *comfyUIAdapter) Name() string             { return a.name }
func (a *comfyUIAdapter) Kind() config.AdapterKind { return config.AdapterComfyUI }

func (a *comfyUIAdapter) Capabilities() Capabilities {
	return Capabilities{
		CanRelease: true,
		// queue_remaining is len(pending) + len(running) (execution.py:1342), so
		// zero is a complete idleness test rather than an approximation of one.
		CanReportIdle: true,
		CanStop:       false,
	}
}

// comfyUISystemStats is the part of the /api/system_stats response this adapter
// reads. Field names are from server.py:686-737 and are unchanged from tag
// v0.3.27 through master, so this shape decodes across that whole range.
type comfyUISystemStats struct {
	System struct {
		Version string `json:"comfyui_version"`
	} `json:"system"`
	Devices []comfyUIDevice `json:"devices"`
}

type comfyUIDevice struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Index is d.index, the torch device ordinal. It is JSON null for devices
	// constructed without one, notably cpu, so it is a pointer: decoding into
	// an int would turn every such device into a match for GPU 0.
	Index *int `json:"index"`
	// TorchVRAMTotal is torch's reserved bytes, and TorchVRAMFree is reserved
	// minus active. Their difference is what live tensors pin. The whole device
	// figures (vram_total, vram_free) are deliberately not read here: they
	// include every other process on the card and are the driver's job to
	// report, not this service's.
	TorchVRAMTotal uint64 `json:"torch_vram_total"`
	TorchVRAMFree  uint64 `json:"torch_vram_free"`
}

// comfyUIPrompt is the /api/prompt response (server.py:747, server.py:1283).
type comfyUIPrompt struct {
	ExecInfo struct {
		QueueRemaining int `json:"queue_remaining"`
	} `json:"exec_info"`
}

// comfyUIFree is the /api/free request body. Both keys are sent explicitly as
// true. The handler reads unload_models and free_memory and nothing else, and
// the worker's lookup is flags.get("unload_models", free_memory)
// (main.py:423), so a single key form depends on that default. Sending both is
// the one form whose meaning does not.
type comfyUIFree struct {
	UnloadModels bool `json:"unload_models"`
	FreeMemory   bool `json:"free_memory"`
}

func (a *comfyUIAdapter) Probe(ctx context.Context) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	var stats comfyUISystemStats
	if err := doJSON(ctx, a.client, http.MethodGet, a.base+"/api/system_stats", nil, &stats, nil); err != nil {
		return Status{}, fmt.Errorf("%s: %w", a.name, err)
	}
	// Every version of the handler emits at least one device. An empty array
	// means the body decoded but is not a system_stats response, and treating
	// that as "up, holding nothing" would be a zero valued success.
	if len(stats.Devices) == 0 {
		return Status{}, fmt.Errorf("%s: /api/system_stats returned no devices", a.name)
	}

	status := Status{
		Up:      true,
		Version: stats.System.Version,
		// The held figure is torch_vram_total - torch_vram_free, an allocator
		// level proxy rather than a per model figure ComfyUI reported, so it is
		// flagged as derived whether or not the device was found.
		HeldEstimated: true,
	}

	if dev, ok := a.device(stats.Devices); ok {
		// Cached free above reserved is not a state torch produces, but the
		// subtraction is on uint64: an inverted pair would underflow into a
		// multi exabyte figure that would dominate every arbitration decision.
		var active uint64
		if dev.TorchVRAMFree < dev.TorchVRAMTotal {
			active = dev.TorchVRAMTotal - dev.TorchVRAMFree
		}
		status.HeldMiB = bytesToMiB(active)
		// One Item for the device rather than none. There is no per model
		// inventory to list, but an empty Items on a service holding gigabytes
		// reads as "holding nothing" in status output, which is the more
		// misleading of the two. The Item names the torch allocation for what
		// it is and its Detail says what a cache empty alone would recover.
		status.Items = []Item{{
			Name:    dev.Name,
			VRAMMiB: status.HeldMiB,
			Detail: fmt.Sprintf("torch allocator on %s device %d, %d MiB reclaimable by cache empty alone; ComfyUI reports no per model inventory",
				dev.Type, a.gpuIndex, bytesToMiB(dev.TorchVRAMFree)),
		}}
	}
	// No match leaves HeldMiB at 0 with no Items. ComfyUI enumerates only the
	// torch devices it can see, so a missing ordinal means it is not using that
	// card and zero is the truthful answer. Falling back to devices[0], which
	// the handler forces to be the primary device (server.py:697-704), would
	// attribute another card's memory to this one and could get ComfyUI evicted
	// over VRAM it does not hold.

	remaining, err := a.queueRemaining(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("%s: %w", a.name, err)
	}
	status.Idle = remaining == 0
	status.IdleKnown = true
	return status, nil
}

// device returns the entry whose index matches the arbitrated ordinal. A nil
// Index is the JSON null case and never matches.
func (a *comfyUIAdapter) device(devices []comfyUIDevice) (comfyUIDevice, bool) {
	for _, d := range devices {
		if d.Index != nil && *d.Index == a.gpuIndex {
			return d, true
		}
	}
	return comfyUIDevice{}, false
}

// queueRemaining reads exec_info.queue_remaining. It counts pending plus
// running (get_tasks_remaining, execution.py:1342), so zero means idle. It is
// used in preference to GET /api/queue, which serialises the full node graph of
// every queued prompt to answer the same question.
func (a *comfyUIAdapter) queueRemaining(ctx context.Context) (int, error) {
	var prompt comfyUIPrompt
	if err := doJSON(ctx, a.client, http.MethodGet, a.base+"/api/prompt", nil, &prompt, nil); err != nil {
		return 0, err
	}
	return prompt.ExecInfo.QueueRemaining, nil
}

// Release asks ComfyUI to unload all models and empty the torch cache, but only
// when the server is idle.
//
// The idleness gate is the point of this method. POST /free sets flags that the
// executor thread reads at main.py:421, after the blocking execute call at
// main.py:393 returns, so on a busy server it returns 200 and frees nothing
// until the running prompt ends. Reporting that 200 as a release would hand the
// VRAM budget to another service which would then OOM against weights ComfyUI
// still has resident.
//
// The alternative, interrupting the running prompt and then freeing, kills a
// user's in flight generation. That is a policy decision for the daemon, not
// something this adapter does on its own.
//
// There is a race this adapter cannot close: nothing stops a user queueing a
// prompt between the check and the free. ComfyUI offers no lock, lease or drain
// mode. The mitigation is to keep the window to two back to back requests with
// no work in between, and to let the daemon confirm the outcome from its own
// before and after VRAM measurement.
func (a *comfyUIAdapter) Release(ctx context.Context) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	remaining, err := a.queueRemaining(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", a.name, err)
	}
	if remaining > 0 {
		return Result{
			Acted:  false,
			Detail: fmt.Sprintf("not attempted: %d item(s) queued or running, and ComfyUI defers a free until the current prompt finishes, so the request would have been silently deferred", remaining),
		}, nil
	}

	body := comfyUIFree{UnloadModels: true, FreeMemory: true}
	// out is nil: the handler answers web.Response(status=200) with an empty
	// body (server.py:1201), and asking doJSON to decode it would turn every
	// successful free into a JSON error. A non 2xx is still a failure, which
	// includes the 500 the handler raises on a body it cannot parse, since
	// await request.json() there is unguarded.
	if err := doJSON(ctx, a.client, http.MethodPost, a.base+"/api/free", body, nil, nil); err != nil {
		return Result{}, fmt.Errorf("%s: %w", a.name, err)
	}

	// Targets is left empty on purpose. ComfyUI unloads everything on every
	// visible torch device (unload_all_models, model_management.py:2063) and
	// names none of it, so there is nothing truthful to list.
	return Result{
		Acted:  true,
		Detail: "asked ComfyUI to unload all models and empty the torch cache while idle; the effect lands on the executor thread, so the freed amount is whatever the daemon's before and after VRAM figures show",
	}, nil
}

// Stop is not available: ComfyUI has no shutdown endpoint, and this adapter
// does not manage the process. Configure a separate systemd-unit service if
// stopping is wanted.
func (a *comfyUIAdapter) Stop(context.Context) (Result, error) {
	return Result{}, errors.New("comfyui adapter cannot stop the process: " + ErrNotSupported.Error())
}
