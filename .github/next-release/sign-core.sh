#!/bin/sh
# sign-core.sh <build> <keys> <out>
#
# Signs a core release from the core-build job of next-release.yml.
# <build>/core-stage.tar holds:
#   bin-<arch>/   sub2api and sub2api-plugin of each linux architecture
#   builtin/      the builtin plugin packages and install-only.txt, as
#                 deploy/docker/build-go.sh made them (throwaway signature)
# <keys> holds release.{key,keyid} and plugin.{key,keyid} (signing-keys.sh).
#
# The builtin packages are re-signed with the plugin key, builtin/trust.pub
# and trust.keyid name that key, and each architecture is staged as
# package-release.sh expects (bin/ + builtin/) and packed into one manifest.
# <out> receives next-core-manifest.json and <bundle_digest>.tar.gz per
# architecture.
#
# Environment: VERSION, TAG, SOURCE_COMMIT, BUILD_ID, SCHEMA (schema-contract
# of the new core); PREV_SCHEMA and PREV_TRUST_SHA256 from release-meta.sh,
# empty for the first core release. sub2api-release must be on PATH.
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
case $(uname -m) in
  x86_64) host=amd64 ;;
  aarch64) host=arm64 ;;
  *) echo "unsupported build host $(uname -m)" >&2; exit 1 ;;
esac
cli=$stage/bin-$host/sub2api-plugin
"$cli" sign --key "$keys/plugin.key" --key-id "$plugin_key_id" "$builtin"/*.s2plugin
"$cli" verify --pub "$builtin/trust.pub" "$builtin"/*.s2plugin

set --
for bin in "$stage"/bin-*; do
  arch=${bin##*/bin-}
  dir=$stage/linux-$arch
  mkdir "$dir"
  cp -a "$bin" "$dir/bin"
  cp -a "$builtin" "$dir/builtin"
  set -- "$@" --platform "linux/$arch=$dir"
done
digests=$(sub2api-release pack "$@" --out "$out" --key "$keys/release.key" --key-id "$(cat "$keys/release.keyid")" \
  --release-id "$TAG" --core-version "$VERSION" --source-commit "$SOURCE_COMMIT" --build-id "$BUILD_ID" \
  --schema "$SCHEMA" --schema-before "${PREV_SCHEMA:-$SCHEMA}" --runtime-abi linux-static-v1)
printf '%s\n' "$digests"
manifest=$(printf '%s\n' "$digests" | sed -n 's/^manifest_digest=//p')
mv "$out/$manifest.json" "$out/next-core-manifest.json"
