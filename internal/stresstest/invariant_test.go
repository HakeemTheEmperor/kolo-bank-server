//go:build stress

package stresstest

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/toluwalase/kolo-bank-server/internal/idempotency"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/testsupport"
)

// model is the test's own record of what every successful operation
// should have done to each account. The ledger is checked against it at
// the end, which is a stronger check than comparing the ledger with
// itself (balances aren't stored anywhere; they are always summed from
// ledger_entries, so they can't drift from the entries by construction).
type model struct {
	mu       sync.Mutex
	balance  map[string]int64
	held     map[string]int64
	txnLegs  map[string]map[string]int64 // reversible txn id -> account -> delta
	txnIDs   []string
	holds    []modelHold
	keys     []string
	opCounts map[string]map[string]int // op -> outcome -> count
}

type modelHold struct {
	id, account string
	amount      int64
}

func (m *model) count(op, outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.opCounts[op] == nil {
		m.opCounts[op] = map[string]int{}
	}
	m.opCounts[op][outcome]++
}

func outcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ledger.ErrInsufficientBalance):
		return "insufficient_balance"
	case errors.Is(err, ledger.ErrHoldNotActive):
		return "hold_not_active"
	case errors.Is(err, ledger.ErrNotReversible):
		return "not_reversible"
	case errors.Is(err, idempotency.ErrKeyReused):
		return "key_reused"
	default:
		if s := sqlState(err); s != "" {
			return "sqlstate_" + s
		}
		return "other_error"
	}
}

// sqlState returns the Postgres error code wrapped in err, if any
// (40P01 is a deadlock, 55P03/57014 are lock or statement timeouts).
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// TestDoubleEntryInvariant runs a concurrent mixed workload (transfers,
// credits, debits, holds, captures, releases, reversals, and requests
// meant to fail) and then audits the ledger.
func TestDoubleEntryInvariant(t *testing.T) {
	const (
		accounts = 20
		ops      = 8000
		workers  = 50
		funding  = 50_000
	)
	p := requirePool(t)
	svc := ledger.NewService(p)
	ctx := context.Background()

	for run := 1; run <= runs(); run++ {
		m := &model{
			balance: map[string]int64{}, held: map[string]int64{},
			txnLegs: map[string]map[string]int64{}, opCounts: map[string]map[string]int{},
		}
		ids := make([]string, accounts)
		for i := range ids {
			ids[i] = newFundedAccount(t, p, svc, funding).ID
			m.balance[ids[i]] = funding
		}
		var firstTxnAt string
		if err := p.QueryRow(ctx, `SELECT now()::text`).Scan(&firstTxnAt); err != nil {
			t.Fatal(err)
		}

		jobs := make(chan int)
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(seed int64) {
				defer wg.Done()
				rng := rand.New(rand.NewSource(seed))
				for range jobs {
					runOneOp(ctx, svc, m, ids, rng)
				}
			}(int64(run*1000 + w))
		}
		for i := 0; i < ops; i++ {
			jobs <- i
		}
		close(jobs)
		wg.Wait()

		audit := auditLedger(t, ids, firstTxnAt, m)
		audit["ops"] = ops
		audit["workers"] = workers
		audit["op_outcomes"] = m.opCounts
		record(t, result{Test: "double_entry", Scenario: "8000 mixed ops, 50 workers, 20 accounts", Protection: "as_shipped", Run: run, Metrics: audit})

		for _, k := range []string{"unbalanced_transactions", "single_leg_transactions", "accounts_disagreeing_with_model", "accounts_below_overdraft", "held_mismatches"} {
			if audit[k].(int) != 0 {
				t.Errorf("run %d: %s = %v", run, k, audit[k])
			}
		}
		if audit["global_entry_sum"].(int64) != 0 {
			t.Errorf("run %d: global entry sum = %v", run, audit["global_entry_sum"])
		}
	}
}

