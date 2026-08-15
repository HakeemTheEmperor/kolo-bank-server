package apikeys_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/toluwalase/kolo-bank-server/internal/apikeys"
	"github.com/toluwalase/kolo-bank-server/internal/testsupport"
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

func TestCreateAndAuthenticate(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := apikeys.NewService(pool)
	ctx := context.Background()
	merchantID := newMerchant(t)

	raw, key, err := svc.Create(ctx, merchantID, apikeys.ModeSandbox, "Test key", []string{"charges:write"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(raw, "kolo_test_") {
		t.Fatalf("raw key = %q, want kolo_test_ prefix", raw)
	}
	if key.KeyPrefix == "" {
		t.Fatal("expected a non-empty key prefix")
	}

	authenticated, err := svc.Authenticate(ctx, raw)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if authenticated.ID != key.ID {
		t.Fatalf("authenticated id = %s, want %s", authenticated.ID, key.ID)
	}
	if !authenticated.HasScope("charges:write") {
		t.Fatal("expected key to have charges:write scope")
	}
	if authenticated.HasScope("payouts:write") {
		t.Fatal("expected key to not have payouts:write scope")
	}
}

func TestAuthenticateRejectsUnknownKey(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := apikeys.NewService(pool)

	_, err := svc.Authenticate(context.Background(), "kolo_test_doesnotexist")
	if !errors.Is(err, apikeys.ErrInvalidKey) {
		t.Fatalf("authenticate unknown key: got %v, want ErrInvalidKey", err)
	}
}

func TestRevokeThenAuthenticateFails(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := apikeys.NewService(pool)
	ctx := context.Background()
	merchantID := newMerchant(t)

	raw, key, err := svc.Create(ctx, merchantID, apikeys.ModeLive, "Live key", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Revoke(ctx, key.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	_, err = svc.Authenticate(ctx, raw)
	if !errors.Is(err, apikeys.ErrInvalidKey) {
		t.Fatalf("authenticate revoked key: got %v, want ErrInvalidKey", err)
	}
}

func TestRotateIssuesNewKeyAndRevokesOld(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := apikeys.NewService(pool)
	ctx := context.Background()
	merchantID := newMerchant(t)

	oldRaw, oldKey, err := svc.Create(ctx, merchantID, apikeys.ModeLive, "Rotating key", []string{"charges:write"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	newRaw, newKey, err := svc.Rotate(ctx, oldKey.ID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if newRaw == oldRaw {
		t.Fatal("expected rotate to produce a different raw key")
	}
	if !newKey.HasScope("charges:write") {
		t.Fatal("expected rotated key to keep the same scopes")
	}

	if _, err := svc.Authenticate(ctx, oldRaw); !errors.Is(err, apikeys.ErrInvalidKey) {
		t.Fatalf("authenticate old key after rotation: got %v, want ErrInvalidKey", err)
	}
	if _, err := svc.Authenticate(ctx, newRaw); err != nil {
		t.Fatalf("authenticate new key: %v", err)
	}
}

func TestListReturnsMerchantKeys(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := apikeys.NewService(pool)
	ctx := context.Background()
	merchantID := newMerchant(t)

	if _, _, err := svc.Create(ctx, merchantID, apikeys.ModeSandbox, "Key 1", nil); err != nil {
		t.Fatalf("create 1: %v", err)
	}
	if _, _, err := svc.Create(ctx, merchantID, apikeys.ModeLive, "Key 2", nil); err != nil {
		t.Fatalf("create 2: %v", err)
	}

	keys, err := svc.List(ctx, merchantID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("key count = %d, want 2", len(keys))
	}
}
