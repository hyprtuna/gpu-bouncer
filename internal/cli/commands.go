package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/hyprtuna/gpu-bouncer/internal/adapter"
	"github.com/hyprtuna/gpu-bouncer/internal/config"
	"github.com/hyprtuna/gpu-bouncer/internal/daemon"
	"github.com/hyprtuna/gpu-bouncer/internal/gpu"
	"github.com/hyprtuna/gpu-bouncer/internal/ipc"
	"github.com/hyprtuna/gpu-bouncer/internal/observe"
	"github.com/hyprtuna/gpu-bouncer/internal/scheduler"
)

// probeTimeout bounds a one shot read only command. The per service timeout
// from the config still applies inside it.
const probeTimeout = 30 * time.Second

// flagSet is a flag.FlagSet whose own output is silenced. The flag package
// prints a parse error and a usage dump before returning the error, which
// would report every mistake twice; here the error is reported once by Main,
// and the usage is printed only for --help, by parseArgs.
type flagSet struct {
	*flag.FlagSet
	env Env
}

func newFlagSet(name string, env Env) *flagSet {
	fs := flag.NewFlagSet("gpu-bouncer "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return &flagSet{FlagSet: fs, env: env}
}

// printHelp writes the command's flags to stdout, the way flag's own default
// usage would, minus the copy it prints on errors.
func (fs *flagSet) printHelp() {
	var buf bytes.Buffer
	fs.SetOutput(&buf)
	fs.PrintDefaults()
	fs.SetOutput(io.Discard)
	fmt.Fprintf(fs.env.Stdout, "Usage of %s:\n%s", fs.Name(), buf.String())
}

// addOutputFlags lets --json, --verbose and --dry-run be given after the
// command name as well as before it. They are global flags, but
// "gpu-bouncer status --json" is what people type, and rejecting it teaches
// nothing. On a command where one of them has no effect it is a no op.
func addOutputFlags(fs *flagSet, g *globals) {
	fs.BoolVar(&g.asJSON, "json", g.asJSON, "emit JSON")
	fs.BoolVar(&g.verbose, "verbose", g.verbose, "include per service reasoning")
	fs.BoolVar(&g.verbose, "v", g.verbose, "include per service reasoning")
	fs.BoolVar(&g.dryRun, "dry-run", g.dryRun, "do not change anything")
}

// noPositionals parses a command that takes no arguments of its own.
func noPositionals(fs *flagSet, args []string, command string) error {
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) > 0 {
		return fmt.Errorf("%s takes no arguments, got %q", command, positional[0])
	}
	return nil
}

// parseArgs parses flags that appear anywhere, before or after the positional
// arguments, and returns the positionals.
//
// The flag package stops at the first non flag argument, which would make
// "gpu-bouncer request assistant --need-mib 6144" fail with a complaint about
// too many service names. That is the order people actually type. Parsing in a
// loop and peeling off one positional per pass keeps the flag package's own
// knowledge of which flags take a value, so a value that looks like a
// positional is never mistaken for one.
func parseArgs(fs *flagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fs.printHelp()
			}
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// runDaemon runs the arbitration service in the foreground. systemd supervises
// it; it does not daemonise itself.
func runDaemon(ctx context.Context, args []string, g *globals, env Env) error {
	fs := newFlagSet("daemon", env)
	socket := fs.String("socket", "", "control socket path (defaults to the standard location)")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	addOutputFlags(fs, g)
	if err := noPositionals(fs, args, "daemon"); err != nil {
		return err
	}

	level := slog.LevelInfo
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("invalid --log-level %q", *logLevel)
	}
	log := slog.New(slog.NewTextHandler(env.Stderr, &slog.HandlerOptions{Level: level}))
	// At debug level every adapter request is logged too. Only the daemon
	// makes requests worth a log; the read only commands print their result.
	adapter.Logger = log

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	source, err := gpu.Open()
	if err != nil {
		// Without a GPU reading the scheduler refuses every action, so
		// starting anyway would be a daemon that can only ever do nothing.
		return fmt.Errorf("cannot read GPU state: %w", err)
	}
	defer func() { _ = source.Close() }()
	log.Info("GPU source opened", "source", source.Name())

	d, err := daemon.New(cfg, source, log, g.dryRun)
	if err != nil {
		return err
	}

	path := *socket
	if path == "" {
		path = ipc.SocketPath()
	}
	return d.Run(ctx, path)
}