func runOneOp(ctx context.Context, svc *ledger.Service, m *model, ids []string, rng *rand.Rand) {
	pick := func() string { return ids[rng.Intn(len(ids))] }
	amt := int64(1 + rng.Intn(5000))
	key := testsupport.RandomKey()
	remember := func(txnID string, legs map[string]int64, reversible bool) {
		m.mu.Lock()
		defer m.mu.Unlock()
		for a, d := range legs {
			m.balance[a] += d
		}
		m.keys = append(m.keys, key)
		if reversible {
			m.txnLegs[txnID] = legs
			m.txnIDs = append(m.txnIDs, txnID)
		}
	}

	switch r := rng.Intn(100); {
	case r < 35: // transfer, some for more than the sender has
		from, to := pick(), pick()
		if from == to {
			to = ids[(indexOf(ids, from)+1)%len(ids)]
		}
		if rng.Intn(10) == 0 {
			amt = 1_000_000 // deliberately unaffordable
		}
		txn, err := svc.Transfer(ctx, from, to, ngn(amt), key)
		m.count("transfer", outcome(err))
		if err == nil {
			remember(txn.ID, map[string]int64{from: -amt, to: amt}, true)
		}
	case r < 45:
		a := pick()
		txn, err := svc.Credit(ctx, a, ngn(amt), key)
		m.count("credit", outcome(err))
		if err == nil {
			remember(txn.ID, map[string]int64{a: amt, ledger.SystemAccountNGN: -amt}, false)
		}
	case r < 55:
		a := pick()
		txn, err := svc.Debit(ctx, a, ngn(amt), key)
		m.count("debit", outcome(err))
		if err == nil {
			remember(txn.ID, map[string]int64{a: -amt, ledger.SystemAccountNGN: amt}, false)
		}
	case r < 70:
		a := pick()
		h, err := svc.PlaceHold(ctx, a, ngn(amt))
		m.count("place_hold", outcome(err))
		if err == nil {
			m.mu.Lock()
			m.holds = append(m.holds, modelHold{id: h.ID, account: a, amount: amt})
			m.held[a] += amt
			m.mu.Unlock()
		}
	case r < 82: // capture a random hold; it may already be resolved
		h, ok := randomHold(m, rng)
		if !ok {
			return
		}
		txn, err := svc.CaptureHold(ctx, h.id, key)
		m.count("capture_hold", outcome(err))
		if err == nil {
			remember(txn.ID, map[string]int64{h.account: -h.amount, ledger.SystemAccountNGN: h.amount}, false)
			m.mu.Lock()
			m.held[h.account] -= h.amount
			m.mu.Unlock()
		}
	case r < 92:
		h, ok := randomHold(m, rng)
		if !ok {
			return
		}
		err := svc.ReleaseHold(ctx, h.id)
		m.count("release_hold", outcome(err))
		if err == nil {
			m.mu.Lock()
			m.held[h.account] -= h.amount
			m.mu.Unlock()
		}
	case r < 97: // reverse a random earlier transfer; repeats must fail
		m.mu.Lock()
		if len(m.txnIDs) == 0 {
			m.mu.Unlock()
			return
		}
		orig := m.txnIDs[rng.Intn(len(m.txnIDs))]
		legs := m.txnLegs[orig]
		m.mu.Unlock()
		txn, err := svc.ReverseTransaction(ctx, orig, key)
		m.count("reverse", outcome(err))
		if err == nil {
			neg := map[string]int64{}
			for a, d := range legs {
				neg[a] = -d
			}
			remember(txn.ID, neg, false)
		}
	default: // reuse an old idempotency key for a different request: must be refused
		m.mu.Lock()
		if len(m.keys) == 0 {
			m.mu.Unlock()
			return
		}
		old := m.keys[rng.Intn(len(m.keys))]
		m.mu.Unlock()
		a := pick()
		_, err := svc.Credit(ctx, a, ngn(amt+7_777_777), old)
		m.count("key_reuse", outcome(err))
		if err == nil {
			// Should never happen; record it so the audit shows it.
			m.mu.Lock()
			m.balance[a] += amt + 7_777_777
			m.mu.Unlock()
		}
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func randomHold(m *model, rng *rand.Rand) (modelHold, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.holds) == 0 {
		return modelHold{}, false
	}
	return m.holds[rng.Intn(len(m.holds))], true
}

func auditLedger(t *testing.T, ids []string, since string, m *model) map[string]any {
	t.Helper()
	p := requirePool(t)
	ctx := context.Background()
	out := map[string]any{}

	var global int64
	if err := p.QueryRow(ctx, `SELECT COALESCE(SUM(amount_minor), 0) FROM ledger_entries`).Scan(&global); err != nil {
		t.Fatal(err)
	}
	out["global_entry_sum"] = global

	var txns, unbalanced, singleLeg int
	var debits, credits int64
	if err := p.QueryRow(ctx, `
		WITH t AS (
			SELECT le.transaction_id, SUM(le.amount_minor) AS s, count(*) AS n,
			       SUM(-le.amount_minor) FILTER (WHERE le.amount_minor < 0) AS d,
			       SUM(le.amount_minor) FILTER (WHERE le.amount_minor > 0) AS c
			FROM ledger_entries le JOIN ledger_transactions lt ON lt.id = le.transaction_id
			WHERE lt.created_at >= $1::timestamptz
			GROUP BY le.transaction_id
		)
		SELECT count(*), count(*) FILTER (WHERE s <> 0), count(*) FILTER (WHERE n < 2),
		       COALESCE(SUM(d), 0), COALESCE(SUM(c), 0)
		FROM t
	`, since).Scan(&txns, &unbalanced, &singleLeg, &debits, &credits); err != nil {
		t.Fatal(err)
	}
	out["transactions_posted"] = txns
	out["unbalanced_transactions"] = unbalanced
	out["single_leg_transactions"] = singleLeg
	out["total_debits_minor"] = debits
	out["total_credits_minor"] = credits

	disagree, belowOverdraft, heldMismatch := 0, 0, 0
	for _, id := range ids {
		var ledgerBal, held, overdraft int64
		if err := p.QueryRow(ctx, `
			SELECT (SELECT COALESCE(SUM(amount_minor), 0) FROM ledger_entries WHERE account_id = $1),
			       (SELECT COALESCE(SUM(amount_minor), 0) FROM holds WHERE account_id = $1 AND status = 'active'),
			       (SELECT overdraft_limit_minor FROM accounts WHERE id = $1)
		`, id).Scan(&ledgerBal, &held, &overdraft); err != nil {
			t.Fatal(err)
		}
		if ledgerBal != m.balance[id] {
			disagree++
			t.Logf("account %s: ledger %d, model %d", id, ledgerBal, m.balance[id])
		}
		if held != m.held[id] {
			heldMismatch++
		}
		if ledgerBal-held < -overdraft {
			belowOverdraft++
		}
	}
	out["accounts_disagreeing_with_model"] = disagree
	out["accounts_below_overdraft"] = belowOverdraft
	out["held_mismatches"] = heldMismatch
	return out
}

// TestDoubleEntryGuard checks the database-side guard on its own by
// writing deliberately unbalanced postings (one leg, no offset), the
// kind of row an application bug would produce. "On" is the shipped
// deferred trigger; "off" disables that trigger inside one transaction
// that is rolled back afterwards, so nothing persists.
func TestDoubleEntryGuard(t *testing.T) {
	const attempts = 100
	p := requirePool(t)
	svc := ledger.NewService(p)
	ctx := context.Background()
	acc := newFundedAccount(t, p, svc, 0)

	insertUnbalanced := func(tx pgx.Tx, i int) error {
		var txnID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO ledger_transactions (idempotency_key, type, state) VALUES ($1, 'credit', 'posted') RETURNING id::text
		`, fmt.Sprintf("guard-%s-%d", testsupport.RandomKey(), i)).Scan(&txnID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO ledger_entries (transaction_id, account_id, amount_minor, currency) VALUES ($1, $2, 100, 'NGN')`, txnID, acc.ID)
		return err
	}

	// On: each attempt in its own transaction, as application code would.
	committed := 0
	for i := 0; i < attempts; i++ {
		tx, err := p.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := insertUnbalanced(tx, i); err != nil {
			_ = tx.Rollback(ctx)
			continue
		}
		if err := tx.Commit(ctx); err == nil {
			committed++
		}
	}
	record(t, result{Test: "double_entry", Scenario: "100 injected one-legged postings", Protection: "balance_trigger_on", Run: 1,
		Metrics: map[string]any{"attempts": attempts, "accepted": committed, "rejected": attempts - committed}})
	if committed != 0 {
		t.Errorf("%d unbalanced postings were accepted", committed)
	}

	// Off: same inserts with the trigger disabled, counted before rollback.
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `ALTER TABLE ledger_entries DISABLE TRIGGER trg_ledger_entries_balanced`); err != nil {
		t.Fatal(err)
	}
	accepted := 0
	for i := 0; i < attempts; i++ {
		if _, err := tx.Exec(ctx, `SAVEPOINT s`); err != nil {
			t.Fatal(err)
		}
		if err := insertUnbalanced(tx, i); err != nil {
			_, _ = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT s`)
			continue
		}
		accepted++
	}
	var drift int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount_minor), 0) FROM ledger_entries`).Scan(&drift); err != nil {
		t.Fatal(err)
	}
	record(t, result{Test: "double_entry", Scenario: "100 injected one-legged postings", Protection: "balance_trigger_off", Run: 1,
		Metrics: map[string]any{"attempts": attempts, "accepted": accepted, "rejected": attempts - accepted, "global_entry_sum_before_rollback": drift}})
}
