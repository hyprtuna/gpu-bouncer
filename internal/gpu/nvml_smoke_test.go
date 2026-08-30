//go:build nvmlsmoke && cgo

// This file is excluded from every normal build. It talks to the real NVIDIA
// driver on the machine it runs on, so it cannot run in CI. Run it by hand:
//
//	go test -tags nvmlsmoke ./internal/gpu/ -v
//
// It is read only. Nothing here changes GPU state or touches any process.
package gpu

import (
	"context"
	"errors"
	"testing"
)

func TestNVMLSmoke(t *testing.T) {
	src, err := openNVML()
	if err != nil {
		t.Skipf("NVML unavailable on this machine: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })

	if got, want := src.Name(), "nvml"; got != want {
		t.Errorf("Name = %q, want %q", got, want)
	}

	devices, err := src.Devices(context.Background())
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if len(devices) == 0 {
		t.Fatal("NVML reported no devices but openNVML succeeded")
	}
	for _, d := range devices {
		t.Logf("device %d %q uuid=%s total=%d MiB used=%d MiB free=%d MiB",
			d.Index, d.Name, d.UUID, d.TotalMiB, d.UsedMiB, d.FreeMiB())
		if d.TotalMiB == 0 {
			t.Errorf("device %d reports 0 MiB total, which cannot be right", d.Index)
		}
		if d.UsedMiB > d.TotalMiB {
			t.Errorf("device %d used %d MiB exceeds total %d MiB", d.Index, d.UsedMiB, d.TotalMiB)
		}
	}

	procs, err := src.Processes(context.Background(), devices[0].Index)
	if err != nil && !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Processes: %v", err)
	}
	for _, p := range procs {
		t.Logf("pid %d holds %d MiB", p.PID, p.UsedMiB)
	}
}
