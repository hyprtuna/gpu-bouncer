# Contributing to gpu-bouncer

gpu-bouncer decides when to take VRAM away from a program someone is using.
Almost every rule below exists because a wrong answer here destroys work that
a user cared about, and because the tests run on machines with real services
on them.

## Development setup

You need Go 1.24 or newer, and staticcheck:

```sh
go install honnef.co/go/tools/cmd/staticcheck@latest
```

That installs into `$(go env GOPATH)/bin`. Put it on your `PATH`. CI pins
`honnef.co/go/tools/cmd/staticcheck@2026.2.1`, so a version skew can show up
as a finding locally that CI does not report, or the reverse. When that
happens, CI's pinned version is the one that decides.

Then:

```sh
git clone https://github.com/hyprtuna/gpu-bouncer
cd gpu-bouncer
go build ./...
go test ./...
```

The `cmd/gpu-bouncer` package is the only main package. Everything else lives
under `internal/`.

## The gate

CI runs one job named `gate`. It is one job on purpose: a green gate is a
complete statement rather than a partial one. Four checks are the ones you
will actually break, and the PR template asks you to confirm them:

```sh
gofmt -l .          # must print nothing
go vet ./...
staticcheck ./...
go test -race ./...
```

The gate also runs these, which are usually green without you thinking about
them:

```sh
go mod tidy && git diff --exit-code -- go.mod go.sum   # go.mod is tidy
go build ./...
CGO_ENABLED=0 go build ./...                           # must still compile
git grep -nP '\x{2014}|\x{2013}' -- .                  # must find nothing
```

The `CGO_ENABLED=0` build matters. go-nvml binds through cgo, so a build
without it cannot use NVML and must degrade to the sysfs source rather than
fail. That path is compiled in CI so it cannot rot.

Run the whole gate before you push:

```sh
gofmt -l . && go vet ./... && staticcheck ./... && go test -race ./...
```

## How tests are structured

Tests live next to the code they cover. There are three shapes.

**Pure logic.** `internal/scheduler` is a pure function of configuration plus
observed state: no I/O, no service contact, nothing changed. It is tested with
golden cases in `scheduler_test.go`. This is where arbitration behaviour
should be pinned down, because a test here needs no fake of anything.

**Adapters.** Every adapter test stands up a `net/http/httptest` server that
imitates the upstream service, and points the adapter at its URL. See
`fakeOllama` in `internal/adapter/ollama_test.go` for the pattern.

**The systemd adapter.** `systemdUnitAdapter` holds a `runner` field, which
the tests replace with `fakeSystemctl` in
`internal/adapter/systemdunit_test.go`. The real `systemctl` is never
executed by any test.

### Never test against a live service

This is not a style preference. Contributors run this suite on their own
machines, which have their own real Ollama, ComfyUI, llama-swap and systemd
units on them, frequently mid generation. A test that hits
`http://127.0.0.1:11434` and calls `Release` unloads somebody's models. A test
that shells out to the real `systemctl stop` takes down somebody's service.
Neither is an acceptable thing for `go test ./...` to do.

So:

- Adapter tests point at an `httptest.Server`. Never at a fixed port, never at
  a URL from the environment.
- The systemd adapter is driven through its injected runner. Never through
  `exec`.
- Nothing in the suite may listen on a fixed path. Daemon tests put their
  socket in `t.TempDir()` and set `GPU_BOUNCER_SOCKET` with `t.Setenv`.

There is exactly one test that touches real hardware, and it is behind a build
tag so it is excluded from every normal build and from CI. Run it by hand:

```sh
go test -tags nvmlsmoke ./internal/gpu/ -v
```

It is read only. It opens NVML, lists devices and reads their memory. It
changes no GPU state and touches no process. If you add to it, keep it that
way.

## Adding an adapter

The interface is `Adapter` in `internal/adapter/adapter.go`:

```go
type Adapter interface {
	Name() string
	Kind() config.AdapterKind
	Capabilities() Capabilities
	Probe(ctx context.Context) (Status, error)
	Release(ctx context.Context) (Result, error)
	Stop(ctx context.Context) (Result, error)
}
```

`internal/adapter/ollama.go` is the worked example. Work through it alongside
this checklist.

1. **Verify the endpoints against upstream first.** Before any code: find the
   handler in the upstream source, or the documented route, and note the
   version you checked. See the honesty rule below.

2. **Add the kind.** In `internal/config/config.go`, add an `AdapterKind`
   constant and list it in `KnownAdapters`. If it talks HTTP, add it to
   `needsEndpoint` so the config validator requires an `endpoint` and rejects
   a `unit`.

3. **Write the adapter.** New file in `internal/adapter/`. Take a
   `config.Service` in a `newX(svc config.Service)` constructor, and keep
   `svc.Timeout.D()` as the per request budget. Use `newHTTPClient()` and the
   `doJSON` / `doText` helpers in `httpclient.go` rather than a fresh
   `http.Client`: they cap the response body, turn a non 2xx into an
   `httpError` carrying the status, and refuse to let a decode failure look
   like success.

