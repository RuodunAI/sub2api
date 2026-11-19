#!/bin/sh
# next-check-plugin-ui.sh [<ui dir>...]
#
# The gate of next/'s CI for plugin native UIs. For every given directory, or
# when none is given for every next/plugins/<name>/ui/native with a
# package.json (discovered, so a new UI project needs no change here or in the
# workflow), it runs
#
#   npm ci
#   npm run typecheck
#   npm test
#   npm run build      (the dist/entry.js the image packs, see build-ui.sh)
#
# typecheck and test use the console's vue-tsc and vitest, so next/web must be
# installed first (`npm ci` in next/web). Run from the repository root; usable
# locally: `sh .github/next-ci/next-check-plugin-ui.sh next/plugins/guard/ui/native`.
# Every project is checked; the failing ones are listed at the end.
set -u

if [ $# -eq 0 ]; then
  for pkg in next/plugins/*/ui/native/package.json; do
    [ -f "$pkg" ] && set -- "$@" "${pkg%/package.json}"
  done
fi
if [ $# -eq 0 ]; then
  echo "::error::no plugin UI projects found under next/plugins/*/ui/native" >&2
  exit 1
fi

# Runs the steps in order and stops at the first failing one, naming it. The
# steps are chained explicitly: set -e has no effect inside the condition of
# the caller's if.
check() {
  cd "$1" || return
  for step in 'npm ci --no-audit --no-fund' 'npm run typecheck' 'npm test' 'npm run build'; do
    $step || { echo "::error::$1: $step failed" >&2; return 1; }
  done
}

failed=''
for dir in "$@"; do
  echo "::group::$dir"
  if (check "$dir"); then
    echo "$dir: ok"
  else
    failed="$failed $dir"
  fi
  echo "::endgroup::"
done
echo "checked $# plugin UI project(s)"
if [ -n "$failed" ]; then
  echo "::error::failing plugin UI projects:$failed" >&2
  exit 1
fi
