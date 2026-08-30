#!/usr/bin/env bash
# Prints the CHANGELOG section for a release tag, for use as the release body.
#
#   release-notes.sh vX.Y.Z[-pre][+build] [CHANGELOG.md] > notes.md
#
# Fails when the CHANGELOG has no "## [X.Y.Z]" section for the tag, when it
# has more than one, or when the top released section is a different version:
# a tag whose notes were not written is not published with somebody else's.
set -euo pipefail

tag=${1:?usage: release-notes.sh vX.Y.Z [CHANGELOG.md]}
changelog=${2:-CHANGELOG.md}
version=${tag#v}

if [ ! -f "$changelog" ]; then
  echo "release-notes: $changelog does not exist" >&2
  exit 1
fi

# A released heading is "## [<semver>]", where semver may carry a pre-release
# or build suffix. "## [Unreleased]" is not one.
heading='^## \[[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]*)?\]'
top=$(grep -m1 -oE "$heading" "$changelog" | sed -e 's/^## \[//' -e 's/\]$//' || true)
if [ -z "$top" ]; then
  echo "release-notes: $changelog has no released section" >&2
  exit 1
fi
if [ "$top" != "$version" ]; then
  echo "release-notes: the top released section of $changelog is $top, but the tag is $tag" >&2
  exit 1
fi
count=$(grep -cF -- "## [$version]" "$changelog" || true)
if [ "$count" -ne 1 ]; then
  echo "release-notes: $changelog has $count sections headed [$version], want exactly one" >&2
  exit 1
fi

# Everything between the section heading and the next heading or the link
# definitions at the end of the file, minus leading and trailing blank lines.
notes=$(awk -v v="$version" '
  on && (/^## \[/ || /^\[[^]]+\]: /) { exit }
  index($0, "## [" v "]") == 1 { on = 1; next }
  on { print }
' "$changelog")
notes=$(printf '%s\n' "$notes" | sed -e '/./,$!d')
if [ -z "$notes" ]; then
  echo "release-notes: the $version section of $changelog is empty" >&2
  exit 1
fi
printf '%s\n' "$notes"
