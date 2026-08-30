package cli

import (
	"encoding/json"
	"strings"

	"github.com/hyprtuna/gpu-bouncer/internal/ipc"
	"github.com/hyprtuna/gpu-bouncer/internal/scheduler"
)

// The --json shapes. They are owned here rather than borrowed from the
// daemon's wire type so that every list is present, empty as [], and every
// key a consumer may read is there whether or not it has a value. INSTALL.md
// documents them.

// statusOutput is `status --json`.
type statusOutput struct {
	OK            bool                 `json:"ok"`
	GPU           *ipc.GPUReport       `json:"gpu"`
	Devices       []ipc.GPUReport      `json:"devices"`
	Services      []ipc.ServiceReport  `json:"services"`
	Claims        []ipc.ClaimReport    `json:"claims"`
	Cooldowns     []ipc.CooldownReport `json:"cooldowns"`
	DaemonRunning bool                 `json:"daemon_running"`
	// DaemonDryRun is null when the daemon is too old to report it. A daemon
	// that plans and never acts must never be reported as one that acts.
	DaemonDryRun *bool             `json:"daemon_dry_run"`
	DaemonConfig *ipc.ConfigReport `json:"daemon_config"`
	// ConfigStale is null when the answer is not known: no daemon answered,
	// the daemon is too old to report what it loaded, or one of the files it
	// loaded cannot be read now.
	ConfigStale *bool `json:"config_stale"`
	// Config is the path the configuration came from, or null when no file
	// was found.
	Config *string `json:"config"`
}

// planOutput is `plan --json`.
type planOutput struct {
	OK   bool           `json:"ok"`
	Plan scheduler.Plan `json:"plan"`
}

// actionOutput is `request --json` and `evict --json`.
type actionOutput struct {
	OK       bool               `json:"ok"`
	Error    string             `json:"error,omitempty"`
	Message  string             `json:"message,omitempty"`
	Plan     scheduler.Plan     `json:"plan"`
	Executed []ipc.ActionResult `json:"executed"`
	// FreeAfterMiB is the GPU's free VRAM read once after every action had
	// finished, the reading the plan itself was built on when it had nothing
	// to run, or null when a reading failed.
	FreeAfterMiB *uint64 `json:"free_after_mib"`
	// TargetMet is present on a request only: whether the free VRAM
	// measured after every action reached the target.
	TargetMet *bool `json:"target_met,omitempty"`
}

// releaseOutput is `release --json`.
type releaseOutput struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// servicesOut carries the nested lists through the same guarantee as the top
// level ones: a list is present and empty as [], never absent and never null.
// A consumer reading services[].items should not have to handle three shapes
// where there are two.
func servicesOut(services []ipc.ServiceReport) []ipc.ServiceReport {
	out := orEmpty(services)
	for i := range out {
		out[i].Items = orEmpty(out[i].Items)
	}
	return out
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// configOut carries daemon_config through the same list guarantee as the top
// level: paths is present, and empty as [], whatever answered. A daemon too
// old to send the list sent the joined path instead, and the two must not
// contradict each other.
func configOut(report *ipc.ConfigReport) *ipc.ConfigReport {
	if report == nil {
		return nil
	}
	out := *report
	out.Paths = pathsOf(*report)
	return &out
}

// pathsOf is the list of files a daemon loaded. A daemon too old to send the
// list still sends the joined form, which status itself built with this
// separator.
func pathsOf(report ipc.ConfigReport) []string {
	if len(report.Paths) > 0 {
		return report.Paths
	}
	if report.Path == "" {
		return []string{}
	}
	return strings.Split(report.Path, ", ")
}

// planOf dereferences a plan, giving an empty one with present lists when
// the daemon sent none.
func planOf(p *scheduler.Plan) scheduler.Plan {
	if p == nil {
		return scheduler.Plan{Actions: []scheduler.Action{}, Notes: []string{}}
	}
	plan := *p
	plan.Actions = orEmpty(plan.Actions)
	plan.Notes = orEmpty(plan.Notes)
	return plan
}

func statusOutputOf(r ipc.Response) statusOutput {
	out := statusOutput{
		OK:           r.OK,
		GPU:          r.GPU,
		Devices:      orEmpty(r.Devices),
		Services:     servicesOut(r.Services),
		Claims:       orEmpty(r.Claims),
		Cooldowns:    orEmpty(r.Cooldowns),
		DaemonConfig: configOut(r.DaemonConfig),
	}
	if r.DaemonRunning != nil {
		out.DaemonRunning = *r.DaemonRunning
	}
	out.DaemonDryRun = r.DaemonDryRun
	out.ConfigStale = r.ConfigStale
	if len(r.Config) > 0 && string(r.Config) != "null" {
		var path string
		if err := json.Unmarshal(r.Config, &path); err == nil {
			out.Config = &path
		}
	}
	return out
}

// planFreeAfter is the figure that measures a plan as a whole. The daemon
// reads the GPU once every action has finished and sends that; a plan with
// nothing to run has no such reading, and the figure that measures it is the
// one the plan itself was built on, because nothing happened in between. It
// stays null when a reading was taken and failed, and when the card could not
// be read at all, which a plan reports as a zero total.
func planFreeAfter(r ipc.Response) *uint64 {
	if r.FreeAfterMiB != nil || len(r.Executed) > 0 {
		return r.FreeAfterMiB
	}
	if plan := planOf(r.Plan); plan.TotalMiB > 0 {
		free := plan.CurrentFreeMiB
		return &free
	}
	return nil
}

// outputOf shapes a daemon reply for the command that made the request.
func outputOf(op ipc.Op, r ipc.Response) any {
	if op == ipc.OpRelease {
		return releaseOutput{OK: r.OK, Message: r.Message}
	}
	out := actionOutput{
		OK:           r.OK,
		Error:        r.Error,
		Message:      r.Message,
		Plan:         planOf(r.Plan),
		Executed:     orEmpty(r.Executed),
		FreeAfterMiB: planFreeAfter(r),
	}
	if op == ipc.OpRequest {
		out.TargetMet = r.TargetMet
	}
	return out
}
