// Package observe turns live GPU and service readings into the Observation the
// scheduler consumes.
//
// It is shared by the daemon and by the read only commands, so that
// `gpu-bouncer plan` sees exactly what the daemon would see. It performs only
// read only work: every adapter call it makes is a Probe.
package observe

import (
	"context"
	"errors"
	"fmt"
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
		a, err := adapter.New(svc, cfg.Policy.GPUIndex)
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

// Devices lists every GPU the source can see, readable or not.
func (o *Observer) Devices(ctx context.Context) ([]gpu.Device, error) {
	if o.source == nil {
		return nil, errors.New("no GPU source could be opened")
	}
	return o.source.Devices(ctx)
}

// Device reads the arbitrated GPU. A non nil error means VRAM could not be
// read and says why, which the scheduler treats as a reason to do nothing.
// When the device exists but is unreadable it is returned alongside the
// error, so status can still identify it.
func (o *Observer) Device(ctx context.Context) (gpu.Device, error) {
	devices, err := o.Devices(ctx)
	if err != nil {
		return gpu.Device{}, err
	}
	if len(devices) == 0 {
		return gpu.Device{}, gpu.ErrNoDevices
	}
	index := o.cfg.Policy.GPUIndex
	dev, ok := gpu.DeviceByIndex(devices, index)
	if !ok {
		return gpu.Device{}, fmt.Errorf("policy.gpu_index %d names no device: the %s source sees %d device(s), indexes 0 to %d",
			index, o.source.Name(), len(devices), len(devices)-1)
	}
	if dev.Unreadable != "" {
		// The reason leads. It is what the operator has to act on, and on
		// an NVIDIA card it is NVML's own failure text; putting sixty
		// characters of device identification in front of it buried the
		// one line that says what to fix. The identification follows, so
		// nothing is lost from the message, only reordered.
		return dev, fmt.Errorf("%s (GPU %d: %s, %s source)", dev.Unreadable, index, describe(dev), o.source.Name())
	}
	return dev, nil
}

// describe names a device the way a human would look it up.
func describe(dev gpu.Device) string {
	label := dev.Name
	if dev.BusID != "" {
		label += ", PCI " + dev.BusID
	}
	return label
}

// Observe probes every configured service concurrently and assembles the
// scheduler's input. A probe failure is recorded against that service rather
// than failing the whole observation, because one unreachable service must not
// blind gpu-bouncer to the rest.
//
// Services are returned in config order, so downstream output is stable.
func (o *Observer) Observe(ctx context.Context) scheduler.Observation {
	obs := scheduler.Observation{}
	device, err := o.Device(ctx)
	if err != nil {
		obs.DeviceErr = err.Error()
	} else {
		obs.Device, obs.DeviceKnown = device, true
	}

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
