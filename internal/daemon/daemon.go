// Package daemon runs the arbitration loop and answers the CLI over a socket.
//
// The daemon is the only component permitted to change a service's state.
// Every mutation passes through Daemon.execute, which enforces the safety
// rules a second time even though the scheduler already applied them: the
// scheduler decides, and the daemon still refuses anything the config forbids.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/hyprtuna/gpu-bouncer/internal/adapter"
	"github.com/hyprtuna/gpu-bouncer/internal/config"
	"github.com/hyprtuna/gpu-bouncer/internal/gpu"
	"github.com/hyprtuna/gpu-bouncer/internal/ipc"
	"github.com/hyprtuna/gpu-bouncer/internal/observe"
	"github.com/hyprtuna/gpu-bouncer/internal/scheduler"
)

// Daemon owns the arbitration loop, the claim table and the control socket.
type Daemon struct {
	cfg      config.Config
	observer *observe.Observer
	log      *slog.Logger

	// dryRun makes every mutation a no op that still logs what it would have
	// done. It is the global --dry-run flag.
	dryRun bool

	// now is the clock. Tests inject one so cooldown timing can be exact.
	now func() time.Time

	mu sync.Mutex
	// claims are held until released or until the daemon restarts. They are
	// deliberately not persisted: a claim that outlived the process that made
	// it would keep evicting services with nobody left to release it.
	claims map[string]scheduler.Claim
	// cooldowns are services a recent action did nothing for, keyed by name.
	// Reactive plans skip them until the recorded time; explicit request and
	// evict do not consult them. Like claims, they live only in memory.
	cooldowns map[string]cooldown
	// inflight names the services an action is running on right now. At most
	// one action per service is ever in flight, and a plan never names a
	// service that is in it.
	inflight map[string]struct{}
}

type cooldown struct {
	until  time.Time
	reason string
}

// New builds a daemon over an already opened GPU source.
func New(cfg config.Config, source gpu.Source, log *slog.Logger, dryRun bool) (*Daemon, error) {
	observer, err := observe.New(cfg, source)
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	return &Daemon{
		cfg:       cfg,
		observer:  observer,
		log:       log,
		dryRun:    dryRun,
		now:       time.Now,
		claims:    make(map[string]scheduler.Claim),
		cooldowns: make(map[string]cooldown),
		inflight:  make(map[string]struct{}),
	}, nil
}

// Run serves the socket and the poll loop until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context, socketPath string) error {
	// Without a VRAM reading the scheduler refuses every action, so a daemon
	// that cannot read its arbitrated GPU could only ever do nothing. Refusing
	// here, with the reason, beats running inert.
	if _, err := d.observer.Device(ctx); err != nil {
		return fmt.Errorf("refusing to start, cannot read the arbitrated GPU: %w", err)
	}

	listener, err := ipc.Listen(socketPath)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()

	d.log.Info("gpu-bouncer daemon started",
		"socket", listener.Path(),
		"services", len(d.cfg.Services),
		"reactive", d.cfg.Policy.Reactive,
		"vram_floor_mib", d.cfg.Policy.VRAMFloorMiB,
		"poll_interval", d.cfg.Policy.PollInterval.D().String(),
		"min_effect_mib", d.cfg.Policy.MinEffectMiB,
		"action_cooldown", d.cfg.Policy.ActionCooldown.D().String(),
		"dry_run", d.dryRun,
	)
	if len(d.cfg.Services) == 0 {
		d.log.Warn("no services are configured, so nothing will ever be acted on",
			"config_sources", d.cfg.Sources)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- listener.Serve(ctx, d.handle) }()

	ticker := time.NewTicker(d.cfg.Policy.PollInterval.D())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			d.log.Info("gpu-bouncer daemon stopping")
			return nil
		case err := <-errCh:
			if err != nil {
				return fmt.Errorf("control socket: %w", err)
			}
			return nil
		case <-ticker.C:
			d.tick(ctx)
		}
	}
}

// tick runs one arbitration round. It is the one caller that honours
// cooldowns: an action the loop already tried and measured as useless is not
// tried again every poll, which for a llama-swap release would mean killing
// in flight requests once per poll_interval forever.
//
// The loop never waits for an action. Each one runs on its own goroutine,
// one per service at a time, so an Ollama drain on one service does not
// stop the next poll from observing every other service and acting on it.
func (d *Daemon) tick(ctx context.Context) {
	obs := d.observation(ctx)
	d.applyCooldowns(&obs)
	plan := scheduler.Decide(d.cfg, obs)
	if plan.Empty() {
		return
	}
	d.log.Info("acting",
		"trigger", string(plan.Trigger),
		"beneficiary", plan.Beneficiary,
		"free_mib", plan.CurrentFreeMiB,
		"target_mib", plan.TargetFreeMiB,
		"actions", len(plan.Actions),
	)
	for _, action := range plan.Actions {
		go d.executeGuarded(ctx, action)
	}
}

