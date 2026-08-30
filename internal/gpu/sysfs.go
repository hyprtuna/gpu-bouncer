package gpu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// sysfsDRMRoot is the real kernel path. Tests point openSysfs elsewhere.
const sysfsDRMRoot = "/sys/class/drm"

// sysfsSource reads amdgpu VRAM counters from sysfs. These files are exposed
// by the amdgpu driver and are world readable, so this works with no special
// privileges and no vendor library.
//
// Reference: Linux amdgpu driver, drivers/gpu/drm/amd/amdgpu/amdgpu_device.c,
// which registers mem_info_vram_total and mem_info_vram_used as device
// attributes. Documented in the kernel GPU driver docs under amdgpu.
type sysfsSource struct {
	root  string
	cards []string // absolute paths to card device directories, in index order
}

func openSysfs(root string) (Source, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", root, err)
	}

	type card struct {
		num  int
		path string
	}
	var found []card
	for _, entry := range entries {
		name := entry.Name()
		// Match card0, card1 and so on, but not card0-DP-1 connectors.
		numPart, ok := strings.CutPrefix(name, "card")
		if !ok {
			continue
		}
		num, err := strconv.Atoi(numPart)
		if err != nil {
			continue
		}
		devicePath := filepath.Join(root, name, "device")
		// Only cards that actually report VRAM are usable.
		if _, err := os.Stat(filepath.Join(devicePath, "mem_info_vram_total")); err != nil {
			continue
		}
		found = append(found, card{num: num, path: devicePath})
	}
	if len(found) == 0 {
		return nil, ErrNoDevices
	}
	sort.Slice(found, func(i, j int) bool { return found[i].num < found[j].num })

	src := &sysfsSource{root: root}
	for _, c := range found {
		src.cards = append(src.cards, c.path)
	}
	return src, nil
}

func (s *sysfsSource) Name() string { return "sysfs" }

func (s *sysfsSource) Devices(ctx context.Context) ([]Device, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	devices := make([]Device, 0, len(s.cards))
	for i, path := range s.cards {
		total, err := readUintFile(filepath.Join(path, "mem_info_vram_total"))
		if err != nil {
			return nil, err
		}
		used, err := readUintFile(filepath.Join(path, "mem_info_vram_used"))
		if err != nil {
			return nil, err
		}
		dev := Device{
			Index:    i,
			TotalMiB: total / BytesPerMiB,
			UsedMiB:  used / BytesPerMiB,
			Name:     sysfsCardName(path),
		}
		devices = append(devices, dev)
	}
	return devices, nil
}

// Processes cannot be answered from these sysfs attributes. Saying so is
// better than returning an empty list, which would read as "nothing is using
// the GPU" and could talk the scheduler into a wrong decision.
func (s *sysfsSource) Processes(ctx context.Context, deviceIndex int) ([]Process, error) {
	return nil, ErrUnsupported
}

func (s *sysfsSource) Close() error { return nil }

// sysfsCardName builds a human label from whatever the card exposes. It is
// cosmetic, so every failure degrades to a generic label rather than an error.
func sysfsCardName(devicePath string) string {
	if data, err := os.ReadFile(filepath.Join(devicePath, "product_name")); err == nil {
		if name := strings.TrimSpace(string(data)); name != "" {
			return name
		}
	}
	vendor := strings.TrimSpace(readStringFile(filepath.Join(devicePath, "vendor")))
	device := strings.TrimSpace(readStringFile(filepath.Join(devicePath, "device")))
	if vendor != "" && device != "" {
		return fmt.Sprintf("PCI %s:%s", strings.TrimPrefix(vendor, "0x"), strings.TrimPrefix(device, "0x"))
	}
	return "unknown GPU"
}

func readStringFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func readUintFile(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	text := strings.TrimSpace(string(data))
	value, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("read %s: %q is not a byte count: %w", path, text, err)
	}
	return value, nil
}

func sortProcesses(procs []Process) {
	sort.Slice(procs, func(i, j int) bool {
		if procs[i].UsedMiB != procs[j].UsedMiB {
			return procs[i].UsedMiB > procs[j].UsedMiB
		}
		return procs[i].PID < procs[j].PID
	})
}
