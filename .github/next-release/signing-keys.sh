#!/bin/sh
# signing-keys.sh <dir>
#
# Writes the plugin signing key of next-release.yml to <dir>/plugin.key (mode
# 0600) and its key ID to <dir>/plugin.keyid. Only core releases need it: it
# re-signs the builtin plugin packages (release manifests are not signed).
# The values come from the step's environment, which the workflow fills only
# in the protected `release` environment:
#
#   SUB2API_PLUGIN_SIGNING_KEY (secret), SUB2API_PLUGIN_KEY_ID (variable)
#
# Nothing is printed from the values.
set -eu
dir=$1
umask 077
mkdir -p "$dir"
missing=
if [ -z "${SUB2API_PLUGIN_SIGNING_KEY:-}" ]; then
  missing="$missing secret SUB2API_PLUGIN_SIGNING_KEY,"
fi
if ! printf '%s' "${SUB2API_PLUGIN_KEY_ID:-}" | grep -Eqx '[A-Za-z0-9][A-Za-z0-9._-]{0,99}'; then
  missing="$missing variable SUB2API_PLUGIN_KEY_ID (letters, digits, . _ -),"
fi
if [ -n "$missing" ]; then
  echo "::error::builtin plugin signing is not configured:${missing%,}. Create the GitHub environment 'release' (Settings > Environments) with them; the key is the one that signs the builtin plugins of the running core (sub2api-plugin keygen for a new deployment). See next/deploy/gateway/github/README.md." >&2
  exit 1
fi
printf '%s\n' "$SUB2API_PLUGIN_SIGNING_KEY" > "$dir/plugin.key"
printf '%s\n' "$SUB2API_PLUGIN_KEY_ID" > "$dir/plugin.keyid"
