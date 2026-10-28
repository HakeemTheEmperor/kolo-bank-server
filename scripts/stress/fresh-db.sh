#!/usr/bin/env bash
# Starts (or restarts from scratch) the throwaway Postgres the stress tests
# run against: postgres:16-alpine with default settings, the same image and
# credentials as docker-compose.yml, on host port 5433 so it never touches
# the dev database. Migrations are applied by the tests themselves.
set -euo pipefail
NAME="${PG_CONTAINER:-kolo-stress-pg}"
PORT="${PG_PORT:-5433}"
docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" -e POSTGRES_USER=kolo -e POSTGRES_PASSWORD=kolo -e POSTGRES_DB=kolo_bank \
  -p "$PORT:5432" postgres:16-alpine >/dev/null
for _ in $(seq 1 60); do
  docker exec "$NAME" pg_isready -U kolo -d kolo_bank >/dev/null 2>&1 && break
  sleep 1
done
sleep 2 # the image restarts postgres once after initdb
docker exec "$NAME" pg_isready -U kolo -d kolo_bank
echo "DATABASE_URL=postgres://kolo:kolo@localhost:$PORT/kolo_bank?sslmode=disable"
