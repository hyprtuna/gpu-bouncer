package gpu

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeCard writes the amdgpu sysfs attributes gpu-bouncer reads.
func fakeCard(t *testing.T, root, card string, totalBytes, usedBytes string, extra map[string]string) {
	t.Helper()
	dir := filepath.Join(root, card, "device")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
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
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSysfsDevices(t *testing.T) {
	root := t.TempDir()
	// 8 GiB total, 2 GiB used.
	fakeCard(t, root, "card0", "8589934592\n", "2147483648\n", map[string]string{
		"product_name": "Radeon Test 9000\n",
	})
	// A second card, listed after card0 even though it is created first in
	// directory order on some filesystems.
	fakeCard(t, root, "card10", "4294967296\n", "0\n", map[string]string{
		"vendor": "0x1002\n",
		"device": "0x744c\n",
	})
	// Connector directories and cards without VRAM attributes must be ignored.
	if err := os.MkdirAll(filepath.Join(root, "card0-DP-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "card3", "device"), 0o755); err != nil {
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

	second := devices[1]
	if got, want := second.TotalMiB, uint64(4096); got != want {
		t.Errorf("card10 TotalMiB = %d, want %d", got, want)
	}
	if got, want := second.Name, "PCI 1002:744c"; got != want {
		t.Errorf("card10 Name = %q, want %q", got, want)
	}
}

// A source that cannot see per process usage must say so rather than return an
// empty list, which would read as "the GPU is idle".
func TestSysfsProcessesUnsupported(t *testing.T) {
	root := t.TempDir()
	fakeCard(t, root, "card0", "1048576\n", "0\n", nil)
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
	if _, err := openSysfs(t.TempDir()); !errors.Is(err, ErrNoDevices) {
		t.Fatalf("openSysfs on an empty root = %v, want ErrNoDevices", err)
	}
}

func TestSysfsMissingRoot(t *testing.T) {
	if _, err := openSysfs(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("openSysfs on a missing root returned no error")
	}
}

func TestSysfsGarbageCounter(t *testing.T) {
	root := t.TempDir()
	fakeCard(t, root, "card0", "not a number\n", "0\n", nil)
	src, err := openSysfs(root)
	if err != nil {
		t.Fatalf("openSysfs: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })
	if _, err := src.Devices(context.Background()); err == nil {
		t.Fatal("Devices accepted a non numeric counter, want error")
	}
}

func TestSysfsContextCancelled(t *testing.T) {
	root := t.TempDir()
	fakeCard(t, root, "card0", "1048576\n", "0\n", nil)
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
