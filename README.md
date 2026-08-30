# gpu-bouncer

One GPU, many local AI services. gpu-bouncer decides who gets the VRAM and
shows squatters the door.

Ollama, ComfyUI and llama.cpp each assume they own the card. None of them can
see the others, so whichever one loads first keeps its memory and the next one
either fails or quietly falls back to running half on the CPU. gpu-bouncer
watches VRAM, and when a service that matters needs room it asks the lower
priority ones to let go, through each tool's own API, with no changes to any of
them.

## The problem, and the fix

Two Ollama services on one 8 GiB card. The transcript below is a real session
on an RTX 4070 Laptop, not a mock up: two disposable Ollama servers on ports
11500 and 11501 stand in for two services on the same card.

```console
# The coding model has the GPU.

$ OLLAMA_HOST=127.0.0.1:11500 ollama ps
NAME                                                     ID              SIZE      PROCESSOR    CONTEXT    UNTIL
hf.co/bartowski/Qwen2.5-Coder-7B-Instruct-GGUF:Q4_K_M    f218460127af    4.7 GB    100% GPU     4096       29 minutes from now

# A second service wants it too.

$ OLLAMA_HOST=127.0.0.1:11501 ollama run hf.co/bartowski/Qwen_Qwen3.5-4B-GGUF:Q4_K_M "ok" >/dev/null 2>&1

$ OLLAMA_HOST=127.0.0.1:11501 ollama ps
NAME                                           ID              SIZE      PROCESSOR          CONTEXT    UNTIL
hf.co/bartowski/Qwen_Qwen3.5-4B-GGUF:Q4_K_M    f946fe6d5e83    3.3 GB    41%/59% CPU/GPU    4096       29 minutes from now

# It did not fail. It silently ran part of the model on the CPU, because
# the other service will not give the memory back.

$ gpu-bouncer status
GPU 0  NVIDIA GeForce RTX 4070 Laptop GPU  (PCI 0000:01:00.0, vendor 0x10de)
  7079 MiB used of 8188 MiB, 1109 MiB free  (source: nvml)

coder            ollama        priority 10   up
  version 0.33.2
  holding 4528 MiB
  - hf.co/bartowski/Qwen2.5-Coder-7B-Instruct-GGUF:Q4_K_M (4528 MiB) expires 2026-08-30T14:12:07+03:00

assistant        ollama        priority 90   up
  version 0.33.2
  holding 1882 MiB
  - hf.co/bartowski/Qwen_Qwen3.5-4B-GGUF:Q4_K_M (1882 MiB) 1282 MiB spilled to host RAM, expires 2026-08-30T14:14:28+03:00

A daemon is running.
Config: /tmp/gbdemo/config.toml

$ gpu-bouncer request assistant --need-mib 6144
Done:
  release coder: acted, free VRAM 1109 MiB to 5794 MiB (+4685 MiB)
    unloaded hf.co/bartowski/Qwen2.5-Coder-7B-Instruct-GGUF:Q4_K_M

# Reload the same model on the same service.

$ OLLAMA_HOST=127.0.0.1:11501 ollama stop hf.co/bartowski/Qwen_Qwen3.5-4B-GGUF:Q4_K_M >/dev/null 2>&1

$ OLLAMA_HOST=127.0.0.1:11501 ollama run hf.co/bartowski/Qwen_Qwen3.5-4B-GGUF:Q4_K_M "ok" >/dev/null 2>&1

$ OLLAMA_HOST=127.0.0.1:11501 ollama ps
NAME                                           ID              SIZE      PROCESSOR    CONTEXT    UNTIL
hf.co/bartowski/Qwen_Qwen3.5-4B-GGUF:Q4_K_M    f946fe6d5e83    3.1 GB    100% GPU     4096       29 minutes from now

# Done. Drop the claim so the coder model can come back.

$ gpu-bouncer release assistant
released the claim held by assistant
```

The second load did not error. It silently ran 41% on the CPU, which is the
part that makes this hard to notice: you just wonder why things got slow. After
gpu-bouncer freed the coding model, reloading the same model on the same
service put it entirely on the GPU.

`gpu-bouncer` never touched a process. It asked the coding service, over
Ollama's own API, to expire what it was holding, and reported the GPU's own
before and after numbers rather than taking the service's word for it.


## How it compares