// observation reads the world and attaches the current claims and the
// services an action is still running on.
func (d *Daemon) observation(ctx context.Context) scheduler.Observation {
	obs := d.observer.Observe(ctx)
	obs.Claims = d.snapshotClaims()
	d.applyInflight(&obs)
	return obs
}

// applyInflight marks the services an action is running on.
func (d *Daemon) applyInflight(obs *scheduler.Observation) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range obs.Services {
		if _, busy := d.inflight[obs.Services[i].Name]; busy {
			obs.Services[i].ActionInFlight = true
		}
	}
}

// acquire claims the one action slot a service has. It reports false when an
// action is already in flight on it.
func (d *Daemon) acquire(service string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, busy := d.inflight[service]; busy {
		return false
	}
	d.inflight[service] = struct{}{}
	return true
}

func (d *Daemon) releaseSlot(service string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.inflight, service)
}

// executeGuarded runs one action under the service's slot. A plan built from
// an observation that predates another action's start can still name the
// same service; the slot closes that window, and the skip is reported in
// the result rather than acted on twice.
func (d *Daemon) executeGuarded(ctx context.Context, action scheduler.Action) ipc.ActionResult {
	if !d.acquire(action.Service) {
		result := ipc.ActionResult{
			Service: action.Service,
			Verb:    string(action.Verb),
			Reason:  action.Reason,
			Detail:  "skipped, an action on it is already in flight",
		}
		d.log.Info("action skipped, one is already in flight", "service", action.Service, "verb", action.Verb)
		return result
	}
	defer d.releaseSlot(action.Service)
	return d.executeOne(ctx, action)
}

// applyCooldowns marks cooling services on an observation and forgets the
// cooldowns that have ended. A cooldown ends at exactly its recorded time.
func (d *Daemon) applyCooldowns(obs *scheduler.Observation) {
	now := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range obs.Services {
		name := obs.Services[i].Name
		c, ok := d.cooldowns[name]
		if !ok {
			continue
		}
		if !now.Before(c.until) {
			delete(d.cooldowns, name)
			continue
		}
		obs.Services[i].CooldownUntil = c.until
	}
}

// recordEffect starts a cooldown for a service whose action gained less than
// policy.min_effect_mib of free VRAM, or failed, and ends any cooldown for a
// service whose action worked. It is the only place cooldowns begin.
func (d *Daemon) recordEffect(r ipc.ActionResult) {
	gain := int64(r.FreeAfterMiB) - int64(r.FreeBeforeMiB)
	d.mu.Lock()
	defer d.mu.Unlock()
	if r.Error == "" && gain >= int64(d.cfg.Policy.MinEffectMiB) {
		delete(d.cooldowns, r.Service)
		return
	}
	reason := fmt.Sprintf("%s freed %d MiB, below min_effect_mib %d", r.Verb, gain, d.cfg.Policy.MinEffectMiB)
	if r.Error != "" {
		reason = r.Verb + " failed: " + r.Error
	}
	until := d.now().Add(d.cfg.Policy.ActionCooldown.D())
	d.cooldowns[r.Service] = cooldown{until: until, reason: reason}
	d.log.Warn("cooldown started, reactive plans will leave this service alone",
		"service", r.Service, "until", until.Format(time.RFC3339), "reason", reason)
}

func (d *Daemon) snapshotCooldowns() []ipc.CooldownReport {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	out := make([]ipc.CooldownReport, 0, len(d.cooldowns))
	for name, c := range d.cooldowns {
		if !now.Before(c.until) {
			continue
		}
		out = append(out, ipc.CooldownReport{Service: name, Until: c.until, Reason: c.reason})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

func (d *Daemon) snapshotClaims() []scheduler.Claim {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]scheduler.Claim, 0, len(d.claims))
	for _, c := range d.claims {
		out = append(out, c)
	}
	// Map iteration order is random, and the scheduler must be deterministic.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].Service < out[j].Service
	})
	return out
}

// execute carries out a plan on behalf of a client and reports what actually
// happened, with the GPU's own free VRAM figure measured either side of each
// action. The client waits for its own actions; the poll loop does not.
func (d *Daemon) execute(ctx context.Context, plan scheduler.Plan) []ipc.ActionResult {
	results := make([]ipc.ActionResult, 0, len(plan.Actions))
	for _, action := range plan.Actions {
		results = append(results, d.executeGuarded(ctx, action))
	}
	return results
}

