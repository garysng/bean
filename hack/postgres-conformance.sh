#!/usr/bin/env bash
# Run the store conformance suite against a real Postgres.
#
# The dialect layer and the compile-time interface assertions between them show that the
# statements are rewritten and the methods exist. Neither can show that a reference count
# survives concurrent acquires on a different engine, or that Reserve refuses to oversell
# when the row locks behave differently. Only running the requirements does that, which is
# why postgres_conformance_test.go skips loudly instead of reporting a pass earned by
# SQLite.
#
# Brings up a throwaway container, runs the suite, and removes it. Nothing outside the
# container is touched: a fixed name so a leftover from an interrupted run is reused rather
# than duplicated, and a high port so it cannot collide with a Postgres someone is using.

set -euo pipefail

NAME="${BEAN_PG_CONTAINER:-bean-store-conformance}"
PORT="${BEAN_PG_PORT:-55432}"
PASSWORD="bean-conformance-local-only"
IMAGE="${BEAN_PG_IMAGE:-postgres:16-alpine}"

cd "$(dirname "$0")/.."

# Only containers this script created are removed, matched by the exact name. A --filter
# on a name pattern would be enough to catch someone else's container on a shared host.
cleanup() {
  if [ "${KEEP:-0}" = "1" ]; then
    echo "KEEP=1, leaving $NAME on port $PORT"
    return
  fi
  docker rm -f "$NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

if docker inspect "$NAME" >/dev/null 2>&1; then
  echo "removing leftover container $NAME"
  docker rm -f "$NAME" >/dev/null
fi

echo "starting $IMAGE as $NAME on 127.0.0.1:$PORT"
# Bound to 127.0.0.1 rather than 0.0.0.0: a throwaway password on a host that runs other
# people's workloads should not be reachable from the network.
docker run -d --name "$NAME" \
  -e POSTGRES_PASSWORD="$PASSWORD" \
  -e POSTGRES_DB=bean \
  -p "127.0.0.1:$PORT:5432" \
  "$IMAGE" >/dev/null

# pg_isready rather than a fixed sleep, and run inside the container so this does not need
# a psql on the host. Postgres starts, runs its init scripts, then restarts -- so a single
# successful probe can be the pre-restart instance. Requiring consecutive successes avoids
# connecting to a server that is about to go away.
echo -n "waiting for postgres"
ready=0
for _ in $(seq 1 60); do
  if docker exec "$NAME" pg_isready -q -U postgres -d bean 2>/dev/null; then
    ready=$((ready + 1))
    if [ "$ready" -ge 3 ]; then
      break
    fi
  else
    ready=0
  fi
  echo -n .
  sleep 1
done
echo
if [ "$ready" -lt 3 ]; then
  echo "postgres did not become ready; container logs follow" >&2
  docker logs "$NAME" 2>&1 | tail -30 >&2
  exit 1
fi

export BEAN_TEST_POSTGRES_DSN="postgres://postgres:$PASSWORD@127.0.0.1:$PORT/bean?sslmode=disable"

# -count=1 defeats the test cache. A cached pass here would be indistinguishable from a
# pass against a database that is no longer running, which is exactly the false green this
# script exists to avoid.
echo "running conformance suite against postgres"
go test ./internal/control/store/ -run Postgres -count=1 -v 2>&1 | tail -60
