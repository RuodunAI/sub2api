#!/bin/sh
# package-gateway.sh <build> <out>
#
# Packages a gateway release from the gateway-build job of next-release.yml:
# <build>/sub2api-gateway_<VERSION>_linux_<arch>.tar.gz per architecture.
# <out> receives next-gateway-manifest.json (unsigned; its SHA-256 is the
# release digest) and the archives.
#
# Environment: VERSION, TAG, SOURCE_COMMIT, BUILD_ID, IMAGE (the pushed
# multi-arch image as REF@sha256:DIGEST; empty in a dry run).
# sub2api-release must be on PATH.
set -eu
build=$1
out=$2
set --
for archive in "$build"/sub2api-gateway_"$VERSION"_linux_*.tar.gz; do
  arch=${archive##*_linux_}
  set -- "$@" --archive "linux/${arch%.tar.gz}=$archive"
done
if [ -n "${IMAGE:-}" ]; then
  set -- "$@" --image "$IMAGE"
fi
sub2api-release pack-gateway "$@" --out "$out" \
  --version "$VERSION" --release-id "$TAG" --source-commit "$SOURCE_COMMIT" --build-id "$BUILD_ID"
cp "$build"/sub2api-gateway_"$VERSION"_linux_*.tar.gz "$out/"
