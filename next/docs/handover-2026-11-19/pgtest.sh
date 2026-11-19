#!/bin/bash
# Usage: pgtest.sh <worktree> <dir-under-next> <cmd...>; runs in golang image with a temp PG + redis (name prefix from PGTEST_PREFIX, default pgt), removed after.
wt="$1"; dir="$2"; shift 2
args=$(printf '%q ' "$@")
N=${PGTEST_PREFIX:-pgt}-$$-$RANDOM
sg docker -c "docker network create $N >/dev/null && docker run -d --rm --name $N-pg --network $N -e POSTGRES_PASSWORD=tmpdb --tmpfs /var/lib/postgresql/data postgres:17-alpine >/dev/null && docker run -d --rm --name $N-redis --network $N redis:7-alpine >/dev/null"
for i in $(seq 60); do sg docker -c "docker exec $N-pg pg_isready -U postgres" >/dev/null 2>&1 && break; sleep 1; done
sleep 2
sg docker -c "docker run --rm --network $N -u 1001:1001 -e HOME=/home/cvp -e GOPATH=/home/cvp/go -e GOCACHE=/home/cvp/.cache/go-build -e GOFLAGS=-buildvcs=false -e TEST_DATABASE_URL='postgres://postgres:tmpdb@$N-pg:5432/postgres?sslmode=disable' -e TEST_REDIS_URL=redis://$N-redis:6379/0 -v /home/cvp/go:/home/cvp/go -v /home/cvp/.cache/go-build:/home/cvp/.cache/go-build -v $wt:$wt -w $wt/next/$dir golang:1.27-bookworm $args"
rc=$?
sg docker -c "docker rm -f $N-pg $N-redis >/dev/null 2>&1; docker network rm $N >/dev/null"
exit $rc
