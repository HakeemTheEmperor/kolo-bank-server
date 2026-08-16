# Load, Soak, and Chaos Testing

## Targets

| Metric | Target |
|---|---|
| Read p99 latency | < 500ms |
| Write (money-movement) p99 latency | < 1s |
| Error rate | < 1% |
| Soak duration | 5 minutes sustained at concurrency 20 |

These are demo-scale targets for a docker-compose-scale deployment (single API instance, single Postgres, no connection pooling tuning beyond defaults) — not a production SLA. They exist so "hit the latency and availability targets" (docs/banking-backend-spec.md §Phase 10) is a script exit code, not eyeballing a log.

## Load/soak testing

`cmd/loadtest` is a standalone Go program (not a `go test` — it drives real HTTP traffic against a running API instance, the same way a real client would). It:

1. Seeds its own test identities directly against Postgres (bypassing onboarding/KYC, the same shortcut `internal/publicapi`'s own tests take) — each with a funded NGN wallet and a logged-in session.
2. Runs a fixed-duration, fixed-concurrency mixed workload: 80% reads (`GET /v1/me/transfers/pending`), 20% writes (`POST /v1/me/transfers`, a small transfer between two seeded accounts, fresh `Idempotency-Key` per request).
3. Reports p50/p95/p99 latency and error rate per scenario, and exits non-zero if either threshold above is breached.

Run it via `make loadtest`, or directly:

```bash
go run ./cmd/loadtest -base-url=http://localhost:8080 -duration=5m -concurrency=20
```

Flags: `-concurrency`, `-duration`, `-seed-accounts`, `-max-error-rate`, `-max-p99-read`, `-max-p99-write`, `-base-url`, `-database-url` (seeding only — never used for the load traffic itself, which goes over real HTTP).

## Chaos testing

`scripts/chaos-drill.sh` (via `make chaos-drill`) runs `cmd/loadtest` in the background and, mid-run, kills and restarts first the `api` container, then the `postgres` container — a real infrastructure failure, not a simulated one. It does **not** gate pass/fail on the load test's own latency/error thresholds (requests genuinely fail during a kill window, by design — that's not a bug). What it does gate on, after everything has restarted and the load has finished:

- **No stuck transactions**: no `ledger_transactions` left `pending` past the existing `stuckTimeout` (2 minutes — the same constant `internal/scheduler` and `internal/externalpayments` already use for their own in-flight resolvers). A chaos-induced stuck transaction should self-heal within that window without any drill-specific intervention.
- **The double-entry invariant holds**: no transaction's `ledger_entries` sum to anything other than zero, across the entire kill/restart cycle. This is the one invariant chaos must never violate, no matter what infrastructure failure happens mid-flight.

Run via `make chaos-drill`. Takes a little over 90 seconds (the load test's duration) plus recovery time.
