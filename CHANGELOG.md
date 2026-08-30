# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.4] - 2026-08-30

A maintenance tail: two wrong messages on paths an operator actually walks,
and a few small edges. An existing config file keeps loading, with one
exception that was never a working setting, a `timeout` over `1h`.

### Fixed

- **A request that had nothing to evict blamed a GPU reading that had
  succeeded.** `request svc --need-mib N`, with nothing lower in priority
  holding VRAM, produces a plan with no actions, and the last line read `how
  much of the 1168 MiB asked for was freed is not known: the GPU could not be
  read`. The same reply carried `plan.current_free_mib`, a reading the daemon
  had just taken, and README and INSTALL both promise `freed X MiB of the Y
  MiB asked for, target not met` here. Nothing ran, so nothing moved that
  reading: the line now says `freed 0 MiB`, and the unreadable-GPU wording is
  kept for the case it describes, an action that ran and left no usable
  reading either side of it.
- **A client and a daemon of different releases reported a config nobody had
  edited as stale, permanently.** Up to 0.1.2 the digest a daemon reported
  covered the file paths as well as their contents; since 0.1.3 it covers the
  contents alone, and nothing on the wire said which. The two could never
  match, so `status` said `config_stale: true` and `restart it to apply your
  edit`, and said it again after the restart. Anyone who upgrades with `go
  install` and has not yet restarted their daemon was in that state. A daemon
  now names the recipe it used, and a client compares only a recipe it
  implements; where it cannot, `config_stale` is null and `status` says the
  comparison could not be made.
- **`daemon_config.paths` was absent rather than empty.** It was the one list
  on the wire still carrying `omitempty`, so a daemon that had loaded no
  config file sent `daemon_config` without it, against the promise that every
  list is present and empty as `[]`. A daemon too old to send the list has its
  joined `path` split into one, so the two never contradict each other.
- **A refused `unit` did not name the file it was in.** Unit names were
  checked after every file had been merged, where nothing knows which file
  supplied a value, so an operator running a system file plus a user overlay
  was told which service and key were wrong but not which file to edit. Every
  numeric bound has named the file all along; units are checked the same way
  now, per file.
- **`status` reported a wedged daemon as no daemon at all.** A daemon that
  accepted the connection and then never answered was waited out for 30
  seconds and reported as `No daemon is running`, which sends an operator to
  start a second one alongside the first. It is reported as running now, with
  the fields it never answered on left null.
- **`release-notes.sh` truncated or overran a section around code fences.** A
  fence was recognised only at column 1, so an indented one never opened and a
  `## [` line inside it ended the section early; and a fence nobody closed
  swallowed the next section into the body, which would publish an older
  release's notes under this tag. Fences are recognised wherever they are
  indented to, and an unclosed one is refused rather than guessed at.

### Changed

- `timeout` gains an upper bound of `1h`. Every deadline built from it is that
  value plus another, and without a bound a config the binary accepted could
  make that sum overflow an int64 and put the client back on the fixed 90
  second wait that a long timeout is set to avoid.
- `free_after_mib` on a `request` or an `evict` reply carries the reading the
  plan itself was built on when the plan had nothing to run, which is how the
  daemon already decides `target_met`. It stays null when a reading was taken
  and failed, and when the card could not be read at all.

### Added

- `digest_recipe` on `daemon_config`: how the daemon computed the digest it
  reports, `content-v1` for this release. It exists so that a client can tell
  a digest it can reproduce from one it cannot, rather than reading a
  disagreement between two recipes as an edit.

## [0.1.3] - 2026-08-30

The last planned fix round. An existing config file keeps loading, with two
exceptions that were never working settings: a bare number on a duration key,
which used to be replaced by the default, and a `drain_timeout` over `10m`.

### Fixed

- **A client gave up after a fixed 90 seconds and blamed a missing daemon.**
  `evict --all-except` over four services that would not drain exited 1 with
  `no gpu-bouncer daemon is listening` while the daemon was still carrying out
  the releases it had been asked for, and the operator was advised to start a
  second one. A client's actions now run concurrently, one at a time per
  service, so a plan costs its longest action rather than the sum of them; the
  daemon sends the plan before carrying it out, and the client waits for the
  largest `timeout` plus `drain_timeout` in that plan, plus ten seconds. A
  failure on a socket that accepted the connection now reads `the daemon
  accepted the request but did not answer within Ns` or `the daemon closed the
  connection`, and no longer suggests starting a daemon. The control
  connection's own cap follows the same bound instead of a flat two minutes.
  A daemon or client of an earlier release still works in either direction.
- **A bare number on a duration key skipped its range check and became the
  default.** `poll_interval = 0`, `action_cooldown = 0`, `timeout = 0` and
  `drain_timeout = 0`, and `-0`, loaded and were silently replaced, while
  `"0s"` was refused: TOML hands an integer to a text unmarshaler as its
  digits and `time.ParseDuration` accepts a unitless `"0"`. A non-string on a
  duration key is now a hard error naming the key and the file. With that
  closed, nothing a file sets can reach a default substitution, which is what
  checking the raw value was written to guarantee.
