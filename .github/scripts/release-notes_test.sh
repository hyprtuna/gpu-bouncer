#!/usr/bin/env bash
# Tests for release-notes.sh. Run from the repository root; the gate runs it.
set -uo pipefail
script=$(dirname "$0")/release-notes.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fail=0

ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1"; fail=1; }

# expect_notes NAME TAG FILE EXPECTED: the script prints exactly EXPECTED.
expect_notes() {
  local got
  if got=$("$script" "$2" "$3" 2>"$tmp/err") && [ "$got" = "$4" ]; then ok "$1"; else bad "$1: got '$got' err '$(cat "$tmp/err")'"; fi
}
# expect_fail NAME TAG FILE SUBSTRING: the script fails and the message contains SUBSTRING.
expect_fail() {
  local err
  if "$script" "$2" "$3" >"$tmp/out" 2>"$tmp/err"; then bad "$1: succeeded with '$(cat "$tmp/out")'"; return; fi
  err=$(cat "$tmp/err")
  case "$err" in *"$4"*) ok "$1";; *) bad "$1: message '$err' lacks '$4'";; esac
}

cat > "$tmp/plain.md" <<'MD'
# Changelog

## [Unreleased]

## [1.2.3] - 2026-01-01

- one
- two

## [1.2.2] - 2025-12-01

- old

[Unreleased]: https://example/compare/v1.2.3...HEAD
[1.2.3]: https://example/compare/v1.2.2...v1.2.3
MD
expect_notes "plain section" v1.2.3 "$tmp/plain.md" $'- one\n- two'
expect_notes "tag without v" 1.2.3 "$tmp/plain.md" $'- one\n- two'
expect_fail "older tag than the top section" v1.2.2 "$tmp/plain.md" "top released section of $tmp/plain.md is 1.2.3, but the tag is v1.2.2"
expect_fail "unknown tag" v9.9.9 "$tmp/plain.md" "but the tag is v9.9.9"
expect_fail "missing file" v1.2.3 "$tmp/absent.md" "does not exist"

cat > "$tmp/pre.md" <<'MD'
## [Unreleased]

## [2.0.0-rc.1] - 2026-02-01

- candidate

## [1.2.3] - 2026-01-01

- one
MD
expect_notes "pre-release tag" v2.0.0-rc.1 "$tmp/pre.md" "- candidate"
expect_fail "release tag while a pre-release is on top" v1.2.3 "$tmp/pre.md" "top released section of $tmp/pre.md is 2.0.0-rc.1"

cat > "$tmp/build.md" <<'MD'
## [1.0.0+build.5] - 2026-03-01

- built
MD
expect_notes "build metadata tag" v1.0.0+build.5 "$tmp/build.md" "- built"

cat > "$tmp/dup.md" <<'MD'
## [1.2.3] - 2026-01-01

- first copy

## [1.2.3] - 2026-01-01

- second copy
MD
expect_fail "duplicated section" v1.2.3 "$tmp/dup.md" "has 2 sections headed [1.2.3], want exactly one"

cat > "$tmp/unreleased.md" <<'MD'
## [Unreleased]

- not yet

## [1.2.3] - 2026-01-01

- one
MD
expect_notes "Unreleased with content on top is skipped" v1.2.3 "$tmp/unreleased.md" "- one"

cat > "$tmp/empty.md" <<'MD'
## [1.2.3] - 2026-01-01

## [1.2.2] - 2025-12-01

- old
MD
expect_fail "empty section" v1.2.3 "$tmp/empty.md" "the 1.2.3 section of $tmp/empty.md is empty"

printf '# Changelog\n\nnothing released\n' > "$tmp/none.md"
expect_fail "no released section" v1.2.3 "$tmp/none.md" "has no released section"

exit $fail
