// Package ipc carries commands between the gpu-bouncer CLI and the daemon.
//
// The transport is a Unix stream socket speaking one JSON request and one JSON
// response per connection. It is deliberately unexciting: the daemon is the
// only thing permitted to change a service's state, so the interesting part of
// this package is who is allowed to connect, not what is said.
//
// The socket is created with mode 0660 and is owned by whichever user runs the
// daemon. A user unit therefore gets a socket only that user can drive, which
// is the intended desktop setup. A system unit gets a root owned socket, and
// commands that mutate state need matching privileges.
package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hyprtuna/gpu-bouncer/internal/gpu"
	"github.com/hyprtuna/gpu-bouncer/internal/scheduler"
)

// Op is a command name.
type Op string

const (
	// OpPing checks that a daemon is listening.
	OpPing Op = "ping"
	// OpStatus returns the daemon's view of the GPU and every service.
	OpStatus Op = "status"
	// OpPlan returns what the daemon would do right now, without doing it.
	OpPlan Op = "plan"
	// OpRequest records a priority claim and acts on it.
	OpRequest Op = "request"
	// OpRelease drops a claim previously made with OpRequest.
	OpRelease Op = "release"
	// OpEvict frees named services now.
	OpEvict Op = "evict"
)

// Request is one command.
type Request struct {
	Op Op `json:"op"`
	// Service is the subject of request, release and evict.
	Service string `json:"service,omitempty"`
	// NeedMiB is how much free VRAM a request wants. Zero means the policy
	// floor.
	NeedMiB uint64 `json:"need_mib,omitempty"`
	// AllExcept turns evict into "evict everything but Service".
	AllExcept bool `json:"all_except,omitempty"`
	// DryRun asks the daemon to plan and report without acting.
	DryRun bool `json:"dry_run,omitempty"`
	// PlanFirst asks the daemon to send the plan it is about to carry out as
	// a preliminary reply, before the results. The client needs it to know
	// how long the work it is waiting for may take: only the daemon holds
	// the timeouts those actions run under. A daemon too old to know the
	// field answers once, which is still correct, just less well bounded.
	PlanFirst bool `json:"plan_first,omitempty"`
}

// ActionResult is what actually happened to one service, with the measured
// VRAM either side. The before and after figures come from the GPU rather than
// from the service, because a service reporting that it unloaded something is
// not evidence that the memory came back.
type ActionResult struct {
	Service       string `json:"service"`
	Verb          string `json:"verb"`
	Reason        string `json:"reason,omitempty"`
	Acted         bool   `json:"acted"`
	Detail        string `json:"detail,omitempty"`
	Error         string `json:"error,omitempty"`
	FreeBeforeMiB uint64 `json:"free_before_mib"`
	FreeAfterMiB  uint64 `json:"free_after_mib"`
}

// GPUReport is the arbitrated device as the daemon last read it.
type GPUReport struct {
	Known    bool   `json:"known"`
	Index    int    `json:"index"`
	Name     string `json:"name,omitempty"`
	BusID    string `json:"bus_id,omitempty"`
	Vendor   string `json:"vendor,omitempty"`
	Source   string `json:"source,omitempty"`
	TotalMiB uint64 `json:"total_mib"`
	UsedMiB  uint64 `json:"used_mib"`
	FreeMiB  uint64 `json:"free_mib"`
	// Error says why Known is false: the source failed, the index names no
	// device, or the device is present but its memory cannot be read.
	Error string `json:"error,omitempty"`
}

// GPUReportOf fills a report from a device reading.
func GPUReportOf(dev gpu.Device, source string) GPUReport {
	return GPUReport{
		Known: dev.Unreadable == "", Index: dev.Index, Name: dev.Name, BusID: dev.BusID, Vendor: dev.Vendor,
		Source: source, TotalMiB: dev.TotalMiB, UsedMiB: dev.UsedMiB, FreeMiB: dev.FreeMiB(), Error: dev.Unreadable,
	}
}

// ServiceReport is one service as the daemon last saw it.
type ServiceReport struct {
	Name          string   `json:"name"`
	Adapter       string   `json:"adapter"`
	Priority      int      `json:"priority"`
	Up            bool     `json:"up"`
	Version       string   `json:"version,omitempty"`
	Items         []string `json:"items,omitempty"`
	HeldMiB       uint64   `json:"held_mib"`
	HeldEstimated bool     `json:"held_estimated"`
	Idle          bool     `json:"idle"`
	IdleKnown     bool     `json:"idle_known"`
	AllowStop     bool     `json:"allow_stop"`
	Error         string   `json:"error,omitempty"`
}

