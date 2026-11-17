#!/bin/sh
# Run on Linux in a trusted build environment after building the core and UI.
# STAGE contains bin/sub2api, optional bin/sub2api-plugin and builtin/.
# Packaging does not upload or deploy anything.
set -eu
if [ "$#" -lt 4 ] || [ "$#" -gt 5 ]; then
  echo 'usage: package-release.sh STAGE OUTPUT RELEASE_ID SOURCE_COMMIT [SOURCE_SCHEMA]' >&2
  exit 2
fi
stage=$1
output=$2
release_id=$3
source_commit=$4
schema=$("$stage/bin/sub2api" schema-contract)
source_schema=${5:-$schema}
core_version=$("$stage/bin/sub2api" version)
[ -n "$schema" ] || { echo 'core did not report a schema contract' >&2; exit 1; }
exec sub2api-release pack --dir "$stage" --out "$output" \
  --release-id "$release_id" --source-commit "$source_commit" \
  --core-version "$core_version" --schema "$schema" --schema-before "$source_schema" --os linux --runtime-abi linux-static-v1
