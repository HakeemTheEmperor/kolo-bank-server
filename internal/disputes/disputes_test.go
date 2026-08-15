package disputes_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/disputes"
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

func TestCreateOpensDispute(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ctx := context.Background()
	svc := disputes.NewService(pool, nil)

	identityID := newIdentity(t, pool)
	d, err := svc.Create(ctx, identityID, "charge", testsupport.RandomUUID(), "unauthorized transaction")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if d.Status != disputes.StatusOpen {
		t.Fatalf("status = %s, want open", d.Status)
	}
}

func TestAdvanceValidTransitionsRecordEvents(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ctx := context.Background()
	svc := disputes.NewService(pool, nil)

	identityID := newIdentity(t, pool)
	d, err := svc.Create(ctx, identityID, "payout", testsupport.RandomUUID(), "never received funds")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := svc.Advance(ctx, d.ID, disputes.StatusInvestigating, "assigned to investigator"); err != nil {
		t.Fatalf("advance to investigating: %v", err)
	}
	resolved, err := svc.Advance(ctx, d.ID, disputes.StatusResolved, "refunded")
	if err != nil {
		t.Fatalf("advance to resolved: %v", err)
	}
	if resolved.Status != disputes.StatusResolved {
		t.Fatalf("status = %s, want resolved", resolved.Status)
	}
	if resolved.Resolution == nil || *resolved.Resolution != "refunded" {
		t.Fatalf("resolution = %v, want \"refunded\"", resolved.Resolution)
	}

	var eventCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM dispute_events WHERE dispute_id = $1`, d.ID).Scan(&eventCount); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if eventCount != 2 {
		t.Fatalf("event count = %d, want 2 (open->investigating, investigating->resolved)", eventCount)
	}
}

func TestAdvanceRejectsInvalidTransition(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ctx := context.Background()
	svc := disputes.NewService(pool, nil)

	identityID := newIdentity(t, pool)
	d, err := svc.Create(ctx, identityID, "charge", testsupport.RandomUUID(), "duplicate charge")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.Advance(ctx, d.ID, disputes.StatusInvestigating, "assigned"); err != nil {
		t.Fatalf("advance to investigating: %v", err)
	}
	if _, err := svc.Advance(ctx, d.ID, disputes.StatusResolved, "refunded"); err != nil {
		t.Fatalf("advance to resolved: %v", err)
	}

	if _, err := svc.Advance(ctx, d.ID, disputes.StatusInvestigating, "reopen"); !errors.Is(err, disputes.ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition (resolved is terminal)", err)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM disputes WHERE id = $1`, d.ID).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != string(disputes.StatusResolved) {
		t.Fatalf("status = %s, want unchanged resolved", status)
	}
}

func TestListByIdentityReturnsNewestFirst(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	ctx := context.Background()
	svc := disputes.NewService(pool, nil)

	identityID := newIdentity(t, pool)
	first, err := svc.Create(ctx, identityID, "charge", testsupport.RandomUUID(), "first")
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	second, err := svc.Create(ctx, identityID, "charge", testsupport.RandomUUID(), "second")
	if err != nil {
		t.Fatalf("create second: %v", err)
	}

	list, err := svc.ListByIdentity(ctx, identityID, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
		t.Fatalf("list = %+v, want [second, first]", list)
	}
}