// Response is one answer. OK is false whenever Error is set.
type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`

	// Preliminary marks the first of two replies on one connection: the plan
	// the daemon is about to carry out. The results follow in a second reply
	// on the same connection. Only a client that set Request.PlanFirst is
	// ever sent one.
	Preliminary bool `json:"preliminary,omitempty"`
	// LongestActionMS is set on a preliminary reply: the longest a single
	// action in Plan may take under the daemon's own config. Actions on
	// different services run concurrently, so it bounds the whole plan.
	LongestActionMS int64 `json:"longest_action_ms,omitempty"`

	// GPU is the arbitrated device. Devices is every device the source sees,
	// so that a wrong gpu_index can be diagnosed from the output alone.
	GPU      *GPUReport      `json:"gpu,omitempty"`
	Devices  []GPUReport     `json:"devices,omitempty"`
	Services []ServiceReport `json:"services,omitempty"`
	Plan     *scheduler.Plan `json:"plan,omitempty"`
	Executed []ActionResult  `json:"executed,omitempty"`
	// TargetMet is set on a request reply: whether the free VRAM measured
	// after the last action is at or above the target the request set.
	TargetMet *bool `json:"target_met,omitempty"`
	// FreeAfterMiB is the GPU's free VRAM read once after every action in
	// Executed had finished. The per action figures are taken as each action
	// starts and ends and the actions overlap, so this is the one figure
	// that measures what the plan as a whole achieved. It is absent when
	// nothing ran or the reading failed.
	FreeAfterMiB *uint64          `json:"free_after_mib,omitempty"`
	Claims       []ClaimReport    `json:"claims,omitempty"`
	Cooldowns    []CooldownReport `json:"cooldowns,omitempty"`
	Message      string           `json:"message,omitempty"`

	// DaemonConfig is set by the daemon on ping and status replies: which
	// files it loaded, their digest, and when. ConfigStale is set by the
	// status command when the files it read differ from what the daemon
	// loaded, which means the daemon is running on an older edit.
	DaemonConfig *ConfigReport `json:"daemon_config,omitempty"`
	ConfigStale  *bool         `json:"config_stale,omitempty"`

	// DaemonDryRun is set by the daemon on ping and status replies: true when
	// it was started with --dry-run and therefore plans but never acts and
	// records no claims.
	DaemonDryRun *bool `json:"daemon_dry_run,omitempty"`

	// DaemonRunning and Config are filled by the status command only, which
	// reuses this type as its report. Config is the path the configuration
	// came from, or JSON null when no file was found.
	DaemonRunning *bool           `json:"daemon_running,omitempty"`
	Config        json.RawMessage `json:"config,omitempty"`
}

// ClaimReport is one outstanding claim.
type ClaimReport struct {
	Service string    `json:"service"`
	NeedMiB uint64    `json:"need_mib"`
	At      time.Time `json:"at"`
}

// ConfigReport identifies the configuration a daemon is running on.
type ConfigReport struct {
	// Path is the file or files loaded, joined the way status prints them.
	Path     string    `json:"path"`
	SHA256   string    `json:"sha256"`
	LoadedAt time.Time `json:"loaded_at"`
}

// CooldownReport is one service that reactive plans are leaving alone until
// Until, because the last action on it had no measurable effect.
type CooldownReport struct {
	Service string    `json:"service"`
	Until   time.Time `json:"until"`
	Reason  string    `json:"reason"`
}

// Claims converts reports back into scheduler claims.
func Claims(reports []ClaimReport) []scheduler.Claim {
	out := make([]scheduler.Claim, 0, len(reports))
	for _, r := range reports {
		out = append(out, scheduler.Claim{Service: r.Service, NeedMiB: r.NeedMiB, At: r.At})
	}
	return out
}

const (
	// EnvSocket overrides socket discovery. Used by the integration tests.
	EnvSocket = "GPU_BOUNCER_SOCKET"

	systemSocket = "/run/gpu-bouncer/gpu-bouncer.sock"
	socketName   = "gpu-bouncer.sock"

	// socketMode keeps the socket private to the user running the daemon.
	socketMode = 0o660

	// maxRequest caps a single request. The protocol has no large payloads,
	// so anything bigger is a client bug or an attempt to exhaust memory.
	maxRequest = 1 << 20
)

// SocketPath returns the socket to use. A user session prefers its own runtime
// directory, so an unprivileged daemon works with no setup at all.
func SocketPath() string {
	if override := os.Getenv(EnvSocket); override != "" {
		return override
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, socketName)
	}
	return systemSocket
}

// SearchPaths lists the sockets a client will try, in order. A user socket
// wins over the system one, matching the config layering.
func SearchPaths() []string {
	if override := os.Getenv(EnvSocket); override != "" {
		return []string{override}
	}
	var paths []string
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		paths = append(paths, filepath.Join(dir, socketName))
	}
	return append(paths, systemSocket)
}

// ErrNoDaemon means nothing was listening on any candidate socket.
var ErrNoDaemon = errors.New("no gpu-bouncer daemon is listening")

const (
	// ClientSlack is how much longer than the work itself a client waits for
	// the results: enough for the exchange and the GPU readings either side
	// of the plan, and not so much that a wedged daemon holds a terminal
	// long past the point of hope.
	ClientSlack = 10 * time.Second

	// PlanWait bounds getting an answer that carries no plan of its own: the
	// daemon probes every configured service before it can reply. It is also
	// the whole wait against a daemon too old to announce its plan.
	PlanWait = 90 * time.Second
)

// WaitFor returns how long a client waits for the results of a plan whose
// longest single action may take longest. Actions on different services run
// concurrently, so a plan's wall time is its longest action and not the sum
// of them; a daemon that reported nothing gets the flat PlanWait.
func WaitFor(longest time.Duration) time.Duration {
	if longest <= 0 {
		return PlanWait
	}
	return longest + ClientSlack
}

// Exchange sends one request to the first daemon that answers and returns its
// final reply. A daemon that announces its plan first is waited on for as
// long as that plan can legitimately take.
func Exchange(ctx context.Context, req Request) (Response, error) {
	var lastErr error
	for _, path := range SearchPaths() {
		resp, connected, err := doAt(ctx, path, req)
		if err == nil {
			return resp, nil
		}
		if connected {
			// Something accepted this connection, so a daemon exists.
			// Folding that into "no daemon is listening" would send an
			// operator to start a second one while the first is still
			// carrying out the plan it was just given.
			return Response{}, err
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = ErrNoDaemon
	}
	return Response{}, fmt.Errorf("%w (tried %v): %v", ErrNoDaemon, SearchPaths(), lastErr)
}

// Do sends one request that carries no plan, which is every read only
// command, and returns the reply.
func Do(ctx context.Context, req Request) (Response, error) {
	req.PlanFirst = false
	return Exchange(ctx, req)
}

// doAt runs one exchange against one socket. The bool reports whether the
// socket accepted the connection, which decides whether a failure means "no
// daemon" or "this daemon".
func doAt(ctx context.Context, path string, req Request) (Response, bool, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return Response{}, false, err
	}
	defer func() { _ = conn.Close() }()

	wait := PlanWait
	if deadline, ok := ctx.Deadline(); ok {
		wait = time.Until(deadline)
	}
	_ = conn.SetDeadline(time.Now().Add(wait))

	encoded, err := json.Marshal(req)
	if err != nil {
		return Response{}, false, fmt.Errorf("encode request: %w", err)
	}
	if _, err := conn.Write(append(encoded, '\n')); err != nil {
		return Response{}, true, connectionError(err, wait)
	}

	reader := bufio.NewReaderSize(conn, 64*1024)
	resp, err := readResponse(reader, wait)
	if err != nil {
		return Response{}, true, err
	}
	if !resp.Preliminary {
		return resp, true, nil
	}

	// The daemon has said what it is about to do. Wait for the results for
	// as long as that work can take under its config, and no longer.
	wait = WaitFor(time.Duration(resp.LongestActionMS) * time.Millisecond)
	_ = conn.SetDeadline(time.Now().Add(wait))
	final, err := readResponse(reader, wait)
	if err != nil {
		return Response{}, true, err
	}
	return final, true, nil
}

func readResponse(reader *bufio.Reader, wait time.Duration) (Response, error) {
	line, err := readLine(reader, maxRequest)
	if err != nil {
		return Response{}, connectionError(err, wait)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return Response{}, fmt.Errorf("decode response: %w", err)
	}
	return resp, nil
}

// connectionError describes a failure on a socket a daemon already accepted.
// The daemon exists, so the message must never suggest starting one: the two
// cases an operator meets are a daemon still working past the wait and a
// daemon that went away in the middle of answering.
func connectionError(err error, wait time.Duration) error {
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		return fmt.Errorf("the daemon accepted the request but did not answer within %s",
			wait.Round(time.Second))
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return errors.New("the daemon closed the connection")
	default:
		return fmt.Errorf("read response: %w", err)
	}
}

// Handler answers one request. It may call send with a preliminary reply
// before returning its final one, which is how a daemon tells a client what
// it is about to do before it takes the time to do it. Only a handler knows
// whether the client asked for that, so only a handler decides.
type Handler func(ctx context.Context, req Request, send func(Response)) Response

// defaultBudget bounds a connection whose listener was given no budget.
const defaultBudget = 2 * time.Minute

// Listener owns a Unix socket and serves requests until its context ends.
type Listener struct {
	path     string
	listener net.Listener
	budget   time.Duration
}

// SetBudget bounds how long one connection may take from the request arriving
// to the answer being written. The daemon sets it from its own config: a plan
// whose longest action is a ten minute drain must not have its answer cut off
// at the default, which is the same mistake as a client giving up too early.
func (l *Listener) SetBudget(d time.Duration) {
	if d > 0 {
		l.budget = d
	}
}

// afterBind, when set by a test, observes the socket between bind and chmod,
// which is the window whose mode the mode guarantee is about.
var afterBind func(path string)

// maxSocketPath is the usable length of a Unix socket path. The kernel's
// sockaddr_un holds 108 bytes including the terminator, and a path over that
// fails with a bare "invalid argument" that says nothing about the cause.
const maxSocketPath = 107

// Listen binds the socket, replacing a stale one left by a crashed daemon.
// A socket that something is still listening on is never removed: that would
// silently steal control from a running daemon.
func Listen(path string) (*Listener, error) {
	if len(path) > maxSocketPath {
		return nil, fmt.Errorf(
			"socket path is %d bytes, which is over the %d byte limit for a Unix socket: %s",
			len(path), maxSocketPath, path)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create socket directory %s: %w", dir, err)
		}
	}

	if _, err := os.Stat(path); err == nil {
		conn, dialErr := net.DialTimeout("unix", path, time.Second)
		if dialErr == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("another gpu-bouncer daemon is already listening on %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket %s: %w", path, err)
		}
	}

	// bind(2) creates the socket at 0777 masked by the process umask, so a
	// chmod afterwards leaves a window in which a permissive umask (a unit
	// with UMask=0000, a shell after umask 0) makes the socket world
	// connectable. The umask is set so that the socket is 0660 from the
	// instant it exists, then restored. The umask is process wide, and
	// Listen runs once at startup before anything else creates files, so the
	// restriction is not observed by any other code.
	old := syscall.Umask(0o777 &^ socketMode)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	if afterBind != nil {
		afterBind(path)
	}
	// The chmod stays as the second line: it corrects the mode on a system
	// whose bind ignores the umask, and it costs nothing on one that honours
	// it, because the mode is already what it is being set to.
	if err := os.Chmod(path, socketMode); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("set permissions on %s: %w", path, err)
	}
	return &Listener{path: path, listener: ln}, nil
}

// Path is the socket this listener owns.
func (l *Listener) Path() string { return l.path }

// Close stops listening and removes the socket file.
func (l *Listener) Close() error {
	err := l.listener.Close()
	_ = os.Remove(l.path)
	return err
}

// Serve accepts connections until ctx is cancelled or Close is called. Each
// connection is handled on its own goroutine, so one slow client cannot block
// the rest.
func (l *Listener) Serve(ctx context.Context, handle Handler) error {
	go func() {
		<-ctx.Done()
		_ = l.listener.Close()
	}()

	for {
		conn, err := l.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// A transient accept failure should not kill the daemon, but a
			// permanently closed listener should.
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		go serveConn(ctx, conn, l.budget, handle)
	}
}

func serveConn(ctx context.Context, conn net.Conn, budget time.Duration, handle Handler) {
	defer func() { _ = conn.Close() }()
	if budget <= 0 {
		budget = defaultBudget
	}
	_ = conn.SetDeadline(time.Now().Add(budget))

	reader := bufio.NewReaderSize(conn, 64*1024)
	line, err := readLine(reader, maxRequest)
	if err != nil {
		writeResponse(conn, Response{Error: "read request: " + err.Error()})
		return
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		writeResponse(conn, Response{Error: "decode request: " + err.Error()})
		return
	}
	// The flag is set here rather than by the handler so that a preliminary
	// reply can never be mistaken for a final one.
	send := func(resp Response) {
		resp.Preliminary = true
		writeResponse(conn, resp)
	}
	writeResponse(conn, handle(ctx, req, send))
}

func writeResponse(conn net.Conn, resp Response) {
	encoded, err := json.Marshal(resp)
	if err != nil {
		// The response could not be encoded, so send a minimal valid one
		// rather than letting the client hang waiting for a line.
		encoded = []byte(`{"ok":false,"error":"the daemon could not encode its response"}`)
	}
	_, _ = conn.Write(append(encoded, '\n'))
}

// readLine reads one newline terminated message, refusing anything over limit.
func readLine(reader *bufio.Reader, limit int) ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := reader.ReadLine()
		if err != nil {
			return nil, err
		}
		buf = append(buf, chunk...)
		if len(buf) > limit {
			return nil, fmt.Errorf("message exceeds %d bytes", limit)
		}
		if !isPrefix {
			return buf, nil
		}
	}
}
