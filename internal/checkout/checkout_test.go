package checkout_test

import (
	"context"
	"errors"
	"testing"

	"github.com/toluwalase/kolo-bank-server/internal/apikeys"
	"github.com/toluwalase/kolo-bank-server/internal/checkout"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
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

func newMerchant(t *testing.T) string {
	t.Helper()
	pool := testsupport.RequireTestPool(t)
	var id string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO identities (kind, status, email, password_hash, legal_name)
		VALUES ('business', 'active', $1, 'unused', 'Test Merchant')
		RETURNING id::text
	`, testsupport.RandomKey()+"@example.com").Scan(&id)
	if err != nil {
		t.Fatalf("insert test merchant: %v", err)
	}
	return id
}

func TestCreateAndGetCheckoutSession(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := checkout.NewService(pool)
	ctx := context.Background()
	merchantID := newMerchant(t)

	sess, err := svc.Create(ctx, merchantID, apikeys.ModeSandbox, mustMoney(t, 25_000_00), "https://merchant.example/success", "https://merchant.example/cancel", testsupport.RandomKey())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sess.Status != checkout.StatusOpen {
		t.Fatalf("status = %s, want open", sess.Status)
	}
	if sess.RedirectURL("https://api.kolobank.example") == "" {
		t.Fatal("expected a non-empty redirect url")
	}

	got, err := svc.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != sess.ID {
		t.Fatalf("got id %s, want %s", got.ID, sess.ID)
	}
}

func TestCreateCheckoutSessionIsIdempotent(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := checkout.NewService(pool)
	ctx := context.Background()
	merchantID := newMerchant(t)
	key := testsupport.RandomKey()

	first, err := svc.Create(ctx, merchantID, apikeys.ModeSandbox, mustMoney(t, 1_000_00), "https://x/success", "https://x/cancel", key)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	second, err := svc.Create(ctx, merchantID, apikeys.ModeSandbox, mustMoney(t, 1_000_00), "https://x/success", "https://x/cancel", key)
	if err != nil {
		t.Fatalf("retried create: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("retried create produced a different session: %s != %s", first.ID, second.ID)
	}
}

func TestGetUnknownSessionFails(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := checkout.NewService(pool)

	_, err := svc.Get(context.Background(), testsupport.RandomUUID())
	if !errors.Is(err, checkout.ErrNotFound) {
		t.Fatalf("get unknown session: got %v, want ErrNotFound", err)
	}
}
