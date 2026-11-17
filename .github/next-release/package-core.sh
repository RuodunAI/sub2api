#!/bin/sh
# package-core.sh <build> <keys> <out>
#
# Packages a core release from the core-build job of next-release.yml.
# <build>/core-stage.tar holds:
#   bin-<arch>/   sub2api and sub2api-plugin of each linux architecture
#   builtin/      the builtin plugin packages and install-only.txt, as
#                 deploy/docker/build-go.sh made them (throwaway signature)
# <keys> holds plugin.{key,keyid} (signing-keys.sh).
#
# The builtin packages are re-signed with the plugin key, builtin/trust.pub
# and trust.keyid name that key, and each architecture is staged as
# package-release.sh expects (bin/ + builtin/) and packed into one manifest.
# <out> receives next-core-manifest.json (unsigned; its SHA-256 is the release
# digest) and <bundle_digest>.tar.gz per architecture.
#
# Environment: VERSION, TAG, SOURCE_COMMIT, BUILD_ID, SCHEMA (schema-contract
# of the new core); SCHEMA_BEFORE and PREV_TRUST_SHA256 from release-meta.sh
# (PREV_TRUST_SHA256 is empty for the first core release). sub2api-release and
# sub2api-plugin must be on PATH, built from source: nothing in <build> is
# executed here, next to the plugin key.
set -eu
build=$1
keys=$2
out=$3
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
tar -C "$stage" -xf "$build/core-stage.tar"

builtin=$stage/builtin
plugin_key_id=$(cat "$keys/plugin.keyid")
sub2api-release pubkey --key "$keys/plugin.key" > "$builtin/trust.pub"
printf '%s\n' "$plugin_key_id" > "$builtin/trust.keyid"
trust=$(sha256sum "$builtin/trust.pub" | cut -d ' ' -f1)
if [ -n "${PREV_TRUST_SHA256:-}" ] && [ "$trust" != "$PREV_TRUST_SHA256" ]; then
  echo "::error::SUB2API_PLUGIN_SIGNING_KEY is not the key whose public half the previous core release ships as builtin/trust.pub; installed plugins would no longer verify (prepare-core-release.sh refused this too)" >&2
  exit 1
fi
sub2api-plugin sign --key "$keys/plugin.key" --key-id "$plugin_key_id" "$builtin"/*.s2plugin
sub2api-plugin verify --pub "$builtin/trust.pub" "$builtin"/*.s2plugin

set --
for bin in "$stage"/bin-*; do
  arch=${bin##*/bin-}
  dir=$stage/linux-$arch
  mkdir "$dir"
  cp -a "$bin" "$dir/bin"
  cp -a "$builtin" "$dir/builtin"
  set -- "$@" --platform "linux/$arch=$dir"
done
digests=$(sub2api-release pack "$@" --out "$out" \
  --release-id "$TAG" --core-version "$VERSION" --source-commit "$SOURCE_COMMIT" --build-id "$BUILD_ID" \
  --schema "$SCHEMA" --schema-before "$SCHEMA_BEFORE" --runtime-abi linux-static-v1)
printf '%s\n' "$digests"
manifest=$(printf '%s\n' "$digests" | sed -n 's/^manifest_digest=//p')
mv "$out/$manifest.json" "$out/next-core-manifest.json"
