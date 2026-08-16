# Kolo Bank Server

Kolo Bank is a mock banking backend: a double-entry ledger, customer onboarding/KYC, internal and external payments, cards, merchant collections/payouts, fraud and compliance controls, and dispute handling — built as a learning/reference project for how a real banking core is put together, end to end, in Go and PostgreSQL.

It is not a production bank. There is no real money, no real card network, no real regulator behind it. Every external dependency (card networks, payment rails, KYC/sanctions screening, SMS/email delivery) is a deterministic in-process stub, chosen so the system's behavior is fully reproducible without secrets or network access. See [Design philosophy](#design-philosophy) for why that matters.

The full product/architecture spec this codebase implements lives at [`../docs/banking-backend-spec.md`](../docs/banking-backend-spec.md) (one level up, in the `kolo-bank/` grouping folder — see [Repository layout](#repository-layout)). This README is the map for navigating the code; the spec is the rationale for *why* it's shaped this way.

---

## Table of contents

- [Quick start](#quick-start)
- [Design philosophy](#design-philosophy)
- [Repository layout](#repository-layout)
- [How a request flows through the system](#how-a-request-flows-through-the-system)
- [Package guide](#package-guide)
- [Data model & migrations](#data-model--migrations)
- [Configuration](#configuration)
- [Testing](#testing)
- [Observability](#observability)
- [API surface](#api-surface)
- [Development workflow](#development-workflow)
- [Contributing](#contributing)

---

## Quick start

Requirements: Docker Desktop only — **no local Go install is needed or used**; the compiler always runs inside a container (see [Development workflow](#development-workflow)).

```bash
# 1. Bring up Postgres + the API
make up

# 2. In another terminal, apply migrations (the api container does NOT
#    self-migrate — this is a deliberate, explicit step)
make migrate

# 3. Confirm it's alive
curl http://localhost:8080/healthz   # -> ok
curl http://localhost:8080/readyz    # -> ready (once Postgres is reachable)
```

Full endpoint-by-endpoint API documentation — auth, idempotency, every request/response shape, every error code, webhook signing — is served straight from the running API at **`http://localhost:8080/docs`**, and lives in the repo as [`docs/api-reference.html`](docs/api-reference.html) if you'd rather open it without the stack running.

`make down` tears the stack down. `make logs` tails both containers. `make psql` drops you into a `psql` shell against the dev database. `make test` runs the full test suite against a real, migrated Postgres inside a throwaway container.

There is no seed data beyond a handful of system accounts created by migration `00007`; everything else (identities, accounts, cards, transfers…) is created by exercising the API or the test suite.

---

## Design philosophy

A few decisions recur throughout the codebase and are worth understanding up front, because they explain a lot of "why is it built this way" questions before they come up:

- **The ledger is the source of truth, and it is dumb on purpose.** `internal/ledger` enforces only hard invariants — balances never go negative without explicit overdraft, every transaction's entries sum to zero (double-entry), holds/captures/releases are atomic. It knows nothing about identities, KYC, fraud, or business policy. Every other package composes the ledger's primitives (`Credit`, `Debit`, `Transfer`, `PlaceHold`, `CaptureHold`, `ReleaseHold`, `ReverseTransaction`) rather than reaching around it. If you're implementing a new money-moving feature and you find yourself wanting to write raw SQL against `ledger_entries`, that's a signal to instead find (or add) the right ledger primitive.
- **No new money-movement primitives after Phase 3.** Every feature since — external payments, bill pay, cooling-off transfers, card authorizations — is a *policy layer* over the same five ledger primitives. This is why, for example, a card purchase and an outbound wire transfer both ultimately call `PlaceHold` then `CaptureHold`: they're the same shape at the ledger boundary, just reached through different business rules.
- **Idempotency and holds, not distributed transactions.** Every mutating endpoint requires an `Idempotency-Key` header; retries are safe by construction rather than by client-side deduplication. Multi-step money movement (place a hold, do some risk/network check, then capture or release) is how the system avoids needing sagas or two-phase commit across the boundary to an external simulated rail.
- **Everything external is a deterministic, controllable stub.** Card networks, ACH/wire rails, KYC/sanctions providers, SMS/email — all are stub implementations behind small interfaces, and all support **marker-string-driven simulation**: embedding a specific string in an input (a card token, a bill reference, a rail counterparty ref) deterministically forces a specific outcome. This is how the test suite exercises declines, timeouts, and failures without mocking framework internals. See the table in [Package guide](#package-guide) for the current marker vocabulary (`RAILFAIL`, `KYCFAIL`, `SANCTIONED`, `FRAUDSCORE`, `NETWORKDECLINE`, `INVALID`, …) — grep for a marker name if you need to find exactly where it's interpreted.
- **Explicit, auditable operations — no hidden cascades.** A dispute doesn't automatically reverse a transaction; a case worker (or an explicit service call in tests) calls `Chargeback`/`Reverse` as a distinct, intentional step. This mirrors how real back-office operations work and keeps the audit trail meaningful.
- **Docker-only workflow.** Go is never invoked on the host. This keeps "clone and run" reproducible across machines without a local toolchain, and matches how the project is built and deployed in every environment (see [Development workflow](#development-workflow)).
- **Package docs are the primary documentation.** Every `internal/*` package has a doc comment at the top of its main file explaining what it does and, importantly, *why it's built the way it is* rather than some plausible alternative. When you're unsure why a package looks a certain way, read that comment before assuming it's incidental.

---

## Repository layout

```
kolo-bank/                      (grouping folder on disk — not itself a git repo)
├── docs/
│   └── banking-backend-spec.md (the full product & architecture spec)
├── kolo-bank-server/            <- you are here (this repo)
└── kolo-bank-web/               (frontend — separate repo, not covered here)
```

Inside `kolo-bank-server/`:

```
cmd/
  api/main.go        Entry point: wires every service together, starts the HTTP
                      server and all background tickers, handles graceful shutdown.
  loadtest/main.go   Standalone load/soak-test driver (docs/load-testing.md).
internal/             All application code. Nothing here is importable outside
                      this module (Go's internal/ convention) — see Package guide.
db/
  migrations/         Goose-managed, numbered SQL migrations (00001, 00002, …).
                      This is the single source of truth for the schema.
docs/
  api-reference.html  The full API reference — also served live at GET /docs
                      (embedded into the binary via docs/docs.go's go:embed).
  dr-runbook.md       Disaster-recovery backup/restore/failover procedure.
  load-testing.md     Load/soak/chaos testing targets and how to run them.
scripts/
  dr-drill.sh         Automated DR drill (make dr-drill).
  chaos-drill.sh      Automated chaos drill (make chaos-drill).
Dockerfile            Multi-stage build: compile in golang:1.25-bookworm,
                      ship a static binary on gcr.io/distroless/static-debian12.
docker-compose.yml    Local dev stack: postgres + api.
Makefile              Every workflow (build/up/down/test/migrate/…), Docker-wrapped.
.github/workflows/    CI: go vet, go test against a real Postgres service
                      container, and a Docker image build, on every push/PR.
```

---

## How a request flows through the system

```
HTTP request
   │
   ▼
internal/httpserver   — logging + tracing middleware, /healthz, /readyz,
   │                     mounts internal/publicapi at /v1/
   ▼
internal/publicapi    — routing, auth (API key or session), scope checks,
   │                     idempotency-key enforcement, JSON in/out
   ▼
a business-policy      — e.g. internal/payments, internal/coolingoff,
  package                 internal/cards, internal/charges: KYC-tier limits,
   │                      risk checks, business rules
   ▼
internal/ledger       — the only package that touches ledger_entries /
   │                     ledger_transactions / holds directly
   ▼
PostgreSQL
```

Background work (settlement cycles, webhook delivery, fee application, matured cooling-off releases, stuck-transfer recovery, compliance report generation, …) runs as tickers started in `cmd/api/main.go`'s `run()`, each following the same shape:

```go
func runX(ctx context.Context, ...deps, logger *slog.Logger) {
    ticker := time.NewTicker(N * time.Second)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            // do the work; log errors, never panic or return them upward
        }
    }
}
```

If you're adding a new background job, follow this exact shape — it's what every existing ticker does, and it's what makes shutdown (`ctx` cancellation) uniformly safe across all of them.

---

## Package guide

Grouped roughly by where each package sits in the request flow above. Each package's own doc comment (top of its main `.go` file) is the authoritative explanation — this table is a map to find the right file, not a replacement for reading it.

### Core ledger & money

| Package | What it does |
|---|---|
| `internal/ledger` | The double-entry ledger itself: accounts, transactions, entries, holds. `Credit`/`Debit`/`Transfer`/`PlaceHold`/`CaptureHold`/`ReleaseHold`/`ReverseTransaction` are the only five primitives every other money-moving feature composes. Enforces balance and double-entry invariants at the DB level (see `db/migrations/00006_ledger_invariants.sql`). |
| `internal/idempotency` | Makes mutating ledger operations safe to retry via the `Idempotency-Key` header contract. |
| `internal/audit` | Append-only audit log trail for money-moving and sensitive actions. |

### Identity, auth, and onboarding

| Package | What it does |
|---|---|
| `internal/identity` | The customer/business record that owns ledger accounts; KYC tier lives here. |
| `internal/onboarding` | Orchestrates identity registration, KYC, and account opening as one flow. |
| `internal/kyc` | KYC pipeline behind a provider interface — `INVALID`/marker-driven stub provider for deterministic pass/fail in tests. |
| `internal/compliance` | Sanctions/PEP screening behind a provider interface (`SANCTIONED` marker). |
| `internal/auth` | Password hashing, session tokens, MFA (TOTP + stub notifier), device tracking. |
| `internal/apikeys` | API-key issuance, scoping, and rotation for merchant integrations. |
| `internal/consent` | Connected-apps & consent dashboard — merchants a customer has authorized, and revocation. |
| `internal/recovery` | Self-service account recovery (locked-out customers) without support intervention. |

### Payments — internal and external

| Package | What it does |
|---|---|
| `internal/payments` | Policy layer over internal transfers: KYC-tier transaction limits, P2P recipient-by-email resolution. |
| `internal/scheduler` | Scheduled and recurring transfers / standing orders, plus a stuck-transfer recovery sweep. |
| `internal/coolingoff` | Confirmation-of-payee (typed name vs. account-of-record) and scam-interruption cooling-off holds on high-risk P2P transfers. |
| `internal/payee` | The name-matching logic `coolingoff` uses to classify a payee match/mismatch/partial. |
| `internal/rails` | Simulated external payment rails (ACH-equivalent, wire, card) behind a registry — `RAILFAIL` marker convention. |
| `internal/externalpayments` | Routes money across the bank's boundary through a simulated rail; the claim → external-call → finalize shape, plus an in-flight resolver for stuck transfers. |
| `internal/bills` | Bill/airtime/data payment validation and payment, including recurring bills, routed through `externalpayments`. |
| `internal/risk` | Real-time transaction monitoring / velocity and sanctions checks gating external payments; also generates SAR/CTR-equivalent regulatory reports. `FRAUDSCORE` marker. |

### Merchant-facing integration API

| Package | What it does |
|---|---|
| `internal/tokens` | Tokenization of payment instruments (a merchant checkout tokenizing a customer-entered card). |
| `internal/charges` | Collections — charging a customer's tokenized card; a thin record over an inbound `externalpayments` transfer. |
| `internal/payouts` | Merchant-initiated payouts; same shape as `charges` but outbound. |
| `internal/checkout` | Backend-only hosted-checkout-session flow. |
| `internal/webhooks` | Signed webhook delivery with retry/backoff for merchant-facing events. |
| `internal/fees` | Rule-based fee resolution and posting on completed charges/payouts. |
| `internal/settlement` | Settlement engine: rolling settlement cycles and reserve holds/releases for merchant funds. |
| `internal/reconciliation` | Automated multi-way reconciliation between the ledger and simulated external statement lines; routes discrepancies to a break queue. |

### Cards & disputes

| Package | What it does |
|---|---|
| `internal/cards` | Card issuing (virtual/physical), controls (freeze, per-card limits, MCC blocks), authorization/settlement against a stubbed network (`NETWORKDECLINE` marker), 3-D Secure, and chargebacks. |
| `internal/disputes` | Dispute and case management for back-office investigation across all dispute-eligible source types (charges, payouts, external transfers, cooling-off transfers, card authorizations). |

### Resilience

| Package | What it does |
|---|---|
| `internal/resilience` | Kill switches (per-integration, per-merchant, per-feature) and system-wide read-only mode. `Service.Check` is called at the top of every money-*initiating* service method (transfers, external payments, card authorization, charges, payouts, bill payments) — never on *resolving* one already approved (cancel, settle, void, chargeback), and never inside `internal/ledger` itself. See the package doc for the full rule and call-site list. Admin-only HTTP surface at `/v1/admin/resilience/*` (its own static-bearer-token auth — see [API surface](#api-surface)). |

### Cross-cutting infrastructure

| Package | What it does |
|---|---|
| `internal/publicapi` | The HTTP layer: routing, auth middleware (API key / session), scopes, idempotency enforcement, request/response JSON — see [API surface](#api-surface). |
| `internal/httpserver` | Top-level HTTP server: `/healthz`, `/readyz`, request logging/tracing middleware, mounts `internal/publicapi`. |
| `internal/config` | Loads runtime configuration from environment variables (see [Configuration](#configuration)). |
| `internal/postgres` | Database connection pool setup. |
| `internal/secrets` | The seam between application code and key management — `LocalKeyProvider` is a dev/test-only stand-in for a real KMS/HSM (see its doc comment; **not** production-suitable). |
| `internal/observability` | Structured logging, metrics, and tracing (OpenTelemetry) wiring. |
| `internal/testsupport` | Shared real-Postgres test harness used across `internal/*` test files. |

### Marker-string simulation vocabulary

A quick-reference index of the controllable-failure convention mentioned in [Design philosophy](#design-philosophy) — embed the marker in the relevant input field to force that outcome deterministically in tests:

| Marker | Where | Forces |
|---|---|---|
| `RAILFAIL` | rail counterparty ref (`internal/rails`) | Simulated rail call fails |
| `KYCFAIL` | KYC submission (`internal/kyc`) | KYC check fails |
| `SANCTIONED` | screened name (`internal/compliance`) | Sanctions/PEP hit |
| `RECONBREAK` | reconciliation input (`internal/reconciliation`) | Statement-line mismatch, routed to break queue |
| `FRAUDSCORE` | transaction context (`internal/risk`) | High-risk score, triggers hold/block |
| `NETWORKDECLINE` | card authorization merchant name (`internal/cards`) | Card network declines |
| `INVALID` | bill reference (`internal/bills`) | Bill reference validation fails |

---

## Data model & migrations

Schema is entirely defined by numbered, sequential [goose](https://github.com/pressly/goose) migrations in `db/migrations/`, applied in order — there is no ORM-managed schema and no "just run the app and it migrates itself." Conventions to follow when adding one:

- `-- +goose Up` / `-- +goose Down` sections; every `Up` should have a working `Down`.
- Primary keys are `UUID PRIMARY KEY DEFAULT gen_random_uuid()`.
- Tables with an `updated_at` column get a `trg_<table>_updated_at` trigger reusing the shared `set_updated_at()` function (defined once, in `00001_create_accounts.sql` — don't redefine it).
- Enum-like columns are `TEXT` with a `CHECK (col IN (...))` constraint, not a Postgres `ENUM` type (keeps `ALTER ... ADD VALUE` out of the picture; widening a check constraint is a plain migration).
- Indexes are named `idx_<table>_<columns>`.
- Migrations are numbered sequentially with no gaps (`00001`, `00002`, …) — check the highest existing number before adding the next one.

Run `make migrate` to apply, `make migrate-down` to roll back one step. The `api` container **does not** run migrations on startup by design — this keeps schema changes an explicit, reviewable operator action rather than something that silently happens on deploy.

---

## Configuration

All configuration is environment variables, loaded once at startup by `internal/config.Load()` (see `internal/config/config.go` — it's short, just read it).

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `DATABASE_URL` | **yes** | — | Postgres DSN. `Load()` fails fast if unset. |
| `HTTP_ADDR` | no | `:8080` | Address the API listens on. |
| `LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, or `error`. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | no | *(unset)* | OpenTelemetry collector endpoint; empty disables export (stdout fallback). |
| `PUBLIC_BASE_URL` | no | `http://localhost:8080` | Used to build fully-qualified URLs returned to API clients (e.g. checkout redirect URLs). |
| `KOLO_KEY_<name>` | yes, per key in use | — | Base64-encoded 32-byte key consumed by `internal/secrets.LocalKeyProvider` (e.g. `KOLO_KEY_ledger-signing`). Dev/test only — see the package's doc comment before using this pattern anywhere near production. |
| `ADMIN_API_KEY` | no | *(empty)* | Static bearer token for the `/v1/admin/resilience/*` surface (kill switches, read-only mode). Empty means that surface is unreachable, not open — see `internal/resilience`. |

`docker-compose.yml` sets sane local-dev values for all of these already — you generally don't need to touch environment variables to get started.

---

## Testing

```bash
make test
```

runs `go test -p 1 ./...` inside a throwaway Go container, against a real Postgres brought up via `docker compose up -d postgres` — not mocked. This is deliberate: constraints, triggers, and transactional behavior are part of the correctness surface here, and mocking the database out would let real bugs (a missing `CHECK`, a broken trigger, a race under `FOR UPDATE`) slip through green tests. `internal/testsupport.RequireTestPool` is the shared harness every package's tests use to get a migrated pool; it skips (not fails) if `DATABASE_URL` isn't set, so `go test ./...` degrades gracefully outside the container. The `-p 1` forces package test binaries to run one at a time rather than in parallel — `internal/resilience`'s kill switches and read-only mode are genuine global singleton state in that one shared database, and several packages' tests deliberately flip real production scopes to prove a guard works, which is only safe if no other package's tests are concurrently relying on that scope being clear.

CI (`.github/workflows/ci.yml`) runs `go vet`, the full test suite against a Postgres service container, and a Docker image build on every push and pull request.

Beyond `go test`, two more verification tools live in `scripts/` (see `docs/dr-runbook.md` and `docs/load-testing.md`):
- `make dr-drill` — a real backup/restore/failover drill against the running stack, reporting measured RTO.
- `make chaos-drill` — runs `make loadtest` in the background while killing and restarting the `api` and `postgres` containers mid-flight, then asserts the system recovered with no stuck transactions and the double-entry invariant intact.
- `make loadtest` — a standalone load/soak test (`cmd/loadtest`) against the running API, reporting p50/p95/p99 latency and error rate.

When adding a feature, match the existing test shape for that layer:
- **Service-layer tests** (`internal/<pkg>/*_test.go`) exercise the service directly against `testsupport.RequireTestPool`, including concurrent/idempotency-retry cases where relevant.
- **HTTP-layer tests** (`internal/publicapi/*_test.go`) use `newTestEnv`/`doRequest` helpers to exercise the full handler stack in-process (`httptest.NewRecorder`, no real listener) — including auth rejection and ownership-check (404-not-403) cases.
- `internal/ledger` additionally has property-based tests (`pgregory.net/rapid`) for its invariants — see `property_test.go`.

---

## Observability

`internal/observability` wires structured `slog`-based logging and OpenTelemetry tracing/metrics. With `OTEL_EXPORTER_OTLP_ENDPOINT` unset (the local-dev default), traces/metrics fall back to stdout exporters rather than failing — useful for local debugging without standing up a collector. Every HTTP request is logged with method, path, status, and duration by `internal/httpserver`'s middleware.

---

## API surface

The full route list is the authoritative source (`internal/publicapi/publicapi.go`'s `New()` function reads as a route table) — this is a summary of the shape, not an exhaustive reference:

- **`GET /healthz`, `GET /readyz`** — liveness/readiness, unauthenticated (`internal/httpserver`).
- **`GET /docs`** — the full API reference (see [`docs/api-reference.html`](docs/api-reference.html)), served straight from the binary via `go:embed` (`docs/docs.go`).
- **`/v1/tokens`, `/v1/charges`, `/v1/payouts`, `/v1/checkout-sessions`** — the merchant integration API, authenticated by `Authorization: Bearer <api key>` plus scope checks (`internal/apikeys`).
- **`/v1/dashboard/*`** — merchant dashboard actions (API-key and webhook-endpoint management), authenticated by a login session.
- **`/v1/me/*`** — customer-facing actions (transfers, authorizations, disputes, cards), authenticated by the same session mechanism as the dashboard — any identity, not just merchants, logs in through it.
- **`/v1/recovery/*`** — deliberately public (pre-authentication, by definition), IP-rate-limited instead of key-rate-limited.
- **`/v1/admin/resilience/*`** — kill switches and read-only mode (`internal/resilience`), authenticated by a single static `Authorization: Bearer <ADMIN_API_KEY>` rather than the session/API-key schemes above — there's no admin-user system in this project. Not exposed to merchants or customers.

Every mutating route requires an `Idempotency-Key` header (`requireIdempotencyKey` middleware) — retried requests with the same key return the original result rather than double-processing.

---

## Development workflow

This project is **Docker-only**: Go is never invoked on the host, in any environment, local or CI-adjacent. The compiler always runs inside a container. This is intentional (see `../docs/banking-backend-spec.md` §9) — it keeps "clone and run" reproducible without anyone needing a matching local Go toolchain.

| Command | What it does |
|---|---|
| `make up` | Build (if needed) and start Postgres + API via `docker compose up --build`. |
| `make down` | Stop and remove the stack. |
| `make logs` | Tail logs from both containers. |
| `make build` | Rebuild just the `api` image. |
| `make psql` | Open a `psql` shell against the dev database. |
| `make migrate` | Apply all pending migrations. |
| `make migrate-down` | Roll back one migration. |
| `make test` | Run the full test suite against a real, migrated Postgres. |
| `make dr-drill` | Backup/restore/failover drill against the running stack (`docs/dr-runbook.md`). Host-run — orchestrates `docker compose` itself. |
| `make chaos-drill` | Kill/restart `api` and `postgres` mid-load and assert clean recovery (`docs/load-testing.md`). Host-run. |
| `make loadtest` | Load/soak test against the running API, reporting p50/p95/p99 latency and error rate (`docs/load-testing.md`). |
| `make tidy` | Run `go mod tidy` in a throwaway container. |

Filesystem placement affects Docker dev speed on Windows — see `../docs/banking-backend-spec.md` §9 for the Windows-filesystem-vs-WSL-native trade-off if hot-reload/rebuild latency becomes a problem; it doesn't affect correctness, only inner-loop speed.

---

## Contributing

This is a solo/learning project built incrementally, phase by phase, against `../docs/banking-backend-spec.md`. If you're picking up work here — including a future version of yourself — a few ground rules keep the codebase coherent:

1. **Read the relevant package doc comment(s) before changing behavior.** They exist specifically to capture *why*, not just *what* — a change that looks like an obvious simplification often already has a documented reason it isn't simpler.
2. **Compose existing primitives before adding new ones.** In particular: don't add a new money-movement mechanism if `ledger.Service`'s five primitives (`Credit`/`Debit`/`Transfer`/`PlaceHold`/`CaptureHold`/`ReleaseHold`/`ReverseTransaction`) already express what you need. Check how a similar existing feature (cards, external payments, cooling-off) composes them before inventing a new shape.
3. **Match the existing conventions exactly** rather than introducing a parallel style:
   - Migrations: sequential numbering, `-- +goose Up/Down`, `set_updated_at()` trigger reuse, `CHECK`-constrained enums — see [Data model & migrations](#data-model--migrations).
   - Background jobs: the `runX(ctx, ...deps, logger)` ticker shape in `cmd/api/main.go` — see [How a request flows through the system](#how-a-request-flows-through-the-system).
   - Simulated external failures: the marker-string convention — see the table in [Package guide](#package-guide) — rather than a bespoke mock/flag mechanism.
   - HTTP handlers: ownership checks return 404, never 403, so a non-owner can't distinguish "not yours" from "doesn't exist."
   - New money-*initiating* service methods call `resilienceSvc.Check(ctx, ...)` first, the same way `payments.Transfer` and `cards.Authorize` do — but methods that only *resolve* work already approved (cancel, void, settle, chargeback) deliberately don't. See `internal/resilience`'s package doc for the full rule.
   - New services are wired the same way every existing one is: constructed once in `cmd/api/main.go`, added to `publicapi.Deps`, passed into `publicapi.New(Deps{...})`, consumed as `a.deps.X` in handlers.
4. **Every mutating operation needs an idempotency story.** New endpoints that create or move money must accept and honor `Idempotency-Key` the same way existing ones do (`UNIQUE(scope, idempotency_key)` constraint + "return existing row on retry" service-layer logic).
5. **Tests run against real Postgres, not mocks** — see [Testing](#testing). New features need both service-layer and (if HTTP-exposed) handler-layer tests following the existing patterns, including at least one deliberately-adversarial case (insufficient balance, wrong owner, expired/invalid input) alongside the happy path.
6. **No secrets, no real external calls, ever.** Every third-party integration in this codebase is and must remain a deterministic local stub — that's what makes the whole test suite runnable offline and reproducibly. If you're integrating something that needs a real API key or network call, it doesn't belong in this codebase as currently scoped without an explicit design discussion first.
7. **Keep `../docs/banking-backend-spec.md` and this README's [Package guide](#package-guide) in sync** when you add a new `internal/*` package or change what an existing one is responsible for — a stale map is worse than no map.

Commit messages should follow [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `refactor:`, `test:`, `docs:`, …) — this is the convention already used throughout the project history.