// runStatus reads GPU and service state directly. It never needs the daemon,
// so it is safe on a machine where gpu-bouncer is only installed, not running.
func runStatus(ctx context.Context, args []string, g *globals, env Env) error {
	fs := newFlagSet("status", env)
	addOutputFlags(fs, g)
	if err := noPositionals(fs, args, "status"); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	// A missing GPU source degrades the output rather than failing it: the
	// per service view is still worth printing.
	var (
		source     gpu.Source
		sourceName = "unavailable"
		sourceErr  error
	)
	if source, sourceErr = gpu.Open(); sourceErr == nil {
		sourceName = source.Name()
		defer func() { _ = source.Close() }()
	}

	observer, err := observe.New(cfg, source)
	if err != nil {
		return err
	}
	report := ipc.Response{OK: true}
	device, deviceErr := observer.Device(ctx)
	gpuReport := ipc.GPUReportOf(device, sourceName)
	if deviceErr != nil {
		gpuReport.Known = false
		if device.Name == "" && device.BusID == "" {
			// No device at all behind the index: report the index that was
			// configured, not the zero of an empty device.
			gpuReport.Index = cfg.Policy.GPUIndex
		}
		switch {
		case sourceErr != nil:
			// The source's own failure is the more useful message, because
			// it names every source that was tried.
			gpuReport.Error = sourceErr.Error()
		case device.Unreadable != "":
			// The heading already names the device; the reason is enough.
			gpuReport.Error = device.Unreadable
		default:
			gpuReport.Error = deviceErr.Error()
		}
	}
	report.GPU = &gpuReport
	if devices, err := observer.Devices(ctx); err == nil {
		for _, d := range devices {
			report.Devices = append(report.Devices, ipc.GPUReportOf(d, sourceName))
		}
	}
	statuses := observer.Statuses(ctx)
	for _, s := range statuses {
		entry := ipc.ServiceReport{
			Name: s.Service.Name, Adapter: string(s.Service.Adapter), Priority: s.Service.Priority,
			Up: s.Status.Up, Version: s.Status.Version, HeldMiB: s.Status.HeldMiB,
			HeldEstimated: s.Status.HeldEstimated, Idle: s.Status.Idle, IdleKnown: s.Status.IdleKnown,
			AllowStop: s.Service.AllowStop,
		}
		for _, item := range s.Status.Items {
			label := item.Name
			if item.VRAMMiB > 0 {
				label += fmt.Sprintf(" (%d MiB)", item.VRAMMiB)
			}
			if item.Detail != "" {
				label += " " + item.Detail
			}
			entry.Items = append(entry.Items, label)
		}
		if s.Err != nil {
			entry.Error = s.Err.Error()
		}
		report.Services = append(report.Services, entry)
	}

	// The daemon is not needed for status, but whether one is running changes
	// what the other commands will do, so it is worth a line.
	daemonUp := false
	// daemonDry is a pointer because there are three answers, not two: yes,
	// no, and a daemon older than this client that does not say. Reading the
	// absence as "no" reported a dry-run daemon as one that acts, which is
	// the one wrong answer here that matters.
	var daemonDry *bool
	var configNote string
	if resp, err := ipc.Do(ctx, ipc.Request{Op: ipc.OpPing}); err == nil && resp.OK {
		daemonUp = true
		daemonDry = resp.DaemonDryRun
		if claims, err := ipc.Do(ctx, ipc.Request{Op: ipc.OpStatus}); err == nil {
			report.Claims = claims.Claims
			report.Cooldowns = claims.Cooldowns
		}
		// The daemon never reloads. If the files it loaded have changed
		// since, every other command is answered from a config the user no
		// longer sees.
		if resp.DaemonConfig != nil {
			report.DaemonConfig = resp.DaemonConfig
			report.ConfigStale, configNote = configDrift(*resp.DaemonConfig)
		} else {
			configNote = "the daemon is older than this client and does not report which config it loaded, " +
				"so whether it is running on an older edit cannot be told"
		}
	}
	report.DaemonRunning = &daemonUp
	report.DaemonDryRun = daemonDry
	report.Config = json.RawMessage("null")
	if len(cfg.Sources) > 0 {
		report.Config, _ = json.Marshal(strings.Join(cfg.Sources, ", "))
	}

	if g.asJSON {
		return writeJSON(env, statusOutputOf(report))
	}
	printStatus(env, report, cfg.Sources, cfg.Policy.GPUIndex, daemonUp, daemonDry, configNote)
	return nil
}

