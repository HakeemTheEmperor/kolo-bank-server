#!/usr/bin/env bash
# Runs every stress test (METRICS.md, tests 1-6) and appends results as
# JSON lines to $STRESS_RESULTS. Expects a fresh database (fresh-db.sh).
#
#   DATABASE_URL=postgres://kolo:kolo@localhost:5433/kolo_bank?sslmode=disable \
#   STRESS_RESULTS=stress-results/run.jsonl bash scripts/stress/run-go-tests.sh
set -euo pipefail
cd "$(dirname "$0")/../.."
: "${DATABASE_URL:?set DATABASE_URL}"
: "${STRESS_RESULTS:?set STRESS_RESULTS}"
mkdir -p "$(dirname "$STRESS_RESULTS")"
# go test runs inside the package directory, so the path must be absolute.
STRESS_RESULTS="$(cd "$(dirname "$STRESS_RESULTS")" && pwd)/$(basename "$STRESS_RESULTS")"
export STRESS_RESULTS STRESS_RUNS="${STRESS_RUNS:-5}" STRESS_CRASH_RUNS="${STRESS_CRASH_RUNS:-3}"

# Default build: every protection as shipped (plus the test-side "off"
# variants that don't need a different build).
go test -tags stress -count=1 -timeout 120m -v ./internal/stresstest \
  -run 'TestConcurrentDebits|TestIdempotency|TestHold|TestDoubleEntry|TestCrash|TestWebhookRetries' 2>&1 \
  | tee "${STRESS_RESULTS%.jsonl}.log" | grep -E '^(=== RUN|--- |ok|FAIL|PASS)|RESULT' | sed 's/,"at":.*//' | cut -c1-300

# stress_nolock build: the ledger's hold row lock removed, for the
# capture/release race comparison only.
go test -tags "stress stress_nolock" -count=1 -timeout 30m -v ./internal/stresstest \
  -run 'TestHoldCaptureReleaseRace' 2>&1 \
  | tee -a "${STRESS_RESULTS%.jsonl}.log" | grep -E '^(--- |ok|FAIL)|RESULT' | sed 's/,"at":.*//' | cut -c1-300
