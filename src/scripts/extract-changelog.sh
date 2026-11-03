#!/usr/bin/env bash
#
# Print the CHANGELOG.md section for a given tag, used as the GitHub release body.
#
#   ./src/scripts/extract-changelog.sh v0.1.0
#
# Run from the repository root: CHANGELOG.md is a repo-root file, and the module
# this script lives in is src/.
set -euo pipefail

tag="${1:?usage: extract-changelog.sh <tag>}"
# Tags come in two forms on the same commit - v0.1.0 drives the release and
# src/v0.1.0 is the module alias Go resolves through. Accept either, so this
# still works if it is ever handed GITHUB_REF_NAME from the aliased tag.
tag="${tag#src/}"
version="${tag#v}"
changelog="${CHANGELOG_FILE:-CHANGELOG.md}"

[ -f "$changelog" ] || { echo "error: $changelog not found" >&2; exit 1; }

# Print lines between "## [<version>]" and the next level-2 heading (another
# release, or the "Pre-0.1.0 history" section that follows 0.1.0).
body="$(awk -v ver="$version" '
  $0 ~ "^## \\[" ver "\\]"  { capture = 1; next }
  capture && /^## /         { exit }
  capture && /^\[[^]]+\]:/  { exit }   # stop at link-reference definitions
  capture                   { print }
' "$changelog" | sed -e 's/[[:space:]]*$//' | awk 'NF {p=1} p')"

# A missing section used to print nothing and exit 0, which is the worst
# available outcome: the release still publishes, with an empty body, and
# nothing anywhere reports a problem. Every release must carry a real
# description, so the absence of one stops the release instead.
if [ -z "$body" ]; then
  echo "error: $changelog has no '## [$version]' section" >&2
  echo "hint: move the [Unreleased] entries into a dated '## [$version]' heading" >&2
  exit 1
fi

printf '%s\n' "$body"
