#!/usr/bin/env bash
# Starts (or restarts from scratch) the throwaway Postgres the stress tests
# run against: postgres:16-alpine with default settings, the same image and
# credentials as docker-compose.yml, on port 5433 (host network) so it never
# touches the dev database. Migrations are applied by the tests themselves.
set -euo pipefail
NAME="${PG_CONTAINER:-kolo-stress-pg}"
PORT="${PG_PORT:-5433}"
docker rm -f "$NAME" >/dev/null 2>&1 || true
# postgres:16-alpine (16.15) as pulled for METRICS.md, pinned by digest.
IMAGE="${PG_IMAGE:-postgres@sha256:0cefaabd882e42bcb2a2f3dc56d3ce4770560ba62fd7f197f1a2e008fd5731ab}"
# Host networking: with -p, every packet between the API and Postgres goes
# through docker-proxy, which used ~60% of a core under load and skewed the
# throughput numbers. EXTRA_PG_ARGS passes extra server settings
# (e.g. "-c shared_preload_libraries=pg_stat_statements" for diagnosis).
docker run -d --name "$NAME" --network host -e POSTGRES_USER=kolo -e POSTGRES_PASSWORD=kolo -e POSTGRES_DB=kolo_bank \
  "$IMAGE" -c port="$PORT" ${EXTRA_PG_ARGS:-} >/dev/null
for _ in $(seq 1 60); do
  docker exec "$NAME" pg_isready -p "$PORT" -U kolo -d kolo_bank >/dev/null 2>&1 && break
  sleep 1
done
sleep 2 # the image restarts postgres once after initdb
docker exec "$NAME" pg_isready -p "$PORT" -U kolo -d kolo_bank
echo "DATABASE_URL=postgres://kolo:kolo@localhost:$PORT/kolo_bank?sslmode=disable"
