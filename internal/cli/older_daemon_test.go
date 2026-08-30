package cli

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyprtuna/gpu-bouncer/internal/ipc"
)

// serveOldDaemon answers every request with a 0.1.1 shaped reply: ok and a
// message, and none of the fields this client knows about. It is the daemon
// an operator has running when they upgrade the client first.
func serveOldDaemon(t *testing.T) {
	t.Helper()
	socket := filepath.Join(shortDir(t), "old.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				line, err := bufio.NewReader(conn).ReadBytes('\n')
				if err != nil {
					return
				}
				var req ipc.Request
				if err := json.Unmarshal(line, &req); err != nil {
					return
				}
				// The 0.1.1 wire shape: no daemon_dry_run, no
				// daemon_config, and no lists on a ping.
				body := `{"ok":true,"message":"gpu-bouncer daemon is running"}`
				if req.Op == ipc.OpStatus {
					body = `{"ok":true,"gpu":{"known":true,"index":0,"total_mib":8192,"used_mib":0,"free_mib":8192}}`
				}
				_, _ = conn.Write(append([]byte(body), '\n'))
			}()
		}
	}()
	t.Setenv(ipc.EnvSocket, socket)
}

// noConfigAnywhere points config resolution at an empty directory, so a test
// reads no file and can never resolve a service on the machine running it.
func noConfigAnywhere(t *testing.T) {
	t.Helper()
	t.Setenv("GPU_BOUNCER_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

// A daemon that did not say whether it is in dry-run mode must not be
// reported as one that acts. A 0.1.1 daemon started with --dry-run plans and
// never acts; a client that read the missing field as false told the operator
// their evictions were being carried out.
func TestAnOlderDaemonsUnsaidFieldsStayUnknown(t *testing.T) {
	noConfigAnywhere(t)
	serveOldDaemon(t)

	code, stdout, stderr := run("status")
	if code != 0 {
		t.Fatalf("status: exit %d: %s", code, stderr)
	}
	for _, want := range []string{
		"A daemon is running.",
		"older than this client and does not report whether it is in dry-run mode",
		"does not report which config it loaded",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status does not say %q:\n%s", want, stdout)
		}
	}
	// The plain claim is the one that must not be made on its own.
	if strings.Contains(stdout, "A daemon is running.\n") && !strings.Contains(stdout, "It is older than this client") {
		t.Errorf("status claims a plain running daemon without the caveat:\n%s", stdout)
	}

	_, jsonOut, _ := run("--json", "status")
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(jsonOut), &decoded); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	for _, key := range []string{"daemon_dry_run", "config_stale", "daemon_config"} {
		raw, present := decoded[key]
		if !present {
			t.Errorf("%s is absent, want the key present and null", key)
			continue
		}
		if string(raw) != "null" {
			t.Errorf("%s = %s, want null: the daemon did not report it", key, raw)
		}
	}
	if string(decoded["daemon_running"]) != "true" {
		t.Errorf("daemon_running = %s, want true", decoded["daemon_running"])
	}
}

// A current daemon still answers definitely, in both directions.
func TestACurrentDaemonReportsDryRunEitherWay(t *testing.T) {
	for _, dry := range []bool{false, true} {
		t.Run(map[bool]string{false: "acting", true: "dry run"}[dry], func(t *testing.T) {
			noConfigAnywhere(t)
			startDaemonWith(t, dry)
			_, jsonOut, _ := run("--json", "status")
			var decoded struct {
				DaemonDryRun *bool `json:"daemon_dry_run"`
			}
			if err := json.Unmarshal([]byte(jsonOut), &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.DaemonDryRun == nil || *decoded.DaemonDryRun != dry {
				t.Errorf("daemon_dry_run = %v, want %v", decoded.DaemonDryRun, dry)
			}
		})
	}
}

// Without a daemon there is nothing to be in dry-run mode, so the answer is
// not known rather than false.
func TestWithoutADaemonDryRunIsUnknown(t *testing.T) {
	noConfigAnywhere(t)
	t.Setenv(ipc.EnvSocket, filepath.Join(shortDir(t), "absent.sock"))
	_, jsonOut, _ := run("--json", "status")
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(jsonOut), &decoded); err != nil {
		t.Fatal(err)
	}
	if string(decoded["daemon_running"]) != "false" {
		t.Fatalf("daemon_running = %s, want false", decoded["daemon_running"])
	}
	if string(decoded["daemon_dry_run"]) != "null" {
		t.Errorf("daemon_dry_run = %s, want null with no daemon", decoded["daemon_dry_run"])
	}
}
