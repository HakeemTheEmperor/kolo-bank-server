package ledger_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/toluwalase/kolo-bank-server/internal/ledger"
)

// newTestOwner inserts a minimal identity row directly via SQL so accounts
// have a real owner to satisfy the FK added in
// db/migrations/00009_add_accounts_owner_fk.sql (Phase 2). It intentionally
// doesn't go through internal/identity or internal/onboarding — ledger
// tests only need an owner to exist, not a full onboarding flow.
func newTestOwner(t *testing.T) string {
	t.Helper()
	var id string
	err := testPool.QueryRow(context.Background(), `
		INSERT INTO identities (kind, status, email, password_hash, legal_name)
		VALUES ('individual', 'active', $1, 'unused', 'Test Owner')
		RETURNING id::text
	`, randomKey()+"@example.com").Scan(&id)
	if err != nil {
		t.Fatalf("insert test owner identity: %v", err)
	}
	return id
}

func newTestAccount(t *testing.T, svc *ledger.Service, overdraft int64) ledger.Account {
	t.Helper()
	acc, err := svc.OpenAccount(context.Background(), newTestOwner(t), ledger.AccountTypeWallet, "NGN", overdraft)
	if err != nil {
		t.Fatalf("open account: %v", err)
	}
	return acc
}

func mustMoney(t *testing.T, minor int64) ledger.Money {
	t.Helper()
	m, err := ledger.NewMoney(minor, "NGN")
	if err != nil {
		t.Fatalf("new money: %v", err)
	}
	return m
}

