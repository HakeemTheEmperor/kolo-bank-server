package consent_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/consent"
	"github.com/toluwalase/kolo-bank-server/internal/testsupport"
)

func newIdentity(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO identities (kind, status, email, password_hash, legal_name)
		VALUES ('individual', 'active', $1, 'unused', 'Test Owner')
		RETURNING id::text
	`, testsupport.RandomKey()+"@example.com").Scan(&id)
	if err != nil {
		t.Fatalf("insert identity: %v", err)
	}
	return id
}

func TestGrantListRevoke(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ctx := context.Background()
	svc := consent.NewService(pool)

	identityID := newIdentity(t, pool)
	merchantID := newIdentity(t, pool)

	grant, err := svc.Grant(ctx, identityID, merchantID, []string{"read_balance", "initiate_payment"})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if grant.Status != consent.StatusActive {
		t.Fatalf("status = %s, want active", grant.Status)
	}

	list, err := svc.ListByIdentity(ctx, identityID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != grant.ID {
		t.Fatalf("list = %+v, want exactly the granted authorization", list)
	}

	if err := svc.Revoke(ctx, identityID, grant.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	list, err = svc.ListByIdentity(ctx, identityID)
	if err != nil {
		t.Fatalf("list after revoke: %v", err)
	}
	if len(list) != 1 || list[0].Status != consent.StatusRevoked {
		t.Fatalf("list after revoke = %+v, want status revoked", list)
	}
}

func TestRevokeIsOwnershipChecked(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ctx := context.Background()
	svc := consent.NewService(pool)

	identityID := newIdentity(t, pool)
	otherIdentityID := newIdentity(t, pool)
	merchantID := newIdentity(t, pool)

	grant, err := svc.Grant(ctx, identityID, merchantID, []string{"read_balance"})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}

	if err := svc.Revoke(ctx, otherIdentityID, grant.ID); !errors.Is(err, consent.ErrNotFound) {
		t.Fatalf("revoke by non-owner err = %v, want ErrNotFound", err)
	}
}

func TestReGrantingRevokedPairReactivatesRatherThanDuplicating(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ctx := context.Background()
	svc := consent.NewService(pool)

	identityID := newIdentity(t, pool)
	merchantID := newIdentity(t, pool)

	first, err := svc.Grant(ctx, identityID, merchantID, []string{"read_balance"})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := svc.Revoke(ctx, identityID, first.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	second, err := svc.Grant(ctx, identityID, merchantID, []string{"read_balance", "initiate_payment"})
	if err != nil {
		t.Fatalf("re-grant: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("re-grant produced a new row %s, want reactivation of %s", second.ID, first.ID)
	}
	if second.Status != consent.StatusActive {
		t.Fatalf("status after re-grant = %s, want active", second.Status)
	}
	if len(second.Scopes) != 2 {
		t.Fatalf("scopes after re-grant = %v, want updated to 2 scopes", second.Scopes)
	}

	list, err := svc.ListByIdentity(ctx, identityID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list = %+v, want exactly one row (no duplicate)", list)
	}
}
