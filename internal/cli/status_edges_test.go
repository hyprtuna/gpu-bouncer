package cli

import (
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyprtuna/gpu-bouncer/internal/ipc"
)

// acceptAndHang stands in for a wedged daemon: it accepts the connection,
// reads the request, and then answers nothing at all, holding the connection
// open until the test ends.
func acceptAndHang(t *testing.T) string {
	t.Helper()
	socket := filepath.Join(shortDir(t), "hung.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { close(done); _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				buf := make([]byte, 4096)
				_, _ = conn.Read(buf)
				<-done
			}()
		}
	}()
	return socket
}

// A daemon that accepts the connection and never answers is running. Reading
// the failure as absence printed "No daemon is running", which sends an
// operator to start a second one alongside the first, and reported the
// fields that daemon never answered on as though they had been answered.
func TestAWedgedDaemonIsReportedAsRunning(t *testing.T) {
	// The wait is the client's own, and this test would otherwise sit out
	// the full half minute to prove one sentence.
	previous := probeTimeout
	probeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { probeTimeout = previous })

	noConfigAnywhere(t)
	t.Setenv(ipc.EnvSocket, acceptAndHang(t))

	code, stdout, stderr := run("status")
	if code != 0 {
		t.Fatalf("status: exit %d, want 0: status is advisory: %s", code, stderr)
	}
	for _, want := range []string{
		"A daemon is running",
		"did not answer within",
		"cannot be reported",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status does not say %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "No daemon is running") {
		t.Errorf("status reports a daemon that answered the connection as absent:\n%s", stdout)
	}

	_, jsonOut, _ := run("--json", "status")
	var decoded struct {
		DaemonRunning bool            `json:"daemon_running"`
		DaemonDryRun  *bool           `json:"daemon_dry_run"`
		ConfigStale   *bool           `json:"config_stale"`
		DaemonConfig  json.RawMessage `json:"daemon_config"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &decoded); err != nil {
		t.Fatalf("--json status is not JSON: %v", err)
	}
	if !decoded.DaemonRunning {
		t.Error("daemon_running = false, want true: the connection was accepted")
	}
	if decoded.DaemonDryRun != nil || decoded.ConfigStale != nil || string(decoded.DaemonConfig) != "null" {
		t.Errorf("daemon_dry_run %v, config_stale %v, daemon_config %s; want all null: the daemon answered none of them",
			decoded.DaemonDryRun, decoded.ConfigStale, decoded.DaemonConfig)
	}
}

// A daemon that closed the connection is running too, and the same line says
// so. This is the fast half of the same distinction.
func TestADaemonThatClosedTheConnectionIsReportedAsRunning(t *testing.T) {
	noConfigAnywhere(t)
	t.Setenv(ipc.EnvSocket, acceptAndClose(t))

	code, stdout, _ := run("status")
	if code != 0 {
		t.Fatalf("status: exit %d, want 0", code)
	}
	if !strings.Contains(stdout, "A daemon is running") || !strings.Contains(stdout, "closed the connection") {
		t.Errorf("status does not report a daemon that closed the connection:\n%s", stdout)
	}
	if strings.Contains(stdout, "No daemon is running") {
		t.Errorf("status reports it as absent:\n%s", stdout)
	}
}

// paths is a list, and INSTALL promises every list is present and empty as
// [], not absent. It was the one list on the wire still carrying omitempty,
// so a daemon that loaded no file sent daemon_config without it.
func TestDaemonConfigPathsIsAlwaysPresent(t *testing.T) {
	startDaemonWith(t, false)
	noConfigAnywhere(t)

	_, jsonOut, _ := run("--json", "status")
	var decoded struct {
		DaemonConfig map[string]json.RawMessage `json:"daemon_config"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &decoded); err != nil {
		t.Fatalf("--json status is not JSON: %v", err)
	}
	raw, present := decoded.DaemonConfig["paths"]
	if !present {
		t.Fatalf("daemon_config has no paths key: %s", jsonOut)
	}
	if string(raw) != "[]" {
		t.Errorf("daemon_config.paths = %s, want []", raw)
	}
}
