package adapter

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/hyprtuna/gpu-bouncer/internal/config"
)

// systemdUnitAdapter observes, and with permission stops, one systemd unit.
//
// It is the only adapter that acts at the process level, and the only one whose
// action the affected program cannot undo by serving the next request. So it is
// deliberately narrow: Probe uses read only verbs, Release does not exist at
// all, and Stop refuses unless the service config sets allow_stop.
//
// systemctl is shelled out to rather than the bus being spoken directly. That
// keeps the daemon free of a D-Bus dependency, and it inherits the system
// versus user manager selection, the unit name completion and the polkit
// prompting rules from the tool the user already reasons about.
type systemdUnitAdapter struct {
	name      string
	unit      string
	userUnit  bool
	allowStop bool
	timeout   time.Duration

	// bin and runner exist so that tests never reach the real systemctl.
	// Stopping a unit is not something a test suite may do on the machine it
	// happens to be running on.
	bin    string
	runner func(ctx context.Context, name string, args ...string) ([]byte, error)

	// stopTimeout bounds `systemctl stop`, which blocks until the job finishes
	// and may legitimately take as long as the unit's TimeoutStopSec; systemd's
	// own default for that is 90s. The per request timeout is sized for status
	// polls and would abandon a stop that was working.
	stopTimeout time.Duration
}

func newSystemdUnit(svc config.Service) *systemdUnitAdapter {
	return &systemdUnitAdapter{
		name:        svc.Name,
		unit:        svc.Unit,
		userUnit:    svc.UserUnit,
		allowStop:   svc.AllowStop,
		timeout:     svc.Timeout.D(),
		bin:         "systemctl",
		runner:      runSystemctl,
		stopTimeout: 90 * time.Second,
	}
}

func (a *systemdUnitAdapter) Name() string             { return a.name }
func (a *systemdUnitAdapter) Kind() config.AdapterKind { return config.AdapterSystemdUnit }

func (a *systemdUnitAdapter) Capabilities() Capabilities {
	return Capabilities{
		// There is no graceful middle here. A unit can be stopped or left
		// alone; systemd has no way to ask the program inside it to hand back
		// memory. A service that can do that should be configured with the
		// adapter for its own API, pointing at the same process.
		CanRelease: false,
		// ActiveState says a process exists, never whether it is doing
		// anything.
		CanReportIdle: false,
		// A unit can always be stopped in principle. Whether gpu-bouncer may is
		// allow_stop, which Stop enforces.
		CanStop: true,
	}
}

func (a *systemdUnitAdapter) Probe(ctx context.Context) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	// is-active exits non zero for anything that is not active. That exit
	// status is the answer, not a failure: treating it as an error would make
	// every stopped service look unprobeable, and a stopped service is exactly
	// the state gpu-bouncer most needs to be able to read. What separates the
	// two cases is stdout. systemctl names the state there even as it exits 3,
	// while a systemctl that could not run at all names nothing.
	out, err := a.runner(ctx, a.bin, a.systemctlArgs("is-active", a.unit)...)
	state := strings.TrimSpace(string(out))
	if state == "" {
		if err != nil {
			return Status{}, fmt.Errorf("%s: %w", a.name, err)
		}
		return Status{}, fmt.Errorf("%s: systemctl is-active %s reported no state", a.name, a.unit)
	}

	status := Status{
		Up: state == "active",
		// A unit tells you nothing about VRAM: systemd knows about processes
		// and cgroups, not about the GPU. HeldEstimated marks the 0 as absent
		// data rather than a measured zero.
		HeldEstimated: true,
		// Idle is left unset, and IdleKnown false says so. An active unit is
		// active whether its process is saturating the GPU or asleep.
	}
	if !status.Up {
		// A unit that does not exist also reports "inactive", and exits 4
		// rather than 3 while doing so. Reporting a misspelled unit name as a
		// service that merely happens to be stopped would hide a config error
		// indefinitely, so the two are told apart here.
		if a.notFound(ctx) {
			return Status{}, fmt.Errorf("%s: systemd knows no unit named %s", a.name, a.unit)
		}
		return status, nil
	}
	status.Items = []Item{{Name: a.unit, Detail: a.detail(ctx, state)}}
	return status, nil
}