- **A dangling `device` symlink dropped the card and renumbered the next
  one.** The virtual card test followed the symlink, so a link pointing
  nowhere read as a card with no PCI vendor. A card counts as virtual only
  when its `device` directory can be read and holds no `vendor`; a dangling
  link, a target this process cannot reach, and an entry that is not a
  directory all keep the card at its index, unreadable with the reason.
- **The NVML failure did not lead on two of the four surfaces that carry
  it.** `plan`'s note and the daemon's refusal to start put sixty characters
  of device identification ahead of `nvml: init: ...`, which is the one line
  saying what to fix. The reason leads everywhere now and the device follows
  in brackets.
- **The staleness warning compared the wrong thing.** The file path was part
  of the digest and the client compared its own files against the daemon's, so
  a client with a different `--config`, a different `XDG_CONFIG_HOME`, or no
  config file at all was permanently told to restart the daemon to apply an
  edit it had not made, and no restart could clear it. The documented setup of
  a system daemon plus a per user client overlay hit this on every `status`.
  The daemon now reports the paths it loaded and a digest over their contents
  only, and the client re-reads those same paths. A file the daemon loaded
  that cannot be read now leaves `config_stale` null and says so, and a daemon
  that loaded no file says `the daemon loaded no config file` rather than
  naming an empty path.
- **INSTALL.md shipped pinned to the previous release.** At the v0.1.2 tag the
  download block still said `version=v0.1.1`, so a reader who copied it
  downloaded and successfully verified the wrong release. Every version string
  is bumped, and the gate now refuses an INSTALL.md naming any release but the
  one at the top of this file, so it cannot recur silently.
- **A field the daemon did not send was reported as false.** A client against
  an older daemon started with `--dry-run` printed `A daemon is running.` and
  `daemon_dry_run: false` while that daemon was planning and never acting.
  `daemon_dry_run` is null when it was not reported, and `status` says the
  daemon is older than the client and does not say.
- **A GPU reading that failed was reported as zero.** The read taken either
  side of an action discarded its error, so a failure logged
  `free_after_mib=0`, a figure nobody measured, and fed the difference from
  the real reading before it, minus the whole card, into the cooldown that
  decides whether an action was worth taking. Both figures are now null in
  `--json`, `unknown` in the log, and the failure itself is logged. An action
  whose effect could not be measured no longer starts a cooldown, because an
  unmeasurable action is not a useless one.
- **Every request to a local service logged `duration=0s`.** The figure was
  rounded to the millisecond, and a call to a service on the same machine
  takes less than that. It is measured to the microsecond now.
- **A systemd `unit` was never validated.** toml 1.6.0 decodes `\e`, so a
  config file could put an escape character into a unit name, where it reached
  a `systemctl` argument and every error message about the service invisibly.
  A unit must now be a systemd unit name: a known type suffix, systemd's own
  character set before it, a letter or digit first so it can never be read as
  an option, and within systemd's 255 byte limit. A refusal shows any control
  character escaped rather than printing it.
- **`services[].items` was absent when empty**, though 0.1.2 promised every
  list present as `[]`. Nested lists now carry that guarantee too, and
  INSTALL names the nested strings that can be absent instead of implying
  they cannot.
- **`message` was documented as appearing only on a dry run.** A plain second
  `request` for a service that already holds a claim carries it as well.
- **The release notes extractor mishandled five inputs.** A `## [` heading or
  a `[x]: url` link definition inside a fenced code block truncated the body
  at the fence; a CRLF changelog put a carriage return at the end of every
  published line and defeated the emptiness check; a section of only spaces or
  tabs was published as spaces or tabs; and a note quoting its own heading in
  prose was counted as a second section, which blocked a legitimate release.
  Fences are tracked, carriage returns are stripped first, headings are
  counted anchored, and a body with no non-whitespace character is empty.

### Changed

- `github.com/BurntSushi/toml` from 1.5.0 to 1.6.0, which accepts TOML 1.1
  syntax that every earlier release refuses, such as a multi-line inline table
  or a trailing comma in one. A config file written for this release may
  therefore not load on 0.1.2 or older. README's Configuration section states
  the level.
- `drain_timeout` gains an upper bound of `10m`. A drain is a client and a
  control connection both waiting on a service that has already been told to
  let go; past ten minutes it is not draining, it is stuck.
