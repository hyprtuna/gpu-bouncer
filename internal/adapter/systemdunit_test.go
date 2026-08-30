package adapter

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hyprtuna/gpu-bouncer/internal/config"
)

// Every test in this file replaces the adapter's runner with fakeSystemctl.
// The real systemctl is never executed: this machine runs real services, and a
// test suite has no business starting or stopping any of them.
type fakeSystemctl struct {
	// reply answers one invocation, keyed on the verb that appears in args.
	reply func(args []string) ([]byte, error)
	calls [][]string
	bins  []string
}

func (f *fakeSystemctl) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.bins = append(f.bins, name)
	f.calls = append(f.calls, append([]string(nil), args...))
	if f.reply == nil {
		return nil, nil
	}
	return f.reply(args)
}

// systemctlVerb returns the first argument that is not a flag, so a fake can answer
// is-active, show and stop without caring whether --user is present.
func systemctlVerb(args []string) string {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			return arg
		}
	}
	return ""
}

// newTestSystemdUnit builds an adapter whose runner is the given fake.
func newTestSystemdUnit(t *testing.T, svc config.Service, fake *fakeSystemctl) *systemdUnitAdapter {
	t.Helper()
	if svc.Name == "" {
		svc.Name = "llamaswap-unit"
	}
	if svc.Unit == "" {
		svc.Unit = "llama-swap.service"
	}
	if svc.Timeout == 0 {
		svc.Timeout = config.Duration(2 * time.Second)
	}
	svc.Adapter = config.AdapterSystemdUnit

	a := newSystemdUnit(svc)
	a.runner = fake.run
	a.stopTimeout = 2 * time.Second
	return a
}

// errExitStatus stands in for the *exec.ExitError that systemctl produces when it
// exits non zero. Its content does not matter; only that it is not nil.
var errExitStatus = errors.New("exit status 3")

