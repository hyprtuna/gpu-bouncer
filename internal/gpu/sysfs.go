package gpu

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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
	// nvmlErr is why NVML was not used, when it was tried and failed. It
	// leads the unreadable reason of an NVIDIA card, because on such a
	// machine that failure, not sysfs, is what the user has to fix.
	nvmlErr error
}

type sysfsCard struct {
	num        int
	devicePath string
	// vendorErr is a failure to stat device/vendor for any reason other than
	// its absence. Such a card is real for all anyone can tell, so it keeps
	// its place in the numbering and is reported unreadable.
	vendorErr error
}

// pciAddressRE matches a PCI address such as 0000:01:00.0.
var pciAddressRE = regexp.MustCompile(`^[0-9a-fA-F]{4,}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F]$`)

// classifyCard decides whether a card directory belongs to a virtual device
// with no VRAM anyone arbitrates, and if not, what stopped its vendor from
// being read.
//
// A card without a PCI vendor is virtual (simpledrm, vkms, a framebuffer)
// and is left out, which is the one case where leaving a card out is right.
// Everything else keeps its index: dropping a card renumbers every card
// after it, so a card that might be real is always listed, unreadable, with
// the reason. The distinction is whether the device directory could be
// reached at all. os.Stat alone cannot make it, because it follows the
// device symlink and so reports a dangling link as an absent vendor file,
// which dropped a real card and renumbered the next one.
func classifyCard(devicePath string) (virtual bool, vendorErr error) {
	// Lstat, so a dangling symlink is seen as the entry it is rather than
	// as the target it fails to reach.
	if _, err := os.Lstat(devicePath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// No device entry at all: nothing is behind this card.
			return true, nil
		}
		return false, err
	}
	info, err := os.Stat(devicePath)
	switch {
	case err != nil:
		// The entry exists but does not lead anywhere readable: a dangling
		// link, or a target this process may not reach. Real for all
		// anyone can tell.
		return false, err
	case !info.IsDir():
		return false, fmt.Errorf("%s is not a directory", devicePath)
	}
	// The device directory can be reached, so the absence of a vendor file
	// in it is a fact about the device and not about our access to it.
	if _, err := os.Stat(filepath.Join(devicePath, "vendor")); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

func openSysfs(root string) (*sysfsSource, error) {
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
		virtual, vendorErr := classifyCard(devicePath)
		if virtual {
			continue
		}
		found = append(found, sysfsCard{num: num, devicePath: devicePath, vendorErr: vendorErr})
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
	// One card that cannot be read never hides the others: each failure is
	// recorded on its own device, and the numbering is fixed at open time.
	devices := make([]Device, 0, len(s.cards))
	for i, c := range s.cards {
		devices = append(devices, s.readCard(i, c))
	}
	return devices, nil
}

// readCard reads one card's identity and counters. Every failure after the
// index is assigned becomes an Unreadable reason on that card.
func (s *sysfsSource) readCard(index int, c sysfsCard) Device {
	dev := Device{Index: index, BusID: sysfsBusID(c.devicePath), Name: "unknown GPU"}
	if c.vendorErr != nil {
		dev.Unreadable = fmt.Sprintf("the card's device directory cannot be read: %v", c.vendorErr)
		return dev
	}
	vendor, err := os.ReadFile(filepath.Join(c.devicePath, "vendor"))
	if err != nil {
		dev.Unreadable = fmt.Sprintf("the card's vendor file cannot be read: %v", err)
		return dev
	}
	dev.Vendor = strings.TrimSpace(string(vendor))
	dev.Name = sysfsCardName(c.devicePath, dev)

	totalPath := filepath.Join(c.devicePath, "mem_info_vram_total")
	if _, err := os.Stat(totalPath); err != nil {
		dev.Unreadable = unreadableReason(dev, s.nvmlErr, nvmlBuiltIn)
		return dev
	}
	total, err := readUintFile(totalPath)
	if err != nil {
		dev.Unreadable = err.Error()
		return dev
	}
	used, err := readUintFile(filepath.Join(c.devicePath, "mem_info_vram_used"))
	if err != nil {
		dev.Unreadable = err.Error()
		return dev
	}
	dev.TotalMiB = total / BytesPerMiB
	dev.UsedMiB = used / BytesPerMiB
	return dev
}

// unreadableReason explains why a listed card has no VRAM counters in sysfs.
// The NVIDIA case is spelled out because it is the one a user can fix, and
// the fix depends on why NVML was not used: a build without NVML support
// needs a different build, a build with it whose NVML failed to load needs
// the driver's library reachable, and that failure text comes first.
func unreadableReason(dev Device, nvmlErr error, builtIn bool) string {
	if dev.Vendor != "0x10de" {
		return fmt.Sprintf("sysfs exposes no mem_info_vram_total for this card (vendor %s), so its VRAM cannot be read", dev.Vendor)
	}
	switch {
	case builtIn && nvmlErr != nil:
		return fmt.Sprintf("%v; sysfs exposes no VRAM counters for an NVIDIA card, and NVML, which this build has, could not be opened: check that libnvidia-ml.so.1 from the NVIDIA driver is installed and loadable by this process, see INSTALL.md", nvmlErr)
	case !builtIn:
		return "this build has no NVML support (built without cgo), and sysfs exposes no VRAM counters for an NVIDIA card: reading it needs a gpu-bouncer build with cgo and the NVIDIA driver, see INSTALL.md"
	default:
		return "sysfs exposes no VRAM counters for an NVIDIA card: reading it needs NVML, which needs a gpu-bouncer build with cgo and the NVIDIA driver, see INSTALL.md"
	}
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
	var device string
	if data, err := os.ReadFile(filepath.Join(devicePath, "device")); err == nil {
		device = strings.TrimSpace(string(data))
	}
	if vendorName := dev.VendorName(); vendorName != "" && device != "" {
		return fmt.Sprintf("%s device %s", vendorName, device)
	}
	if dev.Vendor != "" && device != "" {
		return fmt.Sprintf("PCI %s:%s", strings.TrimPrefix(dev.Vendor, "0x"), strings.TrimPrefix(device, "0x"))
	}
	return "unknown GPU"
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
