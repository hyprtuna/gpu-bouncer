package gpu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// sysfsDRMRoot is the real kernel path. Tests point openSysfs elsewhere.
const sysfsDRMRoot = "/sys/class/drm"

// sysfsSource reads DRM cards from sysfs. Every card backed by a PCI device is
// listed in kernel order, so the numbering matches what the kernel calls
// card0, card1 and so on, minus virtual cards such as simpledrm that have no
// PCI vendor. That order is the same one NVML uses on a machine with a single
// NVIDIA card, and on a hybrid machine it keeps the NVIDIA card at the index a
// user would expect rather than silently promoting the integrated GPU.
//
// VRAM counters come from the amdgpu driver, which registers
// mem_info_vram_total and mem_info_vram_used as device attributes
// (drivers/gpu/drm/amd/amdgpu/amdgpu_device.c). No other in-tree driver
// exposes them, so a card without them is reported as present and unreadable
// rather than dropped: dropping it would renumber every card after it.
type sysfsSource struct {
	root  string
	cards []sysfsCard
}

type sysfsCard struct {
	num        int
	devicePath string
}

// pciAddressRE matches a PCI address such as 0000:01:00.0.
var pciAddressRE = regexp.MustCompile(`^[0-9a-fA-F]{4,}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F]$`)

func openSysfs(root string) (Source, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", root, err)
	}

	var found []sysfsCard
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
		// A card without a PCI vendor is virtual (simpledrm, vkms, a
		// framebuffer) and holds no VRAM anyone arbitrates.
		if _, err := os.Stat(filepath.Join(devicePath, "vendor")); err != nil {
			continue
		}
		found = append(found, sysfsCard{num: num, devicePath: devicePath})
	}
	if len(found) == 0 {
		return nil, ErrNoDevices
	}
	sort.Slice(found, func(i, j int) bool { return found[i].num < found[j].num })
	return &sysfsSource{root: root, cards: found}, nil
}

func (s *sysfsSource) Name() string { return "sysfs" }

func (s *sysfsSource) Devices(ctx context.Context) ([]Device, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	devices := make([]Device, 0, len(s.cards))
	for i, c := range s.cards {
		dev := Device{
			Index:  i,
			Vendor: strings.TrimSpace(readStringFile(filepath.Join(c.devicePath, "vendor"))),
			BusID:  sysfsBusID(c.devicePath),
		}
		dev.Name = sysfsCardName(c.devicePath, dev)

		totalPath := filepath.Join(c.devicePath, "mem_info_vram_total")
		if _, err := os.Stat(totalPath); err != nil {
			dev.Unreadable = sysfsUnreadableReason(dev)
			devices = append(devices, dev)
			continue
		}
		total, err := readUintFile(totalPath)
		if err != nil {
			return nil, err
		}
		used, err := readUintFile(filepath.Join(c.devicePath, "mem_info_vram_used"))
		if err != nil {
			return nil, err
		}
		dev.TotalMiB = total / BytesPerMiB
		dev.UsedMiB = used / BytesPerMiB
		devices = append(devices, dev)
	}
	return devices, nil
}

// sysfsUnreadableReason explains why a listed card has no VRAM figures. The
// NVIDIA case is spelled out because it is the one a user can fix.
func sysfsUnreadableReason(dev Device) string {
	if dev.Vendor == "0x10de" {
		return "sysfs exposes no VRAM counters for an NVIDIA card: reading it needs a gpu-bouncer build with cgo and the NVIDIA driver, see INSTALL.md"
	}
	return fmt.Sprintf("sysfs exposes no mem_info_vram_total for this card (vendor %s), so its VRAM cannot be read", dev.Vendor)
}

// Processes cannot be answered from these sysfs attributes. Saying so is
// better than returning an empty list, which would read as "nothing is using
// the GPU" and could talk the scheduler into a wrong decision.
func (s *sysfsSource) Processes(ctx context.Context, deviceIndex int) ([]Process, error) {
	return nil, ErrUnsupported
}

func (s *sysfsSource) Close() error { return nil }

// sysfsBusID resolves the card's device link to its PCI address. The device
// directory of a PCI card is a symlink into /sys/devices whose last element is
// the address. Anything that does not look like one yields an empty string:
// the bus id is identification, not a figure decisions depend on.
func sysfsBusID(devicePath string) string {
	resolved, err := filepath.EvalSymlinks(devicePath)
	if err != nil {
		return ""
	}
	base := filepath.Base(resolved)
	if !pciAddressRE.MatchString(base) {
		return ""
	}
	return base
}

// sysfsCardName builds a human label from whatever the card exposes. It is
// cosmetic, so every failure degrades to a generic label rather than an error.
func sysfsCardName(devicePath string, dev Device) string {
	if data, err := os.ReadFile(filepath.Join(devicePath, "product_name")); err == nil {
		if name := strings.TrimSpace(string(data)); name != "" {
			return name
		}
	}
	device := strings.TrimSpace(readStringFile(filepath.Join(devicePath, "device")))
	if vendorName := dev.VendorName(); vendorName != "" && device != "" {
		return fmt.Sprintf("%s device %s", vendorName, device)
	}
	if dev.Vendor != "" && device != "" {
		return fmt.Sprintf("PCI %s:%s", strings.TrimPrefix(dev.Vendor, "0x"), strings.TrimPrefix(device, "0x"))
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