func TestSystemdUnitProbe(t *testing.T) {
	tests := []struct {
		name      string
		reply     func(args []string) ([]byte, error)
		wantErr   bool
		wantUp    bool
		wantItems []Item
		wantCalls int
	}{
		{
			name: "an active unit is up and described",
			reply: func(args []string) ([]byte, error) {
				switch systemctlVerb(args) {
				case "is-active":
					return []byte("active\n"), nil
				case "show":
					return []byte("ActiveState=active\nSubState=running\nMainPID=4242\n"), nil
				}
				return nil, errors.New("unexpected verb")
			},
			wantUp:    true,
			wantItems: []Item{{Name: "llama-swap.service", Detail: "active (running), main PID 4242"}},
			wantCalls: 2,
		},
		{
			name: "an inactive unit is an answer, not a failure",
			reply: func(args []string) ([]byte, error) {
				// systemctl is-active exits 3 for an inactive unit while still
				// naming the state on stdout.
				if systemctlVerb(args) == "show" {
					return []byte("LoadState=loaded\n"), nil
				}
				return []byte("inactive\n"), errExitStatus
			},
			wantUp: false,
			// A second read of LoadState, to separate a unit that is stopped
			// from one systemd has never heard of.
			wantCalls: 2,
		},
		{
			name: "a failed unit is reported as down without an error",
			reply: func(args []string) ([]byte, error) {
				if systemctlVerb(args) == "show" {
					return []byte("LoadState=loaded\n"), nil
				}
				return []byte("failed\n"), errExitStatus
			},
			wantUp:    false,
			wantCalls: 2,
		},
		{
			name: "an activating unit is not yet up",
			reply: func(args []string) ([]byte, error) {
				if systemctlVerb(args) == "show" {
					return []byte("LoadState=loaded\n"), nil
				}
				return []byte("activating\n"), errExitStatus
			},
			wantUp:    false,
			wantCalls: 2,
		},
		{
			name: "a systemctl that cannot run at all is a failure",
			reply: func(args []string) ([]byte, error) {
				return nil, errors.New("exec: \"systemctl\": executable file not found in $PATH")
			},
			wantErr:   true,
			wantCalls: 1,
		},
		{
			name: "silence with no error is still a failure",
			reply: func(args []string) ([]byte, error) {
				return []byte("  \n"), nil
			},
			wantErr:   true,
			wantCalls: 1,
		},
		{
			name: "a failed show leaves the probe standing",
			reply: func(args []string) ([]byte, error) {
				if systemctlVerb(args) == "is-active" {
					return []byte("active\n"), nil
				}
				return nil, errors.New("no such property")
			},
			wantUp: true,
			// Detail falls back to the state rather than failing the probe.
			wantItems: []Item{{Name: "llama-swap.service", Detail: "active"}},
			wantCalls: 2,
		},
		{
			name: "show without a MainPID still describes the sub state",
			reply: func(args []string) ([]byte, error) {
				if systemctlVerb(args) == "is-active" {
					return []byte("active\n"), nil
				}
				return []byte("ActiveState=active\nSubState=running\nMainPID=0\n"), nil
			},
			wantUp:    true,
			wantItems: []Item{{Name: "llama-swap.service", Detail: "active (running)"}},
			wantCalls: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeSystemctl{reply: tt.reply}
			a := newTestSystemdUnit(t, config.Service{}, fake)

			status, err := a.Probe(context.Background())
			if len(fake.calls) != tt.wantCalls {
				t.Errorf("systemctl was called %d time(s), want %d; calls: %v", len(fake.calls), tt.wantCalls, fake.calls)
			}
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Probe() succeeded, want an error; status = %+v", status)
				}
				return
			}
			if err != nil {
				t.Fatalf("Probe() failed: %v", err)
			}
			if status.Up != tt.wantUp {
				t.Errorf("Up = %v, want %v", status.Up, tt.wantUp)
			}
			if !reflect.DeepEqual(status.Items, tt.wantItems) {
				t.Errorf("Items = %+v, want %+v", status.Items, tt.wantItems)
			}
			if status.HeldMiB != 0 {
				t.Errorf("HeldMiB = %d, want 0: a unit says nothing about VRAM", status.HeldMiB)
			}
			if !status.HeldEstimated {
				t.Error("HeldEstimated = false, want true: the zero is absent data")
			}
			if status.IdleKnown {
				t.Error("IdleKnown = true, want false: an active unit may be busy or asleep")
			}
		})
	}
}

// TestSystemdUnitProbeIsReadOnly pins the verbs Probe may use. A mutating verb
// slipping into a probe would make `gpu-bouncer status` act on the machine.
func TestSystemdUnitProbeIsReadOnly(t *testing.T) {
	fake := &fakeSystemctl{reply: func(args []string) ([]byte, error) {
		if systemctlVerb(args) == "is-active" {
			return []byte("active\n"), nil
		}
		return []byte("ActiveState=active\nSubState=running\nMainPID=1\n"), nil
	}}
	a := newTestSystemdUnit(t, config.Service{}, fake)

	if _, err := a.Probe(context.Background()); err != nil {
		t.Fatalf("Probe() failed: %v", err)
	}
	readOnly := map[string]bool{"is-active": true, "show": true}
	for _, call := range fake.calls {
		if !readOnly[systemctlVerb(call)] {
			t.Errorf("Probe ran a non read only verb: %v", call)
		}
	}
}

