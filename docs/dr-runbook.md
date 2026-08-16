# Disaster Recovery Runbook

## Scope and caveat

This runbook covers backup/restore/failover for the **single local docker-compose Postgres instance** this project runs (docs/banking-backend-spec.md §9: single-region to start). It is a real drill against real data — `pg_dump`, `pg_restore`, a database rename, and a container restart all genuinely happen — but it is **not** multi-region replication or a production DR posture. What a real deployment would add on top of everything here:

- **Streaming replication / WAL shipping** to a standby in a second region, so failover doesn't require a manual `pg_dump` at all.
- **Off-host, off-region backup storage** — this drill's dump file lives on the same Docker host as the database it was taken from, which is not a real backup (a single host failure destroys both).
- **Automated failover orchestration** (a proxy or DNS cutover), rather than a human running a script.

Everything below is the honest, useful subset of DR practice that's achievable and worth exercising at this project's scale: prove a backup is restorable, and measure how long recovery actually takes.

## Targets

- **RPO (Recovery Point Objective): 0** for this drill's scope. The dump is taken with no concurrent writes during the drill window (the drill script is run against an idle stack), so the restore is exact by construction — no data loss, by definition of the drill, not by any replication guarantee. A live-traffic production RPO would instead be bounded by streaming-replication lag, which this local setup doesn't have and doesn't claim to measure.
- **RTO (Recovery Time Objective): ≤ 5 minutes**, measured end-to-end from "restore initiated" to "the API is serving real reads and writes against the restored data" — not just how long `pg_restore` itself takes. Five minutes is a reasonable target for a docker-compose-scale demo dataset doing a plain logical restore; it is explicitly not a production SLA (a multi-region setup with a hot standby would target seconds, via failover rather than restore).

## Prerequisites

- The stack is up (`make up`) and reachable at `http://localhost:8080`.
- `docker compose` is available on the host running this runbook.

## Backup procedure

```bash
docker compose exec -T postgres pg_dump -U kolo -Fc kolo_bank > backup.dump
```

`-Fc` (custom format) is required — it's what `pg_restore` in the restore step below consumes, and it supports partial/parallel restore if ever needed at larger scale.

## Restore procedure

Restore into a **scratch database on the same Postgres instance** first, so the drill never touches the live `kolo_bank` database until validation has already passed:

```bash
docker compose exec -T postgres psql -U kolo -d postgres -c "DROP DATABASE IF EXISTS kolo_bank_drill;"
docker compose exec -T postgres psql -U kolo -d postgres -c "CREATE DATABASE kolo_bank_drill;"
cat backup.dump | docker compose exec -T postgres pg_restore -U kolo -d kolo_bank_drill --no-owner
```

## Validation procedure

Two checks, both against the scratch database, both must match the pre-backup numbers exactly:

1. **Row counts** on the core ledger tables (`accounts`, `ledger_entries`, `ledger_transactions`, `holds`).
2. **A content checksum** over `ledger_entries`: `SELECT md5(string_agg(id::text || amount_minor::text, '' ORDER BY id)) FROM ledger_entries;` — row counts alone can't catch a restore that silently dropped and re-added different rows; this can.

## Failover procedure (the step that actually proves RTO)

Proving the *API* can run against the restored copy — not just that `psql` sees matching rows — is the part that actually matters for RTO. This project's `api` container has a single fixed `DATABASE_URL` pointing at the `kolo_bank` database name, so the cleanest way to "fail over" locally is to rename databases inside the Postgres container and restart the API against the (now-renamed) restored copy:

```bash
docker compose exec -T postgres psql -U kolo -d postgres -c "ALTER DATABASE kolo_bank RENAME TO kolo_bank_pre_drill;"
docker compose exec -T postgres psql -U kolo -d postgres -c "ALTER DATABASE kolo_bank_drill RENAME TO kolo_bank;"
docker compose restart api
# poll until ready:
until curl -sf http://localhost:8080/healthz >/dev/null; do sleep 1; done
```

Then issue one real read (e.g. `GET /v1/admin/resilience` with the admin key) and one real write (any idempotent-safe mutating call) to prove the restored database is actually live, not just reachable.

**Elapsed time from "restore initiated" to "first successful post-failover read+write" is the number to report as RTO.**

## Cleanup

Always run this after a drill, whether it passed or not, so the stack is left in its original state:

```bash
docker compose exec -T postgres psql -U kolo -d postgres -c "ALTER DATABASE kolo_bank RENAME TO kolo_bank_drill;"
docker compose exec -T postgres psql -U kolo -d postgres -c "ALTER DATABASE kolo_bank_pre_drill RENAME TO kolo_bank;"
docker compose restart api
docker compose exec -T postgres psql -U kolo -d postgres -c "DROP DATABASE IF EXISTS kolo_bank_drill;"
rm -f backup.dump
```

## Automated version

`scripts/dr-drill.sh` (invoked via `make dr-drill`) runs backup → restore-into-scratch → validate → rename-based failover → poll-until-ready → one live read/write → automatic cleanup → prints total elapsed time as the measured RTO, in one command. Run it against an idle stack (no concurrent load) so the RPO=0 assumption above actually holds.

## Suggested drill cadence

Run `make dr-drill` after any migration that changes the ledger schema, and periodically (e.g. monthly) even with no changes — a backup nobody has ever restored is not a verified backup.
