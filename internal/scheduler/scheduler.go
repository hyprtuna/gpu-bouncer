// Package scheduler decides what gpu-bouncer should do, and nothing else.
//
// Decide is a pure function of configuration plus observed state. It performs
// no I/O, contacts no service and changes nothing, which is what makes
// `gpu-bouncer plan` an exact preview of `gpu-bouncer daemon`: both call this
// same function and the plan command simply declines to execute the result.
//
// Priority is an integer and higher wins. Services at equal priority never
// evict one another.
package scheduler

import (
	"fmt"
	"sort"
	"time"

	"github.com/hyprtuna/gpu-bouncer/internal/config"
	"github.com/hyprtuna/gpu-bouncer/internal/gpu"
)

// Verb is what to do to a service.
type Verb string

const (
	// VerbRelease asks a service, through its own API, to drop what it is
	// holding. It is the graceful path and needs no extra permission, because
	// it is the same request the service would honour from any client.
	VerbRelease Verb = "release"
	// VerbStop stops a systemd unit. It is process level action and is only
	// ever emitted for a service whose config sets allow_stop = true.
	VerbStop Verb = "stop"
)

// Trigger explains why a plan exists.
type Trigger string

const (
	// TriggerNone means no demand: the plan is empty by design.
	TriggerNone Trigger = "none"
	// TriggerRequest means an explicit claim was outstanding.
	TriggerRequest Trigger = "request"
	// TriggerReactive means free VRAM sat below the policy floor.
	TriggerReactive Trigger = "reactive"
	// TriggerEvict means an operator asked for a specific eviction.
	TriggerEvict Trigger = "evict"
)

// ServiceState is one configured service as most recently observed. Anything
// gpu-bouncer could not determine is marked, rather than guessed.
type ServiceState struct {
	Name     string
	Priority int
	Adapter  config.AdapterKind

	// Up is whether the service answered its last probe.
	Up bool
	// HeldMiB is the VRAM attributed to this service.
	HeldMiB uint64
	// HeldEstimated marks a figure that was derived rather than measured.
	HeldEstimated bool

	// CanRelease is whether the adapter can ask the service to drop its
	// models over its own API.
	CanRelease bool
	// AllowStop mirrors the config key of the same name.
	AllowStop bool
	// StopUnit is whether a systemd unit is available to stop.
	StopUnit bool

	// Idle is whether the service reports no work in flight. IdleKnown is
	// false for adapters that cannot tell, in which case Idle is ignored.
	Idle      bool
	IdleKnown bool

	// ProbeErr is non empty when the last probe failed. A service we cannot
	// see is never acted on.
	ProbeErr string

	// CooldownUntil, when set, marks a service that a recent action did not
	// help. Reactive plans leave it alone until then; the daemon sets it only
	// on the observations it builds for its own poll loop, so an explicit
	// request or evict never sees it.
	CooldownUntil time.Time

	// ActionInFlight marks a service the daemon is still acting on. No plan
	// names it again until that action has finished: one action per service
	// at a time, and never a second release racing the first one's drain.
	ActionInFlight bool
}

// Claim is an outstanding explicit request for the GPU.
type Claim struct {
	Service string
	// NeedMiB is how much free VRAM the claimant wants. Zero means "at least
	// the policy floor".
	NeedMiB uint64
	At      time.Time
}

// Observation is everything the daemon knew at one instant.
type Observation struct {
	// Device is the arbitrated GPU. DeviceKnown is false when VRAM could not
	// be read at all, in which case the scheduler refuses to act.
	Device      gpu.Device
	DeviceKnown bool
	// DeviceErr says why DeviceKnown is false, so the refusal can be explained.
	DeviceErr string

	Services []ServiceState
	Claims   []Claim
}

// Action is one thing to do to one service.
type Action struct {
	Service string `json:"service"`
	Verb    Verb   `json:"verb"`
	Reason  string `json:"reason"`
	// ExpectFreeMiB is how much VRAM this action is expected to release. It is
	// an expectation, not a promise: the daemon logs the measured before and
	// after figures separately.
	ExpectFreeMiB uint64 `json:"expect_free_mib"`
}

