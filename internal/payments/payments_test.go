package payments_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/identity"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/payments"
	"github.com/toluwalase/kolo-bank-server/internal/resilience"
	"github.com/toluwalase/kolo-bank-server/internal/testsupport"
)

func mustMoney(t *testing.T, minor int64) ledger.Money {
	t.Helper()
	m, err := ledger.NewMoney(minor, "NGN")
	if err != nil {
		t.Fatalf("new money: %v", err)
	}
	return m
}

// newTierAccount inserts a minimal identity at the given KYC tier plus one
// open account, directly via SQL — payments tests need to control tier
// precisely, which the full onboarding flow (internal/onboarding) doesn't
// expose a knob for.
func newTierAccount(t *testing.T, pool *pgxpool.Pool, ledgerSvc *ledger.Service, tier int) (identityID, email, accountID string) {
	t.Helper()
	email = testsupport.RandomKey() + "@example.com"
	err := pool.QueryRow(context.Background(), `
		INSERT INTO identities (kind, status, kyc_tier, email, password_hash, legal_name)
		VALUES ('individual', 'active', $1, $2, 'unused', 'Test Owner')
		RETURNING id::text
	`, tier, email).Scan(&identityID)
	if err != nil {
		t.Fatalf("insert test identity: %v", err)
	}

	acc, err := ledgerSvc.OpenAccount(context.Background(), identityID, ledger.AccountTypeWallet, "NGN", 0)
	if err != nil {
		t.Fatalf("open account: %v", err)
	}
	return identityID, email, acc.ID
}

func newServices(pool *pgxpool.Pool) (*ledger.Service, *payments.Service) {
	ledgerSvc, svc, _ := newServicesWithResilience(pool)
	return ledgerSvc, svc
}

func newServicesWithResilience(pool *pgxpool.Pool) (*ledger.Service, *payments.Service, *resilience.Service) {
	ledgerSvc := ledger.NewService(pool)
	identitySvc := identity.NewService(pool)
	resilienceSvc := resilience.NewService(pool)
	return ledgerSvc, payments.NewService(pool, ledgerSvc, identitySvc, resilienceSvc), resilienceSvc
}

func TestTransferWithinLimitsSucceeds(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ledgerSvc, paymentsSvc := newServices(pool)
	ctx := context.Background()

	_, _, fromAcc := newTierAccount(t, pool, ledgerSvc, 2)
	_, _, toAcc := newTierAccount(t, pool, ledgerSvc, 2)

	if _, err := ledgerSvc.Credit(ctx, fromAcc, mustMoney(t, 100_000_00), testsupport.RandomKey()); err != nil {
		t.Fatalf("seed credit: %v", err)
	}

	if _, err := paymentsSvc.Transfer(ctx, fromAcc, toAcc, mustMoney(t, 10_000_00), testsupport.RandomKey()); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	bal, err := ledgerSvc.GetBalance(ctx, toAcc)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Available.Minor != 10_000_00 {
		t.Fatalf("recipient balance = %d, want 1000000", bal.Available.Minor)
	}
}

func TestTransferExceedsSingleLimitRejected(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ledgerSvc, paymentsSvc := newServices(pool)
	ctx := context.Background()

	// tier 0: single-transfer ceiling is ₦5,000.
	_, _, fromAcc := newTierAccount(t, pool, ledgerSvc, 0)
	_, _, toAcc := newTierAccount(t, pool, ledgerSvc, 0)

	if _, err := ledgerSvc.Credit(ctx, fromAcc, mustMoney(t, 100_000_00), testsupport.RandomKey()); err != nil {
		t.Fatalf("seed credit: %v", err)
	}

	_, err := paymentsSvc.Transfer(ctx, fromAcc, toAcc, mustMoney(t, 6_000_00), testsupport.RandomKey())
	if !errors.Is(err, payments.ErrLimitExceeded) {
		t.Fatalf("transfer over single limit: got %v, want ErrLimitExceeded", err)
	}

	bal, _ := ledgerSvc.GetBalance(ctx, toAcc)
	if bal.Available.Minor != 0 {
		t.Fatalf("recipient balance = %d, want 0 (rejected transfer must not post)", bal.Available.Minor)
	}
}

func TestTransferExceedsDailyLimitRejected(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ledgerSvc, paymentsSvc := newServices(pool)
	ctx := context.Background()

	// tier 0: daily ceiling is ₦5,000; two ₦3,000 transfers exceed it on the second.
	_, _, fromAcc := newTierAccount(t, pool, ledgerSvc, 0)
	_, _, toAcc := newTierAccount(t, pool, ledgerSvc, 0)

	if _, err := ledgerSvc.Credit(ctx, fromAcc, mustMoney(t, 100_000_00), testsupport.RandomKey()); err != nil {
		t.Fatalf("seed credit: %v", err)
	}

	if _, err := paymentsSvc.Transfer(ctx, fromAcc, toAcc, mustMoney(t, 3_000_00), testsupport.RandomKey()); err != nil {
		t.Fatalf("first transfer: %v", err)
	}

	_, err := paymentsSvc.Transfer(ctx, fromAcc, toAcc, mustMoney(t, 3_000_00), testsupport.RandomKey())
	if !errors.Is(err, payments.ErrLimitExceeded) {
		t.Fatalf("second transfer: got %v, want ErrLimitExceeded", err)
	}

	bal, _ := ledgerSvc.GetBalance(ctx, toAcc)
	if bal.Available.Minor != 3_000_00 {
		t.Fatalf("recipient balance = %d, want 300000 (only the first transfer posted)", bal.Available.Minor)
	}
}