func TestSystemdUnitUserFlagPlacement(t *testing.T) {
	tests := []struct {
		name     string
		userUnit bool
		wantArgs []string
	}{
		{
			name:     "a system unit takes no manager flag",
			userUnit: false,
			wantArgs: []string{"is-active", "llama-swap.service"},
		},
		{
			// --user has to come before the verb; systemctl rejects it after.
			name:     "a user unit gets --user before the verb",
			userUnit: true,
			wantArgs: []string{"--user", "is-active", "llama-swap.service"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeSystemctl{reply: func(args []string) ([]byte, error) {
				if systemctlVerb(args) == "show" {
					return []byte("LoadState=loaded\n"), nil
				}
				return []byte("inactive\n"), errExitStatus
			}}
			a := newTestSystemdUnit(t, config.Service{UserUnit: tt.userUnit}, fake)

			if _, err := a.Probe(context.Background()); err != nil {
				t.Fatalf("Probe() failed: %v", err)
			}
			if len(fake.calls) == 0 {
				t.Fatal("systemctl was never called")
			}
			if !reflect.DeepEqual(fake.calls[0], tt.wantArgs) {
				t.Errorf("argv = %v, want %v", fake.calls[0], tt.wantArgs)
			}
			// Every call must carry the manager selection, not just the first:
			// a follow up read against the wrong manager would answer about a
			// different unit entirely.
			for _, call := range fake.calls {
				hasUser := len(call) > 0 && call[0] == "--user"
				if hasUser != tt.userUnit {
					t.Errorf("call %v: --user present = %v, want %v", call, hasUser, tt.userUnit)
				}
			}
			if fake.bins[0] != "systemctl" {
				t.Errorf("binary = %q, want %q", fake.bins[0], "systemctl")
			}
		})
	}
}

// TestSystemdUnitStopRefusedWithoutAllowStop is the most important test here.
// The refusal has to happen before anything is executed, so that a service the
// user never permitted gpu-bouncer to touch cannot be touched by a bug further
// down the function.
func TestSystemdUnitStopRefusedWithoutAllowStop(t *testing.T) {
	fake := &fakeSystemctl{reply: func(args []string) ([]byte, error) {
		t.Fatalf("systemctl was invoked for a service without allow_stop: %v", args)
		return nil, nil
	}}
	a := newTestSystemdUnit(t, config.Service{AllowStop: false}, fake)

	result, err := a.Stop(context.Background())
	if err == nil {
		t.Fatalf("Stop() succeeded without allow_stop; result = %+v", result)
	}
	if !errors.Is(err, ErrNotPermitted) {
		t.Errorf("Stop() error = %v, want it to wrap ErrNotPermitted", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("systemctl was called %v, want no calls at all", fake.calls)
	}
	if result.Acted {
		t.Error("Acted = true, want false")
	}
}

func TestSystemdUnitStop(t *testing.T) {
	tests := []struct {
		name     string
		userUnit bool
		reply    func(args []string) ([]byte, error)
		wantArgs []string
		wantErr  bool
	}{
		{
			name:     "a permitted system unit is stopped",
			wantArgs: []string{"stop", "llama-swap.service"},
		},
		{
			name:     "a permitted user unit is stopped through the user manager",
			userUnit: true,
			wantArgs: []string{"--user", "stop", "llama-swap.service"},
		},
		{
			name: "a systemctl failure is reported",
			reply: func(args []string) ([]byte, error) {
				return nil, errors.New("systemctl stop llama-swap.service: exit status 4: Interactive authentication required.")
			},
			wantArgs: []string{"stop", "llama-swap.service"},
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeSystemctl{reply: tt.reply}
			a := newTestSystemdUnit(t, config.Service{AllowStop: true, UserUnit: tt.userUnit}, fake)

			result, err := a.Stop(context.Background())
			if len(fake.calls) != 1 {
				t.Fatalf("systemctl was called %d time(s), want 1; calls: %v", len(fake.calls), fake.calls)
			}
			if !reflect.DeepEqual(fake.calls[0], tt.wantArgs) {
				t.Errorf("argv = %v, want %v", fake.calls[0], tt.wantArgs)
			}
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Stop() succeeded, want an error; result = %+v", result)
				}
				if result.Acted {
					t.Error("Acted = true after a failed stop, want false")
				}
				return
			}
			if err != nil {
				t.Fatalf("Stop() failed: %v", err)
			}
			if !result.Acted {
				t.Error("Acted = false, want true")
			}
			if want := []string{"llama-swap.service"}; !reflect.DeepEqual(result.Targets, want) {
				t.Errorf("Targets = %v, want %v", result.Targets, want)
			}
			if result.Detail == "" {
				t.Error("Detail is empty, want an explanation of what happened")
			}
		})
	}
}