func (d *Daemon) executeOne(ctx context.Context, action scheduler.Action) ipc.ActionResult {
	result := ipc.ActionResult{
		Service: action.Service,
		Verb:    string(action.Verb),
		Reason:  action.Reason,
	}
	before, _ := d.observer.Device(ctx)
	result.FreeBeforeMiB = before.FreeMiB()

	svc, configured := d.cfg.Service(action.Service)
	if !configured {
		// Unreachable through the scheduler, which only ever names configured
		// services. Checked anyway: this is the boundary that guarantees
		// gpu-bouncer never touches a service the config does not name.
		result.Error = "service is not in the config"
		d.logAction(result)
		return result
	}
	a, ok := d.observer.Adapter(action.Service)
	if !ok {
		result.Error = "no adapter"
		d.logAction(result)
		return result
	}

	if action.Verb == scheduler.VerbStop && !svc.AllowStop {
		// The scheduler already refuses this. Enforced again here because it
		// is the one action that can interrupt a user's work.
		result.Error = "refusing to stop: allow_stop is not set for this service"
		d.logAction(result)
		return result
	}

	if d.dryRun {
		result.Detail = "dry run, nothing was done"
		result.FreeAfterMiB = result.FreeBeforeMiB
		d.logAction(result)
		return result
	}

	var (
		adapterResult adapter.Result
		err           error
	)
	switch action.Verb {
	case scheduler.VerbRelease:
		adapterResult, err = a.Release(ctx)
	case scheduler.VerbStop:
		adapterResult, err = a.Stop(ctx)
	default:
		err = fmt.Errorf("unknown verb %q", action.Verb)
	}

	result.Acted = adapterResult.Acted
	result.Detail = adapterResult.Detail
	if err != nil {
		result.Error = err.Error()
	}

	after, _ := d.observer.Device(ctx)
	result.FreeAfterMiB = after.FreeMiB()
	d.logAction(result)
	d.recordEffect(result)
	return result
}

// logAction emits the one line that matters for auditing what gpu-bouncer did.
func (d *Daemon) logAction(r ipc.ActionResult) {
	attrs := []any{
		"service", r.Service,
		"verb", r.Verb,
		"acted", r.Acted,
		"free_before_mib", r.FreeBeforeMiB,
		"free_after_mib", r.FreeAfterMiB,
	}
	if r.Detail != "" {
		attrs = append(attrs, "detail", r.Detail)
	}
	if r.Error != "" {
		attrs = append(attrs, "error", r.Error)
		d.log.Error("action failed", attrs...)
		return
	}
	d.log.Info("action", attrs...)
}

// handle answers one control socket request.
func (d *Daemon) handle(ctx context.Context, req ipc.Request) ipc.Response {
	switch req.Op {
	case ipc.OpPing:
		dry := d.dryRun
		return ipc.Response{OK: true, Message: "gpu-bouncer daemon is running", DaemonDryRun: &dry}
	case ipc.OpStatus:
		return d.handleStatus(ctx)
	case ipc.OpPlan:
		return d.handlePlan(ctx)
	case ipc.OpRequest:
		return d.handleRequest(ctx, req)
	case ipc.OpRelease:
		return d.handleReleaseClaim(req)
	case ipc.OpEvict:
		return d.handleEvict(ctx, req)
	default:
		return ipc.Response{Error: fmt.Sprintf("unknown operation %q", req.Op)}
	}
}

func (d *Daemon) handleStatus(ctx context.Context) ipc.Response {
	dry := d.dryRun
	resp := ipc.Response{OK: true, DaemonDryRun: &dry}
	device, err := d.observer.Device(ctx)
	report := ipc.GPUReportOf(device, "")
	if err != nil {
		report.Known, report.Error = false, err.Error()
	}
	resp.GPU = &report
	for _, s := range d.observer.Statuses(ctx) {
		report := ipc.ServiceReport{
			Name:          s.Service.Name,
			Adapter:       string(s.Service.Adapter),
			Priority:      s.Service.Priority,
			Up:            s.Status.Up,
			Version:       s.Status.Version,
			HeldMiB:       s.Status.HeldMiB,
			HeldEstimated: s.Status.HeldEstimated,
			Idle:          s.Status.Idle,
			IdleKnown:     s.Status.IdleKnown,
			AllowStop:     s.Service.AllowStop,
		}
		for _, item := range s.Status.Items {
			label := item.Name
			if item.VRAMMiB > 0 {
				label += fmt.Sprintf(" (%d MiB)", item.VRAMMiB)
			}
			if item.Detail != "" {
				label += " " + item.Detail
			}
			report.Items = append(report.Items, label)
		}
		if s.Err != nil {
			report.Error = s.Err.Error()
		}
		resp.Services = append(resp.Services, report)
	}
	for _, c := range d.snapshotClaims() {
		resp.Claims = append(resp.Claims, ipc.ClaimReport{Service: c.Service, NeedMiB: c.NeedMiB, At: c.At})
	}
	resp.Cooldowns = d.snapshotCooldowns()
	return resp
}