// configDrift answers the one question the staleness warning is about: have
// the files the daemon loaded changed since it loaded them? It re-reads the
// daemon's own paths, not this client's.
//
// Comparing the two resolved sets was wrong in both directions. A client with
// a different --config, a different XDG_CONFIG_HOME, or no config file at all
// was permanently "stale" against a daemon whose files had never been touched,
// and the remedy it printed, restart the daemon, could never take effect. A
// system daemon plus a per user client overlay is the documented setup and hit
// that on every status.
//
// The returned pointer is nil when the question could not be answered, which
// is not the same as answering no: the sentence says which it was.
func configDrift(report ipc.ConfigReport) (*bool, string) {
	paths := report.Paths
	if len(paths) == 0 && report.Path != "" {
		// A daemon too old to send the list still sends the joined form,
		// which status itself built with this separator.
		paths = strings.Split(report.Path, ", ")
	}
	if len(paths) == 0 {
		// Not stale: a daemon that loaded no file cannot be running on an
		// older edit of one. Worth saying, because it means the daemon is
		// on built in defaults whatever this client just read.
		fresh := false
		return &fresh, "the daemon loaded no config file"
	}
	joined := strings.Join(paths, ", ")
	now, err := config.ContentDigest(paths)
	if err != nil {
		return nil, fmt.Sprintf("the config the daemon loaded cannot be read now (%v), "+
			"so whether it is running on an older edit cannot be told", err)
	}
	stale := now != report.SHA256
	if !stale {
		return &stale, ""
	}
	return &stale, fmt.Sprintf("the daemon loaded a different config (%s, loaded %s); restart it to apply your edit",
		joined, report.LoadedAt.Format(time.RFC3339))
}

// runPlan shows what would happen now. It asks the daemon when one is running,
// because only the daemon knows the outstanding claims.
func runPlan(ctx context.Context, args []string, g *globals, env Env) error {
	fs := newFlagSet("plan", env)
	addOutputFlags(fs, g)
	if err := noPositionals(fs, args, "plan"); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	if resp, err := ipc.Do(ctx, ipc.Request{Op: ipc.OpPlan}); err == nil {
		if resp.Error != "" {
			return fmt.Errorf("%s", resp.Error)
		}
		if g.asJSON {
			return writeJSON(env, planOutput{OK: true, Plan: planOf(resp.Plan)})
		}
		printPlan(env, *resp.Plan, "the running daemon", g.verbose)
		return nil
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	source, sourceErr := gpu.Open()
	if sourceErr == nil {
		defer func() { _ = source.Close() }()
	}
	observer, err := observe.New(cfg, source)
	if err != nil {
		return err
	}
	// No daemon means no claims, so this is the reactive policy view only.
	plan := scheduler.Decide(cfg, observer.Observe(ctx))
	if g.asJSON {
		return writeJSON(env, planOutput{OK: true, Plan: planOf(&plan)})
	}
	printPlan(env, plan, "no daemon is running, so outstanding claims are not visible", g.verbose)
	return nil
}

// runRequest claims priority for a service.
func runRequest(ctx context.Context, args []string, g *globals, env Env) error {
	fs := newFlagSet("request", env)
	needMiB := fs.Uint64("need-mib", 0, "free VRAM wanted in MiB (default: the policy floor)")
	addOutputFlags(fs, g)
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	service, err := oneService(positional, "request")
	if err != nil {
		return err
	}
	return callDaemon(ctx, g, env, ipc.Request{
		Op: ipc.OpRequest, Service: service, NeedMiB: *needMiB, DryRun: g.dryRun,
	})
}

// runReleaseClaim drops a claim. It does not itself free any VRAM.
func runReleaseClaim(ctx context.Context, args []string, g *globals, env Env) error {
	fs := newFlagSet("release", env)
	addOutputFlags(fs, g)
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	service, err := oneService(positional, "release")
	if err != nil {
		return err
	}
	return callDaemon(ctx, g, env, ipc.Request{
		Op: ipc.OpRelease, Service: service, DryRun: g.dryRun,
	})
}

// runEvict frees named services now.
func runEvict(ctx context.Context, args []string, g *globals, env Env) error {
	fs := newFlagSet("evict", env)
	allExcept := fs.String("all-except", "", "free every configured service except this one")
	addOutputFlags(fs, g)
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}

	req := ipc.Request{Op: ipc.OpEvict, DryRun: g.dryRun}
	switch {
	case *allExcept != "" && len(positional) > 0:
		return fmt.Errorf("give either a service name or --all-except, not both")
	case *allExcept != "":
		req.Service, req.AllExcept = *allExcept, true
	default:
		service, err := oneService(positional, "evict")
		if err != nil {
			return err
		}
		req.Service = service
	}
	return callDaemon(ctx, g, env, req)
}

