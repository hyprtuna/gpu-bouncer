package scheduler

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hyprtuna/gpu-bouncer/internal/config"
	"github.com/hyprtuna/gpu-bouncer/internal/gpu"
)

// cfg builds a config from name/priority pairs. Adapter details do not affect
// Decide, which reads capability from ServiceState, so they are left minimal.
func cfg(reactive bool, floor uint64, defaultWorkload string, prio map[string]int) config.Config {
	c := config.Config{
		Policy: config.Policy{
			VRAMFloorMiB:    floor,
			Reactive:        reactive,
			DefaultWorkload: defaultWorkload,
		},
	}
	names := make([]string, 0, len(prio))
	for n := range prio {
		names = append(names, n)
	}
	// Deterministic order so a config built from a map cannot change results.
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	for _, n := range names {
		c.Services = append(c.Services, config.Service{Name: n, Priority: prio[n]})
	}
	return c
}

// obs builds an Observation for an 8188 MiB GPU with the given used figure.
func obs(usedMiB uint64, services ...ServiceState) Observation {
	return Observation{
		Device:      gpu.Device{Index: 0, Name: "test GPU", TotalMiB: 8188, UsedMiB: usedMiB},
		DeviceKnown: true,
		Services:    services,
	}
}

// releasable is an up service whose adapter can drop models on request.
func releasable(name string, priority int, heldMiB uint64) ServiceState {
	return ServiceState{Name: name, Priority: priority, Up: true, HeldMiB: heldMiB, CanRelease: true}
}

