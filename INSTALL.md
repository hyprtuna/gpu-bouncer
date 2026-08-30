# Installing gpu-bouncer

## Requirements

- Linux. gpu-bouncer reads GPU state through NVML or through
  `/sys/class/drm`, and talks to the daemon over a Unix domain socket.
- An NVIDIA driver, for full accuracy. NVML is the accurate source. Without
  it gpu-bouncer falls back to reading amdgpu counters from `/sys/class/drm`,
  which reports total and used VRAM but not per process usage. See
  [When status says the source is sysfs](#when-status-says-the-source-is-sysfs).
- Go 1.24 or newer, only if you are building from source or using
  `go install`. A release binary needs no Go toolchain.
- A C compiler, only if you are building from source with cgo enabled, which
  is the default. NVML is reached through cgo. A build with `CGO_ENABLED=0`
  needs no C compiler and still works, with the sysfs fallback only.

Release binaries are published for `linux/amd64` only. NVML support requires
cgo, so an arm64 binary would have to be cross compiled, and CI cannot run a
cross compiled binary before publishing it. Build from source on other
architectures.

## Install with go install

```sh
go install github.com/hyprtuna/gpu-bouncer/cmd/gpu-bouncer@latest
```

The `/cmd/gpu-bouncer` suffix is required: the module root holds no main
package. The binary lands in `$(go env GOPATH)/bin`, which is `~/go/bin`
unless you have changed it. Put that directory on your `PATH`.

A cgo build prints a page of `warning: 'nvmlDevice...' is deprecated
[-Wdeprecated-declarations]` lines: they come from the NVML headers bundled
with go-nvml, are harmless, and the build has not failed.

A binary installed this way reports the module version Go recorded in it:

```
$ gpu-bouncer version
gpu-bouncer v0.1.1
```

`gpu-bouncer --version` prints the same line. A plain `go build` in a git
checkout reports what Go records for it, the nearest tag or a pseudo-version
such as `v0.1.1-0.20260830102548-6beb4269d63d+dirty`; a build from a tree
with no git history reports `gpu-bouncer dev`. The `-ldflags` line under
[Build from source](#build-from-source) stamps an exact version either way.

## Install from a release binary

Releases are at https://github.com/hyprtuna/gpu-bouncer/releases. Each one
carries two files: a `.tar.gz` holding the single `gpu-bouncer` binary, and a
`.sha256` file for it.

```sh
version=v0.1.1
base=gpu-bouncer_${version}_linux_amd64.tar.gz

curl -LO "https://github.com/hyprtuna/gpu-bouncer/releases/download/${version}/${base}"
curl -LO "https://github.com/hyprtuna/gpu-bouncer/releases/download/${version}/${base}.sha256"
```

Verify the checksum before you unpack. The `.sha256` file is `sha256sum`
output, so `sha256sum -c` reads it directly. Both files must be in the same
directory, and the name inside the `.sha256` file must match the tarball you
downloaded:

```sh
sha256sum -c "${base}.sha256"
```

That prints `gpu-bouncer_v0.1.1_linux_amd64.tar.gz: OK` and exits 0. Anything
else means the download does not match what was published: stop, and do not
unpack it.

Then unpack and install:

```sh
tar xzf "${base}"
install -Dm755 gpu-bouncer ~/.local/bin/gpu-bouncer
```

Use `sudo install -Dm755 gpu-bouncer /usr/local/bin/gpu-bouncer` instead if
you are going to run the system unit, whose `ExecStart` points there.

The published binary is dynamically linked against glibc and needs glibc 2.34
or newer (Ubuntu 22.04, Debian 12, Fedora 35, RHEL 9 or later); older systems
build from source.

## Build from source

```sh
git clone https://github.com/hyprtuna/gpu-bouncer
cd gpu-bouncer
go build -o gpu-bouncer ./cmd/gpu-bouncer
```

To stamp a version number, set the same variable the release workflow sets:

```sh
go build -trimpath \
  -ldflags "-X github.com/hyprtuna/gpu-bouncer/internal/cli.Version=v0.1.1" \
  -o gpu-bouncer ./cmd/gpu-bouncer
```

To build without cgo, on a machine with no C toolchain or no NVIDIA driver:

```sh
CGO_ENABLED=0 go build -o gpu-bouncer ./cmd/gpu-bouncer
```

That binary compiles and runs. It cannot use NVML at all, and always falls
back to the sysfs source, which cannot read an NVIDIA card's memory; see
[When status says the source is sysfs](#when-status-says-the-source-is-sysfs)
before using it on a machine with one.

## Minimal configuration

Configuration is TOML. Two files are consulted, in this order:

```
/etc/gpu-bouncer/config.toml
$XDG_CONFIG_HOME/gpu-bouncer/config.toml   (defaults to ~/.config)
```

The second is layered on top of the first. Policy keys that the user file
explicitly sets win. Services are matched by `name`, so a user file can
retune one key of a system defined service without restating the rest.

Setting `GPU_BOUNCER_CONFIG` to a path makes that single file the only one
loaded, and that file must then exist. The `--config <path>` flag does the
same thing.

If no config file exists anywhere, gpu-bouncer starts with an empty service
list. That is valid, and it is completely inert: a service that is not named
in the config is invisible to gpu-bouncer and is never touched.

The fully commented example is `packaging/config.example.toml`. A genuinely
minimal file that does something is one service:

```toml
[[service]]
name = "ollama"
adapter = "ollama"
endpoint = "http://127.0.0.1:11434"
priority = 50
```

Save it as `~/.config/gpu-bouncer/config.toml`. Everything else takes a
default: `vram_floor_mib = 512`, `reactive = false`, `poll_interval = "5s"`,
`gpu_index = 0`, `min_effect_mib = 64`, `action_cooldown = "60s"`, and per
service a `timeout` of `5s` and a `drain_timeout` of `30s`. A number outside
its range is an error, not a request for the default: a duration of zero or
less, a `poll_interval` under `1s`, or a negative `vram_floor_mib`,
`min_effect_mib` or `gpu_index`. `priority` is the one key that may be
negative.

With `reactive = false`, which is the default, gpu-bouncer never acts on its
own. It acts only when you run `gpu-bouncer request` or `gpu-bouncer evict`.
Leave it off until `gpu-bouncer plan` has been telling you the right thing
for a while.

The four adapter kinds are `ollama`, `comfyui`, `llama-swap` and
`systemd-unit`. The first three require `endpoint` and reject `unit`;
`systemd-unit` requires `unit` and rejects `endpoint`.

If llama-swap is configured with API keys, export
`GPU_BOUNCER_LLAMA_SWAP_API_KEY` in the daemon's environment. It is an
environment variable rather than a config key so that the secret does not
have to be written into a file in `/etc`. Without it, every llama-swap
inventory and release call gets a 401.

## systemd setup

Two units ship in `packaging/systemd/`. They differ in who owns the control
socket.

### Which one to choose

Pick the **user unit** on a single user desktop where Ollama, ComfyUI or
llama-swap run as your own user. The daemon runs as you, the socket lands in
`$XDG_RUNTIME_DIR/gpu-bouncer.sock` and is owned by you, so
`gpu-bouncer request`, `release` and `evict` work with no root at all.

Pick the **system unit** when the services being arbitrated are themselves
system services, or when gpu-bouncer needs to stop a system unit. The daemon
runs as root, so the socket it creates is owned by root. The socket is created
with mode 0660 by the daemon's own user, so on a system unit the state
changing commands need root as well.

There is no third option that gives a root daemon a user drivable socket.
Permission on the socket is exactly the permission of the user running the
daemon.

### User unit

```sh
install -Dm644 packaging/systemd/gpu-bouncer-user.service \
  ~/.config/systemd/user/gpu-bouncer.service
systemctl --user daemon-reload
systemctl --user enable --now gpu-bouncer
```

The unit's `ExecStart` is `%h/.local/bin/gpu-bouncer daemon`. If you used
`go install`, the binary is in `~/go/bin` instead, so either copy it to
`~/.local/bin` or edit the unit.

Socket: `$XDG_RUNTIME_DIR/gpu-bouncer.sock`, typically
`/run/user/$(id -u)/gpu-bouncer.sock`.

### System unit

```sh
sudo install -Dm644 packaging/systemd/gpu-bouncer.service \
  /etc/systemd/system/gpu-bouncer.service
sudo systemctl daemon-reload
sudo systemctl enable --now gpu-bouncer
```

The unit's `ExecStart` is `/usr/local/bin/gpu-bouncer daemon`. It sets
`RuntimeDirectory=gpu-bouncer`, so systemd creates `/run/gpu-bouncer` and
removes it again when the service stops.

Socket: `/run/gpu-bouncer/gpu-bouncer.sock`.

The unit applies modest hardening and deliberately stops there. The
`systemd-unit` adapter has to reach the system manager to stop a unit, and
NVML has to reach the NVIDIA device nodes. Tightening either of those is what
breaks the service in practice.

### Where the client looks

A client tries, in order:

1. `$XDG_RUNTIME_DIR/gpu-bouncer.sock`
2. `/run/gpu-bouncer/gpu-bouncer.sock`

so a user daemon wins over a system one, matching the config layering. Setting
`GPU_BOUNCER_SOCKET` replaces that list with the single path you give it, on
both the client and the daemon. `gpu-bouncer daemon --socket <path>` sets it
for the daemon alone.

### The daemon does not reload its configuration

The daemon reads the config files once, at startup, and never again. Every
client invocation reads them afresh, so after an edit `status` shows the new
file while `plan`, `request`, `release` and `evict` are answered by a daemon
that still holds the old one. `status` notices: when the files it read differ
from what the daemon loaded, it ends with

```
the daemon loaded a different config (/home/you/.config/gpu-bouncer/config.toml, loaded 2026-08-30T12:00:00+03:00); restart it to apply your edit
```

and `--json status` sets `config_stale` to `true` next to `daemon_config`,
which carries the path, a SHA-256 of the loaded files and the load time.
Restart the daemon to apply the edit:

```sh
systemctl --user restart gpu-bouncer      # user unit
sudo systemctl restart gpu-bouncer        # system unit
```

or stop and start it again if you are running it in the foreground. A
restart drops every outstanding claim, which are deliberately not persisted.

### Running the daemon in the foreground

The daemon does not fork. It runs in the foreground and expects systemd to
supervise it, which also means you can just run it yourself to watch it:

```sh
gpu-bouncer daemon --log-level debug
```

`--log-level` takes `debug`, `info`, `warn` or `error`, and defaults to
`info`. Logs go to stderr. At `debug` the daemon writes one line per poll
with the VRAM reading and, per service, whether it is up, what it holds,
whether it is idle, its probe error, its cooldown and its claim, and one
line per HTTP request an adapter makes, with the method, the URL, the status
and the duration. Request headers are never logged, so a llama-swap API key
never appears.

## Verify it works

All three of these are safe. `status` and `plan` are read only: they probe
services with each adapter's read only call and never ask anything to give up
VRAM. Neither one needs a daemon.

```sh
gpu-bouncer version
```

```
gpu-bouncer v0.1.1
```

```sh
gpu-bouncer status
```

```
GPU 0  NVIDIA GeForce RTX 4070  (PCI 0000:01:00.0, vendor 0x10de)
  7104 MiB used of 12282 MiB, 5178 MiB free  (source: nvml)

ollama           ollama        priority 50   up
  version 0.33.2
  holding 4900 MiB
  - llama3:8b (4900 MiB) expires 2026-08-30T12:04:31+02:00

No daemon is running: gpu-bouncer is observing only, and will not act.
Config: /home/you/.config/gpu-bouncer/config.toml
```

A held figure printed as `holding 0 MiB (estimated)` is not a measurement of
zero. It means the adapter has no VRAM figure to report at all. The
`llama-swap` and `systemd-unit` adapters are always in that position, and the
`comfyui` adapter's figure is a torch allocator proxy rather than a per model
number, so it is marked estimated too.

```sh
gpu-bouncer plan
```

```
Trigger: none
Free VRAM: 5178 MiB

No action.

Notes:
  - reactive mode is off and nothing was requested

Source: no daemon is running, so outstanding claims are not visible
```

`plan` asks the daemon when one is running, because only the daemon knows the
outstanding claims. With no daemon it computes the same plan locally from the
reactive policy alone, and the `Source:` line says which of the two you got.
The plan is produced by the same function the daemon executes, so it is an
exact preview rather than an approximation.

Add `--json` to any of these for machine readable output, and `--verbose` to
see the scheduler's per service reasoning in the text output. Both are
accepted before or after the command name. With `--json`, an error is also a
JSON object, `{"ok": false, "error": "..."}` on stdout, with the same exit
code as the text mode. `--json status` carries `daemon_running` and `config`
(the path, or `null` when no file was found) alongside the `gpu`, `devices`
and `services` objects.

### The JSON shapes

Every `--json` response is one object on stdout. Lists are always present,
empty as `[]`. An error on any command is `{"ok": false, "error": "..."}`
with the same exit code as the text mode, including exit 2 for no command or
an unknown one.

`status`:

| Key | Type | Meaning |
|---|---|---|
| `ok` | bool | always true when the command ran |
| `gpu` | object | the arbitrated device: `known`, `index`, `name`, `bus_id`, `vendor`, `source`, `total_mib`, `used_mib`, `free_mib`, `error`. `index` is the configured `gpu_index` even when no device is behind it; `error` says why `known` is false |
| `devices` | list of the same object | every device the source sees |
| `services` | list | per service: `name`, `adapter`, `priority`, `up`, `version`, `items`, `held_mib`, `held_estimated`, `idle`, `idle_known`, `allow_stop`, `error` |
| `claims` | list | outstanding claims: `service`, `need_mib`, `at`; empty without a daemon |
| `cooldowns` | list | services the daemon's loop is leaving alone: `service`, `until`, `reason`; empty without a daemon |
| `daemon_running` | bool | whether a daemon answered |
| `daemon_dry_run` | bool | whether that daemon plans and never acts |
| `daemon_config` | object or null | what the daemon loaded: `path`, `sha256`, `loaded_at`; null without a daemon |
| `config_stale` | bool | true when the files this command read differ from `daemon_config` |
| `config` | string or null | the file(s) this command read; null when none was found |

`plan`: `{"ok": true, "plan": {...}}` where `plan` has `trigger`,
`beneficiary`, `current_free_mib`, `target_free_mib`, `total_mib`, `actions`
(list of `service`, `verb`, `reason`, `expect_free_mib`) and `notes` (list of
strings).

`request` and `evict`: `ok`, `error` (only when an executed action failed:
`N of M actions failed`), `message` (only on a dry run or a dry-run daemon),
`plan` as above, `executed` (list of `service`, `verb`, `reason`, `acted`,
`detail`, `error`, `free_before_mib`, `free_after_mib`), and on `request`
only `target_met` (bool: the free VRAM measured after the last action is at
or above the target).

`release`: `{"ok": true, "message": "..."}`. `version`: `{"version": "..."}`.

`request`, `release` and `evict` do change state, and they refuse to run
without a daemon rather than acting directly. The daemon is the only component
allowed to act, because it is the one holding the config that says which
services may be touched at all. `--dry-run`, accepted globally and after any
command name, makes the daemon plan and report without acting; on `status`,
`plan` and `version` it changes nothing because they never act. When any
action a command executed failed, the text output ends with
`N of M actions failed` and the command exits 1; an action the daemon
declined with a reason, such as a busy ComfyUI, is not a failure. A `request`
that could not reach its target is not a failure either: it exits 0 and its
last line says `freed X MiB of the Y MiB asked for, target not met`, with
`target_met` false in JSON.

## Uninstall

```sh
# Stop and disable whichever unit you installed.
systemctl --user disable --now gpu-bouncer
rm ~/.config/systemd/user/gpu-bouncer.service
systemctl --user daemon-reload

# Or, for the system unit:
sudo systemctl disable --now gpu-bouncer
sudo rm /etc/systemd/system/gpu-bouncer.service
sudo systemctl daemon-reload

# The binary, wherever you put it.
rm ~/.local/bin/gpu-bouncer          # or ~/go/bin/gpu-bouncer
sudo rm /usr/local/bin/gpu-bouncer   # if you installed it system wide

# Configuration.
rm ~/.config/gpu-bouncer/config.toml
sudo rm -r /etc/gpu-bouncer
```

There is nothing to clean up for the socket. It lives in a runtime directory,
the daemon removes it when it shuts down, and `/run` and `/run/user` do not
survive a reboot in any case. A socket file left behind by a crashed daemon is
detected and replaced on the next start; a socket something is still listening
on is never removed, so a second daemon fails to start rather than silently
taking control from the first.

gpu-bouncer stores no state anywhere else. Claims live in the daemon's memory
and are deliberately not persisted.

## Troubleshooting

### No daemon is running

```
$ gpu-bouncer evict comfyui
gpu-bouncer: no gpu-bouncer daemon is listening (tried [/run/user/1000/gpu-bouncer.sock /run/gpu-bouncer/gpu-bouncer.sock]): dial unix /run/user/1000/gpu-bouncer.sock: connect: no such file or directory
Start one with "gpu-bouncer daemon", or enable the service. status and plan work without a daemon
```

The exit code is 1. The bracketed list is exactly the paths that were tried,
in order, so it also tells you whether `GPU_BOUNCER_SOCKET` took effect.

`status` and `plan` still work in this state. `status` will end with
`No daemon is running: gpu-bouncer is observing only, and will not act.`

Start one with `gpu-bouncer daemon`, or `systemctl --user start gpu-bouncer`,
or `sudo systemctl start gpu-bouncer`.

### Permission denied reaching the socket

```
gpu-bouncer: no gpu-bouncer daemon is listening (tried [/run/gpu-bouncer/gpu-bouncer.sock]): dial unix /run/gpu-bouncer/gpu-bouncer.sock: connect: permission denied
```

The socket exists but your user cannot open it. It is created with mode 0660
and owned by whoever runs the daemon, so this is the normal result of running
a client as your user against a system daemon running as root.

Either run the state changing commands with `sudo`, or switch to the user unit
so the daemon runs as you. Do not loosen the socket's mode: anything that can
open it can ask the daemon to unload your models.

### A service behind a reverse proxy reads as down with a redirect error

```
comfyui          comfyui       priority 20   down
  error: comfyui: GET http://127.0.0.1:8188/api/system_stats: HTTP 302 redirect to https://127.0.0.1:8188/api/system_stats refused: gpu-bouncer only talks to the endpoint the config names
```

A reverse proxy that answers `http` with a redirect to `https` makes a
working service read as `down`, by design: gpu-bouncer never follows a
redirect, because a 3xx would send the request, and trust the answer, of a
host or scheme the config never named. Point `endpoint` at the URL the
service actually answers on, here the `https` one.

### When status says the source is sysfs

```
GPU 0  NVIDIA device 0x2820  (PCI 0000:01:00.0, vendor 0x10de)
  state unavailable  (source: sysfs)
  sysfs exposes no VRAM counters for an NVIDIA card: reading it needs a gpu-bouncer build with cgo and the NVIDIA driver, see INSTALL.md
Other GPUs, not arbitrated (policy.gpu_index = 0):
  GPU 1  AMD device 0x1900  (PCI 0000:05:00.0, vendor 0x1002), 1149 MiB used of 2048 MiB
```

`source: nvml` is the accurate path. `source: sysfs` means NVML could not be
opened and gpu-bouncer fell back to reading `/sys/class/drm`. Three things
cause it, and the reason line on an NVIDIA card says which:

- The binary was built with `CGO_ENABLED=0`. go-nvml binds through cgo, so
  such a binary can never use NVML. The reason starts with
  `this build has no NVML support (built without cgo)`. Rebuild with cgo
  enabled.
- `libnvidia-ml.so.1` is not loadable. go-nvml resolves it at run time, so a
  binary built with cgo still starts on a machine with no NVIDIA driver, or
  with a broken library on the loader path, and falls back. The reason then
  starts with NVML's own error, for example
  `nvml: init: ERROR_LIBRARY_NOT_FOUND;`, followed by
  `NVML, which this build has, could not be opened`. Install the NVIDIA
  driver, or check that the daemon's process can load the library and see
  the device nodes.
- There is no NVIDIA GPU. On an AMD card sysfs is the only source there is,
  and it is working as intended.

A card whose sysfs entries cannot be read, for example a `device` directory
the daemon's user may not traverse, or a counter file that does not parse, is
still listed at its index as unreadable with the error, so no other card is
renumbered and no other card is hidden by it.

The sysfs source lists every DRM card that sits on a PCI device, in kernel
order, and numbers them in that order. Virtual cards such as simpledrm are
skipped. VRAM counters in sysfs exist only for the amdgpu driver, so an NVIDIA
card is listed with its PCI address and vendor but marked unreadable, and it
keeps its index. That matters on a laptop with an NVIDIA card and an AMD
integrated GPU, which is the output above: the NVIDIA card is GPU 0, the
integrated GPU is GPU 1, and an earlier gpu-bouncer that enumerated only
amdgpu cards would have called the integrated GPU "GPU 0" and arbitrated its
2 GiB carve-out on behalf of services that run on the NVIDIA card.

When the arbitrated index is unreadable, `plan` reports it as the reason no
action is safe, `gpu-bouncer daemon` refuses to start with the same reason,
and `evict` and `request` refuse to act. The fix on such a machine is the
cgo build plus the NVIDIA driver, not a different `gpu_index`: pointing
`gpu_index` at the integrated GPU would make the daemon start, and then evict
services over memory pressure on a card none of them use.

What you lose on sysfs for a readable AMD card is per process VRAM
accounting, which that source cannot answer and reports as unsupported rather
than as an empty list. Total, used and free VRAM are read, so arbitration of
that card has the figures it makes decisions from.

### GPU state unavailable

```
GPU  state unavailable
  no usable GPU source: nvml: init: ...
  sysfs: read /sys/class/drm: ...
```

Neither source opened. The message lists every attempt and why each failed.

```
GPU  state unavailable
  policy.gpu_index 1 names no device: the nvml source sees 1 device(s), indexes 0 to 0
```

A source opened, but `gpu_index` points past the devices it sees. The
`devices` list in `--json status` shows what it does see. A third form, where
the device exists but its memory cannot be read, is under
[When status says the source is sysfs](#when-status-says-the-source-is-sysfs).

In every form `status` degrades to the per service view rather than failing
outright, but the scheduler refuses to act at all without a VRAM reading:
`plan` says `GPU state could not be read, so no action is safe:` followed by
the same reason, `evict` and `request` refuse the same way, and
`gpu-bouncer daemon` refuses to start with `refusing to start, cannot read the
arbitrated GPU:` and the reason, rather than running as a daemon that could
only ever do nothing.

### gpu-bouncer never does anything

Check `gpu-bouncer status` first. If it says
`No services are configured, so gpu-bouncer will never act.` then either no
config file was found, in which case the output lists the paths that were
searched, or the file named on the `Config:` line at the end declares no
`[[service]]` block. The daemon line and the `Config:` line are printed
whether or not services are configured.

If services are listed but `plan` keeps saying `No action.`, read the `Notes:`
block. Every service the scheduler considered and passed over appears there
with the reason, so an empty plan is never silent. Common ones: reactive mode
is off and nothing was requested; free VRAM is already at or above the floor;
a candidate is not below the beneficiary's priority; a candidate has no
release API and `allow_stop` is false; a candidate is busy and a release would
silently not take effect; a candidate is cooling down after an action on it
freed nothing, which `status` lists with the time the cooldown ends.