// runVersion prints the version. It parses flags like every other command,
// so "version --help" explains itself and "version foo" is refused.
func runVersion(args []string, g *globals, env Env) error {
	fs := newFlagSet("version", env)
	addOutputFlags(fs, g)
	if err := noPositionals(fs, args, "version"); err != nil {
		return err
	}
	if g.asJSON {
		return writeJSON(env, map[string]string{"version": version()})
	}
	fmt.Fprintf(env.Stdout, "gpu-bouncer %s\n", version())
	return nil
}

func oneService(args []string, command string) (string, error) {
	switch len(args) {
	case 1:
		return args[0], nil
	case 0:
		return "", fmt.Errorf("%s needs a service name", command)
	default:
		return "", fmt.Errorf("%s takes one service name, got %d", command, len(args))
	}
}

// callDaemon sends a mutating request. These commands refuse to work without a
// daemon rather than acting directly: the daemon is the single place that
// holds the config saying which services may be touched.
func callDaemon(ctx context.Context, g *globals, env Env, req ipc.Request) error {
	// The daemon answers with the plan before carrying it out, so the wait
	// is sized from the work rather than from a guess. There is no context
	// deadline here for that reason: a plan whose longest drain is ten
	// minutes is waited out, and a daemon that goes quiet is not.
	req.PlanFirst = true
	resp, err := ipc.Exchange(ctx, req)
	if err != nil {
		if errors.Is(err, ipc.ErrNoDaemon) {
			return fmt.Errorf("%w\nStart one with \"gpu-bouncer daemon\", or enable the service. "+
				"status and plan work without a daemon", err)
		}
		// A daemon took the request. Telling the operator to start one
		// would be wrong, and starting a second one would be worse.
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("%s", resp.Error)
	}

	// An action that failed is a failed command, however many others went
	// through: a script that asked for room and did not get it must find
	// out from the exit code. An action the daemon declined with a reason,
	// for example a busy ComfyUI, carries no error and is not a failure.
	failed := 0
	for _, r := range resp.Executed {
		if r.Error != "" {
			failed++
		}
	}
	if failed > 0 {
		resp.OK = false
		resp.Error = fmt.Sprintf("%d of %d actions failed", failed, len(resp.Executed))
	}

	if g.asJSON {
		if err := writeJSON(env, outputOf(req.Op, resp)); err != nil {
			return err
		}
	} else {
		printOutcome(env, resp, g.verbose)
		if req.Op == ipc.OpRequest && resp.TargetMet != nil {
			fmt.Fprintln(env.Stdout, shortfallLine(resp))
		}
		if failed > 0 {
			fmt.Fprintln(env.Stdout, resp.Error)
		}
	}
	if failed > 0 {
		return exitCode(1)
	}
	return nil
}

// shortfallLine says how much of the room a request asked for was actually
// freed, whether or not the target was reached, so the answer to "did I get
// it" is on the last line and not only in --json.
func shortfallLine(resp ipc.Response) string {
	plan := planOf(resp.Plan)
	if plan.TargetFreeMiB <= plan.CurrentFreeMiB {
		return fmt.Sprintf("the %s asked for were already free", mib(plan.TargetFreeMiB))
	}
	asked := plan.TargetFreeMiB - plan.CurrentFreeMiB
	var freed uint64
	// The actions overlap, so only the reading the daemon takes once they
	// have all finished measures the plan as a whole. A daemon that sent
	// none leaves the last action's own figure, which is what an older one
	// reports.
	if n := len(resp.Executed); n > 0 {
		first, last := resp.Executed[0].FreeBeforeMiB, resp.Executed[n-1].FreeAfterMiB
		if resp.FreeAfterMiB != nil {
			last = *resp.FreeAfterMiB
		}
		if last > first {
			freed = last - first
		}
	}
	line := fmt.Sprintf("freed %s of the %s asked for", mib(freed), mib(asked))
	if resp.TargetMet != nil && !*resp.TargetMet {
		line += ", target not met"
	}
	return line
}