func TestSystemdUnitReleaseIsNotSupported(t *testing.T) {
	fake := &fakeSystemctl{reply: func(args []string) ([]byte, error) {
		t.Fatalf("Release invoked systemctl: %v", args)
		return nil, nil
	}}
	a := newTestSystemdUnit(t, config.Service{AllowStop: true}, fake)

	result, err := a.Release(context.Background())
	if err == nil {
		t.Fatalf("Release() succeeded, want an error; result = %+v", result)
	}
	if !errors.Is(err, ErrNotSupported) {
		t.Errorf("Release() error = %v, want it to wrap ErrNotSupported", err)
	}
	if !reflect.DeepEqual(result, Result{}) {
		t.Errorf("Result = %+v, want the zero value", result)
	}
	if len(fake.calls) != 0 {
		t.Errorf("systemctl was called %v, want no calls at all", fake.calls)
	}
}

func TestSystemdUnitIdentityAndCapabilities(t *testing.T) {
	a := newSystemdUnit(config.Service{
		Name:    "ollama-unit",
		Adapter: config.AdapterSystemdUnit,
		Unit:    "ollama.service",
		Timeout: config.Duration(time.Second),
	})

	if a.Name() != "ollama-unit" {
		t.Errorf("Name() = %q, want %q", a.Name(), "ollama-unit")
	}
	if a.Kind() != config.AdapterSystemdUnit {
		t.Errorf("Kind() = %q, want %q", a.Kind(), config.AdapterSystemdUnit)
	}
	// CanRelease must stay false: Release has no implementation, and a true
	// here would have the scheduler plan a step that can only ever fail.
	want := Capabilities{CanRelease: false, CanReportIdle: false, CanStop: true}
	if got := a.Capabilities(); got != want {
		t.Errorf("Capabilities() = %+v, want %+v", got, want)
	}
}

func TestParseSystemctlShow(t *testing.T) {
	// Values may contain '=' and may be empty; only the first separator counts.
	out := []byte("ActiveState=active\nSubState=running\nMainPID=0\nExecStart=/usr/bin/x --flag=1\nDescription=\n\n")
	props := parseSystemctlShow(out)

	want := map[string]string{
		"ActiveState": "active",
		"SubState":    "running",
		"MainPID":     "0",
		"ExecStart":   "/usr/bin/x --flag=1",
		"Description": "",
	}
	if !reflect.DeepEqual(props, want) {
		t.Errorf("parseSystemctlShow() = %+v, want %+v", props, want)
	}
}

// systemctl reports a nonexistent unit as "inactive" with exit 4, exactly as
// it reports a real stopped unit with exit 3. Only LoadState separates them,
// and a config naming a unit that does not exist must not read as a service
// that merely happens to be stopped.
func TestSystemdUnitProbeDistinguishesMissingUnit(t *testing.T) {
	tests := []struct {
		name      string
		loadState string
		wantErr   bool
	}{
		{name: "a real stopped unit is simply down", loadState: "loaded", wantErr: false},
		{name: "a unit systemd does not know is a config error", loadState: "not-found", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newSystemdUnit(config.Service{
				Name: "legacy", Adapter: config.AdapterSystemdUnit,
				Unit: "legacy.service", Timeout: config.Duration(time.Second),
			})
			a.runner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
				for _, arg := range args {
					if strings.Contains(arg, "LoadState") {
						return []byte("LoadState=" + tt.loadState + "\n"), nil
					}
				}
				// is-active: "inactive" on stdout alongside a non zero exit.
				return []byte("inactive\n"), errors.New("exit status 3")
			}

			status, err := a.Probe(context.Background())
			if tt.wantErr {
				if err == nil {
					t.Fatal("Probe succeeded for a unit systemd does not know, want an error")
				}
				if !strings.Contains(err.Error(), "legacy.service") {
					t.Errorf("error = %q, want it to name the unit", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if status.Up {
				t.Error("Up = true, want false for a stopped unit")
			}
		})
	}
}
