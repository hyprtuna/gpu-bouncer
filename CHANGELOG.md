# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.1] - 2026-08-30

A fix round after the release audit of 0.1.0. Nothing here changes the
config file format for an existing file: the new keys all have defaults.

### Fixed

- **The sysfs source silently arbitrated a different physical GPU.** It
  enumerated only amdgpu cards and numbered them by slice position, so on a
  laptop with an NVIDIA card and an AMD integrated GPU a `CGO_ENABLED=0`
  build, or a cgo build whose NVML failed to load, called the integrated GPU
  "GPU 0" and would have evicted services over memory pressure on a card none
  of them use. The source now lists every DRM card backed by a PCI device, in
  kernel order, and keeps a card whose VRAM it cannot read (an NVIDIA card, or
  an AMD card without `mem_info_vram_total`) in the numbering as present and
  unreadable, with the reason. When the arbitrated index is unreadable,
  `plan` says why, `request` and `evict` refuse, and `daemon` refuses to
  start with a message that says an NVIDIA card needs the cgo build and the
  driver.
- **A failed action exited 0.** `request`, `evict` and `release` now exit 1
  when any executed action carries an error, the text output ends with
  `N of M actions failed`, and the JSON response has `ok` false. An action the
  daemon declined with a reason is not a failure.
- **Reactive mode could repeat a useless action forever.** A service that
  reloaded the moment it was released was released once per poll, each
  logged with the same free VRAM before and after; for llama-swap that loop
  kills in flight requests. After an action whose measured gain is below
  `policy.min_effect_mib`, or that failed, the service now enters a cooldown
  of `policy.action_cooldown` during which the daemon's own plans skip it
  with a note naming when the cooldown ends. `request` and `evict` bypass it,
  and `status` lists active cooldowns.
- **The HTTP client trusted the far end too much.** A 3xx was followed to a
  host the config never named and its answer accepted; it is now an error
  naming the `Location`. A body over 4 MiB was silently truncated and its
  first 4 MiB decoded as success; it now fails with `response larger than
  4 MiB`. An endpoint with a password in it was accepted, sent as Basic auth
  and echoed into every error string; it is now a config error that points
  llama-swap users at `GPU_BOUNCER_LLAMA_SWAP_API_KEY`, and every URL in an
  error string is redacted regardless.
- **The ComfyUI adapter ignored `gpu_index`** and always read torch device 0.
  It now reads the arbitrated device, assuming the torch device ordinal
  equals the NVML index.
- **An Ollama release that never drained blocked for a hardcoded 30 s and
  was reported as acted.** The wait is now bounded by the new per service
  `drain_timeout`, and a drain that times out is a failed action with
  `acted` false and the error `still loaded after 30s`.
- **Three scheduler guarantees the documentation made and the code did not
  keep.** `evict` and `evict --all-except` acted without a VRAM reading; they
  now refuse like everything else. A service passed over for its priority,
  for being down, for a failed probe or for holding nothing produced no note;
  every passed over service now gets one, with the reason.
  `evict --all-except` with a misspelled name reported `Free VRAM: 0 MiB`;
  it now reports the real figure. A plan's expected free VRAM could exceed
  the card's total; it is now capped at it.
- **A `gpu_index` past the device count was accepted and left `status`
  printing `state unavailable` with no reason.** It now names the index and
  the device count, `status` prints it, and `daemon` refuses to start on it.
- **`timeout` and `poll_interval` of zero or less were silently replaced by
  the default.** They are now hard errors naming the key and the file, as are
  `drain_timeout` and `action_cooldown`.
- **`status` omitted the daemon line and `Config:` when no services were
  configured**, so an empty list could not be traced to the file that
  produced it. Both are printed regardless.
- **The control socket was created at 0777 masked by the umask and then
  chmod'ed to 0660**, world connectable for a moment under a permissive
  umask. It is now 0660 from the instant it exists.
- **A `go install` build reported its version as `dev`.** It now reports the
  module version Go recorded in the binary; the release ldflag keeps
  precedence.
- **The README transcript was not what was recorded.** It is re-recorded with
  every printed command being the command run, and pasted in unedited.
- **`gpu-bouncer --help` printed the usage twice, a flag parse error was
  printed twice around a usage dump, and `version` ignored its arguments.**
  The usage is printed once, a flag error is one line with exit 1, and
  `version` parses its flags and rejects positional arguments.
- The `unload_all_models` citation in the ComfyUI adapter pointed at the
  wrong line of `model_management.py`.
- The v0.1.0 release body was GitHub's generated text naming one pull
  request; it has been replaced with the 0.1.0 section of this file.

### Changed

- With `--json`, every error is now a JSON object `{"ok": false, "error":
  "..."}` on stdout, with the same exit code as before. The `plan` object
  uses snake_case keys (`trigger`, `beneficiary`, `current_free_mib`,
  `target_free_mib`, `total_mib`, `actions`, `notes`) like every sibling
  object; `--json version` prints `{"version": "..."}`; `--json status`
  gains `daemon_running`, `config` and a `devices` list.
- `--json`, `--verbose` and `--dry-run` are accepted after every command
  name, as no ops where they have no effect.
- `evict <name>` and `release <name>` for a service the config does not name
  exit 1 with the message `request` already used, and `evict --all-except`
  with an unknown name keeps refusing and exits 1.
- `status` prints each device's PCI bus id and vendor, and lists every other
  device the source sees under the arbitrated one.
- An unknown key in a `[[service]]` block is reported against the service
  by name.
- The release workflow publishes the CHANGELOG section for the tag as the
  release body, and fails when the section is missing or the top released
  section is a different version. `actions/checkout` and `actions/setup-go`
  are on v7 and `softprops/action-gh-release` on v3.
- The public `.gitignore` no longer names local tooling.

### Added

- `--version`, an alias for the `version` command.
- Per service `drain_timeout` (default `"30s"`).
- `policy.min_effect_mib` (default `64`) and `policy.action_cooldown`
  (default `"60s"`).
- A weekly Dependabot check for GitHub Actions.
- INSTALL.md states the glibc floor of the release binary (2.34) and explains
  the NVML header warnings a cgo build prints.

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
  action requires `allow_stop = true` on that service, checked by the
  scheduler, again by the daemon at the point of action, and again inside
  the systemd adapter's `Stop`. Graceful release
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

[Unreleased]: https://github.com/hyprtuna/gpu-bouncer/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/hyprtuna/gpu-bouncer/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/hyprtuna/gpu-bouncer/releases/tag/v0.1.0
