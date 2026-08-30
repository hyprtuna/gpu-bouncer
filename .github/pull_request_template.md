## What this changes

<!-- One or two sentences. -->

## Why

<!-- The problem this solves. Link an issue if there is one. -->

## Verification

<!--
What did you actually run? Paste the output. "Should work" is not verification.
-->

- [ ] `gofmt -l .` is empty
- [ ] `go vet ./...` is clean
- [ ] `staticcheck ./...` is clean
- [ ] `go test -race ./...` passes
- [ ] New behaviour has a test that fails without the change

## If this touches an adapter

- [ ] The endpoints are cited in a comment, against upstream source or docs
- [ ] Tests cover success, refusal, timeout and a garbage response
- [ ] Nothing mutates a service unless the config names it
