package observe

import (
	"context"
	"strings"
	"testing"

	"github.com/hyprtuna/gpu-bouncer/internal/config"
	"github.com/hyprtuna/gpu-bouncer/internal/gpu"
)

type fakeSource struct{ devices []gpu.Device }

func (fakeSource) Name() string                                    { return "fake" }
func (f fakeSource) Devices(context.Context) ([]gpu.Device, error) { return f.devices, nil }
func (fakeSource) Processes(context.Context, int) ([]gpu.Process, error) {
	return nil, gpu.ErrUnsupported
}
func (fakeSource) Close() error { return nil }

func observer(t *testing.T, index int, devices ...gpu.Device) *Observer {
	t.Helper()
	cfg := config.Defaults()
	cfg.Policy.GPUIndex = index
	o, err := New(cfg, fakeSource{devices: devices})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// A gpu_index that names no device must say so, with the count, rather than
// leave status printing "state unavailable" with no reason.
func TestDeviceIndexOutOfRange(t *testing.T) {
	two := []gpu.Device{{Index: 0, Name: "a", TotalMiB: 8192}, {Index: 1, Name: "b", TotalMiB: 4096}}
	_, err := observer(t, 9, two...).Device(context.Background())
	if err == nil {
		t.Fatal("Device found gpu_index 9 among two devices")
	}
	want := "policy.gpu_index 9 names no device: the fake source sees 2 device(s), indexes 0 to 1"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}

	obs := observer(t, 9, two...).Observe(context.Background())
	if obs.DeviceKnown || obs.DeviceErr != want {
		t.Errorf("Observe: DeviceKnown = %v, DeviceErr = %q; want false and the same message", obs.DeviceKnown, obs.DeviceErr)
	}

	dev, err := observer(t, 1, two...).Device(context.Background())
	if err != nil || dev.Name != "b" {
		t.Errorf("Device(1) = %+v, %v; want device b", dev, err)
	}
}

// A present but unreadable device is returned with its error, so status can
// name the card the index landed on.
func TestDeviceUnreadable(t *testing.T) {
	dev, err := observer(t, 0, gpu.Device{Index: 0, Name: "NVIDIA device 0x2820", BusID: "0000:01:00.0", Vendor: "0x10de",
		Unreadable: "sysfs exposes no VRAM counters for an NVIDIA card"}).Device(context.Background())
	if err == nil {
		t.Fatal("an unreadable device was returned without an error")
	}
	if dev.BusID != "0000:01:00.0" {
		t.Errorf("device = %+v, want it returned alongside the error", dev)
	}
	for _, want := range []string{"GPU 0", "NVIDIA device 0x2820", "PCI 0000:01:00.0", "fake source", "no VRAM counters"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}

func TestDeviceNoSource(t *testing.T) {
	o, err := New(config.Defaults(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.Device(context.Background()); err == nil {
		t.Error("Device with no source returned no error")
	}
}

// The reason has to come first. On an NVIDIA card whose NVML could not be
// opened, NVML's own error is the one line that says what to fix, and sixty
// characters of device identification in front of it buried it.
func TestDeviceErrorLeadsWithTheReason(t *testing.T) {
	const reason = "nvml: init: ERROR_LIBRARY_NOT_FOUND; sysfs exposes no VRAM counters for an NVIDIA card, " +
		"and NVML, which this build has, could not be opened: check that libnvidia-ml.so.1 from the NVIDIA " +
		"driver is installed and loadable by this process, see INSTALL.md"
	dev := gpu.Device{Index: 0, Name: "NVIDIA device 0x2820", BusID: "0000:01:00.0", Vendor: "0x10de", Unreadable: reason}

	_, err := observer(t, 0, dev).Device(context.Background())
	if err == nil {
		t.Fatal("an unreadable device was returned without an error")
	}
	if !strings.HasPrefix(err.Error(), "nvml: init: ERROR_LIBRARY_NOT_FOUND;") {
		t.Errorf("error = %q, want it to begin with the NVML failure", err)
	}
	// The identification is still there, behind the reason.
	for _, want := range []string{"GPU 0", "NVIDIA device 0x2820", "PCI 0000:01:00.0", "fake source"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to still contain %q", err, want)
		}
	}

	// Observe carries the same text, which is what the plan note and the
	// daemon's refusal to start are built from.
	obs := observer(t, 0, dev).Observe(context.Background())
	if obs.DeviceErr != err.Error() {
		t.Errorf("DeviceErr = %q, want the same text as Device's error %q", obs.DeviceErr, err)
	}
}
