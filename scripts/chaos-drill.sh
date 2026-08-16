#!/usr/bin/env bash
# Chaos drill (docs/banking-backend-spec.md §Phase 10): runs a load test in
# the background, kills the api and postgres containers mid-load, restarts
# them, and asserts the system recovers with no permanently stuck holds and
# the double-entry invariant intact. Pragmatic and scriptable, not a new
# framework.
set -uo pipefail

COMPOSE="docker compose"
GO_IMAGE="golang:1.25-bookworm"
NETWORK="kolo-bank-server_default"
DEV_DB_URL="postgres://kolo:kolo@postgres:5432/kolo_bank?sslmode=disable"
# `pwd -W` (git-bash/MSYS on Windows) prints the Windows-native path so
# `docker run -v` gets a form Docker can actually resolve; on real
# Unix/WSL, `pwd -W` doesn't exist and this falls back to plain `pwd`.
# MSYS_NO_PATHCONV=1 additionally stops MSYS from mangling the in-container
# path arguments below (-w /src) — a no-op on real Unix/WSL.
HOSTDIR="$(pwd -W 2>/dev/null || pwd)"

echo "== chaos drill started: $(date -Iseconds) =="

echo "== starting background load =="
MSYS_NO_PATHCONV=1 docker run --rm --network "$NETWORK" -v "$HOSTDIR:/src" -w /src \
  -e DATABASE_URL="$DEV_DB_URL" \
  "$GO_IMAGE" go run ./cmd/loadtest -base-url=http://api:8080 -database-url="$DEV_DB_URL" -duration=90s -concurrency=10 -seed-accounts=10 &
LOADPID=$!

sleep 20
echo "== killing api container =="
$COMPOSE kill api
sleep 5
$COMPOSE start api
echo "waiting for api to recover..."
until curl -sf http://localhost:8080/healthz >/dev/null 2>&1; do sleep 1; done
echo "api recovered"

sleep 20
echo "== stopping postgres container =="
$COMPOSE stop postgres
sleep 5
$COMPOSE start postgres
echo "waiting for postgres to recover..."
until $COMPOSE exec -T postgres pg_isready -U kolo -d kolo_bank >/dev/null 2>&1; do sleep 1; done
echo "postgres recovered"
echo "waiting for api to reconnect..."
until curl -sf http://localhost:8080/healthz >/dev/null 2>&1; do sleep 1; done
echo "api reconnected"

echo "== waiting for background load to finish =="
wait $LOADPID
LOAD_EXIT=$?
# The load test's own latency/error-rate thresholds are tuned for a stable
# run — requests genuinely fail during the kill windows above by design,
# so a nonzero loadtest exit here is expected and NOT a chaos-drill
# failure on its own. What matters is that the system recovered cleanly
# afterward, which the two checks below verify directly.
echo "background load exit code: $LOAD_EXIT (informational — see note above)"

echo "== checking for stuck state =="
# stuckTimeout mirrors internal/scheduler and internal/externalpayments'
# own stuckTimeout constant (2 minutes) — a chaos-induced stuck transaction
# should self-heal via ResolveStuck within that window; poll a bit past it.
STUCK=-1
for i in $(seq 1 30); do
  STUCK=$($COMPOSE exec -T postgres psql -U kolo -d kolo_bank -tAc "
    SELECT count(*) FROM ledger_transactions
    WHERE state = 'pending' AND created_at < now() - interval '2 minutes';")
  STUCK=$(echo "$STUCK" | tr -d '[:space:]')
  if [ "$STUCK" = "0" ]; then
    break
  fi
  sleep 5
done
echo "stuck pending transactions past stuckTimeout: $STUCK"

echo "== checking the double-entry invariant =="
UNBALANCED=$($COMPOSE exec -T postgres psql -U kolo -d kolo_bank -tAc "
  SELECT count(*) FROM (
    SELECT transaction_id FROM ledger_entries GROUP BY transaction_id HAVING sum(amount_minor) != 0
  ) t;")
UNBALANCED=$(echo "$UNBALANCED" | tr -d '[:space:]')
echo "unbalanced transactions: $UNBALANCED"

echo "== summary =="
FAIL=0
if [ "$STUCK" != "0" ]; then
  echo "FAIL: $STUCK transaction(s) still stuck past stuckTimeout"
  FAIL=1
fi
if [ "$UNBALANCED" != "0" ]; then
  echo "FAIL: $UNBALANCED transaction(s) violate the double-entry invariant"
  FAIL=1
fi

if [ "$FAIL" = "0" ]; then
  echo "== chaos drill passed: system recovered cleanly from container kills =="
  exit 0
fi
echo "== chaos drill FAILED =="
exit 1
