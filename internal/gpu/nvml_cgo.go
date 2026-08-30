//go:build cgo

package gpu

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// nvmlSource reads through the NVIDIA Management Library. go-nvml loads
// libnvidia-ml.so at run time, so a binary built with cgo still starts on a
// machine that has no NVIDIA driver: Init simply fails and Open falls back.
type nvmlSource struct {
	mu sync.Mutex
}

func openNVML() (Source, error) {
	if ret := nvml.Init(); ret != nvml.SUCCESS {
		return nil, fmt.Errorf("init: %s", nvml.ErrorString(ret))
	}
	src := &nvmlSource{}
	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		_ = src.Close()
		return nil, fmt.Errorf("device count: %s", nvml.ErrorString(ret))
	}
	if count == 0 {
		_ = src.Close()
		return nil, ErrNoDevices
	}
	return src, nil
}

func (s *nvmlSource) Name() string { return "nvml" }

func (s *nvmlSource) Devices(ctx context.Context) ([]Device, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("nvml device count: %s", nvml.ErrorString(ret))
	}
	devices := make([]Device, 0, count)
	for i := 0; i < count; i++ {
		handle, ret := nvml.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("nvml device %d: %s", i, nvml.ErrorString(ret))
		}
		mem, ret := nvml.DeviceGetMemoryInfo(handle)
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("nvml device %d memory: %s", i, nvml.ErrorString(ret))
		}
		dev := Device{
			Index:    i,
			TotalMiB: mem.Total / BytesPerMiB,
			UsedMiB:  mem.Used / BytesPerMiB,
		}
		// Name, UUID and the PCI address are identification. A driver that
		// will not report them is not a reason to fail a status read.
		if name, ret := nvml.DeviceGetName(handle); ret == nvml.SUCCESS {
			dev.Name = name
		}
		if uuid, ret := nvml.DeviceGetUUID(handle); ret == nvml.SUCCESS {
			dev.UUID = uuid
		}
		if pci, ret := nvml.DeviceGetPciInfo(handle); ret == nvml.SUCCESS {
			dev.BusID = nvmlBusID(pci)
			// PciDeviceId packs the device id in the high half and the
			// vendor id in the low half.
			dev.Vendor = fmt.Sprintf("0x%04x", pci.PciDeviceId&0xffff)
		}
		devices = append(devices, dev)
	}
	return devices, nil
}

func (s *nvmlSource) Processes(ctx context.Context, deviceIndex int) ([]Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	handle, ret := nvml.DeviceGetHandleByIndex(deviceIndex)
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("nvml device %d: %s", deviceIndex, nvml.ErrorString(ret))
	}

	seen := make(map[int]uint64)
	// A process can appear in either list, and a few appear in both, so the
	// two are merged by PID rather than concatenated.
	compute, ret := nvml.DeviceGetComputeRunningProcesses(handle)
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("nvml device %d compute processes: %s", deviceIndex, nvml.ErrorString(ret))
	}
	graphics, ret := nvml.DeviceGetGraphicsRunningProcesses(handle)
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("nvml device %d graphics processes: %s", deviceIndex, nvml.ErrorString(ret))
	}
	for _, info := range append(compute, graphics...) {
		used := info.UsedGpuMemory / BytesPerMiB
		if existing, ok := seen[int(info.Pid)]; !ok || used > existing {
			seen[int(info.Pid)] = used
		}
	}

	procs := make([]Process, 0, len(seen))
	for pid, used := range seen {
		procs = append(procs, Process{PID: pid, UsedMiB: used})
	}
	sortProcesses(procs)
	return procs, nil
}

func (s *nvmlSource) Close() error {
	if ret := nvml.Shutdown(); ret != nvml.SUCCESS {
		return fmt.Errorf("nvml shutdown: %s", nvml.ErrorString(ret))
	}
	return nil
}

// nvmlBusID turns NVML's fixed size, NUL terminated bus id into a string in
// the same spelling sysfs uses, so the two sources can be compared by eye.
// NVML writes the domain as eight hex digits; sysfs uses four.
func nvmlBusID(pci nvml.PciInfo) string {
	var b []byte
	for _, c := range pci.BusId {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	id := strings.ToLower(string(b))
	if domain, rest, ok := strings.Cut(id, ":"); ok && len(domain) > 4 && strings.TrimLeft(domain[:len(domain)-4], "0") == "" {
		id = domain[len(domain)-4:] + ":" + rest
	}
	return id
}
