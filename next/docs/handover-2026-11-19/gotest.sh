#!/bin/bash
# Usage: gotest.sh <worktree-root> <dir-under-next> <go args...>
# Runs the Go toolchain in the golang:1.27-bookworm image as cvp, with cvp's module and build caches.
wt="$1"; dir="$2"; shift 2
args=$(printf '%q ' "$@")
exec sg docker -c "docker run --rm -u 1001:1001 -e HOME=/home/cvp -e GOPATH=/home/cvp/go -e GOCACHE=/home/cvp/.cache/go-build -e GOFLAGS=-buildvcs=false -v /home/cvp/go:/home/cvp/go -v /home/cvp/.cache/go-build:/home/cvp/.cache/go-build -v $wt:$wt -w $wt/next/$dir golang:1.27-bookworm $args"