// steps renders a plan's actions as "service:verb" so a golden case reads as
// the exact sequence expected, in order.
func steps(p Plan) []string {
	out := make([]string, 0, len(p.Actions))
	for _, a := range p.Actions {
		out = append(out, fmt.Sprintf("%s:%s", a.Service, a.Verb))
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func hasNote(p Plan, substr string) bool {
	for _, n := range p.Notes {
		if strings.Contains(n, substr) {
			return true
		}
	}
	return false
}

// TestDecideGolden pins the exact ordered decision for each situation. A
// change to any expected sequence here is a change to the product's behaviour.
func TestDecideGolden(t *testing.T) {
	tests := []struct {
		name        string
		cfg         config.Config
		obs         Observation
		wantSteps   []string
		wantTrigger Trigger
		wantBenefit string
		wantNote    string
	}{
		{
			name: "reactive off and nothing requested does nothing",
			cfg:  cfg(false, 2048, "", map[string]int{"ollama": 50, "comfyui": 20}),
			obs: obs(8000,
				releasable("ollama", 50, 0),
				releasable("comfyui", 20, 7000)),
			wantSteps:   nil,
			wantTrigger: TriggerNone,
			wantNote:    "reactive mode is off",
		},
		{
			name: "free VRAM above the floor does nothing",
			cfg:  cfg(true, 2048, "", map[string]int{"ollama": 50, "comfyui": 20}),
			obs: obs(1000,
				releasable("ollama", 50, 0),
				releasable("comfyui", 20, 1000)),
			wantSteps:   nil,
			wantTrigger: TriggerNone,
			wantNote:    "at or above the 2048 MiB floor",
		},
		{
			name: "reactive frees the lower priority holder",
			cfg:  cfg(true, 2048, "", map[string]int{"ollama": 50, "comfyui": 20}),
			obs: obs(7188, // 1000 MiB free, below the 2048 floor
				releasable("ollama", 50, 200),
				releasable("comfyui", 20, 6988)),
			wantSteps:   []string{"comfyui:release"},
			wantTrigger: TriggerReactive,
			wantBenefit: "ollama",
		},
		{
			name: "equal priority never evicts",
			cfg:  cfg(true, 2048, "", map[string]int{"ollama": 50, "comfyui": 50}),
			obs: obs(7188,
				releasable("ollama", 50, 200),
				releasable("comfyui", 50, 6988)),
			wantSteps:   nil,
			wantTrigger: TriggerReactive,
			wantBenefit: "comfyui", // highest priority up, name order settles the tie
			wantNote:    "best effort",
		},
		{
			name: "lowest priority goes first, then the largest holder",
			cfg: cfg(true, 4096, "", map[string]int{
				"ollama": 90, "comfyui": 20, "sdnext": 20, "trainer": 10,
			}),
			obs: obs(8000, // 188 MiB free
				releasable("ollama", 90, 100),
				releasable("comfyui", 20, 3000),
				releasable("sdnext", 20, 4000),
				releasable("trainer", 10, 900)),
			// trainer is the lowest priority so it goes first even though it
			// is the smallest. Within priority 20, sdnext is larger than
			// comfyui so it goes next. That reaches the target, so comfyui is
			// left alone.
			wantSteps:   []string{"trainer:release", "sdnext:release"},
			wantTrigger: TriggerReactive,
			wantBenefit: "ollama",
			wantNote:    "comfyui left alone",
		},
		{
			name: "an explicit request outranks reactive policy",
			cfg:  cfg(true, 1024, "", map[string]int{"ollama": 50, "comfyui": 20, "trainer": 10}),
			obs: func() Observation {
				o := obs(7188,
					releasable("ollama", 50, 100),
					releasable("comfyui", 20, 3000),
					releasable("trainer", 10, 4088))
				o.Claims = []Claim{{Service: "comfyui", NeedMiB: 4096, At: time.Unix(100, 0)}}
				return o
			}(),
			// comfyui asked, so memory is freed for comfyui, and only from
			// services below it. ollama at 50 is untouchable here.
			wantSteps:   []string{"trainer:release"},
			wantTrigger: TriggerRequest,
			wantBenefit: "comfyui",
		},
		{
			name: "a request cannot evict a higher priority service",
			cfg:  cfg(true, 1024, "", map[string]int{"ollama": 50, "comfyui": 20}),
			obs: func() Observation {
				o := obs(8100, // 88 MiB free
					releasable("ollama", 50, 8000),
					releasable("comfyui", 20, 0))
				o.Claims = []Claim{{Service: "comfyui", NeedMiB: 4096, At: time.Unix(100, 0)}}
				return o
			}(),
			wantSteps:   nil,
			wantTrigger: TriggerRequest,
			wantBenefit: "comfyui",
			wantNote:    "best effort: the plan expects to free 0 MiB of the 4008 MiB needed",
		},
		{
			name: "the highest priority claim wins when several are outstanding",
			cfg:  cfg(true, 1024, "", map[string]int{"ollama": 50, "comfyui": 20, "trainer": 10}),
			obs: func() Observation {
				o := obs(7188,
					releasable("ollama", 50, 100),
					releasable("comfyui", 20, 3000),
					releasable("trainer", 10, 4088))
				o.Claims = []Claim{
					{Service: "trainer", NeedMiB: 2048, At: time.Unix(50, 0)},
					{Service: "ollama", NeedMiB: 4096, At: time.Unix(100, 0)},
				}
				return o
			}(),
			// ollama outranks trainer despite asking later, so memory is
			// freed for ollama. Trainer alone covers the shortfall, so
			// comfyui is spared.
			wantSteps:   []string{"trainer:release"},
			wantTrigger: TriggerRequest,
			wantBenefit: "ollama",
			wantNote:    "comfyui left alone",
		},
		{
			name: "a busy service is skipped rather than falsely reported freed",
			cfg:  cfg(true, 2048, "", map[string]int{"ollama": 50, "comfyui": 20}),
			obs: obs(7188,
				releasable("ollama", 50, 200),
				ServiceState{Name: "comfyui", Priority: 20, Up: true, HeldMiB: 6988,
					CanRelease: true, IdleKnown: true, Idle: false}),
			wantSteps:   nil,
			wantTrigger: TriggerReactive,
			wantBenefit: "ollama",
			wantNote:    "comfyui skipped, it is busy",
		},
		{
			name: "a unit is stopped only when allow_stop permits it",
			cfg:  cfg(true, 2048, "", map[string]int{"ollama": 50, "legacy": 20}),
			obs: obs(7188,
				releasable("ollama", 50, 200),
				ServiceState{Name: "legacy", Priority: 20, Up: true, HeldMiB: 6988,
					CanRelease: false, StopUnit: true, AllowStop: true}),
			wantSteps:   []string{"legacy:stop"},
			wantTrigger: TriggerReactive,
			wantBenefit: "ollama",
		},
		{
			name: "without allow_stop a stop only service is left alone",
			cfg:  cfg(true, 2048, "", map[string]int{"ollama": 50, "legacy": 20}),
			obs: obs(7188,
				releasable("ollama", 50, 200),
				ServiceState{Name: "legacy", Priority: 20, Up: true, HeldMiB: 6988,
					CanRelease: false, StopUnit: true, AllowStop: false}),
			wantSteps:   nil,
			wantTrigger: TriggerReactive,
			wantBenefit: "ollama",
			wantNote:    "legacy skipped, it has no release API and allow_stop is false",
		},
		{
			name: "a service whose probe failed is never acted on, and says so",
			cfg:  cfg(true, 2048, "", map[string]int{"ollama": 50, "comfyui": 20}),
			obs: obs(7188,
				releasable("ollama", 50, 200),
				ServiceState{Name: "comfyui", Priority: 20, Up: true, HeldMiB: 6988,
					CanRelease: true, ProbeErr: "connection refused"}),
			wantSteps:   nil,
			wantTrigger: TriggerReactive,
			wantBenefit: "ollama",
			wantNote:    "comfyui left alone, its last probe failed: connection refused",
		},
		{
			name: "a down service is not a candidate, and says so",
			cfg:  cfg(true, 2048, "", map[string]int{"ollama": 50, "comfyui": 20}),
			obs: obs(7188,
				releasable("ollama", 50, 200),
				ServiceState{Name: "comfyui", Priority: 20, Up: false, HeldMiB: 6988, CanRelease: true}),
			wantSteps:   nil,
			wantTrigger: TriggerReactive,
			wantBenefit: "ollama",
			wantNote:    "comfyui left alone, it is not running",
		},
		{
			name: "a peer at equal priority holding VRAM is passed over with a note",
			cfg:  cfg(true, 100000, "", map[string]int{"benef": 50, "peer": 50}),
			obs: obs(354,
				releasable("benef", 50, 100),
				releasable("peer", 50, 3000)),
			wantSteps:   nil,
			wantTrigger: TriggerReactive,
			wantBenefit: "benef",
			wantNote:    "peer left alone, priority 50 is not below benef at 50",
		},
		{
			name: "a higher priority holder is passed over with a note",
			cfg:  cfg(true, 4096, "ollama", map[string]int{"ollama": 50, "big": 90}),
			obs: obs(8000,
				releasable("ollama", 50, 100),
				releasable("big", 90, 7000)),
			wantSteps:   nil,
			wantTrigger: TriggerReactive,
			wantBenefit: "ollama",
			wantNote:    "big left alone, priority 90 is not below ollama at 50",
		},
		{
			name: "the default workload is defended when it is up",
			cfg:  cfg(true, 2048, "comfyui", map[string]int{"ollama": 90, "comfyui": 20, "trainer": 10}),
			obs: obs(7188,
				releasable("ollama", 90, 100),
				releasable("comfyui", 20, 3000),
				releasable("trainer", 10, 4088)),
			// comfyui is defended despite ollama outranking it, and only
			// trainer sits below comfyui.
			wantSteps:   []string{"trainer:release"},
			wantTrigger: TriggerReactive,
			wantBenefit: "comfyui",
		},
		{
			name: "a default workload that is down falls back to the top live service",
			cfg:  cfg(true, 2048, "comfyui", map[string]int{"ollama": 90, "comfyui": 20, "trainer": 10}),
			obs: obs(7188,
				releasable("ollama", 90, 100),
				ServiceState{Name: "comfyui", Priority: 20, Up: false, CanRelease: true},
				releasable("trainer", 10, 6988)),
			wantSteps:   []string{"trainer:release"},
			wantTrigger: TriggerReactive,
			wantBenefit: "ollama",
			wantNote:    `default workload "comfyui" is not up`,
		},
		{
			name: "no live service means nobody to free memory for",
			cfg:  cfg(true, 2048, "", map[string]int{"ollama": 50, "comfyui": 20}),
			obs: obs(8100,
				ServiceState{Name: "ollama", Priority: 50, Up: false},
				ServiceState{Name: "comfyui", Priority: 20, Up: false}),
			wantSteps:   nil,
			wantTrigger: TriggerNone,
			wantNote:    "no configured service is up",
		},
		{
			name: "a zero floor with reactive on defends nothing",
			cfg:  cfg(true, 0, "", map[string]int{"ollama": 50, "comfyui": 20}),
			obs: obs(8100,
				releasable("ollama", 50, 100),
				releasable("comfyui", 20, 8000)),
			wantSteps:   nil,
			wantTrigger: TriggerNone,
			wantNote:    "there is no floor to defend",
		},
		{
			name: "a service holding nothing is not evicted, and says so",
			cfg:  cfg(true, 2048, "", map[string]int{"ollama": 50, "comfyui": 20, "idle": 5}),
			obs: obs(7188,
				releasable("ollama", 50, 200),
				releasable("comfyui", 20, 6988),
				releasable("idle", 5, 0)),
			wantSteps:   []string{"comfyui:release"},
			wantTrigger: TriggerReactive,
			wantBenefit: "ollama",
			wantNote:    "idle left alone, it holds no VRAM",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := Decide(tt.cfg, tt.obs)
			if got := steps(plan); !equal(got, tt.wantSteps) {
				t.Errorf("actions = %v, want %v\nnotes: %v", got, tt.wantSteps, plan.Notes)
			}
			if plan.Trigger != tt.wantTrigger {
				t.Errorf("trigger = %q, want %q", plan.Trigger, tt.wantTrigger)
			}
			if tt.wantBenefit != "" && plan.Beneficiary != tt.wantBenefit {
				t.Errorf("beneficiary = %q, want %q", plan.Beneficiary, tt.wantBenefit)
			}
			if tt.wantNote != "" && !hasNote(plan, tt.wantNote) {
				t.Errorf("no note containing %q\nnotes: %v", tt.wantNote, plan.Notes)
			}
			// An empty plan must never be silent.
			if plan.Empty() && len(plan.Notes) == 0 {
				t.Error("plan is empty and carries no explanation")
			}
		})
	}
}

// Without a readable GPU the daemon must do nothing at all, whatever the
// config says, because every decision depends on a free VRAM figure.
func TestDecideRefusesWithoutGPUState(t *testing.T) {
	c := cfg(true, 4096, "", map[string]int{"ollama": 50, "comfyui": 20})
	o := Observation{
		DeviceKnown: false,
		Services: []ServiceState{
			releasable("ollama", 50, 100),
			releasable("comfyui", 20, 8000),
		},
		Claims: []Claim{{Service: "ollama", NeedMiB: 8000, At: time.Unix(1, 0)}},
	}
	plan := Decide(c, o)
	if !plan.Empty() {
		t.Fatalf("actions = %v, want none when GPU state is unknown", steps(plan))
	}
	if !hasNote(plan, "GPU state could not be read") {
		t.Errorf("notes = %v, want an explanation", plan.Notes)
	}
}

// Decide must not depend on the order services arrive in.
func TestDecideIsOrderIndependent(t *testing.T) {
	c := cfg(true, 4096, "", map[string]int{"ollama": 90, "comfyui": 20, "sdnext": 20, "trainer": 10})
	forward := obs(8000,
		releasable("ollama", 90, 100),
		releasable("comfyui", 20, 3000),
		releasable("sdnext", 20, 4000),
		releasable("trainer", 10, 900))
	reversed := obs(8000,
		releasable("trainer", 10, 900),
		releasable("sdnext", 20, 4000),
		releasable("comfyui", 20, 3000),
		releasable("ollama", 90, 100))

	a, b := steps(Decide(c, forward)), steps(Decide(c, reversed))
	if !equal(a, b) {
		t.Fatalf("plan depends on input order: %v vs %v", a, b)
	}
}

func TestExpectedFreeMiB(t *testing.T) {
	c := cfg(true, 4096, "", map[string]int{"ollama": 90, "comfyui": 20})
	plan := Decide(c, obs(8000, releasable("ollama", 90, 100), releasable("comfyui", 20, 7000)))
	if got, want := plan.CurrentFreeMiB, uint64(188); got != want {
		t.Errorf("CurrentFreeMiB = %d, want %d", got, want)
	}
	if got, want := plan.ExpectedFreeMiB(), uint64(7188); got != want {
		t.Errorf("ExpectedFreeMiB = %d, want %d", got, want)
	}

	// A service claiming more than the card holds must not make the plan
	// promise more than the card has.
	plan = Decide(cfg(true, 8000, "", map[string]int{"ollama": 90, "comfyui": 20}),
		obs(354, releasable("ollama", 90, 100), releasable("comfyui", 20, 4096)))
	if got, want := plan.ExpectedFreeMiB(), uint64(8188); got != want {
		t.Errorf("ExpectedFreeMiB = %d, want it capped at the card's %d", got, want)
	}
}

// Every service considered and passed over must appear in the notes with
// its reason, one note per service, whichever reason dropped it.
func TestEveryPassedOverServiceIsNoted(t *testing.T) {
	c := cfg(true, 100000, "benef", map[string]int{"benef": 50, "peer": 50, "boss": 90, "down": 10, "broken": 10, "empty": 10})
	plan := Decide(c, obs(354,
		releasable("benef", 50, 100),
		releasable("peer", 50, 3000),
		releasable("boss", 90, 1000),
		ServiceState{Name: "down", Priority: 10, Up: false, HeldMiB: 500, CanRelease: true},
		ServiceState{Name: "broken", Priority: 10, Up: true, HeldMiB: 500, CanRelease: true, ProbeErr: "connection refused"},
		releasable("empty", 10, 0)))
	if plan.Beneficiary != "benef" {
		t.Fatalf("beneficiary = %q, want benef, the default workload", plan.Beneficiary)
	}
	for _, want := range []string{
		"peer left alone, priority 50 is not below benef at 50",
		"boss left alone, priority 90 is not below benef at 50",
		"down left alone, it is not running",
		"broken left alone, its last probe failed: connection refused",
		"empty left alone, it holds no VRAM",
	} {
		if !hasNote(plan, want) {
			t.Errorf("no note %q\nnotes: %v", want, plan.Notes)
		}
	}
	if hasNote(plan, "benef left alone") {
		t.Errorf("the beneficiary was noted as passed over: %v", plan.Notes)
	}
	// down, broken and empty are all unusable, so the plan is rightly empty;
	// the notes above are what keep that emptiness from being silent.
	if !plan.Empty() {
		t.Errorf("actions = %v, want none", steps(plan))
	}
}

// The operator override is still a decision about a card, and without a
// reading of that card it refuses like Decide does.
func TestEvictRefusesWithoutGPUState(t *testing.T) {
	o := Observation{
		DeviceKnown: false,
		DeviceErr:   "nvml device 0 memory: Unknown Error",
		Services:    []ServiceState{releasable("ollama", 50, 3000), releasable("comfyui", 20, 2000)},
	}
	for name, plan := range map[string]Plan{
		"evict":            Evict(o, []string{"ollama"}),
		"evict all except": EvictAllExcept(o, "comfyui"),
	} {
		if !plan.Empty() {
			t.Errorf("%s: actions = %v, want none without a GPU reading", name, steps(plan))
		}
		if !hasNote(plan, "GPU state could not be read, so no action is safe: nvml device 0 memory") {
			t.Errorf("%s: notes = %v, want the refusal with its reason", name, plan.Notes)
		}
	}
}

func TestEvict(t *testing.T) {
	o := obs(7000,
		releasable("ollama", 50, 3000),
		ServiceState{Name: "comfyui", Priority: 20, Up: true, HeldMiB: 4000,
			CanRelease: true, IdleKnown: true, Idle: false},
		ServiceState{Name: "legacy", Priority: 10, Up: true, HeldMiB: 100,
			StopUnit: true, AllowStop: false},
		ServiceState{Name: "down", Priority: 5, Up: false})

	t.Run("evicts regardless of priority", func(t *testing.T) {
		plan := Evict(o, []string{"ollama"})
		if got, want := steps(plan), []string{"ollama:release"}; !equal(got, want) {
			t.Fatalf("actions = %v, want %v", got, want)
		}
		if plan.Trigger != TriggerEvict {
			t.Errorf("trigger = %q, want %q", plan.Trigger, TriggerEvict)
		}
	})

	t.Run("still honours every safety rule", func(t *testing.T) {
		plan := Evict(o, []string{"comfyui", "legacy", "down", "ghost"})
		if got := steps(plan); len(got) != 0 {
			t.Fatalf("actions = %v, want none: every one of these is forbidden or pointless", got)
		}
		for _, want := range []string{
			"comfyui skipped, it is busy",
			"legacy skipped, it has no release API and allow_stop is false",
			"down skipped, it is not running",
			"ghost skipped, it is not in the config",
		} {
			if !hasNote(plan, want) {
				t.Errorf("no note containing %q\nnotes: %v", want, plan.Notes)
			}
		}
	})
}

func TestEvictAllExcept(t *testing.T) {
	o := obs(7000,
		releasable("ollama", 50, 3000),
		releasable("comfyui", 20, 3000),
		releasable("trainer", 10, 1000))

	t.Run("frees everything but the kept service", func(t *testing.T) {
		plan := EvictAllExcept(o, "ollama")
		want := []string{"comfyui:release", "trainer:release"}
		if got := steps(plan); !equal(got, want) {
			t.Fatalf("actions = %v, want %v", got, want)
		}
		if plan.Beneficiary != "ollama" {
			t.Errorf("beneficiary = %q, want ollama", plan.Beneficiary)
		}
	})

	t.Run("a misspelled keep name evicts nothing", func(t *testing.T) {
		plan := EvictAllExcept(o, "olama")
		if !plan.Empty() {
			t.Fatalf("actions = %v, want none: a typo must not clear the GPU", steps(plan))
		}
		if !hasNote(plan, "is not in the config") {
			t.Errorf("notes = %v, want an explanation", plan.Notes)
		}
		// The refusal still reports the real free figure, not zero.
		if got, want := plan.CurrentFreeMiB, uint64(1188); got != want {
			t.Errorf("CurrentFreeMiB = %d, want %d on the refusal plan", got, want)
		}
	})
}