// handlePlan answers `gpu-bouncer plan` with what the next tick would do,
// cooldowns included, so the preview matches the loop.
func (d *Daemon) handlePlan(ctx context.Context) ipc.Response {
	obs := d.observation(ctx)
	d.applyCooldowns(&obs)
	plan := scheduler.Decide(d.cfg, obs)
	return ipc.Response{OK: true, Plan: &plan}
}

// handleRequest records a claim and acts on it immediately, so that a client
// that asked for room does not have to wait for the next poll.
func (d *Daemon) handleRequest(ctx context.Context, req ipc.Request) ipc.Response {
	if req.Service == "" {
		return ipc.Response{Error: "request needs a service name"}
	}
	if _, ok := d.cfg.Service(req.Service); !ok {
		return ipc.Response{Error: fmt.Sprintf("service %q is not in the config", req.Service)}
	}

	// A dry run, whether the request's own or the daemon's, must not leave a
	// claim behind: a claim a dry-run daemon can never act on would still be
	// listed by status and could never be released. The plan it reports is
	// still the one the real request would produce.
	dryRun := req.DryRun || d.dryRun
	claim := scheduler.Claim{Service: req.Service, NeedMiB: req.NeedMiB, At: time.Now()}
	if !dryRun {
		d.mu.Lock()
		d.claims[req.Service] = claim
		d.mu.Unlock()
		d.log.Info("claim recorded", "service", claim.Service, "need_mib", claim.NeedMiB)
	}

	obs := d.observer.Observe(ctx)
	if dryRun {
		obs.Claims = append(d.snapshotClaims(), claim)
	} else {
		obs.Claims = d.snapshotClaims()
	}
	d.applyInflight(&obs)

	plan := scheduler.Decide(d.cfg, obs)
	resp := ipc.Response{OK: true, Plan: &plan}
	if d.dryRun {
		resp.Message = "the daemon is running in dry-run mode: it plans and never acts, so no claim was recorded"
		return resp
	}
	if req.DryRun {
		resp.Message = "dry run, nothing was done"
		return resp
	}
	resp.Executed = d.execute(ctx, plan)
	return resp
}

func (d *Daemon) handleReleaseClaim(req ipc.Request) ipc.Response {
	if req.Service == "" {
		return ipc.Response{Error: "release needs a service name"}
	}
	if _, ok := d.cfg.Service(req.Service); !ok {
		return ipc.Response{Error: fmt.Sprintf("service %q is not in the config", req.Service)}
	}
	if d.dryRun {
		// A dry-run daemon records no claims, so there is never one to drop.
		return ipc.Response{OK: true, Message: "the daemon is running in dry-run mode: it holds no claims, so there is nothing to release"}
	}
	if req.DryRun {
		d.mu.Lock()
		_, had := d.claims[req.Service]
		d.mu.Unlock()
		if !had {
			return ipc.Response{OK: true, Message: fmt.Sprintf("dry run: %s holds no claim", req.Service)}
		}
		return ipc.Response{OK: true, Message: fmt.Sprintf("dry run: would release the claim held by %s", req.Service)}
	}
	d.mu.Lock()
	_, had := d.claims[req.Service]
	delete(d.claims, req.Service)
	d.mu.Unlock()

	if !had {
		return ipc.Response{OK: true, Message: fmt.Sprintf("%s held no claim", req.Service)}
	}
	d.log.Info("claim released", "service", req.Service)
	return ipc.Response{OK: true, Message: fmt.Sprintf("released the claim held by %s", req.Service)}
}

func (d *Daemon) handleEvict(ctx context.Context, req ipc.Request) ipc.Response {
	if req.Service == "" {
		return ipc.Response{Error: "evict needs a service name"}
	}
	if _, ok := d.cfg.Service(req.Service); !ok {
		// A typo must be visible to a script, so this is an error and not
		// an empty plan. For --all-except it is also the guard that keeps a
		// typo from clearing the GPU.
		return ipc.Response{Error: fmt.Sprintf("service %q is not in the config", req.Service)}
	}
	obs := d.observation(ctx)

	var plan scheduler.Plan
	if req.AllExcept {
		plan = scheduler.EvictAllExcept(obs, req.Service)
	} else {
		plan = scheduler.Evict(obs, []string{req.Service})
	}

	resp := ipc.Response{OK: true, Plan: &plan}
	if req.DryRun || d.dryRun {
		resp.Message = "dry run, nothing was done"
		return resp
	}
	resp.Executed = d.execute(ctx, plan)
	return resp
}