| | What it manages | Client changes needed |
|---|---|---|
| [llama-swap](https://github.com/mostlygeek/llama-swap) | Only the backends it launched itself | None, but it cannot see Ollama or ComfyUI |
| Cooperative lease daemons | Whatever asks it for a lease | Yes: every client must be taught to ask |
| **gpu-bouncer** | **Stock services it did not start** | **None** |

llama-swap is very good at what it does, and if every model you run goes
through llama-swap you may not need this. gpu-bouncer solves the other problem:
a machine where Ollama, ComfyUI and something else all run independently and
none of them will cooperate.

## What it does not do

Being clear about this matters more than the feature list.

| Not this | Why |
|---|---|
| Preempt processes it was not told about | A service is invisible to gpu-bouncer unless the config names it. There is no global sweep of whatever holds VRAM. |
| Kill processes | The strongest thing it does is stop a systemd unit, and only where `allow_stop = true`. It never signals a process directly. |
| Interrupt a running ComfyUI job | A busy ComfyUI is skipped and the reason is reported. See [adapters](#adapters). |
| Guarantee memory is free after acting | It reports the GPU's own before and after figures instead of claiming success. Ollama in particular frees VRAM some time after it says it will. |
| Work on Windows | Linux only in v0.1. |
| Report per process VRAM on AMD | AMD cards are read only: total and used VRAM from sysfs, no attribution. Full support needs NVML, which is NVIDIA only. |
| Arbitrate more than one GPU | v0.1 arbitrates the single GPU named by `gpu_index`. |

## Quickstart

```sh
go install github.com/hyprtuna/gpu-bouncer/cmd/gpu-bouncer@latest
```

A minimal config at `~/.config/gpu-bouncer/config.toml`:

```toml
[policy]
vram_floor_mib = 2048
reactive = false          # start here: observe first, act later

[[service]]
name = "ollama"
adapter = "ollama"
endpoint = "http://127.0.0.1:11434"
priority = 50

[[service]]
name = "comfyui"
adapter = "comfyui"
endpoint = "http://127.0.0.1:8188"
priority = 20
```

Check what it sees. Both of these are read only and safe to run anywhere,
with or without the daemon:

```sh
gpu-bouncer status
gpu-bouncer plan
```

When the plan looks right, run the daemon:

```sh
install -Dm644 packaging/systemd/gpu-bouncer-user.service \
  ~/.config/systemd/user/gpu-bouncer.service
systemctl --user enable --now gpu-bouncer
```

Turn on `reactive = true` once you trust what `plan` tells you. See
[INSTALL.md](INSTALL.md) for the system service, release binaries and
troubleshooting.

## Commands

| Command | What it does | Needs the daemon |
|---|---|---|
| `gpu-bouncer daemon` | Runs the arbitration service in the foreground | is the daemon |
| `gpu-bouncer status` | GPU and per service state | no |
| `gpu-bouncer plan` | What would happen right now, and why | no |
| `gpu-bouncer request <service>` | Claim priority, freeing room if needed | yes |
| `gpu-bouncer release <service>` | Drop a claim made with `request` | yes |
| `gpu-bouncer evict <service>` | Free one service now | yes |
| `gpu-bouncer evict --all-except <service>` | Free everything else | yes |

`--dry-run`, `--json` and `--verbose` are accepted before or after any command
name. `--dry-run` is the honest preview: the daemon builds the same plan it
would have executed, reports it, and does nothing. With `--json` every
response, errors included, is one JSON object on stdout; INSTALL.md documents
the shapes.

Exit codes:

| Code | Meaning |
|---|---|
| `0` | The command did what it says. For `request` this includes not getting all the room asked for: the last line says `freed X MiB of the Y MiB asked for`, and `--json` carries `target_met`. |
| `1` | An error, or an executed action that failed (`N of M actions failed`). A daemon that took the request and then stopped answering or closed the connection is reported as `the daemon accepted the request but did not answer within Ns` or `the daemon closed the connection`, not as a missing daemon. |
| `2` | No command, or an unknown command. |

A claim made with `request` stands until you `release` it or the daemon
restarts; a second `request` for the same service updates the amount and keeps
the claim's original place in line. While it stands the daemon keeps defending
it, so a claim you forget about will keep freeing lower priority services.
`gpu-bouncer status` lists outstanding claims and cooldowns.


## Configuration

Two files are read, in order, and the second is layered on top of the first per
key, so a user file can retune one service without restating it:

```
/etc/gpu-bouncer/config.toml
$XDG_CONFIG_HOME/gpu-bouncer/config.toml     (defaults to ~/.config)
```

`GPU_BOUNCER_CONFIG` overrides both with a single file. Unknown keys are an
error rather than being ignored, because a misspelled policy key would silently
change what the daemon is willing to do. So is a number outside its range: a
negative `vram_floor_mib`, `min_effect_mib` or `gpu_index`, a duration of zero
or less, a `poll_interval` under `1s`, or a `drain_timeout` over `10m`. Only
`priority` may be negative, and it has no range beyond what an integer holds.
Duration keys take a quoted duration string such as `"5s"`; a bare number on
one is a wrong type, not a zero.

### `[policy]`

| Key | Default | Meaning |
|---|---|---|
| `vram_floor_mib` | `512` | Free VRAM to defend. Reactive mode engages below it. |
| `reactive` | `false` | Act without being asked. |
| `poll_interval` | `"5s"` | How often to sample VRAM and probe services. At least `"1s"`: every poll probes every service. |
| `default_workload` | unset | The service reactive mode defends. Unset means the highest priority service that is up. |
| `gpu_index` | `0` | Which GPU to arbitrate. |
| `min_effect_mib` | `64` | The smallest measured gain in free VRAM that counts as an action having worked. |
| `action_cooldown` | `"60s"` | After an action that gained less than `min_effect_mib`, or failed, reactive plans leave that service alone for this long. `request` and `evict` bypass it. `status` lists active cooldowns. |

### `[[service]]`

| Key | Default | Meaning |
|---|---|---|
| `name` | required | How you refer to it on the command line. |
| `adapter` | required | `ollama`, `comfyui`, `llama-swap` or `systemd-unit`. |
| `endpoint` | required for HTTP adapters | Base URL, for example `http://127.0.0.1:11434`. |
| `unit` | required for `systemd-unit` | The unit name. |
| `user_unit` | `false` | Use `systemctl --user`. |
| `priority` | `0` | Higher wins. Equal priority never evicts. |
| `allow_stop` | `false` | Required before any process level action. |
| `timeout` | `"5s"` | Bounds every request to this service. |
| `drain_timeout` | `"30s"` | How long a release waits for the service to confirm the unload, at most `"10m"`. Only `ollama` waits; a release still loaded when it expires is a failed action. |

See [packaging/config.example.toml](packaging/config.example.toml) for a
commented version.

## How it decides

`plan` and `daemon` call the same function, so the plan is a preview and not a
second guess.

1. An explicit `request` wins, by priority, then by who asked first.
2. Otherwise, if reactive mode is on and free VRAM is under the floor, the
   service being defended is `default_workload`, or the highest priority
   service that is up.
3. Services strictly below the beneficiary are freed, lowest priority first,
   and within a priority the largest holder first, until the target is met.
4. Equal priority never evicts, so two services cannot take turns throwing each
   other out.

It declines rather than guesses in three cases, each of which would otherwise
produce a success that did not happen: the GPU cannot be read, a service's
probe failed, or a service is busy in a way that would make the release a no-op.
After an action that measurably freed nothing, the daemon's own loop leaves
that service alone for `action_cooldown`, so a service that reloads the moment
it is released is not released once per poll forever; an explicit `request` or
`evict` still acts. The loop never waits for an action: each one runs on its
own, at most one per service at a time, so an Ollama drain on one service
does not stop the next poll from observing and acting on the others, and a
plan passes over a service whose action is still in flight. Every empty plan
says why, one note per service passed over.

## Adapters

Each adapter was written against upstream source, and the endpoints are cited
in the code. Each has real limits, listed here rather than discovered later.

| Adapter | Release mechanism | Limits |
|---|---|---|
| `ollama` | A generate call with `keep_alive` 0 and no prompt, per loaded model | Ollama has no unload endpoint. A 200 only schedules expiry, so the adapter waits for the model to leave `/api/ps`, and even that is not proof the VRAM is back. Ollama cannot report whether it is busy. |
| `comfyui` | `POST /api/free` with `unload_models` and `free_memory` | ComfyUI accepts this while a job is running, answers 200, and does nothing until the job ends. The adapter checks the queue first and declines instead. It reports no per model inventory, so held VRAM is derived from torch's allocator for the arbitrated device and marked estimated. v0.1 assumes the torch device ordinal equals the NVML index, which holds unless `CUDA_VISIBLE_DEVICES` reorders devices for ComfyUI. |
| `llama-swap` | `POST /api/models/unload` | Synchronous, unlike Ollama. Reports no VRAM figure at all, so held memory is an honest zero marked estimated. Set `GPU_BOUNCER_LLAMA_SWAP_API_KEY` if your instance requires a key. |
| `systemd-unit` | none | Can only stop, never release, and only with `allow_stop = true`. A unit says nothing about VRAM. |

## Safety

- Read only by default. `status` and `plan` change nothing and need no daemon.
- Nothing is touched unless the config names it.
- Graceful release goes through a service's own public API: it is the same
  request any client of that service could make.
- No process level action without `allow_stop = true` on that service. The
  check is enforced in the scheduler, again in the daemon, and again in the
  adapter before it execs anything.
- On any adapter error, gpu-bouncer does nothing and reports. `request`,
  `release` and `evict` then exit 1; `status` and `plan` show the error next
  to the service and exit 0, because observing is not acting.
- The HTTP adapters never follow a redirect and never send credentials in a
  URL: a 3xx is an error naming where the service tried to send them, and an
  endpoint with a password in it is refused at config time.
- The control socket is created `0660`, owned by whoever runs the daemon.

Reporting a vulnerability: [SECURITY.md](SECURITY.md).

## Documentation

- [INSTALL.md](INSTALL.md)
- [CONTRIBUTING.md](CONTRIBUTING.md)
- [CHANGELOG.md](CHANGELOG.md)

## Background

Ollama being unable to unload while another process holds VRAM is
[ollama#9926](https://github.com/ollama/ollama/issues/9926), open since March
2025.

## License

MIT. See [LICENSE](LICENSE).