func writeJSON(env Env, value any) error {
	encoder := json.NewEncoder(env.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

// mib renders a memory figure with its unit, so no output line is a bare
// number whose unit the reader has to guess.
func mib(v uint64) string { return fmt.Sprintf("%d MiB", v) }

// gpuHeading is the first line of a device: its index, name and PCI identity.
func gpuHeading(g ipc.GPUReport) string {
	heading := fmt.Sprintf("GPU %d  %s", g.Index, g.Name)
	var id []string
	if g.BusID != "" {
		id = append(id, "PCI "+g.BusID)
	}
	if g.Vendor != "" {
		id = append(id, "vendor "+g.Vendor)
	}
	if len(id) > 0 {
		heading += "  (" + strings.Join(id, ", ") + ")"
	}
	return heading
}

func printStatus(env Env, report ipc.Response, sources []string, gpuIndex int, daemonUp bool, daemonDry *bool, configNote string) {
	out := env.Stdout

	switch {
	case report.GPU != nil && report.GPU.Known:
		g := report.GPU
		fmt.Fprintln(out, gpuHeading(*g))
		fmt.Fprintf(out, "  %s used of %s, %s free  (source: %s)\n",
			mib(g.UsedMiB), mib(g.TotalMiB), mib(g.FreeMiB), g.Source)
	case report.GPU != nil && report.GPU.Name != "":
		// The device exists but its memory cannot be read. Name it, so the
		// user can see which card the index landed on.
		g := report.GPU
		fmt.Fprintln(out, gpuHeading(*g))
		fmt.Fprintf(out, "  state unavailable  (source: %s)\n", g.Source)
		fmt.Fprintf(out, "  %s\n", g.Error)
	default:
		fmt.Fprintln(out, "GPU  state unavailable")
		if report.GPU != nil && report.GPU.Error != "" {
			fmt.Fprintf(out, "  %s\n", report.GPU.Error)
		}
	}
	// Every other device the source sees, so a wrong gpu_index shows up as
	// the card the user meant sitting one line below the one they got.
	others := 0
	for _, d := range report.Devices {
		if d.Index != gpuIndex {
			others++
		}
	}
	if others > 0 {
		fmt.Fprintf(out, "Other GPUs, not arbitrated (policy.gpu_index = %d):\n", gpuIndex)
		for _, d := range report.Devices {
			if d.Index == gpuIndex {
				continue
			}
			if d.Known {
				fmt.Fprintf(out, "  %s, %s used of %s\n", gpuHeading(d), mib(d.UsedMiB), mib(d.TotalMiB))
			} else {
				fmt.Fprintf(out, "  %s, unreadable\n", gpuHeading(d))
			}
		}
	}
	fmt.Fprintln(out)

	if len(report.Services) == 0 {
		fmt.Fprintln(out, "No services are configured, so gpu-bouncer will never act.")
		if len(sources) == 0 {
			fmt.Fprintln(out, "No config file was found. Searched:")
			for _, p := range config.SearchPaths() {
				fmt.Fprintf(out, "  %s\n", p)
			}
		}
		fmt.Fprintln(out)
	}

	for _, s := range report.Services {
		state := "down"
		if s.Up {
			state = "up"
		}
		fmt.Fprintf(out, "%-16s %-13s priority %-4d %s\n", s.Name, s.Adapter, s.Priority, state)

		if s.Error != "" {
			fmt.Fprintf(out, "  error: %s\n", s.Error)
			fmt.Fprintln(out)
			continue
		}
		if s.Version != "" {
			fmt.Fprintf(out, "  version %s\n", s.Version)
		}

		held := mib(s.HeldMiB)
		if s.HeldEstimated {
			held += " (estimated)"
		}
		fmt.Fprintf(out, "  holding %s\n", held)

		if s.IdleKnown {
			busy := "idle"
			if !s.Idle {
				busy = "busy"
			}
			fmt.Fprintf(out, "  %s\n", busy)
		}
		for _, item := range s.Items {
			fmt.Fprintf(out, "  - %s\n", item)
		}
		if s.AllowStop {
			fmt.Fprintln(out, "  allow_stop is set: gpu-bouncer may stop this unit")
		}
		fmt.Fprintln(out)
	}

	if len(report.Claims) > 0 {
		fmt.Fprintln(out, "Outstanding claims:")
		for _, c := range report.Claims {
			fmt.Fprintf(out, "  %s wants %s since %s\n", c.Service, mib(c.NeedMiB), c.At.Format(time.RFC3339))
		}
		fmt.Fprintln(out)
	}
	if len(report.Cooldowns) > 0 {
		fmt.Fprintln(out, "Cooling down, left alone by reactive plans:")
		for _, c := range report.Cooldowns {
			fmt.Fprintf(out, "  %s until %s: %s\n", c.Service, c.Until.Format(time.RFC3339), c.Reason)
		}
		fmt.Fprintln(out)
	}

	switch {
	case daemonUp && daemonDry == nil:
		fmt.Fprintln(out, "A daemon is running. It is older than this client and does not report "+
			"whether it is in dry-run mode, so it may be planning and never acting.")
	case daemonUp && *daemonDry:
		fmt.Fprintln(out, "A daemon is running in dry-run mode: it plans and never acts.")
	case daemonUp:
		fmt.Fprintln(out, "A daemon is running.")
	default:
		fmt.Fprintln(out, "No daemon is running: gpu-bouncer is observing only, and will not act.")
	}
	if len(sources) > 0 {
		fmt.Fprintf(out, "Config: %s\n", strings.Join(sources, ", "))
	}
	if configNote != "" {
		fmt.Fprintln(out, configNote)
	}
}

func printPlan(env Env, plan scheduler.Plan, origin string, verbose bool) {
	out := env.Stdout
	fmt.Fprintf(out, "Trigger: %s", plan.Trigger)
	if plan.Beneficiary != "" {
		fmt.Fprintf(out, " (for %s)", plan.Beneficiary)
	}
	fmt.Fprintln(out)

	if plan.TargetFreeMiB > 0 {
		fmt.Fprintf(out, "Free VRAM: %s now, target %s\n", mib(plan.CurrentFreeMiB), mib(plan.TargetFreeMiB))
	} else {
		fmt.Fprintf(out, "Free VRAM: %s\n", mib(plan.CurrentFreeMiB))
	}
	fmt.Fprintln(out)

	if plan.Empty() {
		fmt.Fprintln(out, "No action.")
	} else {
		fmt.Fprintln(out, "Would do, in order:")
		for i, a := range plan.Actions {
			fmt.Fprintf(out, "  %d. %s %s  (expects to free %s)\n", i+1, a.Verb, a.Service, mib(a.ExpectFreeMiB))
			fmt.Fprintf(out, "     because %s\n", a.Reason)
		}
		fmt.Fprintf(out, "\nExpected free VRAM afterwards: %s\n", mib(plan.ExpectedFreeMiB()))
	}

	// Notes are the reason an empty plan is never a silent one, so they are
	// shown even without --verbose when there is nothing else to say.
	if len(plan.Notes) > 0 && (verbose || plan.Empty()) {
		fmt.Fprintln(out, "\nNotes:")
		for _, n := range plan.Notes {
			fmt.Fprintf(out, "  - %s\n", n)
		}
	}
	fmt.Fprintf(out, "\nSource: %s\n", origin)
}

func printOutcome(env Env, resp ipc.Response, verbose bool) {
	out := env.Stdout
	if resp.Message != "" {
		fmt.Fprintln(out, resp.Message)
	}
	if resp.Plan != nil && (verbose || len(resp.Executed) == 0) {
		printPlan(env, *resp.Plan, "the running daemon", verbose)
		fmt.Fprintln(out)
	}
	if len(resp.Executed) == 0 {
		return
	}

	fmt.Fprintln(out, "Done:")
	for _, r := range resp.Executed {
		outcome := "no change"
		if r.Acted {
			outcome = "acted"
		}
		if r.Error != "" {
			outcome = "failed"
		}
		freed := int64(r.FreeAfterMiB) - int64(r.FreeBeforeMiB)
		fmt.Fprintf(out, "  %s %s: %s, free VRAM %s to %s (%+d MiB)\n",
			r.Verb, r.Service, outcome, mib(r.FreeBeforeMiB), mib(r.FreeAfterMiB), freed)
		if r.Detail != "" {
			fmt.Fprintf(out, "    %s\n", r.Detail)
		}
		if r.Error != "" {
			fmt.Fprintf(out, "    error: %s\n", r.Error)
		}
	}
}