- A plan's actions run concurrently, one at a time per service. `evict
  --all-except` over four stuck services costs one drain rather than four.

### Added

- `free_after_mib` on a `request` and an `evict` reply: the GPU's free VRAM
  read once after every action has finished, which is what `target_met` and
  the `freed X MiB of the Y MiB asked for` line are measured against. The per
  action figures describe overlapping windows now that actions run together.
- `paths` on `daemon_config`, the list of files the daemon loaded, so a client
  can re-read exactly those files rather than split a joined string.
- `install-version.sh`, with its own tests, run by the gate: every `vX.Y.Z` in
  INSTALL.md must be the version at the top of this file.

## [0.1.2] - 2026-08-30

A second fix round. As before, an existing config file keeps loading: the
one new bound, a `poll_interval` under `1s`, was never a working setting.

### Fixed

- **A negative `vram_floor_mib` or `min_effect_mib` was accepted and wrapped
  to 18446744073709551615**, which made reactive mode fire on every poll and
  disabled the cooldown, while `poll_interval = "-1s"` was already refused.
  Every numeric key now has a declared range checked on the raw value before
  it reaches a typed field; a negative unsigned key is a hard error naming
  the key and the file. `poll_interval` has a lower bound of `1s`.
  `priority` stays signed by design. A test enumerates every numeric field
  by reflection and fails when one has no bound.
- **A `--dry-run` daemon recorded claims it could never release.** It now
  records nothing: `request` returns the plan and says the daemon is in
  dry-run mode, `release` says there is nothing to release, and `status`
  says `A daemon is running in dry-run mode: it plans and never acts.`
- **The sysfs source dropped a card whose `device` directory it could not
  traverse**, renumbering every card after it, and **one card with a missing
  or unparsable counter hid every card**. Only a missing `device/vendor`
  means a virtual card now; any other failure leaves that card in the
  numbering as present and unreadable with the error, and the other cards
  stay readable. An unreadable vendor file is reported as such.
- **A cgo build whose NVML failed to load was told it needs a build with
  cgo.** The fallback source keeps the NVML error, and an NVIDIA card's
  reason now starts with it (`nvml: init: ...`) and says NVML is present in
  this build but could not be opened; a build without NVML support says so
  instead. `plan`'s note and the daemon's refusal carry the same text.
- **The poll loop blocked on actions.** An Ollama drain on one service
  stopped the observation of every other service for up to `drain_timeout`.
  Actions now run on their own goroutines, at most one per service at a
  time; a plan passes over a service whose action is still in flight, with a
  note; observation, claims and `status` continue every poll; a client's
  `request` or `evict` still waits for its own actions.
- **`gpu-bouncer --json` with no command printed the usage and no JSON, and
  `--json <unknown command>` printed the usage before the JSON.** Both emit
  only `{"ok": false, "error": "..."}` and exit 2.
- **`status --json` reported `gpu.index` 0 for a `gpu_index` that names no
  device**; it reports the configured index.
- **A second `request` for the same service reset its timestamp**, losing an
  equal priority tie to a peer that asked in between. It now updates the
  amount, keeps the original time, and says `updated the claim held since
  <time>`.
- **`--log-level debug` logged nothing beyond `info`.** See Added.
- **`release-notes.sh` could not publish a pre-release tag and silently
  concatenated a duplicated section.** Headings may carry a pre-release or
  build suffix, a duplicated version is refused, and the script has tests the
  gate runs.
- A service name echoed into an error is elided at 80 characters. Every
  expired cooldown is swept whether or not its service was observed.
  `evict --all-except` with an unknown name and no GPU reading reports both
  reasons. The `unload_all_models` citation was already fixed in 0.1.1; the
  README's "exits 1" sentence is scoped to the state changing commands.

### Changed

- The `--json` shapes are owned by the command line: every list is present,
  empty as `[]`, never null or absent, and INSTALL.md documents the complete
  `status`, `plan`, `request`, `evict`, `release` and `version` responses.
- A `request` that could not reach its target still exits 0, but its last
  text line always reads `freed X MiB of the Y MiB asked for`, with
  `, target not met` when it was not, and the JSON carries `target_met`.
- README documents the exit codes: 0, 1 (an error or a failed action), 2 (no
  command or an unknown one).

### Added

- **`status` notices a stale daemon config.** The daemon never reloads; its
  ping and status replies now carry `daemon_config` (path, SHA-256 of the
  loaded files, load time), and `status` ends with `the daemon loaded a
  different config (<path>, loaded <time>); restart it to apply your edit`
  when the files it read differ, with `config_stale` in JSON. INSTALL.md
  documents that the daemon does not reload and shows the restart command
  for each unit.
- **Debug logging.** At `--log-level debug` the daemon writes one line per
  poll with the VRAM reading and every service's observed state, and one
  line per adapter HTTP request with method, redacted URL, status and
  duration. Headers are never logged, and a test proves a llama-swap API
  key never reaches the log.
- `daemon_dry_run`, `daemon_config`, `config_stale` and `target_met` in the
  JSON output; `claims` and `cooldowns` documented.
- A troubleshooting entry for a reverse proxy that redirects `http` to
  `https`, which makes a working service read as `down` by design.
- Dependabot watches Go modules weekly.

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

[Unreleased]: https://github.com/hyprtuna/gpu-bouncer/compare/v0.1.4...HEAD
[0.1.4]: https://github.com/hyprtuna/gpu-bouncer/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/hyprtuna/gpu-bouncer/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/hyprtuna/gpu-bouncer/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/hyprtuna/gpu-bouncer/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/hyprtuna/gpu-bouncer/releases/tag/v0.1.0
