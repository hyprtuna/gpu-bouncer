// Package adapter talks to one AI service through that service's own API.
//
// Every adapter obeys the same contract:
//
//   - Probe is read only and safe to run anywhere, including against a service
//     gpu-bouncer has no permission to touch.
//   - Release asks the service, over its own public API, to give up VRAM. It is
//     the same request any client of that service could make, so it needs no
//     special permission.
//   - Stop is process level action. It is only reachable for a service whose
//     config sets allow_stop = true, and the daemon enforces that before it
//     ever calls this method.
//
// An adapter reports what it does not know rather than filling in a plausible
// number. HeldEstimated and IdleKnown exist so the scheduler can tell a
// measurement from a guess.
package adapter

import (
	"context"
	"errors"
	"fmt"

	"github.com/hyprtuna/gpu-bouncer/internal/config"
)

// ErrNotSupported means this adapter has no such capability. It is not a
// failure: the caller should consult Capabilities first.
var ErrNotSupported = errors.New("action not supported by this adapter")

// ErrNotPermitted means the config forbids the action, for example a stop on a
// service without allow_stop.
var ErrNotPermitted = errors.New("action not permitted by this service's config")

// Item is one thing a service is holding: a loaded model, or a running job.
type Item struct {
	Name string
	// VRAMMiB is what this item holds, or 0 when the service does not say.
	VRAMMiB uint64
	// Detail is a short human note, for example a model's expiry time.
	Detail string
}

// Status is one probe result.
type Status struct {
	// Up is whether the service answered.
	Up bool
	// Version is whatever the service calls itself, for display only.
	Version string
	// Items are the models or jobs the service reports holding.
	Items []Item
	// HeldMiB is the VRAM attributed to this service. HeldEstimated marks a
	// figure derived rather than reported, so status can say so out loud.
	HeldMiB       uint64
	HeldEstimated bool
	// Idle reports whether the service has no work in flight. IdleKnown is
	// false for services that cannot answer, and Idle must then be ignored.
	Idle      bool
	IdleKnown bool
}

// Capabilities describes what an adapter can actually do, so that the
// scheduler never plans an action that would be refused or silently ignored.
type Capabilities struct {
	// CanRelease is whether the service exposes a way to drop VRAM.
	CanRelease bool
	// CanReportIdle is whether Probe fills in Idle.
	CanReportIdle bool
	// CanStop is whether a unit exists that could be stopped. It says nothing
	// about whether stopping is permitted; that is allow_stop.
	CanStop bool
}

// Result describes what an action did. Detail is written into the log line
// next to the measured before and after VRAM figures.
type Result struct {
	// Acted is false when the adapter correctly did nothing, for example a
	// release against a service that held nothing.
	Acted bool
	// Targets names what was acted on, for example the models unloaded.
	Targets []string
	Detail  string
}

// Adapter drives one configured service.
type Adapter interface {
	// Name is the configured service name.
	Name() string
	// Kind is the adapter kind from the config.
	Kind() config.AdapterKind
	// Capabilities is static for the life of the adapter.
	Capabilities() Capabilities
	// Probe reads the service's current state. It never changes anything.
	Probe(ctx context.Context) (Status, error)
	// Release asks the service to give up VRAM through its own API.
	Release(ctx context.Context) (Result, error)
	// Stop halts the service at the process level. Adapters that cannot do
	// this return ErrNotSupported.
	Stop(ctx context.Context) (Result, error)
}

// New builds the adapter for a validated service config.
func New(svc config.Service) (Adapter, error) {
	switch svc.Adapter {
	case config.AdapterOllama:
		return newOllama(svc), nil
	case config.AdapterComfyUI:
		return newComfyUI(svc), nil
	case config.AdapterLlamaSwap:
		return newLlamaSwap(svc), nil
	case config.AdapterSystemdUnit:
		return newSystemdUnit(svc), nil
	default:
		return nil, fmt.Errorf("no adapter for kind %q", svc.Adapter)
	}
}

// bytesToMiB converts a byte count as reported by a service. Values below one
// MiB round up to 1 rather than to 0, so that "holding something small" never
// displays as "holding nothing".
func bytesToMiB(b uint64) uint64 {
	if b == 0 {
		return 0
	}
	if mib := b / (1024 * 1024); mib > 0 {
		return mib
	}
	return 1
}
