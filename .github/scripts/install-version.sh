#!/usr/bin/env bash
# Checks that every release version named in INSTALL.md is the one at the top
# of the CHANGELOG.
#
#   install-version.sh [INSTALL.md] [CHANGELOG.md]
#
# INSTALL.md tells a reader which tarball to download and which checksum to
# verify. At the v0.1.2 tag it still said version=v0.1.1, so a reader who
# copied the block downloaded the previous release and verified its checksum
# successfully, with nothing anywhere to say the wrong thing had happened.
# The gate runs this, so an INSTALL.md that lags the CHANGELOG fails on the
# pull request and again on the tag, before anything is published.
#
# The rule is deliberately blunt: every vX.Y.Z in the file is the release the
# file describes. A deliberate reference to some other release has to be
# written without the leading v, which reads as prose rather than as
# something to copy.
set -euo pipefail

install=${1:-INSTALL.md}
changelog=${2:-CHANGELOG.md}

for file in "$install" "$changelog"; do
  if [ ! -f "$file" ]; then
    echo "install-version: $file does not exist" >&2
    exit 1
  fi
done

# The same released heading release-notes.sh uses: "## [Unreleased]" is not
# one, and a pre-release or build suffix is.
heading='^## \[[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]*)?\]'
top=$(grep -m1 -oE "$heading" "$changelog" | sed -e 's/^## \[//' -e 's/\]$//' || true)
if [ -z "$top" ]; then
  echo "install-version: $changelog has no released section" >&2
  exit 1
fi

# Every version in the file, with the line it sits on. The token carries any
# pre-release or build suffix, so a pre-release release such as v2.0.0-rc.1 is
# compared whole rather than as its base.
found=$(grep -nEo 'v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]*)?' "$install" || true)
if [ -z "$found" ]; then
  echo "install-version: $install names no version at all, so nobody can tell which release it describes" >&2
  exit 1
fi

# A suffix on the release itself is allowed: the file's pseudo-version example
# is "<the release>-0.<timestamp>-<hash>+dirty", which has to move with it.
# Anything naming a different release is what this is here to catch.
wrong=""
count=0
while IFS= read -r entry; do
  [ -n "$entry" ] || continue
  count=$((count + 1))
  version=${entry#*:}
  # A version at the end of a sentence picks up the full stop.
  while [ "${version%.}" != "$version" ]; do version=${version%.}; done
  case "$version" in
    "v$top" | "v$top-"* | "v$top+"*) ;;
    *) wrong="${wrong}${entry}
" ;;
  esac
done <<EOF
$found
EOF

if [ -n "$wrong" ]; then
  echo "install-version: $install names a version other than $top, the top released section of $changelog:" >&2
  printf '%s' "$wrong" | sed 's/^/  line /' >&2
  echo "A reader copying that block downloads and verifies the wrong release. Bump $install." >&2
  exit 1
fi

echo "install-version: $install names v$top and nothing else ($count occurrence(s))"
