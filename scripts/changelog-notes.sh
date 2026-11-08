#!/bin/sh
# Print the CHANGELOG.md section for one release, without its heading.
# Usage: sh scripts/changelog-notes.sh v0.3.0 [CHANGELOG.md]
# Exits non-zero when the version has no "## vX.Y.Z" section or it is empty.
set -eu

version=${1:?usage: changelog-notes.sh vX.Y.Z [CHANGELOG.md]}
file=${2:-CHANGELOG.md}

awk -v version="$version" '
  /^## / {
    if (found) exit
    if ($2 == version) { found = 1; next }
  }
  found {
    if ($0 ~ /^[[:space:]]*$/) { blank++; next }
    if (printed) { while (blank-- > 0) print "" }
    blank = 0
    print
    printed = 1
  }
  END { if (!printed) exit 1 }
' "$file"
