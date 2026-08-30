#!/usr/bin/env bash
# Tests for install-version.sh. Run from the repository root; the gate runs it.
set -uo pipefail
script=$(dirname "$0")/install-version.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fail=0

ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1"; fail=1; }

# expect_ok NAME INSTALL CHANGELOG: the script accepts the pair.
expect_ok() {
  if "$script" "$2" "$3" >"$tmp/out" 2>"$tmp/err"; then ok "$1"; else bad "$1: $(cat "$tmp/err")"; fi
}
# expect_fail NAME INSTALL CHANGELOG SUBSTRING: it refuses, and says SUBSTRING.
expect_fail() {
  local err
  if "$script" "$2" "$3" >"$tmp/out" 2>"$tmp/err"; then bad "$1: accepted '$(cat "$tmp/out")'"; return; fi
  err=$(cat "$tmp/err")
  case "$err" in *"$4"*) ok "$1";; *) bad "$1: message '$err' lacks '$4'";; esac
}

cat > "$tmp/changelog.md" <<'MD'
# Changelog

## [Unreleased]

## [1.2.3] - 2026-01-01

- one

## [1.2.2] - 2025-12-01

- old
MD

cat > "$tmp/match.md" <<'MD'
version=v1.2.3
That prints `gpu-bouncer_v1.2.3_linux_amd64.tar.gz: OK`.
  -ldflags "-X .../cli.Version=v1.2.3"
MD
expect_ok "every version is the top section" "$tmp/match.md" "$tmp/changelog.md"

# The failure this script exists for: the shape INSTALL.md had at the v0.1.2
# tag, where the download block still pinned the previous release.
cat > "$tmp/lagging.md" <<'MD'
version=v1.2.2
That prints `gpu-bouncer_v1.2.2_linux_amd64.tar.gz: OK`.
MD
expect_fail "the download block lags the changelog" "$tmp/lagging.md" "$tmp/changelog.md" "names a version other than 1.2.3"
expect_fail "the lagging file's lines are named" "$tmp/lagging.md" "$tmp/changelog.md" "line 1:v1.2.2"

# One stale mention among correct ones is still a failure, and the line is named.
cat > "$tmp/one_stale.md" <<'MD'
version=v1.2.3
An older note about v1.2.2.
MD
expect_fail "one stale mention among correct ones" "$tmp/one_stale.md" "$tmp/changelog.md" "line 2:v1.2.2"

# A pseudo-version carries the release it is based on, which has to move too.
cat > "$tmp/pseudo.md" <<'MD'
version=v1.2.3
such as `v1.2.2-0.20260830102548-6beb4269d63d+dirty`
MD
expect_fail "a stale pseudo-version" "$tmp/pseudo.md" "$tmp/changelog.md" "line 2:v1.2.2"

cat > "$tmp/pseudo_ok.md" <<'MD'
version=v1.2.3
such as `v1.2.3-0.20260830102548-6beb4269d63d+dirty`
MD
expect_ok "a current pseudo-version" "$tmp/pseudo_ok.md" "$tmp/changelog.md"

# A version without the leading v is prose about some other release, not
# something a reader copies, so it is left alone.
cat > "$tmp/prose.md" <<'MD'
version=v1.2.3
A file that uses this syntax does not load on 1.2.2 or older.
MD
expect_ok "prose about an older release, written without the v" "$tmp/prose.md" "$tmp/changelog.md"

# A file that names no release at all cannot be checked, and cannot be right.
printf 'no versions here at all\n' > "$tmp/none.md"
expect_fail "no version anywhere" "$tmp/none.md" "$tmp/changelog.md" "names no version at all"

# A pre-release at the top of the changelog is a released section too.
cat > "$tmp/pre_changelog.md" <<'MD'
# Changelog

## [2.0.0-rc.1] - 2026-02-01

- candidate
MD
printf 'version=v2.0.0-rc.1\n' > "$tmp/pre_install.md"
expect_ok "a pre-release at the top" "$tmp/pre_install.md" "$tmp/pre_changelog.md"
printf 'version=v2.0.0\n' > "$tmp/pre_wrong.md"
expect_fail "the release while a pre-release is on top" "$tmp/pre_wrong.md" "$tmp/pre_changelog.md" "names a version other than 2.0.0-rc.1"

# Missing files and a changelog with nothing released.
expect_fail "missing install file" "$tmp/absent.md" "$tmp/changelog.md" "does not exist"
expect_fail "missing changelog" "$tmp/match.md" "$tmp/absent.md" "does not exist"
printf '# Changelog\n\n## [Unreleased]\n\n- soon\n' > "$tmp/unreleased.md"
expect_fail "nothing released yet" "$tmp/match.md" "$tmp/unreleased.md" "has no released section"

# And the repository's own files, which is what the gate actually runs.
expect_ok "the repository's INSTALL.md against its CHANGELOG.md" INSTALL.md CHANGELOG.md

if [ "$fail" -ne 0 ]; then echo "install-version_test: FAILED"; exit 1; fi
echo "install-version_test: all ok"