func TestSendToRecipientEmailSucceeds(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ledgerSvc, paymentsSvc := newServices(pool)
	ctx := context.Background()

	_, _, fromAcc := newTierAccount(t, pool, ledgerSvc, 2)
	_, recipientEmail, toAcc := newTierAccount(t, pool, ledgerSvc, 2)

	if _, err := ledgerSvc.Credit(ctx, fromAcc, mustMoney(t, 50_000_00), testsupport.RandomKey()); err != nil {
		t.Fatalf("seed credit: %v", err)
	}

	if _, err := paymentsSvc.SendToRecipientEmail(ctx, fromAcc, recipientEmail, mustMoney(t, 1_000_00), testsupport.RandomKey()); err != nil {
		t.Fatalf("send to recipient email: %v", err)
	}

	bal, err := ledgerSvc.GetBalance(ctx, toAcc)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.Available.Minor != 1_000_00 {
		t.Fatalf("recipient balance = %d, want 100000", bal.Available.Minor)
	}
}

func TestSendToRecipientEmailUnknownRecipient(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ledgerSvc, paymentsSvc := newServices(pool)
	ctx := context.Background()

	_, _, fromAcc := newTierAccount(t, pool, ledgerSvc, 2)
	if _, err := ledgerSvc.Credit(ctx, fromAcc, mustMoney(t, 50_000_00), testsupport.RandomKey()); err != nil {
		t.Fatalf("seed credit: %v", err)
	}

	_, err := paymentsSvc.SendToRecipientEmail(ctx, fromAcc, "nobody-"+testsupport.RandomKey()+"@example.com", mustMoney(t, 1_000_00), testsupport.RandomKey())
	if !errors.Is(err, payments.ErrRecipientNotFound) {
		t.Fatalf("send to unknown recipient: got %v, want ErrRecipientNotFound", err)
	}
}

func TestTransfer_BlockedByReadOnly(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ledgerSvc, paymentsSvc, resilienceSvc := newServicesWithResilience(pool)
	ctx := context.Background()

	_, _, fromAcc := newTierAccount(t, pool, ledgerSvc, 2)
	_, _, toAcc := newTierAccount(t, pool, ledgerSvc, 2)
	if _, err := ledgerSvc.Credit(ctx, fromAcc, mustMoney(t, 100_000_00), testsupport.RandomKey()); err != nil {
		t.Fatalf("seed credit: %v", err)
	}

	if _, err := resilienceSvc.SetReadOnly(ctx, true, "drill", "test"); err != nil {
		t.Fatalf("SetReadOnly(true): %v", err)
	}
	t.Cleanup(func() {
		if _, err := resilienceSvc.SetReadOnly(context.Background(), false, "", "test-cleanup"); err != nil {
			t.Errorf("cleanup SetReadOnly(false): %v", err)
		}
	})

	_, err := paymentsSvc.Transfer(ctx, fromAcc, toAcc, mustMoney(t, 10_000_00), testsupport.RandomKey())
	if !errors.Is(err, resilience.ErrReadOnly) {
		t.Fatalf("transfer during read-only mode: got %v, want ErrReadOnly", err)
	}
}

func TestTransfer_BlockedByFeatureKillSwitch(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ledgerSvc, paymentsSvc, resilienceSvc := newServicesWithResilience(pool)
	ctx := context.Background()

	_, _, fromAcc := newTierAccount(t, pool, ledgerSvc, 2)
	_, _, toAcc := newTierAccount(t, pool, ledgerSvc, 2)
	if _, err := ledgerSvc.Credit(ctx, fromAcc, mustMoney(t, 100_000_00), testsupport.RandomKey()); err != nil {
		t.Fatalf("seed credit: %v", err)
	}

	if _, err := resilienceSvc.SetKillSwitch(ctx, resilience.Feature("transfer"), false, "incident", "test"); err != nil {
		t.Fatalf("SetKillSwitch: %v", err)
	}
	t.Cleanup(func() {
		if _, err := resilienceSvc.SetKillSwitch(context.Background(), resilience.Feature("transfer"), true, "", "test-cleanup"); err != nil {
			t.Errorf("cleanup SetKillSwitch: %v", err)
		}
	})

	_, err := paymentsSvc.Transfer(ctx, fromAcc, toAcc, mustMoney(t, 10_000_00), testsupport.RandomKey())
	var killErr *resilience.ErrKillSwitchTripped
	if !errors.As(err, &killErr) {
		t.Fatalf("transfer while feature:transfer is tripped: got %v, want *ErrKillSwitchTripped", err)
	}
}
