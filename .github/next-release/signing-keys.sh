#!/bin/sh
# signing-keys.sh <dir> release|plugin...
#
# Writes each named signing key of next-release.yml to <dir>/<name>.key (mode
# 0600) and its key ID to <dir>/<name>.keyid. The values come from the step's
# environment, which the workflow fills only in the protected `release`
# environment:
#
#   release  SUB2API_RELEASE_SIGNING_KEY (secret), SUB2API_RELEASE_KEY_ID (variable)
#   plugin   SUB2API_PLUGIN_SIGNING_KEY  (secret), SUB2API_PLUGIN_KEY_ID  (variable)
#
# Nothing is printed from the values.
set -eu
dir=$1
shift
umask 077
mkdir -p "$dir"
missing=
for name in "$@"; do
  case $name in
    release) key=${SUB2API_RELEASE_SIGNING_KEY:-} id=${SUB2API_RELEASE_KEY_ID:-} secret=SUB2API_RELEASE_SIGNING_KEY var=SUB2API_RELEASE_KEY_ID ;;
    plugin) key=${SUB2API_PLUGIN_SIGNING_KEY:-} id=${SUB2API_PLUGIN_KEY_ID:-} secret=SUB2API_PLUGIN_SIGNING_KEY var=SUB2API_PLUGIN_KEY_ID ;;
    *) echo "unknown key $name" >&2; exit 2 ;;
  esac
  if [ -z "$key" ]; then
    missing="$missing secret $secret,"
  fi
  if ! printf '%s' "$id" | grep -Eqx '[A-Za-z0-9][A-Za-z0-9._-]{0,99}'; then
    missing="$missing variable $var (letters, digits, . _ -),"
  fi
  printf '%s\n' "$key" > "$dir/$name.key"
  printf '%s\n' "$id" > "$dir/$name.keyid"
done
if [ -n "$missing" ]; then
  echo "::error::release signing is not configured:${missing%,}. Create the GitHub environment 'release' (Settings > Environments) with these secrets and variables; generate keys with 'sub2api-release keygen' (release) and 'sub2api-plugin keygen' (plugin) and commit the release public key as next/gateway/internal/release/trusted/<key id>.pub. See next/deploy/gateway/github/README.md." >&2
  exit 1
fi