func TestCreditAndDebit(t *testing.T) {
	pool := requireTestPool(t)
	svc := ledger.NewService(pool)
	ctx := context.Background()

	acc := newTestAccount(t, svc, 0)

	if _, err := svc.Credit(ctx, acc.ID, mustMoney(t, 1000), randomKey()); err != nil {
		t.Fatalf("credit: %v", err)
	}
	bal, err := svc.GetBalance(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Available.Minor != 1000 {
		t.Fatalf("available = %d, want 1000", bal.Available.Minor)
	}

	if _, err := svc.Debit(ctx, acc.ID, mustMoney(t, 400), randomKey()); err != nil {
		t.Fatalf("debit: %v", err)
	}
	bal, err = svc.GetBalance(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Available.Minor != 600 {
		t.Fatalf("available = %d, want 600", bal.Available.Minor)
	}

	// Debit beyond available balance must be rejected and leave the
	// balance untouched.
	if _, err := svc.Debit(ctx, acc.ID, mustMoney(t, 1000), randomKey()); !errors.Is(err, ledger.ErrInsufficientBalance) {
		t.Fatalf("debit over balance: got err %v, want ErrInsufficientBalance", err)
	}
	bal, err = svc.GetBalance(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Available.Minor != 600 {
		t.Fatalf("available after rejected debit = %d, want unchanged 600", bal.Available.Minor)
	}
}

func TestTransfer(t *testing.T) {
	pool := requireTestPool(t)
	svc := ledger.NewService(pool)
	ctx := context.Background()

	from := newTestAccount(t, svc, 0)
	to := newTestAccount(t, svc, 0)

	if _, err := svc.Credit(ctx, from.ID, mustMoney(t, 1000), randomKey()); err != nil {
		t.Fatalf("seed credit: %v", err)
	}
	if _, err := svc.Transfer(ctx, from.ID, to.ID, mustMoney(t, 300), randomKey()); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	fromBal, err := svc.GetBalance(ctx, from.ID)
	if err != nil {
		t.Fatalf("get from balance: %v", err)
	}
	toBal, err := svc.GetBalance(ctx, to.ID)
	if err != nil {
		t.Fatalf("get to balance: %v", err)
	}
	if fromBal.Available.Minor != 700 {
		t.Fatalf("from available = %d, want 700", fromBal.Available.Minor)
	}
	if toBal.Available.Minor != 300 {
		t.Fatalf("to available = %d, want 300", toBal.Available.Minor)
	}
}

func TestIdempotentRetrySameKey(t *testing.T) {
	pool := requireTestPool(t)
	svc := ledger.NewService(pool)
	ctx := context.Background()

	acc := newTestAccount(t, svc, 0)
	key := randomKey()

	txn1, err := svc.Credit(ctx, acc.ID, mustMoney(t, 500), key)
	if err != nil {
		t.Fatalf("first credit: %v", err)
	}
	txn2, err := svc.Credit(ctx, acc.ID, mustMoney(t, 500), key)
	if err != nil {
		t.Fatalf("retried credit: %v", err)
	}
	if txn1.ID != txn2.ID {
		t.Fatalf("retried credit produced a different transaction: %s != %s", txn1.ID, txn2.ID)
	}

	bal, err := svc.GetBalance(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Available.Minor != 500 {
		t.Fatalf("available = %d, want 500 (retry must not double-post)", bal.Available.Minor)
	}
}

func TestIdempotentConcurrentSameKey(t *testing.T) {
	pool := requireTestPool(t)
	svc := ledger.NewService(pool)
	ctx := context.Background()

	acc := newTestAccount(t, svc, 0)
	key := randomKey()

	const n = 10
	results := make([]ledger.Transaction, n)
	errs := make([]error, n)

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = svc.Credit(ctx, acc.ID, mustMoney(t, 1000), key)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent credit %d: %v", i, err)
		}
	}
	for i := 1; i < n; i++ {
		if results[i].ID != results[0].ID {
			t.Fatalf("concurrent credits produced different transactions: %s != %s", results[0].ID, results[i].ID)
		}
	}

	bal, err := svc.GetBalance(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Available.Minor != 1000 {
		t.Fatalf("available = %d, want 1000 (exactly one post despite %d concurrent callers)", bal.Available.Minor, n)
	}
}

func TestConcurrentDebitsNoOverdraft(t *testing.T) {
	pool := requireTestPool(t)
	svc := ledger.NewService(pool)
	ctx := context.Background()

	acc := newTestAccount(t, svc, 0)
	if _, err := svc.Credit(ctx, acc.ID, mustMoney(t, 1000), randomKey()); err != nil {
		t.Fatalf("seed credit: %v", err)
	}

	const attempts = 20 // 20 * 100 = 2000 requested against a balance of 1000
	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded := 0

	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			defer wg.Done()
			_, err := svc.Debit(ctx, acc.ID, mustMoney(t, 100), randomKey())
			switch {
			case err == nil:
				mu.Lock()
				succeeded++
				mu.Unlock()
			case errors.Is(err, ledger.ErrInsufficientBalance):
				// expected once the balance is exhausted
			default:
				t.Errorf("unexpected debit error: %v", err)
			}
		}()
	}
	wg.Wait()

	if succeeded != 10 {
		t.Fatalf("succeeded = %d, want exactly 10 (no overdraft, no lost updates)", succeeded)
	}

	bal, err := svc.GetBalance(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Available.Minor != 0 {
		t.Fatalf("final available = %d, want 0", bal.Available.Minor)
	}
}

func TestHoldPlaceReleaseCapture(t *testing.T) {
	pool := requireTestPool(t)
	svc := ledger.NewService(pool)
	ctx := context.Background()

	acc := newTestAccount(t, svc, 0)
	if _, err := svc.Credit(ctx, acc.ID, mustMoney(t, 1000), randomKey()); err != nil {
		t.Fatalf("seed credit: %v", err)
	}

	hold, err := svc.PlaceHold(ctx, acc.ID, mustMoney(t, 400))
	if err != nil {
		t.Fatalf("place hold: %v", err)
	}

	bal, err := svc.GetBalance(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Ledger.Minor != 1000 || bal.Pending.Minor != 400 || bal.Available.Minor != 600 {
		t.Fatalf("after hold: ledger=%d pending=%d available=%d, want 1000/400/600",
			bal.Ledger.Minor, bal.Pending.Minor, bal.Available.Minor)
	}

	// A hold beyond the now-reduced available balance must be rejected.
	if _, err := svc.PlaceHold(ctx, acc.ID, mustMoney(t, 700)); !errors.Is(err, ledger.ErrInsufficientBalance) {
		t.Fatalf("oversized hold: got err %v, want ErrInsufficientBalance", err)
	}

	if _, err := svc.CaptureHold(ctx, hold.ID, randomKey()); err != nil {
		t.Fatalf("capture hold: %v", err)
	}

	bal, err = svc.GetBalance(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Ledger.Minor != 600 || bal.Pending.Minor != 0 || bal.Available.Minor != 600 {
		t.Fatalf("after capture: ledger=%d pending=%d available=%d, want 600/0/600",
			bal.Ledger.Minor, bal.Pending.Minor, bal.Available.Minor)
	}

	// Capturing again (idempotent retry with a fresh key) must fail cleanly
	// since the hold is no longer active.
	if _, err := svc.CaptureHold(ctx, hold.ID, randomKey()); !errors.Is(err, ledger.ErrHoldNotActive) {
		t.Fatalf("double capture: got err %v, want ErrHoldNotActive", err)
	}
}

func TestHoldRelease(t *testing.T) {
	pool := requireTestPool(t)
	svc := ledger.NewService(pool)
	ctx := context.Background()

	acc := newTestAccount(t, svc, 0)
	if _, err := svc.Credit(ctx, acc.ID, mustMoney(t, 1000), randomKey()); err != nil {
		t.Fatalf("seed credit: %v", err)
	}

	hold, err := svc.PlaceHold(ctx, acc.ID, mustMoney(t, 400))
	if err != nil {
		t.Fatalf("place hold: %v", err)
	}
	if err := svc.ReleaseHold(ctx, hold.ID); err != nil {
		t.Fatalf("release hold: %v", err)
	}

	bal, err := svc.GetBalance(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Pending.Minor != 0 || bal.Available.Minor != 1000 {
		t.Fatalf("after release: pending=%d available=%d, want 0/1000", bal.Pending.Minor, bal.Available.Minor)
	}
}

func TestAccountStateTransitionEnforced(t *testing.T) {
	pool := requireTestPool(t)
	svc := ledger.NewService(pool)
	ctx := context.Background()

	acc := newTestAccount(t, svc, 0)

	if err := svc.TransitionAccountState(ctx, acc.ID, ledger.AccountStateFrozen); err != nil {
		t.Fatalf("open -> frozen: %v", err)
	}
	if err := svc.TransitionAccountState(ctx, acc.ID, ledger.AccountStateDormant); !errors.Is(err, ledger.ErrInvalidTransition) {
		t.Fatalf("frozen -> dormant: got err %v, want ErrInvalidTransition", err)
	}
}
