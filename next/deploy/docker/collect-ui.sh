#!/bin/sh
# collect-ui.sh <ctx> <dst>
# Copies only what the frontend build needs, keeping the repository layout, so
# the npm stage cache is not invalidated by Go changes. Dockerfile.dockerignore
# keeps node_modules and earlier UI build output out of the context.
set -eu
CTX=$1
DST=$2
mkdir -p "$DST/server/web" "$DST/deploy/docker"
cp -a "$CTX/deploy/docker/build-ui.sh" "$DST/deploy/docker/"
cp -a "$CTX/server/web/dist" "$DST/server/web/dist"
# Data files outside web/ that the console imports (vue-tsc -b also checks specs).
for f in sdk/permvectors/vectors.json; do
  mkdir -p "$DST/$(dirname "$f")"
  cp -a "$CTX/$f" "$DST/$f"
done
cp -a "$CTX/web" "$DST/web"
for ui in "$CTX"/plugins/*/ui; do
  [ -d "$ui" ] || continue
  rel=${ui#"$CTX"/}
  mkdir -p "$DST/$(dirname "$rel")"
  cp -a "$ui" "$DST/$rel"
done