// Plan is the scheduler's complete answer, including what it decided not to do.
type Plan struct {
	Trigger Trigger `json:"trigger"`
	// Beneficiary is the service the plan is freeing memory for, if any.
	Beneficiary string `json:"beneficiary"`
	// CurrentFreeMiB and TargetFreeMiB frame the decision. TotalMiB is the
	// card's size, which caps what any plan can expect to free.
	CurrentFreeMiB uint64   `json:"current_free_mib"`
	TargetFreeMiB  uint64   `json:"target_free_mib"`
	TotalMiB       uint64   `json:"total_mib"`
	Actions        []Action `json:"actions"`
	// Notes record every service considered and passed over, and why. They
	// exist so that an empty plan is never silent.
	Notes []string `json:"notes"`
}

// newPlan starts a plan whose lists are empty rather than absent, so they
// serialise as [] and a consumer never has to handle null.
func newPlan(trigger Trigger) Plan {
	return Plan{Trigger: trigger, Actions: []Action{}, Notes: []string{}}
}

// deviceUnknownNote is the one reason every plan gives for refusing without a
// VRAM reading, with the cause appended when the observer recorded one.
func (o Observation) deviceUnknownNote() string {
	note := "GPU state could not be read, so no action is safe"
	if o.DeviceErr != "" {
		note += ": " + o.DeviceErr
	}
	return note
}

// Empty reports whether the plan would change anything.
func (p Plan) Empty() bool { return len(p.Actions) == 0 }

// ExpectedFreeMiB is free VRAM now plus what the actions expect to release,
// capped at the card's total: a service's own account of its holdings can
// exceed what the driver sees, and a plan must not promise more memory than
// the card has.
func (p Plan) ExpectedFreeMiB() uint64 {
	total := p.CurrentFreeMiB
	for _, a := range p.Actions {
		total += a.ExpectFreeMiB
	}
	if p.TotalMiB > 0 && total > p.TotalMiB {
		return p.TotalMiB
	}
	return total
}

// Decide returns the plan for one observation. It never returns an error: an
// undecidable situation produces an empty plan carrying the reason in Notes,
// because a daemon that cannot see clearly must do nothing rather than guess.
func Decide(cfg config.Config, obs Observation) Plan {
	plan := newPlan(TriggerNone)
	if !obs.DeviceKnown {
		plan.Notes = append(plan.Notes, obs.deviceUnknownNote())
		return plan
	}
	plan.CurrentFreeMiB = obs.Device.FreeMiB()
	plan.TotalMiB = obs.Device.TotalMiB

	beneficiary, target, trigger, notes := selectDemand(cfg, obs)
	plan.Notes = append(plan.Notes, notes...)
	plan.Trigger = trigger
	plan.Beneficiary = beneficiary
	plan.TargetFreeMiB = target
	if trigger == TriggerNone {
		return plan
	}

	if plan.CurrentFreeMiB >= target {
		plan.Notes = append(plan.Notes, fmt.Sprintf(
			"%d MiB already free, at or above the %d MiB target", plan.CurrentFreeMiB, target))
		return plan
	}
	needed := target - plan.CurrentFreeMiB

	benefit, haveBenefit := findService(obs.Services, beneficiary)
	if !haveBenefit {
		plan.Notes = append(plan.Notes, fmt.Sprintf("service %q is not configured", beneficiary))
		plan.Trigger = TriggerNone
		return plan
	}

	candidates, passedOver := evictionCandidates(obs.Services, benefit)
	plan.Notes = append(plan.Notes, passedOver...)
	freed := uint64(0)
	for _, c := range candidates {
		if freed >= needed {
			plan.Notes = append(plan.Notes, fmt.Sprintf(
				"%s left alone, the target is already met without it", c.Name))
			continue
		}
		verb, reason, ok := chooseVerb(c)
		if !ok {
			plan.Notes = append(plan.Notes, fmt.Sprintf("%s skipped, %s", c.Name, reason))
			continue
		}
		plan.Actions = append(plan.Actions, Action{
			Service:       c.Name,
			Verb:          verb,
			Reason:        fmt.Sprintf("priority %d is below %s at %d, %s", c.Priority, benefit.Name, benefit.Priority, reason),
			ExpectFreeMiB: c.HeldMiB,
		})
		freed += c.HeldMiB
	}

	if freed < needed {
		plan.Notes = append(plan.Notes, fmt.Sprintf(
			"best effort: the plan expects to free %d MiB of the %d MiB needed", freed, needed))
	}
	return plan
}

