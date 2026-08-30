# Security policy

## Reporting a vulnerability

Report privately through GitHub Security Advisories:

https://github.com/hyprtuna/gpu-bouncer/security/advisories/new

If that page is unavailable to you, open an issue using the "Security contact"
template. Do not put any details of the vulnerability in that issue: say only
that you have something to report, and you will be contacted for the details.

Please do not open a public issue describing a vulnerability.

## Scope

gpu-bouncer runs as a local daemon and exposes a Unix domain socket. The
things most worth scrutiny are:

- The control socket. It is created with mode 0660 and is owned by the user
  running the daemon. Anything that lets an unintended user drive it is in
  scope.
- The boundary that keeps gpu-bouncer from touching services the config does
  not name, and the `allow_stop` gate in front of every process level action.
- Config parsing, and the HTTP clients that talk to Ollama, ComfyUI and
  llama-swap.

gpu-bouncer does not listen on a network socket and does not accept remote
input. It sends requests to service endpoints that the config names.

## Supported versions

The most recent release is supported.
