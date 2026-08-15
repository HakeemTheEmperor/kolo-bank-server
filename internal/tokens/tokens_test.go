package tokens_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/toluwalase/kolo-bank-server/internal/apikeys"
	"github.com/toluwalase/kolo-bank-server/internal/testsupport"
	"github.com/toluwalase/kolo-bank-server/internal/tokens"
)

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

func TestCreateTokenMasksCardNumber(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := tokens.NewService(pool)
	ctx := context.Background()
	merchantID := newMerchant(t)

	tok, err := svc.Create(ctx, merchantID, apikeys.ModeSandbox, "4242424242424242", testsupport.RandomKey())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasSuffix(tok.MaskedPAN, "4242") || strings.Contains(tok.MaskedPAN, "424242424242") {
		t.Fatalf("masked pan = %q, want only last 4 digits visible", tok.MaskedPAN)
	}
	if tok.CardBrand != "visa" {
		t.Fatalf("card brand = %q, want visa", tok.CardBrand)
	}
	if tok.WillFail {
		t.Fatal("expected a normal card number to not be flagged will_fail")
	}
}

func TestCreateTokenDeclineNumberFlagged(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := tokens.NewService(pool)
	ctx := context.Background()
	merchantID := newMerchant(t)

	tok, err := svc.Create(ctx, merchantID, apikeys.ModeSandbox, "4000000000000002", testsupport.RandomKey())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !tok.WillFail {
		t.Fatal("expected reserved decline number to be flagged will_fail")
	}
}

func TestCreateTokenRejectsInvalidNumber(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := tokens.NewService(pool)
	ctx := context.Background()
	merchantID := newMerchant(t)

	_, err := svc.Create(ctx, merchantID, apikeys.ModeSandbox, "not-a-card", testsupport.RandomKey())
	if !errors.Is(err, tokens.ErrInvalidCardNumber) {
		t.Fatalf("create with invalid number: got %v, want ErrInvalidCardNumber", err)
	}
}

func TestCreateTokenIsIdempotent(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := tokens.NewService(pool)
	ctx := context.Background()
	merchantID := newMerchant(t)
	key := testsupport.RandomKey()

	first, err := svc.Create(ctx, merchantID, apikeys.ModeSandbox, "4242424242424242", key)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	second, err := svc.Create(ctx, merchantID, apikeys.ModeSandbox, "4242424242424242", key)
	if err != nil {
		t.Fatalf("retried create: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("retried create produced a different token: %s != %s", first.ID, second.ID)
	}
}
