package gpu

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCard writes the DRM sysfs attributes gpu-bouncer reads. The card's
// device directory is a symlink to a PCI address under devices/, as it is in
// the real tree, so that the bus id resolves. An empty vendor makes a virtual
// card with no device link at all. Empty VRAM counters are not written, which
// is what a non amdgpu card looks like.
func fakeCard(t *testing.T, root, card, vendor, busID, totalBytes, usedBytes string, extra map[string]string) {
	t.Helper()
	cardDir := filepath.Join(root, card)
	if err := os.MkdirAll(cardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if vendor == "" {
		return
	}
	pciDir := filepath.Join(root, "devices", busID)
	if err := os.MkdirAll(pciDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(pciDir, filepath.Join(cardDir, "device")); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"vendor":              vendor + "\n",
		"mem_info_vram_total": totalBytes,
		"mem_info_vram_used":  usedBytes,
	}
	for k, v := range extra {
		files[k] = v
	}
	for name, body := range files {
		if body == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(pciDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// amdCard is a readable amdgpu card.
func amdCard(t *testing.T, root, card, busID, totalBytes, usedBytes string) {
	t.Helper()
	fakeCard(t, root, card, "0x1002", busID, totalBytes, usedBytes, map[string]string{"device": "0x1900\n"})
}

func openFake(t *testing.T, root string) []Device {
	t.Helper()
	src, err := openSysfs(root)
	if err != nil {
		t.Fatalf("openSysfs: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })
	devices, err := src.Devices(context.Background())
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	return devices
}

func TestSysfsDevices(t *testing.T) {
	root := t.TempDir()
	// 8 GiB total, 2 GiB used.
	fakeCard(t, root, "card0", "0x1002", "0000:03:00.0", "8589934592\n", "2147483648\n", map[string]string{
		"product_name": "Radeon Test 9000\n",
	})
	// A second card, listed after card0 even though it is created first in
	// directory order on some filesystems.
	fakeCard(t, root, "card10", "0x1002", "0000:0a:00.0", "4294967296\n", "0\n", map[string]string{
		"device": "0x744c\n",
	})
	// Connector directories must be ignored.
	if err := os.MkdirAll(filepath.Join(root, "card0-DP-1"), 0o755); err != nil {
		t.Fatal(err)
	}

	src, err := openSysfs(root)
	if err != nil {
		t.Fatalf("openSysfs: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })

	if got, want := src.Name(), "sysfs"; got != want {
		t.Errorf("Name = %q, want %q", got, want)
	}

	devices, err := src.Devices(context.Background())
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if got, want := len(devices), 2; got != want {
		t.Fatalf("len(Devices) = %d, want %d: %+v", got, want, devices)
	}

	// card0 sorts before card10: numeric order, not lexical.
	first := devices[0]
	if got, want := first.TotalMiB, uint64(8192); got != want {
		t.Errorf("card0 TotalMiB = %d, want %d", got, want)
	}
	if got, want := first.UsedMiB, uint64(2048); got != want {
		t.Errorf("card0 UsedMiB = %d, want %d", got, want)
	}
	if got, want := first.FreeMiB(), uint64(6144); got != want {
		t.Errorf("card0 FreeMiB = %d, want %d", got, want)
	}
	if got, want := first.Name, "Radeon Test 9000"; got != want {
		t.Errorf("card0 Name = %q, want %q", got, want)
	}
	if got, want := first.BusID, "0000:03:00.0"; got != want {
		t.Errorf("card0 BusID = %q, want %q", got, want)
	}
	if got, want := first.Vendor, "0x1002"; got != want {
		t.Errorf("card0 Vendor = %q, want %q", got, want)
	}
	if first.Unreadable != "" {
		t.Errorf("card0 Unreadable = %q, want readable", first.Unreadable)
	}

	second := devices[1]
	if got, want := second.TotalMiB, uint64(4096); got != want {
		t.Errorf("card10 TotalMiB = %d, want %d", got, want)
	}
	if got, want := second.Name, "AMD device 0x744c"; got != want {
		t.Errorf("card10 Name = %q, want %q", got, want)
	}
}

// The case that made this test file grow: a laptop with an NVIDIA card as
// card1 and an AMD integrated GPU as card2. The NVIDIA card has no VRAM
// counters in sysfs. It must still be index 0, marked unreadable with a reason
// that names the fix, and the AMD card must be index 1. Dropping the NVIDIA
// card would make the default gpu_index arbitrate the wrong physical GPU.
func TestSysfsHybridKeepsNVIDIAAtIndexZero(t *testing.T) {
	root := t.TempDir()
	fakeCard(t, root, "card1", "0x10de", "0000:01:00.0", "", "", map[string]string{"device": "0x2820\n"})
	amdCard(t, root, "card2", "0000:05:00.0", "2147483648\n", "1152385024\n")

	devices := openFake(t, root)
	if got, want := len(devices), 2; got != want {
		t.Fatalf("len(Devices) = %d, want %d: %+v", got, want, devices)
	}

	nv := devices[0]
	if nv.Index != 0 {
		t.Errorf("NVIDIA card Index = %d, want 0", nv.Index)
	}
	if nv.Vendor != "0x10de" || nv.VendorName() != "NVIDIA" {
		t.Errorf("NVIDIA card Vendor = %q (%q), want 0x10de NVIDIA", nv.Vendor, nv.VendorName())
	}
	if nv.BusID != "0000:01:00.0" {
		t.Errorf("NVIDIA card BusID = %q, want 0000:01:00.0", nv.BusID)
	}
	if nv.Name != "NVIDIA device 0x2820" {
		t.Errorf("NVIDIA card Name = %q", nv.Name)
	}
	if nv.Unreadable == "" {
		t.Fatal("NVIDIA card is reported readable through sysfs, which has no counters for it")
	}
	for _, want := range []string{"NVIDIA", "cgo", "driver"} {
		if !strings.Contains(nv.Unreadable, want) {
			t.Errorf("Unreadable = %q, want it to mention %q", nv.Unreadable, want)
		}
	}

	amd := devices[1]
	if amd.Index != 1 {
		t.Errorf("AMD card Index = %d, want 1", amd.Index)
	}
	if amd.Unreadable != "" {
		t.Errorf("AMD card Unreadable = %q, want readable", amd.Unreadable)
	}
	if got, want := amd.TotalMiB, uint64(2048); got != want {
		t.Errorf("AMD card TotalMiB = %d, want %d", got, want)
	}
	if got, want := amd.UsedMiB, uint64(1099); got != want {
		t.Errorf("AMD card UsedMiB = %d, want %d", got, want)
	}
	if amd.BusID != "0000:05:00.0" {
		t.Errorf("AMD card BusID = %q, want 0000:05:00.0", amd.BusID)
	}
}

func TestSysfsAMDOnlyIsReadableAtIndexZero(t *testing.T) {
	root := t.TempDir()
	amdCard(t, root, "card0", "0000:03:00.0", "17179869184\n", "1073741824\n")

	devices := openFake(t, root)
	if len(devices) != 1 {
		t.Fatalf("len(Devices) = %d, want 1: %+v", len(devices), devices)
	}
	d := devices[0]
	if d.Index != 0 || d.Unreadable != "" || d.TotalMiB != 16384 || d.UsedMiB != 1024 {
		t.Errorf("device = %+v, want index 0, readable, 16384 MiB total, 1024 MiB used", d)
	}
}

// A virtual card (simpledrm, vkms) has no PCI vendor. It is skipped and does
// not consume an index, so the first physical card is still index 0.
func TestSysfsVirtualCardIsSkipped(t *testing.T) {
	root := t.TempDir()
	fakeCard(t, root, "card0", "", "", "", "", nil)
	amdCard(t, root, "card1", "0000:03:00.0", "8589934592\n", "0\n")

	devices := openFake(t, root)
	if len(devices) != 1 {
		t.Fatalf("len(Devices) = %d, want 1: %+v", len(devices), devices)
	}
	if d := devices[0]; d.Index != 0 || d.Vendor != "0x1002" || d.TotalMiB != 8192 {
		t.Errorf("device = %+v, want the AMD card at index 0", d)
	}
}

// An AMD card whose driver exposes no counters is present and unreadable with
// a reason, not silently dropped.
func TestSysfsAMDWithoutCountersIsUnreadable(t *testing.T) {
	root := t.TempDir()
	fakeCard(t, root, "card0", "0x1002", "0000:03:00.0", "", "", map[string]string{"device": "0x1900\n"})
	devices := openFake(t, root)
	if len(devices) != 1 {
		t.Fatalf("len(Devices) = %d, want 1: %+v", len(devices), devices)
	}
	if got := devices[0].Unreadable; !strings.Contains(got, "mem_info_vram_total") {
		t.Errorf("Unreadable = %q, want it to name the missing attribute", got)
	}
}

// A source that cannot see per process usage must say so rather than return an
// empty list, which would read as "the GPU is idle".
func TestSysfsProcessesUnsupported(t *testing.T) {
	root := t.TempDir()
	amdCard(t, root, "card0", "0000:03:00.0", "1048576\n", "0\n")
	src, err := openSysfs(root)
	if err != nil {
		t.Fatalf("openSysfs: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })

	procs, err := src.Processes(context.Background(), 0)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Processes error = %v, want ErrUnsupported", err)
	}
	if procs != nil {
		t.Errorf("Processes = %v, want nil alongside the error", procs)
	}
}

func TestSysfsNoCards(t *testing.T) {
	root := t.TempDir()
	// A virtual card alone is no card at all.
	fakeCard(t, root, "card0", "", "", "", "", nil)
	if _, err := openSysfs(root); !errors.Is(err, ErrNoDevices) {
		t.Fatalf("openSysfs on a root with only a virtual card = %v, want ErrNoDevices", err)
	}
	if _, err := openSysfs(t.TempDir()); !errors.Is(err, ErrNoDevices) {
		t.Fatalf("openSysfs on an empty root = %v, want ErrNoDevices", err)
	}
}

func TestSysfsMissingRoot(t *testing.T) {
	if _, err := openSysfs(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("openSysfs on a missing root returned no error")
	}
}

// One card whose counters cannot be read must not hide the others: it is
// reported unreadable with the reason, and every other card keeps its index
// and its figures.
func TestSysfsOneBadCardDoesNotBlindTheSource(t *testing.T) {
	for _, tt := range []struct {
		name       string
		total      string
		used       string
		wantReason string
	}{
		{"garbage total", "not a number\n", "0\n", "is not a byte count"},
		{"total without used", "8589934592\n", "", "mem_info_vram_used"},
		{"garbage used", "8589934592\n", "x\n", "is not a byte count"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			amdCard(t, root, "card0", "0000:01:00.0", "4294967296\n", "0\n")
			amdCard(t, root, "card1", "0000:03:00.0", tt.total, tt.used)
			amdCard(t, root, "card2", "0000:05:00.0", "2147483648\n", "1048576\n")

			devices := openFake(t, root)
			assertNumbering(t, devices, "0000:01:00.0", "0000:03:00.0", "0000:05:00.0")
			if devices[0].Unreadable != "" || devices[0].TotalMiB != 4096 {
				t.Errorf("card0 = %+v, want readable at 4096 MiB", devices[0])
			}
			if !strings.Contains(devices[1].Unreadable, tt.wantReason) {
				t.Errorf("card1 Unreadable = %q, want it to mention %q", devices[1].Unreadable, tt.wantReason)
			}
			if devices[2].Unreadable != "" || devices[2].TotalMiB != 2048 || devices[2].UsedMiB != 1 {
				t.Errorf("card2 = %+v, want readable at 2048 MiB with 1 MiB used", devices[2])
			}
		})
	}
}

// assertNumbering checks that the devices are exactly these bus ids at
// indexes 0, 1, 2 and so on: an unreadable card keeps its place.
func assertNumbering(t *testing.T, devices []Device, busIDs ...string) {
	t.Helper()
	if len(devices) != len(busIDs) {
		t.Fatalf("got %d devices, want %d: %+v", len(devices), len(busIDs), devices)
	}
	for i, want := range busIDs {
		if devices[i].Index != i || devices[i].BusID != want {
			t.Errorf("devices[%d] = index %d bus %q, want index %d bus %q", i, devices[i].Index, devices[i].BusID, i, want)
		}
	}
}

// A device directory this process cannot traverse belongs to a real card.
// The card stays in the numbering, unreadable with the error, and the cards
// after it are not renumbered. Root can traverse anything, so the case is
// skipped there rather than silently passing.
func TestSysfsUnreadableDeviceDirKeepsItsIndex(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not apply to root")
	}
	root := t.TempDir()
	fakeCard(t, root, "card0", "0x10de", "0000:01:00.0", "", "", map[string]string{"device": "0x2820\n"})
	amdCard(t, root, "card1", "0000:05:00.0", "2147483648\n", "0\n")
	locked := filepath.Join(root, "devices", "0000:01:00.0")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	devices := openFake(t, root)
	assertNumbering(t, devices, "0000:01:00.0", "0000:05:00.0")
	if !strings.Contains(devices[0].Unreadable, "device directory cannot be read") || !strings.Contains(devices[0].Unreadable, "permission denied") {
		t.Errorf("card0 Unreadable = %q, want the permission error", devices[0].Unreadable)
	}
	if devices[1].Unreadable != "" || devices[1].TotalMiB != 2048 {
		t.Errorf("card1 = %+v, want readable at index 1", devices[1])
	}
}

// A vendor file that exists but cannot be read is reported as such, not as
// an empty vendor.
func TestSysfsUnreadableVendorFileIsNamed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not apply to root")
	}
	root := t.TempDir()
	amdCard(t, root, "card0", "0000:03:00.0", "8589934592\n", "0\n")
	vendor := filepath.Join(root, "devices", "0000:03:00.0", "vendor")
	if err := os.Chmod(vendor, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(vendor, 0o644) })

	devices := openFake(t, root)
	assertNumbering(t, devices, "0000:03:00.0")
	if !strings.Contains(devices[0].Unreadable, "vendor file cannot be read") || strings.Contains(devices[0].Unreadable, "(vendor )") {
		t.Errorf("Unreadable = %q, want it to say the vendor file could not be read", devices[0].Unreadable)
	}
}

// The reason for an unreadable NVIDIA card depends on why NVML is not in
// use, and when NVML was tried and failed its error comes first.
func TestUnreadableReason(t *testing.T) {
	nv := Device{Vendor: "0x10de"}
	nvmlErr := errors.New("nvml: init: Driver/library version mismatch")
	for _, tt := range []struct {
		name    string
		dev     Device
		nvmlErr error
		builtIn bool
		prefix  string
		want    []string
		wantNot []string
	}{
		{"NVML built in and failed", nv, nvmlErr, true, "nvml: init: Driver/library version mismatch;",
			[]string{"NVML, which this build has, could not be opened", "libnvidia-ml.so.1"}, []string{"without cgo"}},
		{"built without NVML", nv, errors.New("nvml: built without cgo"), false, "this build has no NVML support",
			[]string{"build with cgo and the NVIDIA driver"}, []string{"libnvidia-ml.so.1"}},
		{"AMD card without counters", Device{Vendor: "0x1002"}, nvmlErr, true, "sysfs exposes no mem_info_vram_total",
			[]string{"vendor 0x1002"}, []string{"nvml"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := unreadableReason(tt.dev, tt.nvmlErr, tt.builtIn)
			if !strings.HasPrefix(got, tt.prefix) {
				t.Errorf("reason = %q, want prefix %q", got, tt.prefix)
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("reason = %q, want it to contain %q", got, w)
				}
			}
			for _, w := range tt.wantNot {
				if strings.Contains(got, w) {
					t.Errorf("reason = %q, must not contain %q", got, w)
				}
			}
		})
	}
}

// Open carries the NVML failure into the sysfs source it falls back to.
func TestSysfsSourceCarriesNVMLError(t *testing.T) {
	root := t.TempDir()
	fakeCard(t, root, "card0", "0x10de", "0000:01:00.0", "", "", map[string]string{"device": "0x2820\n"})
	src, err := openSysfs(root)
	if err != nil {
		t.Fatal(err)
	}
	src.nvmlErr = errors.New("nvml: init: Driver/library version mismatch")
	devices, err := src.Devices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(devices[0].Unreadable, "nvml: init: Driver/library version mismatch") {
		t.Errorf("Unreadable = %q, want the NVML error first", devices[0].Unreadable)
	}
}

func TestSysfsContextCancelled(t *testing.T) {
	root := t.TempDir()
	amdCard(t, root, "card0", "0000:03:00.0", "1048576\n", "0\n")
	src, err := openSysfs(root)
	if err != nil {
		t.Fatalf("openSysfs: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := src.Devices(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Devices with a cancelled context = %v, want context.Canceled", err)
	}
}

func TestFreeMiBNeverUnderflows(t *testing.T) {
	// Used above Total should never wrap around to a huge free figure, which
	// would tell the scheduler there is plenty of room.
	d := Device{TotalMiB: 100, UsedMiB: 150}
	if got := d.FreeMiB(); got != 0 {
		t.Errorf("FreeMiB = %d, want 0", got)
	}
}

func TestDeviceByIndex(t *testing.T) {
	devices := []Device{{Index: 0, Name: "a"}, {Index: 2, Name: "c"}}
	if d, ok := DeviceByIndex(devices, 2); !ok || d.Name != "c" {
		t.Errorf("DeviceByIndex(2) = %+v, %v; want the device named c", d, ok)
	}
	if _, ok := DeviceByIndex(devices, 1); ok {
		t.Error("DeviceByIndex(1) found a device, want not found")
	}
}

func TestSortProcesses(t *testing.T) {
	procs := []Process{{PID: 5, UsedMiB: 100}, {PID: 2, UsedMiB: 900}, {PID: 9, UsedMiB: 100}}
	sortProcesses(procs)
	want := []Process{{PID: 2, UsedMiB: 900}, {PID: 5, UsedMiB: 100}, {PID: 9, UsedMiB: 100}}
	for i := range want {
		if procs[i] != want[i] {
			t.Fatalf("sortProcesses = %+v, want %+v (largest first, then PID)", procs, want)
		}
	}
}
