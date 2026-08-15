package ledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/toluwalase/kolo-bank-server/internal/ledger"
)

func TestReverseTransactionRestoresBalances(t *testing.T) {
	pool := requireTestPool(t)
	svc := ledger.NewService(pool)
	ctx := context.Background()

	a := newTestAccount(t, svc, 0)
	b := newTestAccount(t, svc, 0)

	if _, err := svc.Credit(ctx, a.ID, mustMoney(t, 1000), randomKey()); err != nil {
		t.Fatalf("seed credit: %v", err)
	}
	txn, err := svc.Transfer(ctx, a.ID, b.ID, mustMoney(t, 400), randomKey())
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}

	balA, _ := svc.GetBalance(ctx, a.ID)
	balB, _ := svc.GetBalance(ctx, b.ID)
	if balA.Available.Minor != 600 || balB.Available.Minor != 400 {
		t.Fatalf("pre-reversal balances a=%d b=%d, want 600/400", balA.Available.Minor, balB.Available.Minor)
	}

	if _, err := svc.ReverseTransaction(ctx, txn.ID, randomKey()); err != nil {
		t.Fatalf("reverse transaction: %v", err)
	}

	balA, _ = svc.GetBalance(ctx, a.ID)
	balB, _ = svc.GetBalance(ctx, b.ID)
	if balA.Available.Minor != 1000 || balB.Available.Minor != 0 {
		t.Fatalf("post-reversal balances a=%d b=%d, want 1000/0", balA.Available.Minor, balB.Available.Minor)
	}

	var globalSum int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount_minor), 0) FROM ledger_entries`).Scan(&globalSum); err != nil {
		t.Fatalf("sum entries: %v", err)
	}
	if globalSum != 0 {
		t.Fatalf("global ledger sum = %d, want 0", globalSum)
	}
}

func TestDoubleReverseRejected(t *testing.T) {
	pool := requireTestPool(t)
	svc := ledger.NewService(pool)
	ctx := context.Background()

	a := newTestAccount(t, svc, 0)
	b := newTestAccount(t, svc, 0)
	if _, err := svc.Credit(ctx, a.ID, mustMoney(t, 1000), randomKey()); err != nil {
		t.Fatalf("seed credit: %v", err)
	}
	txn, err := svc.Transfer(ctx, a.ID, b.ID, mustMoney(t, 400), randomKey())
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}

	if _, err := svc.ReverseTransaction(ctx, txn.ID, randomKey()); err != nil {
		t.Fatalf("first reverse: %v", err)
	}

	// A different idempotency key exercises the "already reversed" state
	// machine rejection rather than an idempotent replay of the first call.
	if _, err := svc.ReverseTransaction(ctx, txn.ID, randomKey()); !errors.Is(err, ledger.ErrNotReversible) {
		t.Fatalf("second reverse: got %v, want ErrNotReversible", err)
	}
}

func TestReverseUnknownTransactionFails(t *testing.T) {
	pool := requireTestPool(t)
	svc := ledger.NewService(pool)
	ctx := context.Background()

	_, err := svc.ReverseTransaction(ctx, randomUUID(), randomKey())
	if !errors.Is(err, ledger.ErrTransactionNotFound) {
		t.Fatalf("reverse unknown transaction: got %v, want ErrTransactionNotFound", err)
	}
}

func TestReverseIsIdempotent(t *testing.T) {
	pool := requireTestPool(t)
	svc := ledger.NewService(pool)
	ctx := context.Background()

	a := newTestAccount(t, svc, 0)
	b := newTestAccount(t, svc, 0)
	if _, err := svc.Credit(ctx, a.ID, mustMoney(t, 1000), randomKey()); err != nil {
		t.Fatalf("seed credit: %v", err)
	}
	txn, err := svc.Transfer(ctx, a.ID, b.ID, mustMoney(t, 400), randomKey())
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}

	key := randomKey()
	r1, err := svc.ReverseTransaction(ctx, txn.ID, key)
	if err != nil {
		t.Fatalf("first reverse: %v", err)
	}
	r2, err := svc.ReverseTransaction(ctx, txn.ID, key)
	if err != nil {
		t.Fatalf("retried reverse: %v", err)
	}
	if r1.ID != r2.ID {
		t.Fatalf("retried reverse produced a different transaction: %s != %s", r1.ID, r2.ID)
	}

	balA, _ := svc.GetBalance(ctx, a.ID)
	if balA.Available.Minor != 1000 {
		t.Fatalf("available after idempotent double-reverse = %d, want 1000 (only one reversal posted)", balA.Available.Minor)
	}
}
