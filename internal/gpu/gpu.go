// Package gpu reads GPU memory state.
//
// Everything the rest of gpu-bouncer needs from a GPU is behind Source, so the
// scheduler can be tested without a GPU and so a machine with no NVIDIA driver
// still gets a useful `gpu-bouncer status`.
//
// Two sources ship in v0.1:
//
//	nvml   NVIDIA Management Library, the accurate one. Needs cgo and a
//	       loadable libnvidia-ml.so, both resolved at run time.
//	sysfs  read only amdgpu counters under /sys/class/drm. Reports memory
//	       but not per process usage.
package gpu

import (
	"context"
	"errors"
	"fmt"
)

// ErrUnsupported is returned by a Source for a capability it does not have,
// for example per process memory on the sysfs source.
var ErrUnsupported = errors.New("not supported by this GPU source")

// ErrNoDevices means the source loaded but found no GPU.
var ErrNoDevices = errors.New("no GPU devices found")

// BytesPerMiB is the divisor used everywhere gpu-bouncer reports memory.
const BytesPerMiB = 1024 * 1024

// Device is one GPU at one moment.
type Device struct {
	Index    int
	UUID     string
	Name     string
	TotalMiB uint64
	UsedMiB  uint64
}

// FreeMiB is the memory not currently in use. It is derived rather than read,
// so that Total and Used can never disagree with it.
func (d Device) FreeMiB() uint64 {
	if d.UsedMiB > d.TotalMiB {
		return 0
	}
	return d.TotalMiB - d.UsedMiB
}

// Process is one process holding GPU memory.
type Process struct {
	PID     int
	UsedMiB uint64
}

// Source reads GPU state. Every method is read only: nothing in this package
// can change what a GPU is doing.
type Source interface {
	// Name identifies the source in `status` output, for example "nvml".
	Name() string
	// Devices lists every GPU the source can see.
	Devices(ctx context.Context) ([]Device, error)
	// Processes lists processes holding memory on one device. It returns
	// ErrUnsupported on sources that cannot see per process usage.
	Processes(ctx context.Context, deviceIndex int) ([]Process, error)
	// Close releases whatever the source holds.
	Close() error
}

// Open returns the most accurate available Source. It tries NVML first and
// falls back to sysfs, so that read only commands keep working on a machine
// with no NVIDIA driver, or on a build made without cgo.
//
// The returned error is only non nil when no source at all could be opened.
// It wraps every attempt so the user can see why each one failed.
func Open() (Source, error) {
	var attempts []error

	if src, err := openNVML(); err == nil {
		return src, nil
	} else {
		attempts = append(attempts, fmt.Errorf("nvml: %w", err))
	}

	if src, err := openSysfs(sysfsDRMRoot); err == nil {
		return src, nil
	} else {
		attempts = append(attempts, fmt.Errorf("sysfs: %w", err))
	}

	return nil, fmt.Errorf("no usable GPU source: %w", errors.Join(attempts...))
}

// DeviceByIndex picks one device out of a list by its Index field, which is
// not necessarily its position in the slice.
func DeviceByIndex(devices []Device, index int) (Device, bool) {
	for _, d := range devices {
		if d.Index == index {
			return d, true
		}
	}
	return Device{}, false
}
