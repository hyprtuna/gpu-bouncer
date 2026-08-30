// Package observe turns live GPU and service readings into the Observation the
// scheduler consumes.
//
// It is shared by the daemon and by the read only commands, so that
// `gpu-bouncer plan` sees exactly what the daemon would see. It performs only
// read only work: every adapter call it makes is a Probe.
package observe

import (
	"context"
	"sync"

	"github.com/hyprtuna/gpu-bouncer/internal/adapter"
	"github.com/hyprtuna/gpu-bouncer/internal/config"
	"github.com/hyprtuna/gpu-bouncer/internal/gpu"
	"github.com/hyprtuna/gpu-bouncer/internal/scheduler"
)

// Observer probes a fixed set of services against one GPU source.
type Observer struct {
	cfg      config.Config
	source   gpu.Source
	adapters map[string]adapter.Adapter
}

// New builds an Observer for every service in cfg. It fails only if an adapter
// cannot be constructed at all, which is a configuration error rather than a
// runtime one.
func New(cfg config.Config, source gpu.Source) (*Observer, error) {
	adapters := make(map[string]adapter.Adapter, len(cfg.Services))
	for _, svc := range cfg.Services {
		a, err := adapter.New(svc)
		if err != nil {
			return nil, err
		}
		adapters[svc.Name] = a
	}
	return &Observer{cfg: cfg, source: source, adapters: adapters}, nil
}

// Adapter returns the adapter for a configured service.
func (o *Observer) Adapter(name string) (adapter.Adapter, bool) {
	a, ok := o.adapters[name]
	return a, ok
}

// Config returns the configuration this Observer was built from.
func (o *Observer) Config() config.Config { return o.cfg }

// Device reads the arbitrated GPU. The boolean is false when VRAM could not be
// read, which the scheduler treats as a reason to do nothing.
func (o *Observer) Device(ctx context.Context) (gpu.Device, bool) {
	if o.source == nil {
		return gpu.Device{}, false
	}
	devices, err := o.source.Devices(ctx)
	if err != nil || len(devices) == 0 {
		return gpu.Device{}, false
	}
	if dev, ok := gpu.DeviceByIndex(devices, o.cfg.Policy.GPUIndex); ok {
		return dev, true
	}
	return gpu.Device{}, false
}

// Observe probes every configured service concurrently and assembles the
// scheduler's input. A probe failure is recorded against that service rather
// than failing the whole observation, because one unreachable service must not
// blind gpu-bouncer to the rest.
//
// Services are returned in config order, so downstream output is stable.
func (o *Observer) Observe(ctx context.Context) scheduler.Observation {
	obs := scheduler.Observation{}
	obs.Device, obs.DeviceKnown = o.Device(ctx)

	states := make([]scheduler.ServiceState, len(o.cfg.Services))
	var wg sync.WaitGroup
	for i, svc := range o.cfg.Services {
		wg.Add(1)
		go func(i int, svc config.Service) {
			defer wg.Done()
			states[i] = o.probeOne(ctx, svc)
		}(i, svc)
	}
	wg.Wait()

	obs.Services = states
	return obs
}

// probeOne builds one ServiceState. It never returns an error: an unreachable
// service is a state, not a failure.
func (o *Observer) probeOne(ctx context.Context, svc config.Service) scheduler.ServiceState {
	state := scheduler.ServiceState{
		Name:      svc.Name,
		Priority:  svc.Priority,
		Adapter:   svc.Adapter,
		AllowStop: svc.AllowStop,
	}
	a, ok := o.adapters[svc.Name]
	if !ok {
		state.ProbeErr = "no adapter"
		return state
	}
	caps := a.Capabilities()
	state.CanRelease = caps.CanRelease
	state.StopUnit = caps.CanStop

	status, err := a.Probe(ctx)
	if err != nil {
		// A service that is simply not running is the common case here. It is
		// still recorded as a probe error so that nothing acts on it, and the
		// message is kept for status output.
		state.ProbeErr = err.Error()
		return state
	}
	state.Up = status.Up
	state.HeldMiB = status.HeldMiB
	state.HeldEstimated = status.HeldEstimated
	state.Idle = status.Idle
	state.IdleKnown = status.IdleKnown
	return state
}

// ServiceStatus is one service as `gpu-bouncer status` reports it. It keeps
// the item lists and version strings that the scheduler does not need but a
// human reading the output does.
type ServiceStatus struct {
	Service config.Service
	Status  adapter.Status
	Caps    adapter.Capabilities
	Err     error
}

// Statuses probes every configured service concurrently, in config order.
func (o *Observer) Statuses(ctx context.Context) []ServiceStatus {
	out := make([]ServiceStatus, len(o.cfg.Services))
	var wg sync.WaitGroup
	for i, svc := range o.cfg.Services {
		wg.Add(1)
		go func(i int, svc config.Service) {
			defer wg.Done()
			entry := ServiceStatus{Service: svc}
			if a, ok := o.adapters[svc.Name]; ok {
				entry.Caps = a.Capabilities()
				entry.Status, entry.Err = a.Probe(ctx)
			} else {
				entry.Err = adapter.ErrNotSupported
			}
			out[i] = entry
		}(i, svc)
	}
	wg.Wait()
	return out
}
