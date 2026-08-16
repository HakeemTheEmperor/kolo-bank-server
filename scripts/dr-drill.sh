#!/usr/bin/env bash
# DR drill (docs/dr-runbook.md): backup -> restore into a scratch database ->
# validate row counts + a content checksum -> fail over via database rename
# and an api restart -> prove the API is genuinely live against the
# restored copy with one real read and one real write -> clean up -> report
# the measured RTO. Run against an idle stack (no concurrent load), so the
# RPO=0 assumption in the runbook actually holds.
set -euo pipefail

COMPOSE="docker compose"
ADMIN_KEY="${ADMIN_API_KEY:-dev-admin-key}"
BACKUP_FILE="$(mktemp -t kolo_dr_drill.XXXXXX)"
RESTORE_DB="kolo_bank_drill"
LIVE_DB="kolo_bank"
PRE_DRILL_DB="kolo_bank_pre_drill"

# swap_databases renames FROM -> a temporary holding name, TO -> FROM's old
# name, stopping the api container first (Postgres refuses to rename a
# database with active connections — the api's pgxpool holds exactly that)
# and starting it again afterward.
swap_databases() {
  local from="$1" to="$2"
  $COMPOSE stop api >/dev/null
  $COMPOSE exec -T postgres psql -U kolo -d postgres -c "
    SELECT pg_terminate_backend(pid) FROM pg_stat_activity
    WHERE datname IN ('$from', '$to') AND pid != pg_backend_pid();" >/dev/null
  $COMPOSE exec -T postgres psql -U kolo -d postgres -c "ALTER DATABASE $from RENAME TO ${from}_swap_tmp;"
  $COMPOSE exec -T postgres psql -U kolo -d postgres -c "ALTER DATABASE $to RENAME TO $from;"
  $COMPOSE exec -T postgres psql -U kolo -d postgres -c "ALTER DATABASE ${from}_swap_tmp RENAME TO $to;"
  $COMPOSE start api >/dev/null
  until curl -sf http://localhost:8080/healthz >/dev/null 2>&1; do sleep 1; done
}

cleanup() {
  echo "== cleanup =="
  if $COMPOSE exec -T postgres psql -U kolo -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname = '$PRE_DRILL_DB'" | grep -q 1; then
    echo "swapping back to the original database..."
    swap_databases "$LIVE_DB" "$PRE_DRILL_DB" >/dev/null
    # After this, LIVE_DB is the original again and PRE_DRILL_DB now holds
    # what was the restored copy — rename that back to the scratch name.
    $COMPOSE exec -T postgres psql -U kolo -d postgres -c "ALTER DATABASE $PRE_DRILL_DB RENAME TO $RESTORE_DB;" >/dev/null 2>&1 || true
  fi
  $COMPOSE exec -T postgres psql -U kolo -d postgres -c "DROP DATABASE IF EXISTS $RESTORE_DB;" >/dev/null || true
  rm -f "$BACKUP_FILE"
}
trap cleanup EXIT

echo "== DR drill started: $(date -Iseconds) =="
start=$(date +%s)

echo "== 1. backup =="
$COMPOSE exec -T postgres pg_dump -U kolo -Fc "$LIVE_DB" > "$BACKUP_FILE"

echo "== 2. capture pre-restore state =="
before_counts=$($COMPOSE exec -T postgres psql -U kolo -d "$LIVE_DB" -tAc "
  SELECT 'accounts:' || count(*) FROM accounts
  UNION ALL SELECT 'ledger_entries:' || count(*) FROM ledger_entries
  UNION ALL SELECT 'ledger_transactions:' || count(*) FROM ledger_transactions
  UNION ALL SELECT 'holds:' || count(*) FROM holds
  ORDER BY 1;")
before_checksum=$($COMPOSE exec -T postgres psql -U kolo -d "$LIVE_DB" -tAc "
  SELECT md5(coalesce(string_agg(id::text || amount_minor::text, '' ORDER BY id), ''))
  FROM ledger_entries;")

restore_start=$(date +%s)

echo "== 3. restore into scratch database =="
$COMPOSE exec -T postgres psql -U kolo -d postgres -c "DROP DATABASE IF EXISTS $RESTORE_DB;" >/dev/null
$COMPOSE exec -T postgres psql -U kolo -d postgres -c "CREATE DATABASE $RESTORE_DB;" >/dev/null
cat "$BACKUP_FILE" | $COMPOSE exec -T postgres pg_restore -U kolo -d "$RESTORE_DB" --no-owner

restore_end=$(date +%s)

echo "== 4. validate =="
after_counts=$($COMPOSE exec -T postgres psql -U kolo -d "$RESTORE_DB" -tAc "
  SELECT 'accounts:' || count(*) FROM accounts
  UNION ALL SELECT 'ledger_entries:' || count(*) FROM ledger_entries
  UNION ALL SELECT 'ledger_transactions:' || count(*) FROM ledger_transactions
  UNION ALL SELECT 'holds:' || count(*) FROM holds
  ORDER BY 1;")
after_checksum=$($COMPOSE exec -T postgres psql -U kolo -d "$RESTORE_DB" -tAc "
  SELECT md5(coalesce(string_agg(id::text || amount_minor::text, '' ORDER BY id), ''))
  FROM ledger_entries;")

if [ "$before_counts" != "$after_counts" ]; then
  echo "ROW COUNT MISMATCH"
  echo "before: $before_counts"
  echo "after:  $after_counts"
  exit 1
fi
echo "row counts match"

if [ "$before_checksum" != "$after_checksum" ]; then
  echo "CHECKSUM MISMATCH: before=$before_checksum after=$after_checksum"
  exit 1
fi
echo "checksum matches: $after_checksum"

echo "== 5. failover: swap in the restored copy and restart the api =="
failover_start=$(date +%s)
# swap_databases(from, to): from="$LIVE_DB" ends up holding what "$to"
# ($RESTORE_DB) held, and the original $LIVE_DB content ends up parked
# under the $to name — i.e. after this, kolo_bank IS the restored copy,
# and kolo_bank_drill holds what kolo_bank used to be. We immediately
# rename that parked copy to PRE_DRILL_DB so cleanup knows where to find
# it (see the cleanup function above).
swap_databases "$LIVE_DB" "$RESTORE_DB"
$COMPOSE exec -T postgres psql -U kolo -d postgres -c "ALTER DATABASE $RESTORE_DB RENAME TO $PRE_DRILL_DB;"

echo "== 6. prove the restored database is genuinely live =="
read_status=$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $ADMIN_KEY" http://localhost:8080/v1/admin/resilience)
if [ "$read_status" != "200" ]; then
  echo "post-failover read failed: status=$read_status"
  exit 1
fi
echo "live read OK (200)"

write_status=$(curl -s -o /dev/null -w "%{http_code}" -X PUT \
  -H "Authorization: Bearer $ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"enabled":true,"updated_by":"dr-drill"}' \
  "http://localhost:8080/v1/admin/resilience/kill-switches/feature/dr-drill-proof")
if [ "$write_status" != "200" ]; then
  echo "post-failover write failed: status=$write_status"
  exit 1
fi
echo "live write OK (200)"

failover_end=$(date +%s)
end=$(date +%s)

echo "== summary =="
echo "backup+restore+validate: $((restore_end - start))s"
echo "restore only:            $((restore_end - restore_start))s"
echo "failover (rename+restart+ready+read+write): $((failover_end - failover_start))s"
echo "RTO (restore-initiated -> first live read+write): $((failover_end - restore_start))s"
echo "total drill time: $((end - start))s"
echo "== DR drill passed =="
