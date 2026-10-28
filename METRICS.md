# Measured results: concurrency, retries, crashes and load

Every number in this file comes from a test run recorded in
`docs/metrics-data/` (JSON lines from the Go stress tests, k6 summary
exports). Where a scenario ran more than once the table gives the range
across runs, never an average or the best run. `scripts/stress/summarize.py`
rebuilds the tables from those files.

"Before" means the code as it was at `39703c3` (the last commit before this
work). "After" means the branch with the fixes listed under
[Bugs found](#bugs-found).

## Contents

- [How the relevant parts work](#how-the-relevant-parts-work)
- [Environment](#environment)
- [How to rerun](#how-to-rerun)
- Results:
  [1 Concurrent debits](#1-concurrent-debits-against-one-account) ·
  [2 Double-entry](#2-double-entry-invariant) ·
  [3 Idempotency](#3-idempotency) ·
  [4 Holds](#4-hold--capture--release) ·
  [5 Crash recovery](#5-crash-recovery) ·
  [6 Webhooks](#6-webhook-retries) ·
  [7 Throughput](#7-throughput)
- [Bugs found](#bugs-found)
- [CV bullets](#cv-bullets)

## How the relevant parts work

**Ledger** (`internal/ledger`). Accounts, transactions and entries in
Postgres. Balances are never stored: every read sums `ledger_entries`, and
"available" is that sum minus active holds. Two triggers in
`db/migrations/00006_ledger_invariants.sql` enforce the rules: a deferred
constraint trigger rejects any transaction whose entries don't sum to zero,
and a `BEFORE INSERT` trigger on negative entries locks the account row
(`SELECT ... FOR UPDATE`), sums its entries and active holds, and rejects
the entry if available balance would fall below the overdraft limit. That
row lock is what serializes concurrent debits. Isolation is Postgres's
default, read committed.

**Idempotency** (`internal/idempotency`). `Execute` inserts the key into
`idempotency_keys` in the same transaction as the caller's writes. A second
caller with the same key blocks on the primary key until the first
commits, then reads the stored response. All ledger primitives use it. The
HTTP layer requires an `Idempotency-Key` header on every mutating route.
Some services (external transfers, cooling-off transfers, charges,
payouts, card authorizations) do their own "look up by key, else create"
on their own tables instead.

**Holds** (`internal/ledger/holds.go`). `PlaceHold` locks the account and
checks available balance. `CaptureHold` and `ReleaseHold` lock the hold row
and require it to be `active`. Differences from the brief:

- Capture has no amount parameter. It always captures exactly the held
  amount, so "capture more than was held" can't be expressed and was not
  tested. There is no partial capture either.
- `holds.expires_at` exists but nothing sets it or acts on it, so holds
  never expire on their own.

**Payment workers.** There are no separate worker processes. `cmd/api`
runs background goroutines: `externalpayments.ProcessPending` every 3 s
claims up to 20 pending external transfers (`FOR UPDATE SKIP LOCKED`),
runs risk scoring, calls a simulated rail and captures or releases the
hold. `ResolveStuck` every 60 s retries rows left `processing` for more
than 2 minutes, relying on idempotency keys so a retry can't post twice.
The rails are in-process stubs (`internal/rails`) with fixed latency and
marker strings (`RAILFAIL`) for failures. For the crash test I wrote
`internal/stresstest/crashworker`, a small binary that runs the same
service code in its own process so it can be SIGKILLed.

**Webhooks** (`internal/webhooks/delivery.go`). One delivery row per event
and endpoint. Workers claim due rows with `FOR UPDATE SKIP LOCKED`, POST
the HMAC-signed payload with a 5 s timeout, and on failure retry after
1 m, 5 m, 30 m and 2 h, giving up after 5 attempts. Delivery is
at-least-once. Requests carry no event or delivery ID header; the payload
does include the charge or payout ID, which a receiver can deduplicate on.

**Transfer endpoint.** `POST /v1/me/transfers` goes through
`coolingoff.Send`. A transfer to a payee the sender has paid before (with a
matching name and under 500,000 NGN) posts immediately through
`payments.Transfer`. A first-time payee, a name mismatch or a large amount
instead places a hold that a ticker releases into a transfer after 2
minutes. The load test uses the immediate path.

**Load testing.** The repo had a Go load driver (`cmd/loadtest`) but no
k6. I added `scripts/stress/transfers.js` and a seeder.

## Environment

| | |
|---|---|
| Machine | cloud VM, 4 vCPU Intel Xeon @ 2.80 GHz, 1 thread per core, 15 GiB RAM, no swap |
| OS | Ubuntu 24.04.5 LTS, kernel 6.18.44 |
| Go | go1.25.7 linux/amd64 |
| Docker | 29.9.2 |
| PostgreSQL | 16.15 (`postgres:16-alpine`, digest `sha256:0cefaabd882e…5731ab`), default config: `max_connections=100`, `shared_buffers=128MB`, `fsync=on`, `synchronous_commit=on`, `deadlock_timeout=1s`, isolation read committed |
| k6 | v2.3.0 (`grafana/k6`, digest `sha256:3e4186aea62e…d9a5e`) |
| pgx | v5.10.0 |
| Pool, stress tests | `MaxConns=50` (`STRESS_POOL_MAX_CONNS`), so goroutines race inside Postgres rather than queueing for a connection |
| Pool, API under k6 | pgxpool default `max(4, NumCPU)` = 4, or `pool_max_conns=20` where stated |

Postgres, the API, the test process and k6 all ran on the same 4 vCPUs, so
absolute throughput numbers are lower than they would be with separate
machines.

Postgres runs in Docker on the host network. The before-fix Go stress run
(`before.jsonl`) used a `-p` port mapping instead, which routes traffic
through `docker-proxy`; I switched to the host network after seeing
`docker-proxy` use about 60% of a core in a `top` reading under load. This only affects
timings, not the correctness counts. Every timing comparison below
(double-entry wall time, history growth, k6) was rerun with both versions
on the host network.

## How to rerun

```bash
bash scripts/stress/fresh-db.sh
```

```bash
export DATABASE_URL="postgres://kolo:kolo@localhost:5433/kolo_bank?sslmode=disable"
```

Tests 1-6 and the history test (about 25 minutes; `STRESS_RUNS` and
`STRESS_CRASH_RUNS` set repetitions, default 5 and 3):

```bash
STRESS_RESULTS=stress-results/run.jsonl bash scripts/stress/run-go-tests.sh
```

```bash
python3 scripts/stress/summarize.py stress-results/run.jsonl
```

A single test, for example the concurrent debits:

```bash
go test -tags stress -count=1 -v ./internal/stresstest -run TestConcurrentDebits
```

The hold race without the hold row lock (a separate build tag):

```bash
go test -tags "stress stress_nolock" -count=1 -v ./internal/stresstest -run TestHoldCaptureReleaseRace
```

Test 7 (k6). Start from a fresh database for each configuration:

```bash
LABEL=default-pool bash scripts/stress/run-k6.sh
```

```bash
LABEL=pool20 API_DATABASE_URL="$DATABASE_URL&pool_max_conns=20" bash scripts/stress/run-k6.sh
```

```bash
python3 scripts/stress/summarize.py --k6 stress-results/k6
```

To measure the before-fix code, check out `39703c3` (or `d90e7a5`, the same
application code with these test scripts) in a worktree and pass
`API_SRC=<that worktree>` to `run-k6.sh`, or run the Go tests from inside
it.

### How protections are turned off

Production behaviour is unchanged in every case.

| Test | Protection | How it is removed for the comparison |
|---|---|---|
| 1 | account row lock in `ledger_check_available_balance` | the test replaces the SQL function with a copy minus `FOR UPDATE` and restores the original on cleanup |
| 2 | balanced-transaction trigger | `ALTER TABLE ... DISABLE TRIGGER` inside one transaction that is rolled back |
| 3 | idempotency keys | the client sends a fresh key per copy (what a client without retry keys does) |
| 4 | hold row lock | build tag `stress_nolock` empties the `holdRowLock` constant (`internal/ledger/holdlock_nolock.go`); default builds keep `FOR UPDATE` |
| 6 | retries | read from the same run: what had been delivered on the first attempt |

Test 5 has no "off" variant: there is no switch for crash recovery short of
deleting the resolver.

## Results

### 1. Concurrent debits against one account

500 concurrent transfers of 100 from an account holding 1,000 (enough for
10). 5 runs each.

| Row lock | Succeeded | Rejected | Overdrafts | Final balance |
|---|---|---|---|---|
| on, before | 10 | 490 | 0 | 0 |
| on, after | 10 | 490 | 0 | 0 |
| off, before | 37 to 53 | 447 to 463 | 27 to 43 | -4,300 to -2,700 |
| off, after | 19 to 56 | 444 to 481 | 9 to 46 | -4,600 to -900 |

"Overdrafts" is successful transfers beyond the 10 the balance covered.

### 2. Double-entry invariant

8,000 operations from 50 workers over 20 accounts: transfers (10% of them
for more than the sender has), credits, debits, hold placements, captures
and releases of random holds (often already resolved), reversals of earlier
transfers (repeats must fail), and reuse of old idempotency keys with a
different payload (must be refused). Afterwards the test checks the ledger
against its own record of what every successful operation should have
done. 5 runs each.

| Check | Before | After |
|---|---|---|
| Transactions posted | 3,575 to 3,950 | 3,778 to 4,109 |
| Total debits = total credits | yes (8,211,959 to 9,369,778) | yes (8,690,467 to 9,613,643) |
| Unbalanced transactions | 0 | 0 |
| Transactions with fewer than 2 entries | 0 | 0 |
| Sum of every entry in the database | 0 | 0 |
| Accounts whose balance differs from the test's model | 0 | 0 |
| Accounts with active holds differing from the model | 0 | 0 |
| Accounts below their overdraft limit | 0 | 0 |
| Key reuse with a different payload accepted | 0 | 0 |
| Valid operations killed by deadlock (`40P01`) | 251 to 350 | 0 |
| Captures wrongly rejected for insufficient balance | 57 to 94 | 0 |
| Valid operations killed by deadlock, host-network rerun | 250 to 364 | 0 |
| Wall time for the 5 runs, host-network rerun | 480.5 s | 37.7 s |

The deadlocks and rejected captures were bugs 2 and 1 below. The wall-time
gap is most likely the deadlocks: each one holds its locks for
`deadlock_timeout` (1 s) before Postgres aborts a transaction, and other
workers queue behind them. I didn't measure that directly. Nothing
posted wrongly: each failure rolled back. Because balances are always summed
from entries, a stored balance can't disagree with the entries; the model
comparison is the stronger check.

The database guard on its own, with 100 deliberately one-legged postings
(the kind an application bug would write):

| Balance trigger | Accepted | Rejected |
|---|---|---|
| on | 0 | 100 |
| off (inside a rolled-back transaction) | 100 | 0 |

### 3. Idempotency

100 copies of one request with the same key. "Off" gives each copy its own
key. 5 runs each.

| Entry point | Key | Before | After |
|---|---|---|---|
| `ledger.Transfer`, concurrent | same | 1 posting, 0 errors | 1 posting, 0 errors |
| | fresh | 100 postings | 100 postings |
| `POST /v1/me/transfers`, known payee, concurrent | same | 1 posting, 100 × HTTP 201 | 1 posting, 100 × HTTP 201 |
| | fresh | 100 postings | 100 postings |
| `POST /v1/me/transfers`, first-time payee, concurrent | same | 1 pending transfer, 100 holds (99 orphaned, 9,900 reserved), 1 × 201 and 99 × 500 | 1 pending transfer, 1 hold, 100 × 201 |
| | fresh | 100 pending transfers, 100 holds | same |
| `POST /v1/me/transfers`, first-time payee, one after another | same | 1 pending transfer, 100 holds (99 orphaned), 1 × 201 and 99 × 500 | 1 pending transfer, 1 hold, 100 × 201 |
| `externalpayments.SendOutbound`, concurrent | same | 1 transfer, 100 holds (99 orphaned, 9,900 reserved), 99 errors | 1 transfer, 1 hold, 0 errors |
| | fresh | 100 transfers, 100 holds | same |

An orphaned hold is an active hold that no transfer row points at. Nothing
would ever capture or release it, so the money stays unavailable to the
customer for good. This was bug 3.

### 4. Hold / capture / release

200 holds of 300 on accounts holding 1,000. Each hold gets 5 concurrent
captures and 5 concurrent releases. 5 runs each.

| Hold row lock | Invalid state changes | Holds resolved more than once | Money created or lost |
|---|---|---|---|
| on, before | 0 | 0 | 0 |
| on, after | 0 | 0 | 0 |
| off, before | 9 to 54 | 9 to 45 | 0 to 8,400 |
| off, after | 31 to 67 | 19 to 57 | 3,600 to 12,300 |

An invalid state change is any successful capture or release after the
first one on the same hold. "Money created or lost" sums, per account,
the gap between the actual debit and the debit implied by the hold's final
status.

Capturing one hold on a balance of 1,000, 50 trials per amount:

| Hold amount | Captured, before | Captured, after |
|---|---|---|
| 300 | 50/50 | 50/50 |
| 500 | 50/50 | 50/50 |
| 501 | 0/50 | 50/50 |
| 600 | 0/50 | 50/50 |
| 1,000 | 0/50 | 50/50 |

This was bug 1.

### 5. Crash recovery

**Real process kills.** The crash worker submits 400 outbound external
transfers (100 accounts × 4, a quarter of them set to fail at the rail)
while processing them. The test SIGKILLs it at a random moment 300-1,500
ms after each start, 25 times, restarts it each time, then lets the last
instance run until every transfer is final. 3 runs each.

| | Before | After |
|---|---|---|
| Transfers interrupted mid-flight (in `processing` at a kill) | 137 to 162 | 136 to 156 |
| Reached a final state | 400/400 | 400/400 |
| Completed / failed | 300 / 100 | 300 / 100 |
| Stuck, not final | 0 | 0 |
| Double postings | 0 | 0 |
| Completed without exactly one capture | 0 | 0 |
| Failed with a hold still active | 0 | 0 |
| Accounts with the wrong balance | 0 | 0 |
| Orphaned holds | 0 to 1 (106 reserved) | 0 |
| Recovery time after the last restart | 111.4 to 120.4 s | 111.4 to 117.4 s |

Recovery time is set by `stuckTimeout` (2 minutes): a transfer claimed by a
killed process waits that long before the resolver retries it. The worker
claims 20 at a time and processes them one by one, so a single kill can
strand up to 20. A trial run before the recorded set left 4 orphaned holds
(410 reserved).

**Targeted crash points.** Random kills rarely land in the few
milliseconds between two writes, so this test builds the exact database
state a crash at each step boundary leaves, backdates `attempted_at` past
the 2-minute timeout (as the existing `externalpayments` tests do), runs
the real resolver three times and checks the outcome. 20 transfers per
point.

| Crash point | Should end | Before | After |
|---|---|---|---|
| after claim, before the rail call | completed | 20/20 | 20/20 |
| after capture, before marking completed | completed | 20/20, 0 double postings | 20/20, 0 double postings |
| after hold release, before marking failed | failed | 0/20, all stuck in `processing` forever | 20/20 |
| risk "block" recorded, not yet applied | failed | 0/20, all 20 paid out | state can no longer occur |
| risk "hold" recorded, not yet applied | held for review | 0/20, all 20 paid out | state can no longer occur |
| risk block: hold released, nothing recorded yet | failed | (boundary did not exist) | 20/20 |
| risk hold: nothing recorded yet | held for review | (boundary did not exist) | 20/20 |

The risk fix (bug 5) moved the step boundaries, so before and after are
tested at the boundaries each version actually has.

### 6. Webhook retries

400 events to one endpoint whose behaviour depends on the event: about 60%
answer 200; 20% answer 500 twice, then 200; 10% process the event but
answer after the 5 s client timeout; 5% hang past the timeout once; 5%
always answer 500. Four delivery workers run concurrently, as four API
instances would. Between rounds the test sets `next_attempt_at` to now so
the 1 m to 2 h backoff plays out in seconds; nothing else is changed. 5
runs each (webhook code was not changed by this work).

| | Before | After |
|---|---|---|
| Delivered with retries | 376 to 388 of 400 (94.0% to 97.0%) | 375 to 383 of 400 (93.8% to 95.8%) |
| Delivered, of endpoints that could ever succeed | 100% | 100% |
| Given up after 5 attempts | 12 to 24 (all "always 500") | 17 to 25 (all "always 500") |
| Delivered on the first attempt only (retries off) | 234 to 246 (58.5% to 61.5%) | 230 to 248 (57.5% to 62.0%) |
| Events the receiver processed twice | 34 to 49 | 31 to 40 |
| Duplicates caused by concurrent workers | 0 | 0 |

Every duplicate came from the "slow" group: the receiver handled the event,
answered after the timeout, and the sender retried. That's expected with
at-least-once delivery. Receivers need to deduplicate, and an event ID
header would make that easier (not changed).

### 7. Throughput

k6 against `POST /v1/me/transfers` (known payee, immediate path), each VU
sending 1-kobo transfers from its own sender to its own recipient with a
new idempotency key per request. 30 s per level. Each level uses 200 freshly
seeded senders; see the history test below for why. 2 runs per level, each
configuration on a fresh database.

Before (default pool, 4 connections):

| VUs | req/s | p50 ms | p95 ms | p99 ms | errors |
|---|---|---|---|---|---|
| 10 | 287 to 290 | 34 to 35 | 48 to 50 | 55 to 57 | 0% |
| 25 | 348 to 352 | 69 to 70 | 90 to 96 | 105 to 125 | 0% |
| 50 | 335 to 337 | 143 | 187 to 191 | 211 to 259 | 0% |
| 100 | 333 to 339 | 293 to 300 | 347 to 349 | 373 to 384 | 0% |
| 200 | 338 to 370 | 537 to 582 | 631 to 692 | 664 to 763 | 0% |

After, default pool:

| VUs | req/s | p50 ms | p95 ms | p99 ms | errors |
|---|---|---|---|---|---|
| 10 | 362 to 382 | 25 to 27 | 38 to 39 | 44 | 0% |
| 25 | 373 to 394 | 61 to 64 | 83 to 87 | 97 to 101 | 0% |
| 50 | 364 to 381 | 129 to 135 | 157 to 161 | 172 to 175 | 0% |
| 100 | 404 to 405 | 242 to 243 | 282 to 286 | 369 to 400 | 0% |
| 200 | 415 to 449 | 444 to 475 | 502 to 551 | 536 to 631 | 0% |

After, `pool_max_conns=20`:

| VUs | req/s | p50 ms | p95 ms | p99 ms | errors |
|---|---|---|---|---|---|
| 10 | 411 to 418 | 22 | 42 | 54 to 55 | 0% |
| 25 | 448 to 449 | 52 to 53 | 85 to 87 | 105 to 110 | 0% |
| 50 | 409 to 444 | 109 to 117 | 147 to 165 | 178 to 239 | 0% |
| 100 | 431 to 447 | 221 to 229 | 267 to 284 | 297 to 318 | 0% |
| 200 | 454 to 458 | 428 to 431 | 524 to 529 | 564 to 594 | 0% |

**Where it degrades.** In all three configurations throughput stops rising
at about 25 VUs. From there each doubling of VUs roughly doubles latency
with no gain in requests per second, so requests are queueing. The
machine's 4 vCPUs were close to fully busy (about 95% in one `top` reading
during a run) with k6, the API and Postgres sharing them. No request failed at any level, up to
200 VUs.

**What changed between the tables.** Three changes, applied in this order:

1. `perf(coolingoff)`: `isFirstTimePayee` used `count(*)` over every past
   transfer to the payee when only "any" matters. Under load it was the
   most expensive statement in `pg_stat_statements` (5.8 ms mean, 11,458
   calls in a 30 s run at 50 VUs). Now `EXISTS`.
2. `perf(coolingoff)`: the daily-limit query, which sums every transfer the
   sender made today, ran twice per transfer (`coolingoff.Send` called
   `CheckLimits`, then `payments.Transfer` checked again). For a sender with
   20,000 entries one run took about 41 ms. The immediate path now relies
   on the check inside `payments.Transfer`. A new test checks that limits
   still apply on both paths.
3. `chore(compose)`: `pool_max_conns=20` in the compose DSN. A config
   change, not code.

The figures for items 1 and 2 (5.8 ms mean, 11,458 calls, about 41 ms)
come from one diagnostic run each, with `pg_stat_statements` loaded and
`EXPLAIN ANALYZE` respectively. Their raw output is not in
`docs/metrics-data/`; the before/after tables are.

**History growth.** Every transfer sums over the sender's past entries in
three places: the balance trigger, the daily-limit query and the payee
check. One sender, 200 sequential transfers at each history size, 3 runs
each:

| Sender's entries | HTTP p50, before | HTTP p50, after | HTTP p95, before | HTTP p95, after | Ledger-only p50, after |
|---|---|---|---|---|---|
| 2 | 16.1 to 17.7 ms | 5.9 to 7.1 ms | 23.1 to 28.5 ms | 8.5 to 10.1 ms | 2.2 to 2.6 ms |
| 1,000 | 13.7 to 15.6 ms | 8.5 to 10.0 ms | 19.8 to 23.7 ms | 12.2 to 14.7 ms | 2.5 to 2.7 ms |
| 5,000 | 34.6 to 38.9 ms | 16.4 to 18.4 ms | 56.5 to 67.0 ms | 25.8 to 28.2 ms | 3.2 to 3.9 ms |
| 10,000 | 64.7 to 70.8 ms | 27.8 to 29.2 ms | 95.4 to 110.8 ms | 41.5 to 46.3 ms | 3.9 to 4.8 ms |
| 20,000 | 127.6 to 134.6 ms | 50.7 to 54.7 ms | 171.7 to 184.5 ms | 77.0 to 88.4 ms | 6.7 to 7.0 ms |

Latency still grows roughly linearly with history after the changes,
mostly from the daily-limit query, which does one index lookup per
transfer the sender made today. A proper fix would keep running balances
and daily totals instead of summing on every request. That's a design
change, so I didn't make it here. This is also why the k6 runs reseed
senders per level: my first k6 runs reused senders across levels and
latency rose with accumulated history as well as with load.

## Bugs found

All fixed, each in its own commit with a regression test in the normal
`go test ./...` suite. Each regression test fails on the old code: run
there, all six failed, and the deadlock test had 133 of 200 transfers
deadlock.

1. **A hold could not be captured if it exceeded half the balance**
   (`c7356f4`). `CaptureHold` posted the debit while the hold was still
   `active`, so the balance trigger subtracted both. External outbound
   transfers and card settlement capture holds, so a payment of most of an
   account's balance would succeed at the rail and then fail to post. Fix:
   mark the hold captured before posting, in the same transaction.
2. **Opposite transfers deadlocked** (`e2494d4`). The debit leg locks its
   account `FOR UPDATE` and the credit leg takes `FOR KEY SHARE` on its
   account through the foreign key. Legs were written in caller order, so
   A→B and B→A locked the same rows in opposite orders. In the mixed
   workload, 250 to 364 of about 7,980 operations per run (3.1% to 4.6%,
   10 runs) failed this way. Fix: write legs in account-id
   order.
3. **Retries leaked reserved money** (`ab32b9f`). `SendOutbound` and the
   cooling-off path committed the hold, then inserted the row that owns it.
   A retry placed a second hold, failed the insert on the unique key and
   returned 500, leaving that hold active. A crash between the two commits
   did the same. Fix: new `ledger.PlaceHoldTx` so both writes share one
   transaction, plus a transaction-scoped advisory lock on the idempotency
   key so a retry waits and then returns the original row.
4. **Some crashed transfers stayed stuck forever** (`b045a38`). After a
   crash between releasing the hold of a rail-rejected transfer and marking
   it failed, every resolver pass retried the release, got
   `ErrHoldNotActive` and stopped. Fix: treat that error as "already
   released", as `coolingoff.finalizeRelease` already did.
5. **A crash could pay out a blocked transfer** (`24e0ff8`). Risk scoring
   wrote its decision, then separately applied it. External payments skip
   scoring once a decision row exists, so after a crash in between, blocked
   and held transfers went straight to the rail. Fix: record and apply the
   decision in one transaction; for a block, release the hold first so a
   crash only means the transfer is scored again.

Not fixed, noted for later:

- `payments.checkLimits` reads today's total and then transfers in a
  separate step, so concurrent transfers can together exceed the daily
  limit. I didn't measure this.
- Charges, payouts and card authorizations use the same "look up by key,
  then create" pattern that caused bug 3. I didn't test them.
- Webhook receivers get duplicates on timeouts (test 6), and requests
  carry no event ID header.
- `coolingoff.finalizeRelease` releases the hold, then transfers in a
  separate step, so a crash in between leaves the money briefly
  unreserved. I didn't test this.
- Holds never expire (`expires_at` is never set).

## CV bullets

- Built a double-entry ledger in Go and PostgreSQL; in a test of 500
  concurrent transfers against an account funded for 10, it allowed 0
  overdrafts, against 9 to 46 per run with row locking disabled.
- Wrote stress and crash tests that found 5 bugs, including a deadlock
  that failed 3 to 5% of operations and retries that left customers' money
  held indefinitely; fixed all 5 with regression tests.
- After the fixes, 100 concurrent copies of one transfer request produce
  exactly 1 posting and 1 hold, where before they created 99 orphaned holds
  and 99 HTTP 500 errors.
- In a test that SIGKILLed the payment worker 25 times during 400
  transfers, every transfer reached a final state 111 to 117 s after the
  last restart, with 0 double postings and 0 wrong balances.
- Raised transfer API throughput from 287-370 to 409-458 req/s under k6
  on 4 vCPUs, with 0 errors at up to 200 virtual users, by removing a
  duplicate query, replacing a `count(*)` with `EXISTS` and sizing the
  connection pool.
