#!/usr/bin/env bash
# Throughput test for POST /v1/me/transfers (METRICS.md, test 7).
#
# Builds and starts the API on the host against $DATABASE_URL, then for
# each VU level seeds 200 fresh sender/recipient pairs and runs k6 (grafana/k6
# in Docker, host network). Writes one k6 summary JSON per level to $OUT_DIR.
#
#   DATABASE_URL=postgres://kolo:kolo@localhost:5433/kolo_bank?sslmode=disable \
#   LABEL=default-pool LEVELS="10 25 50 100 200" DURATION=30s \
#   bash scripts/stress/run-k6.sh
#
# The API reads its pool size from DATABASE_URL, so the pool comparison is
# done by passing API_DATABASE_URL with &pool_max_conns=N appended.
set -euo pipefail
cd "$(dirname "$0")/../.."

: "${DATABASE_URL:?set DATABASE_URL}"
API_DATABASE_URL="${API_DATABASE_URL:-$DATABASE_URL}"
LABEL="${LABEL:-run}"
LEVELS="${LEVELS:-10 25 50 100 200}"
DURATION="${DURATION:-30s}"
OUT_DIR="${OUT_DIR:-stress-results/k6}/$LABEL"
PORT="${PORT:-18080}"
# k6 v2.3.0, pinned by digest so reruns use the same build.
K6_IMAGE="${K6_IMAGE:-grafana/k6@sha256:3e4186aea62e11a45560bfc51e19eeafe82fc07ce069c78aff44b7ff893d9a5e}"
mkdir -p "$OUT_DIR"

# API_SRC builds the API from another checkout (e.g. a worktree of an
# older commit) while the seeder and k6 script come from this one.
API_SRC="${API_SRC:-.}"
(cd "$API_SRC" && go build -o "$OLDPWD/$OUT_DIR/api" ./cmd/api)
go run -tags stress ./internal/stresstest/k6seed -senders 0 -out /dev/null # apply migrations

# env(1) because the key variable's name contains a dash.
env HTTP_ADDR=":$PORT" DATABASE_URL="$API_DATABASE_URL" LOG_LEVEL=info \
  "KOLO_KEY_ledger-signing=MDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDA=" \
  "$OUT_DIR/api" > "$OUT_DIR/api.log" 2>&1 &
API_PID=$!
trap 'kill $API_PID 2>/dev/null || true' EXIT
ready=""
for _ in $(seq 1 50); do curl -sf "http://localhost:$PORT/readyz" >/dev/null && ready=1 && break; sleep 0.2; done
[ -n "$ready" ] || { echo "api did not become ready; see $OUT_DIR/api.log" >&2; exit 1; }

for vus in $LEVELS; do
  # Fresh senders per level. Every transfer sums the sender's whole
  # history (balance trigger, daily limit, payee check), so reusing
  # senders across levels would mix the load effect with history growth.
  go run -tags stress ./internal/stresstest/k6seed -senders 200 -out "$OUT_DIR/pairs-$vus.json" -migrations db/migrations >/dev/null
  echo "== $LABEL: $vus VUs for $DURATION =="
  docker run --rm --network host --user "$(id -u):$(id -g)" -v "$PWD:/src" -w /src "$K6_IMAGE" run --quiet \
    -e BASE_URL="http://localhost:$PORT" -e PAIRS="/src/$OUT_DIR/pairs-$vus.json" \
    --vus "$vus" --duration "$DURATION" \
    --summary-export "/src/$OUT_DIR/summary-$vus.json" \
    scripts/stress/transfers.js | grep -E "http_reqs|http_req_duration|http_req_failed|checks" || true
done