// notFound reports whether systemd has no such unit. A LoadState it cannot
// read is treated as "the unit exists", because the conservative reading of an
// unclear answer is the one that does not invent a config error.
func (a *systemdUnitAdapter) notFound(ctx context.Context) bool {
	out, err := a.runner(ctx, a.bin, a.systemctlArgs("show", "--property=LoadState", a.unit)...)
	if err != nil {
		return false
	}
	return parseSystemctlShow(out)["LoadState"] == "not-found"
}

// detail reads a few descriptive properties for the status display.
//
// A failure is swallowed on purpose: is-active has already established the
// state that decisions are made from, and losing a cosmetic string is not a
// reason to report a running service as unprobeable.
func (a *systemdUnitAdapter) detail(ctx context.Context, state string) string {
	out, err := a.runner(ctx, a.bin, a.systemctlArgs("show", "--property=ActiveState,SubState,MainPID", a.unit)...)
	if err != nil {
		return state
	}
	props := parseSystemctlShow(out)
	sub := props["SubState"]
	if sub == "" {
		return state
	}
	detail := state + " (" + sub + ")"
	if pid := props["MainPID"]; pid != "" && pid != "0" {
		detail += ", main PID " + pid
	}
	return detail
}

// Release is not available. See Capabilities: a unit has no graceful way to
// give up VRAM, and pretending otherwise would let the scheduler plan a step
// that could only ever be a no-op.
func (a *systemdUnitAdapter) Release(context.Context) (Result, error) {
	return Result{}, fmt.Errorf("systemd-unit adapter has no graceful release for %s: %w", a.unit, ErrNotSupported)
}

// Stop halts the unit.
func (a *systemdUnitAdapter) Stop(ctx context.Context) (Result, error) {
	// The daemon checks allow_stop too. Checking again here is not redundant:
	// this is the one call in gpu-bouncer that can destroy work a user cares
	// about, and a permission test that exists in only one place is one
	// refactor away from not existing. The refusal comes before any exec, so a
	// service without allow_stop never causes systemctl to run at all.
	if !a.allowStop {
		return Result{}, fmt.Errorf("service %q: stopping unit %s requires allow_stop = true: %w", a.name, a.unit, ErrNotPermitted)
	}

	ctx, cancel := context.WithTimeout(ctx, a.stopTimeout)
	defer cancel()

	if _, err := a.runner(ctx, a.bin, a.systemctlArgs("stop", a.unit)...); err != nil {
		return Result{}, fmt.Errorf("%s: %w", a.name, err)
	}
	return Result{Acted: true, Targets: []string{a.unit}, Detail: "stopped " + a.unit}, nil
}

// systemctlArgs prefixes --user for a user unit. The flag has to precede the
// verb; systemctl does not accept it after one.
func (a *systemdUnitAdapter) systemctlArgs(rest ...string) []string {
	if !a.userUnit {
		return rest
	}
	return append([]string{"--user"}, rest...)
}

// parseSystemctlShow turns systemctl's `Key=Value` output into a map. `show` is
// called without --value precisely so that the keys are present: --value prints
// bare values in an order the adapter would otherwise have to assume.
func parseSystemctlShow(out []byte) map[string]string {
	props := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found || key == "" {
			continue
		}
		props[key] = value
	}
	return props
}

// runSystemctl is the real runner, used everywhere except in tests.
//
// stdout is returned even on failure, because is-active prints the state and
// exits non zero in the same breath and that state is the answer Probe wants.
// stderr goes into the error, because a systemctl failure without it is a bare
// exit status: "exit status 4" does not say whether the unit name was wrong or
// the caller lacked the permission to act on it.
func runSystemctl(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), nil
	}
	invocation := name + " " + strings.Join(args, " ")
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		return stdout.Bytes(), fmt.Errorf("%s: %w: %s", invocation, err, msg)
	}
	return stdout.Bytes(), fmt.Errorf("%s: %w", invocation, err)
}
