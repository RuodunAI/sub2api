#!/bin/sh
# release-meta.sh <tag>
#
# Validates a release tag of next-release.yml and appends the release facts to
# $GITHUB_OUTPUT (or prints them):
#
#   line      core | gateway                 (tag core-v<semver> / gateway-v<semver>)
#   version   X.Y.Z, no prefix, pre-release or build suffix
#   prev_tag  newest published stable release of the same line ('' if none)
#   schema_before, prev_trust_sha256
#             core only: schema_after and the builtin/trust.pub digest signed
#             in prev_tag's next-core-manifest.json. Without a prev_tag,
#             schema_before must come from SCHEMA_BEFORE (the schema-contract
#             of the core the first release upgrades) and prev_trust_sha256
#             is ''.
#
# The version must be greater than prev_tag's. Needs GH_TOKEN (contents: read),
# GITHUB_REPOSITORY, gh and jq.
set -eu

fail() {
  echo "::error::$*" >&2
  exit 1
}

tag=${1:-}
case $tag in *[!a-z0-9.-]*) fail "tag '$tag' has characters other than a-z 0-9 . -" ;; esac
semver='(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)'
line=$(printf '%s\n' "$tag" | sed -En "s/^(core|gateway)-v${semver}\$/\\1/p")
[ -n "$line" ] || fail "tag '$tag' is not core-vX.Y.Z or gateway-vX.Y.Z (strict semver, no pre-release)"
version=${tag#"$line"-v}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

gh api --paginate "repos/$GITHUB_REPOSITORY/releases?per_page=100" \
  --jq '.[] | select(.draft == false and .prerelease == false) | .tag_name' > "$tmp/tags" ||
  fail "cannot list the releases of $GITHUB_REPOSITORY"
prev=$(sed -En "s/^${line}-v(${semver})\$/\\1/p" "$tmp/tags" | sort -V | tail -n1)
if [ -n "$prev" ]; then
  newest=$(printf '%s\n%s\n' "$prev" "$version" | sort -V | tail -n1)
  [ "$version" != "$prev" ] && [ "$newest" = "$version" ] ||
    fail "$tag must be greater than the published $line-v$prev"
fi
prev_tag=${prev:+$line-v$prev}

schema_before=${SCHEMA_BEFORE:-}
prev_trust=
if [ "$line" = core ] && [ -z "$prev_tag" ]; then
  printf '%s\n' "$schema_before" | grep -Eqx '[0-9a-f]{64}' ||
    fail "the first core-v release needs schema_before, the schema-contract of the core it upgrades: run this workflow by hand with the tag and schema_before"
elif [ -n "$schema_before" ]; then
  fail "schema_before is only for the first core-v release"
elif [ "$line" = core ]; then
  gh release download "$prev_tag" --repo "$GITHUB_REPOSITORY" --pattern next-core-manifest.json --dir "$tmp"
  payload=$(jq -r .payload "$tmp/next-core-manifest.json" | base64 -d)
  schema_before=$(printf '%s' "$payload" | jq -r '.schema_after // empty')
  prev_trust=$(printf '%s' "$payload" | jq -r '[.platforms[].files[] | select(.path == "builtin/trust.pub") | .sha256] | unique | if length == 1 then .[0] else "" end')
  [ -n "$schema_before" ] || fail "$prev_tag's next-core-manifest.json has no schema_after"
fi

{
  echo "line=$line"
  echo "version=$version"
  echo "prev_tag=$prev_tag"
  echo "schema_before=$schema_before"
  echo "prev_trust_sha256=$prev_trust"
} >> "${GITHUB_OUTPUT:-/dev/stdout}"
echo "release $tag: line $line, version $version, previous ${prev_tag:-none}"