4. **Fill in `Capabilities` honestly.** `CanRelease` only if the service has a
   real way to drop VRAM. `CanReportIdle` only if `Probe` actually fills in
   `Idle`. `CanStop` only if there is a process to stop. The scheduler uses
   these to avoid planning an action that would be refused or silently
   ignored, so an optimistic value here becomes a logged success that did not
   happen.

5. **`Probe` must be read only.** It runs on every poll and from
   `gpu-bouncer status`, including against services gpu-bouncer has no
   permission to touch. It must never change anything.

6. **`Release` must be the service's own public API.** It is the same request
   any client of that service could make, so it needs no special permission.
   Return `Acted: false` with a `Detail` when there was correctly nothing to
   do, rather than reporting a release that did not happen.

7. **`Stop` is process level.** If your adapter cannot do it, return an error
   wrapping `ErrNotSupported`, as the ollama, comfyui and llama-swap adapters
   all do. If it can, check `allow_stop` inside `Stop` before doing anything.
   The daemon checks it too; both checks are deliberate.

8. **Register it.** Add the case to `New` in `adapter.go`.

9. **Document it.** Add a commented `[[service]]` block to
   `packaging/config.example.toml`, and mention the adapter's real limits.

10. **Test it.** An `httptest` fake, covering at minimum: a healthy probe,
    a refusal, a timeout, and a garbage response. The PR template asks for
    those four.

## The honesty rule for adapters

Two rules, both non negotiable.

**Endpoints are verified, not assumed.** Every route an adapter calls must be
checked against upstream source or upstream documentation, and cited in a
comment at the top of the adapter with the version you checked. The existing
adapters do this:

```go
// Endpoints verified against ollama/ollama at v0.33.2:
//
//	GET  /api/version   server/routes.go, returns {"version":"0.33.2"}
//	GET  /api/ps        server/routes.go PsHandler, response type api.ProcessResponse
//	                    in api/types.go, whose per model VRAM field is size_vram
//	POST /api/generate   server/routes.go:403, the unload short circuit
```

Cite the file, and the line or symbol where you can. A reviewer has to be able
to check your claim without guessing which of three plausible routes you meant.
Where the upstream behaviour is surprising, say so in the comment: the ollama
adapter records that a 200 from the unload short circuit means expiry was
scheduled and not that VRAM is free, which is why it then polls `/api/ps`.

**An adapter reports what it does not know.** `Status` has two fields for
exactly this:

- `HeldEstimated` marks a VRAM figure that was derived rather than reported by
  the service. It also marks a zero that means "no data" rather than
  "measured zero".
- `IdleKnown` is false when the adapter cannot tell whether the service is
  busy, and `Idle` must then be ignored.

Do not invent a plausible number. llama-swap exposes no VRAM figure anywhere,
so its adapter reports `HeldMiB: 0` with `HeldEstimated: true` rather than
guessing from a model name. llama-swap's `/running` reports a lifecycle state
and not activity, so `CanReportIdle` is false rather than reading "ready" as
idle. A wrong guess in either field feeds straight into an eviction decision
that kills someone's in flight generation.

The same applies to what an adapter reports having done. ComfyUI's `POST
/free` returns 200 on a busy server and frees nothing until the running prompt
finishes, so that adapter checks the queue first and returns
`Acted: false` with the reason rather than claiming a release.

## Commits

**Conventional Commits.** `type(scope): description`, lowercase, no trailing
full stop. The scope is the package or area. Real examples from this
repository:

```
feat(gpu): read VRAM through NVML with a sysfs fallback
feat(adapter): drive ollama, comfyui, llama-swap and systemd units
ci: add the gate job and a reviewed release workflow
chore(packaging): systemd units and a documented example config
docs: security policy, issue templates and code owners
```

**One logical change per commit.** A commit that changes an adapter and
reformats an unrelated package is two commits.

**No em dashes or en dashes, anywhere.** Not in code, comments, docs, commit
messages or PR descriptions. Use a plain hyphen. The gate enforces it, and you
can check before you push:

```sh
git grep -nP '\x{2014}|\x{2013}'
```

That must find nothing.

## Pull requests

1. Branch, commit, push, open a PR.
2. Fill in `.github/pull_request_template.md`. It is the checklist, and the
   Verification section wants what you actually ran and its output. "Should
   work" is not verification.
3. The `gate` check must be green. The branch ruleset requires a check named
   exactly `gate`, so a red or missing gate blocks the merge.
4. New behaviour needs a test that fails without the change.

If your change touches an adapter, the template's last three boxes apply: the
endpoints are cited against upstream, the tests cover success, refusal,
timeout and a garbage response, and nothing mutates a service unless the
config names it.

## Security

Do not open a public issue for a vulnerability. See
[SECURITY.md](SECURITY.md).
