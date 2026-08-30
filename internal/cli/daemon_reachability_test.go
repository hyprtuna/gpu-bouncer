package cli

import (
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyprtuna/gpu-bouncer/internal/ipc"
)

// acceptAndClose stands in for a daemon that takes the request and then goes
// away: it accepts, reads, and closes without answering.
func acceptAndClose(t *testing.T) string {
	t.Helper()
	socket := filepath.Join(shortDir(t), "gb.sock")
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
			buf := make([]byte, 4096)
			_, _ = conn.Read(buf)
			_ = conn.Close()
		}
	}()
	return socket
}

// A daemon that took the request and then went away must not be reported as
// a daemon that does not exist. The advice that follows the absent case,
// "start one", is wrong here and starting a second daemon would be worse.
func TestAConnectedFailureIsNotBlamedOnAMissingDaemon(t *testing.T) {
	t.Setenv(ipc.EnvSocket, acceptAndClose(t))

	for _, args := range [][]string{
		{"evict", "x"},
		{"request", "x"},
		{"release", "x"},
	} {
		t.Run(args[0], func(t *testing.T) {
			code, stdout, stderr := run(args...)
			if code != 1 {
				t.Errorf("exit = %d, want 1", code)
			}
			if want := "the daemon closed the connection"; !strings.Contains(stderr, want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, want)
			}
			for _, unwanted := range []string{"no gpu-bouncer daemon is listening", "Start one with"} {
				if strings.Contains(stderr, unwanted) {
					t.Errorf("stderr = %q, want no %q", stderr, unwanted)
				}
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want it empty", stdout)
			}
		})
	}
}

// The absent case is unchanged: nothing listening anywhere still says so, and
// still says how to fix it.
func TestNoDaemonStillAdvisesStartingOne(t *testing.T) {
	t.Setenv(ipc.EnvSocket, filepath.Join(shortDir(t), "absent.sock"))

	code, _, stderr := run("evict", "x")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	for _, want := range []string{"no gpu-bouncer daemon is listening", "Start one with \"gpu-bouncer daemon\""} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr, want)
		}
	}
}