// selectDemand answers who wants the GPU and how much free VRAM they need.
func selectDemand(cfg config.Config, obs Observation) (beneficiary string, target uint64, trigger Trigger, notes []string) {
	floor := cfg.Policy.VRAMFloorMiB

	// An explicit claim always outranks reactive policy. Among several, the
	// highest priority wins; equal priority is settled by who asked first, so
	// the answer never depends on map or slice ordering.
	if len(obs.Claims) > 0 {
		best, ok := bestClaim(cfg, obs)
		if ok {
			need := best.NeedMiB
			if need == 0 {
				need = floor
			}
			return best.Service, need, TriggerRequest, nil
		}
		notes = append(notes, "outstanding claims name no configured service, ignoring them")
	}

	if !cfg.Policy.Reactive {
		return "", 0, TriggerNone, append(notes, "reactive mode is off and nothing was requested")
	}
	if floor == 0 {
		return "", 0, TriggerNone, append(notes, "reactive mode is on but policy.vram_floor_mib is 0, so there is no floor to defend")
	}
	if obs.Device.FreeMiB() >= floor {
		return "", 0, TriggerNone, append(notes, fmt.Sprintf(
			"%d MiB free is at or above the %d MiB floor", obs.Device.FreeMiB(), floor))
	}

	// Reactive mode defends the configured default workload, or failing that
	// the highest priority service that is currently up.
	if wl := cfg.Policy.DefaultWorkload; wl != "" {
		svc, ok := findService(obs.Services, wl)
		if ok && svc.Up && svc.ProbeErr == "" {
			return wl, floor, TriggerReactive, notes
		}
		notes = append(notes, fmt.Sprintf("default workload %q is not up, falling back to the highest priority live service", wl))
	}
	top, ok := highestPriorityUp(obs.Services)
	if !ok {
		return "", 0, TriggerNone, append(notes, "no configured service is up, so there is nobody to free memory for")
	}
	return top.Name, floor, TriggerReactive, notes
}

// bestClaim picks the winning claim: highest priority, then earliest.
func bestClaim(cfg config.Config, obs Observation) (Claim, bool) {
	type scored struct {
		claim    Claim
		priority int
	}
	var live []scored
	for _, c := range obs.Claims {
		svc, ok := cfg.Service(c.Service)
		if !ok {
			continue
		}
		live = append(live, scored{claim: c, priority: svc.Priority})
	}
	if len(live) == 0 {
		return Claim{}, false
	}
	sort.SliceStable(live, func(i, j int) bool {
		if live[i].priority != live[j].priority {
			return live[i].priority > live[j].priority
		}
		if !live[i].claim.At.Equal(live[j].claim.At) {
			return live[i].claim.At.Before(live[j].claim.At)
		}
		return live[i].claim.Service < live[j].claim.Service
	})
	return live[0].claim, true
}

// evictionCandidates returns the services that may be freed on behalf of
// beneficiary, in the order they should be freed: lowest priority first, and
// within a priority the largest holder first so the target is reached in the
// fewest disruptions. The ordering is total, so the plan is deterministic.
//
// Every service it passes over gets a note saying why, so that an empty plan
// is never silent about a peer that was considered.
func evictionCandidates(services []ServiceState, beneficiary ServiceState) (candidates []ServiceState, notes []string) {
	for _, s := range services {
		switch {
		case s.Name == beneficiary.Name:
			continue
		case s.ProbeErr != "":
			notes = append(notes, fmt.Sprintf("%s left alone, its last probe failed: %s", s.Name, s.ProbeErr))
			continue
		case !s.Up:
			notes = append(notes, fmt.Sprintf("%s left alone, it is not running", s.Name))
			continue
		case s.Priority >= beneficiary.Priority:
			// Equal priority never evicts, so a tie cannot ping pong.
			notes = append(notes, fmt.Sprintf("%s left alone, priority %d is not below %s at %d",
				s.Name, s.Priority, beneficiary.Name, beneficiary.Priority))
			continue
		case s.HeldMiB == 0:
			notes = append(notes, fmt.Sprintf("%s left alone, it holds no VRAM", s.Name))
			continue
		case !s.CooldownUntil.IsZero():
			notes = append(notes, fmt.Sprintf("%s left alone, cooling down until %s after an action on it freed nothing",
				s.Name, s.CooldownUntil.Format(time.RFC3339)))
			continue
		case s.ActionInFlight:
			notes = append(notes, fmt.Sprintf("%s left alone, an action on it is still in flight", s.Name))
			continue
		}
		candidates = append(candidates, s)
	}
	out := candidates
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		if out[i].HeldMiB != out[j].HeldMiB {
			return out[i].HeldMiB > out[j].HeldMiB
		}
		return out[i].Name < out[j].Name
	})
	return out, notes
}

