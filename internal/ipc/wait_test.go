package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The wait is the work's, not a guess. Actions on different services run
// concurrently, so a plan takes as long as its longest action, and the client
// allows that plus enough slack for the exchange either side of it.
func TestWaitForIsTheLongestActionPlusSlack(t *testing.T) {
	tests := []struct {
		name    string
		longest time.Duration
		want    time.Duration
	}{
		{"nothing reported falls back to the flat wait", 0, PlanWait},
		{"a negative report falls back too", -time.Second, PlanWait},
		{"one short action", 3 * time.Second, 13 * time.Second},
		{"the default drain plus the default timeout", 35 * time.Second, 45 * time.Second},
		{"four services draining at once still wait for one", 35 * time.Second, 45 * time.Second},
		{"the longest drain a config may set", 10*time.Minute + 5*time.Second, 10*time.Minute + 15*time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := WaitFor(tt.longest); got != tt.want {
				t.Errorf("WaitFor(%s) = %s, want %s", tt.longest, got, tt.want)
			}
		})
	}
}

// serveOnce answers one connection with the lines given, then does what after
// says. It stands in for a daemon that goes quiet or goes away.
// shortDir is a temporary directory for a Unix socket. t.TempDir folds the
// test's name and TMPDIR into its path, and a socket path is capped at 107
// bytes, so a long test name on a machine with a long TMPDIR produces a
// socket that cannot be bound. This keeps the path short whatever the test
// is called, which for this package is the difference between testing the
// error wording and testing the path length guard by accident.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func serveOnce(t *testing.T, lines []Response, hold time.Duration) string {
	t.Helper()
	socket := filepath.Join(shortDir(t), "gb.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf)
		for _, line := range lines {
			encoded, _ := json.Marshal(line)
			_, _ = conn.Write(append(encoded, '\n'))
		}
		time.Sleep(hold)
	}()
	return socket
}

// A daemon that took the request and then stopped answering must be reported
// as exactly that. "No daemon is listening" would send an operator to start a
// second one while the first is still carrying out the plan it was given.
func TestSilentDaemonIsNotReportedAsAbsent(t *testing.T) {
	socket := serveOnce(t, nil, 5*time.Second)
	t.Setenv(EnvSocket, socket)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := Exchange(ctx, Request{Op: OpEvict, Service: "x", PlanFirst: true})
	if err == nil {
		t.Fatal("Exchange succeeded against a daemon that never answered")
	}
	if got, want := err.Error(), "the daemon accepted the request but did not answer within 1s"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
	if errors.Is(err, ErrNoDaemon) || strings.Contains(err.Error(), ErrNoDaemon.Error()) {
		t.Errorf("error = %q, want no claim that nothing is listening", err)
	}
}

// The wait for the results is the announced plan's, not a flat one. A plan of
// one millisecond is given up on after the slack, not after PlanWait, which
// is the whole point of announcing it.
func TestTheWaitIsSizedFromTheAnnouncedPlan(t *testing.T) {
	socket := serveOnce(t, []Response{{OK: true, Preliminary: true, LongestActionMS: 1}}, time.Minute)
	t.Setenv(EnvSocket, socket)

	start := time.Now()
	_, err := Exchange(context.Background(), Request{Op: OpEvict, Service: "x", PlanFirst: true})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Exchange succeeded against a daemon that announced a plan and never finished it")
	}
	if want := "the daemon accepted the request but did not answer within"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
	if elapsed < ClientSlack {
		t.Errorf("gave up after %s, want at least the %s slack", elapsed, ClientSlack)
	}
	if elapsed > ClientSlack+5*time.Second {
		t.Errorf("waited %s for a plan announced as 1ms, want about the %s slack, not the %s fallback",
			elapsed, ClientSlack, PlanWait)
	}
}

// A daemon that went away mid answer is reported as that and not as absent.
func TestClosedConnectionIsNotReportedAsAbsent(t *testing.T) {
	socket := serveOnce(t, []Response{{OK: true, Preliminary: true, LongestActionMS: 600000}}, 0)
	t.Setenv(EnvSocket, socket)

	start := time.Now()
	_, err := Exchange(context.Background(), Request{Op: OpEvict, Service: "x", PlanFirst: true})
	if err == nil {
		t.Fatal("Exchange succeeded against a daemon that closed the connection")
	}
	if got, want := err.Error(), "the daemon closed the connection"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
	// The close is noticed at once rather than waited out to the ten minute
	// bound the preliminary reply announced.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("noticed the close after %s, want at once", elapsed)
	}
}

// Nothing listening anywhere is still the plain "no daemon" error, with the
// sockets it tried.
func TestNothingListeningIsStillNoDaemon(t *testing.T) {
	t.Setenv(EnvSocket, filepath.Join(shortDir(t), "absent.sock"))
	_, err := Exchange(context.Background(), Request{Op: OpEvict, Service: "x", PlanFirst: true})
	if !errors.Is(err, ErrNoDaemon) {
		t.Fatalf("error = %v, want ErrNoDaemon", err)
	}
}

// The second reply is the answer. A client that stopped at the preliminary
// one would report a plan as though it had been carried out.
func TestExchangeReturnsTheFinalReply(t *testing.T) {
	socket := serveOnce(t, []Response{
		{OK: true, Preliminary: true, LongestActionMS: 1000},
		{OK: true, Message: "done", Executed: []ActionResult{{Service: "x", Verb: "release", Acted: true}}},
	}, 0)
	t.Setenv(EnvSocket, socket)

	resp, err := Exchange(context.Background(), Request{Op: OpEvict, Service: "x", PlanFirst: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Preliminary {
		t.Error("Exchange returned the preliminary reply")
	}
	if resp.Message != "done" || len(resp.Executed) != 1 {
		t.Errorf("resp = %+v, want the final reply", resp)
	}
}

// A daemon too old to announce its plan answers once. That reply is final,
// and the client must not sit waiting for a second one.
func TestASingleReplyIsFinal(t *testing.T) {
	socket := serveOnce(t, []Response{{OK: true, Message: "old daemon"}}, 0)
	t.Setenv(EnvSocket, socket)

	resp, err := Exchange(context.Background(), Request{Op: OpEvict, Service: "x", PlanFirst: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message != "old daemon" {
		t.Errorf("message = %q, want the one reply the daemon sent", resp.Message)
	}
}
