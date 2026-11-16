#!/bin/sh
# changelog-notes.sh <line> <version> <changelog> <out dir>
#
# Writes the release title and notes of one version, taken from CHANGELOG.md,
# to <out dir>/title.txt and <out dir>/notes.md. Fails when there is none.
#
# Heading format: the release is the first level-3 heading ("### ") whose text
# names "核心 X.Y.Z" (line core) or "网关 X.Y.Z" (line gateway), the version not
# followed by another digit or ".digit"; one heading may name both lines, e.g.
#   ### 核心 0.1.290 + 插件 + 网关 0.1.134：…（`e3906cc68`，…）
# The title is that heading's text, the notes everything up to the next
# heading of level 1 to 3 ("####" subsections are included).
set -eu
[ "$#" = 4 ] || { echo 'usage: changelog-notes.sh <core|gateway> <version> <changelog> <out dir>' >&2; exit 2; }
case $1 in
  core) label=核心 ;;
  gateway) label=网关 ;;
  *) echo "unknown line $1" >&2; exit 2 ;;
esac
mkdir -p "$4"
awk -v needle="$label $2" -v title="$4/title.txt" -v notes="$4/notes.md" '
function names(s,   i, rest) {
  while ((i = index(s, needle)) > 0) {
    rest = substr(s, i + length(needle))
    if (rest !~ /^(\.?[0-9])/) return 1
    s = rest
  }
  return 0
}
/^#{1,3} / {
  if (found) exit
  if (/^### / && names($0)) {
    found = 1
    print substr($0, 5) > title
    printf "" > notes
    next
  }
}
found { print > notes }
END { exit found ? 0 : 1 }
' "$3" || {
  echo "::error::$3 has no \"### \" heading naming \"$label $2\"; add the release section before tagging" >&2
  exit 1
}