// chooseVerb picks the least invasive action a service permits, or explains
// why none is permitted. Graceful release is always preferred, and a stop is
// only ever reachable through an explicit allow_stop.
func chooseVerb(s ServiceState) (Verb, string, bool) {
	if s.IdleKnown && !s.Idle {
		// ComfyUI accepts a free request while a job is running, returns 200
		// and does nothing, because it reads the flag only after the job
		// finishes. Emitting an action here would log a success that did not
		// happen.
		return "", "it is busy and a release would silently not take effect", false
	}
	if s.CanRelease {
		return VerbRelease, "its API can be asked to drop what it holds", true
	}
	if s.StopUnit && s.AllowStop {
		return VerbStop, "it has no release API and allow_stop permits stopping its unit", true
	}
	if s.StopUnit && !s.AllowStop {
		return "", "it has no release API and allow_stop is false", false
	}
	return "", "its adapter offers no way to release VRAM", false
}

func findService(services []ServiceState, name string) (ServiceState, bool) {
	for _, s := range services {
		if s.Name == name {
			return s, true
		}
	}
	return ServiceState{}, false
}

func highestPriorityUp(services []ServiceState) (ServiceState, bool) {
	var best ServiceState
	found := false
	for _, s := range services {
		if !s.Up || s.ProbeErr != "" {
			continue
		}
		if !found || s.Priority > best.Priority || (s.Priority == best.Priority && s.Name < best.Name) {
			best, found = s, true
		}
	}
	return best, found
}

// Evict builds a plan that frees the named services now, regardless of
// priority or of how much VRAM is free. It is the operator override behind
// `gpu-bouncer evict`, and it still refuses anything the safety rules forbid.
func Evict(obs Observation, names []string) Plan {
	plan := newPlan(TriggerEvict)
	if !obs.DeviceKnown {
		// Same rule as Decide: an operator override is still an action the
		// daemon would take on a card it cannot see, and the before and
		// after figures it reports would both be zero.
		plan.Notes = append(plan.Notes, obs.deviceUnknownNote())
		return plan
	}
	plan.CurrentFreeMiB = obs.Device.FreeMiB()
	plan.TotalMiB = obs.Device.TotalMiB
	for _, name := range names {
		svc, ok := findService(obs.Services, name)
		if !ok {
			plan.Notes = append(plan.Notes, fmt.Sprintf("%s skipped, it is not in the config", name))
			continue
		}
		if svc.ProbeErr != "" {
			plan.Notes = append(plan.Notes, fmt.Sprintf("%s skipped, its last probe failed: %s", name, svc.ProbeErr))
			continue
		}
		if !svc.Up {
			plan.Notes = append(plan.Notes, fmt.Sprintf("%s skipped, it is not running", name))
			continue
		}
		if svc.ActionInFlight {
			plan.Notes = append(plan.Notes, fmt.Sprintf("%s skipped, an action on it is still in flight", name))
			continue
		}
		verb, reason, ok := chooseVerb(svc)
		if !ok {
			plan.Notes = append(plan.Notes, fmt.Sprintf("%s skipped, %s", name, reason))
			continue
		}
		plan.Actions = append(plan.Actions, Action{
			Service:       name,
			Verb:          verb,
			Reason:        "explicitly evicted, " + reason,
			ExpectFreeMiB: svc.HeldMiB,
		})
	}
	return plan
}

// EvictAllExcept builds a plan that frees every configured service except the
// named one. The kept service does not have to be running.
func EvictAllExcept(obs Observation, keep string) Plan {
	if _, ok := findService(obs.Services, keep); !ok {
		plan := newPlan(TriggerEvict)
		plan.Notes = append(plan.Notes, fmt.Sprintf("refusing to evict anything: %q is not in the config, and a typo here would clear the GPU", keep))
		if obs.DeviceKnown {
			plan.CurrentFreeMiB = obs.Device.FreeMiB()
			plan.TotalMiB = obs.Device.TotalMiB
		} else {
			// Both reasons to refuse are reported: fixing the name would
			// still leave the GPU unreadable.
			plan.Notes = append(plan.Notes, obs.deviceUnknownNote())
		}
		return plan
	}
	var names []string
	for _, s := range obs.Services {
		if s.Name != keep {
			names = append(names, s.Name)
		}
	}
	sort.Strings(names)
	plan := Evict(obs, names)
	plan.Beneficiary = keep
	return plan
}
