# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-08-30

First release. gpu-bouncer arbitrates one GPU between local AI services that
do not cooperate with each other.

### Added

- **Daemon.** `gpu-bouncer daemon` runs the arbitration loop in the
  foreground under systemd supervision. It samples VRAM and probes every
  configured service on `policy.poll_interval`, and serves a Unix domain
  control socket. It is the only component permitted to change a service's
  state. Claims are held in memory and are deliberately not persisted, so a
  claim cannot outlive the process that made it. `--socket` overrides the
  socket path and `--log-level` takes `debug`, `info`, `warn` or `error`.

- **Read only commands.** `gpu-bouncer status` reports GPU and per service
  state; `gpu-bouncer plan` reports what would happen right now without doing
  it. Both work with or without a daemon running. `plan` asks the daemon when
  one is listening, because only the daemon knows the outstanding claims, and
  it names which of the two sources answered. The plan is produced by the same
  pure function the daemon executes, so it is an exact preview.

- **State changing commands.** `gpu-bouncer request <service>` records a
  priority claim and acts on it immediately, with `--need-mib` to ask for a
  specific amount of free VRAM. `gpu-bouncer release <service>` drops a claim
  and frees nothing by itself. `gpu-bouncer evict <service>` frees one service
  now, and `evict --all-except <service>` frees every configured service but
  one. All three refuse to run without a daemon rather than acting directly.

- **Global flags.** `--config` to use one file instead of the search path,
  `--dry-run` to plan and report without changing anything, `--json` for
  machine readable output, and `-v` / `--verbose` for the scheduler's per
  service reasoning.

- **Layered TOML configuration.** `/etc/gpu-bouncer/config.toml` then
  `$XDG_CONFIG_HOME/gpu-bouncer/config.toml`, the second layered on the first.
  Policy keys the user file explicitly sets win, and services are matched by
  name so a user file can retune one key of a system defined service without
  restating the rest. `GPU_BOUNCER_CONFIG` makes a single named file the only
  one loaded. Unknown keys are a hard error rather than being ignored. With no
  config file anywhere, the result is valid and completely inert.

- **GPU reading through NVML, with a sysfs fallback.** NVML is the accurate
  source and needs cgo plus a loadable `libnvidia-ml.so`, both resolved at run
  time. When NVML cannot be opened, gpu-bouncer falls back to reading amdgpu
  VRAM counters under `/sys/class/drm`, which reports total and used memory
  but returns an explicit unsupported error for per process usage rather than
  an empty list. `status` names the source it used. A `CGO_ENABLED=0` build
  compiles and runs with the sysfs source only.

- **Priority scheduler.** A pure function of configuration plus observation:
  no I/O, no service contact. Priority is an integer and higher wins; services
  at equal priority never evict one another. Eviction candidates are ordered
  lowest priority first and, within a priority, largest holder first, so the
  plan is deterministic. Every service considered and passed over is recorded
  in the plan's notes, so an empty plan is never silent. With no VRAM reading
  at all, the scheduler refuses to act.

- **Ollama adapter.** Reads `/api/version` and `/api/ps`, and releases through
  the `/api/generate` `keep_alive: 0` short circuit, verified against
  ollama/ollama v0.33.2. It confirms `done_reason` is `unload` rather than
  trusting the 200, then polls `/api/ps` until the models are gone, because
  Ollama schedules expiry and returns before the memory is back. Limits: it
  reports per model `size_vram` figures, but Ollama exposes no in flight
  request count, so idleness is unknown; it cannot stop the process.

- **ComfyUI adapter.** Reads `/api/system_stats` and `/api/prompt`, and
  releases through `POST /api/free`, verified against ComfyUI 0.34.0. Limits:
  ComfyUI serialises no per model inventory, so the held figure is the torch
  allocator's reserved minus free for the arbitrated device and is reported as
  estimated. `exec_info.queue_remaining` is pending plus running, so idleness
  is exact, and release refuses outright when anything is queued or running,
  because ComfyUI would return 200 and free nothing until the current prompt
  finished. It cannot stop the process.

- **llama-swap adapter.** Reads `/health` and `/running`, and releases through
  `POST /api/models/unload`, verified against mostlygeek/llama-swap v250. The
  unload is synchronous, so no drain poll is needed, but it kills in flight
  requests. `GPU_BOUNCER_LLAMA_SWAP_API_KEY` supplies a bearer token when the
  server has API keys configured; it is an environment variable so the secret
  need not be written into a config file in `/etc`. Limits: llama-swap reports
  no VRAM figure anywhere, so held VRAM is 0 marked estimated rather than
  guessed; `/running` reports a lifecycle state and not activity, so idleness
  is unknown; it cannot stop the process.

- **systemd unit adapter.** Observes a unit with `systemctl is-active` and
  `systemctl show`, and can stop it. `--user` is supported through
  `user_unit`. It is the only adapter that acts at the process level. Limits:
  there is no graceful release, because systemd cannot ask a program to hand
  back memory; a unit reports nothing about VRAM, so held VRAM is 0 marked
  estimated; an active unit says nothing about whether it is busy. A
  misspelled unit name is reported as a config error rather than as a stopped
  service.

- **Safety model.** Read only by default: `reactive` is off, so nothing is
  acted on without an explicit `request` or `evict`. A service that is not
  named in the config is invisible to gpu-bouncer and is never touched, and
  the daemon re-checks that at the point of action. Every process level
  action requires `allow_stop = true` on that service, checked both by the
  scheduler and again inside the systemd adapter's `Stop`. Graceful release
  through a service's own API needs no such permission, because it is the same
  request any client could make. The control socket is created with mode 0660
  and owned by the user running the daemon, so a user unit yields a socket
  only that user can drive. Every action is logged with the GPU's own measured
  free VRAM either side of it, because a service reporting that it unloaded
  something is not evidence that the memory came back.

- **Packaging.** A system unit and a user unit in `packaging/systemd/`, and a
  commented `packaging/config.example.toml`.

- **CI and releases.** A single `gate` job running gofmt, `go vet`,
  staticcheck, a build, a `CGO_ENABLED=0` build, `go test -race` and a check
  that no em or en dash appears in the repository. Releases are built for
  linux/amd64 only, verified by running the binary, published as a `.tar.gz`
  with a `.sha256` alongside it, and gated behind a GitHub environment that
  requires a human reviewer.

[Unreleased]: https://github.com/hyprtuna/gpu-bouncer/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/hyprtuna/gpu-bouncer/releases/tag/v0.1.0
