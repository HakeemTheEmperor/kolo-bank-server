package onboarding_test

import (
	"context"
	"errors"
	"testing"

	"github.com/toluwalase/kolo-bank-server/internal/compliance"
	"github.com/toluwalase/kolo-bank-server/internal/identity"
	"github.com/toluwalase/kolo-bank-server/internal/kyc"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/onboarding"
	"github.com/toluwalase/kolo-bank-server/internal/testsupport"
)

func testEmail() string {
	return testsupport.RandomKey() + "@example.com"
}

func TestRegisterIndividualSucceeds(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := onboarding.NewService(pool, kyc.NewStubProvider(), compliance.NewStubScreener())
	ctx := context.Background()

	id, err := svc.RegisterIndividual(ctx, testEmail(), nil, "hashed-password", "Jane Doe", "1 Main St")
	if err != nil {
		t.Fatalf("register individual: %v", err)
	}
	if id.Status != identity.StatusActive {
		t.Fatalf("status = %s, want active", id.Status)
	}
	if id.KYCTier != 2 {
		t.Fatalf("kyc tier = %d, want 2", id.KYCTier)
	}
}

func TestRegisterIndividualKYCFailIsRejected(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := onboarding.NewService(pool, kyc.NewStubProvider(), compliance.NewStubScreener())
	ctx := context.Background()

	id, err := svc.RegisterIndividual(ctx, testEmail(), nil, "hashed-password", "KYCFAIL Doe", "1 Main St")
	if err != nil {
		t.Fatalf("register individual: %v", err)
	}
	if id.Status != identity.StatusRejected {
		t.Fatalf("status = %s, want rejected", id.Status)
	}
	if id.KYCTier != 0 {
		t.Fatalf("kyc tier = %d, want 0", id.KYCTier)
	}
}

func TestRegisterIndividualSanctionsHitIsRejected(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := onboarding.NewService(pool, kyc.NewStubProvider(), compliance.NewStubScreener())
	ctx := context.Background()

	id, err := svc.RegisterIndividual(ctx, testEmail(), nil, "hashed-password", "SANCTIONED Person", "1 Main St")
	if err != nil {
		t.Fatalf("register individual: %v", err)
	}
	if id.Status != identity.StatusRejected {
		t.Fatalf("status = %s, want rejected", id.Status)
	}
}

func TestRegisterBusinessWithBeneficialOwners(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := onboarding.NewService(pool, kyc.NewStubProvider(), compliance.NewStubScreener())
	ctx := context.Background()

	owners := []onboarding.BeneficialOwnerInput{
		{FullName: "Alice Owner", OwnershipPercent: 60},
		{FullName: "Bob Owner", OwnershipPercent: 40},
	}

	id, err := svc.RegisterBusiness(ctx, testEmail(), nil, "hashed-password", "Acme Ltd", "1 Main St", owners)
	if err != nil {
		t.Fatalf("register business: %v", err)
	}
	if id.Status != identity.StatusActive {
		t.Fatalf("status = %s, want active", id.Status)
	}
	if id.Kind != identity.KindBusiness {
		t.Fatalf("kind = %s, want business", id.Kind)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM beneficial_owners WHERE business_identity_id = $1`, id.ID).Scan(&count); err != nil {
		t.Fatalf("count beneficial owners: %v", err)
	}
	if count != 2 {
		t.Fatalf("beneficial owner count = %d, want 2", count)
	}
}

func TestDuplicateEmailRejected(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := onboarding.NewService(pool, kyc.NewStubProvider(), compliance.NewStubScreener())
	ctx := context.Background()

	email := testEmail()
	if _, err := svc.RegisterIndividual(ctx, email, nil, "hashed-password", "First Person", "1 Main St"); err != nil {
		t.Fatalf("first registration: %v", err)
	}

	_, err := svc.RegisterIndividual(ctx, email, nil, "hashed-password", "Second Person", "1 Main St")
	if !errors.Is(err, identity.ErrEmailTaken) {
		t.Fatalf("second registration: got %v, want ErrEmailTaken", err)
	}
}

func TestAuditLogRecordsRegistration(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := onboarding.NewService(pool, kyc.NewStubProvider(), compliance.NewStubScreener())
	ctx := context.Background()

	id, err := svc.RegisterIndividual(ctx, testEmail(), nil, "hashed-password", "Audit Test", "1 Main St")
	if err != nil {
		t.Fatalf("register individual: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM audit_log WHERE actor_identity_id = $1 AND action IN ('identity.registered', 'identity.activated')
	`, id.ID).Scan(&count); err != nil {
		t.Fatalf("count audit log rows: %v", err)
	}
	if count != 2 {
		t.Fatalf("audit log rows = %d, want 2 (registered + activated)", count)
	}
}

// TestRegisteredIdentityCanOwnALedgerAccount composes Phase 1 and Phase 2:
// once an identity is active, it can own a real ledger account (the FK
// added in 00009_add_accounts_owner_fk.sql), and an unknown owner id is
// rejected.
func TestRegisteredIdentityCanOwnALedgerAccount(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	onboardingSvc := onboarding.NewService(pool, kyc.NewStubProvider(), compliance.NewStubScreener())
	ledgerSvc := ledger.NewService(pool)
	ctx := context.Background()

	id, err := onboardingSvc.RegisterIndividual(ctx, testEmail(), nil, "hashed-password", "Ledger Owner", "1 Main St")
	if err != nil {
		t.Fatalf("register individual: %v", err)
	}

	if _, err := ledgerSvc.OpenAccount(ctx, id.ID, ledger.AccountTypeWallet, "NGN", 0); err != nil {
		t.Fatalf("open account for registered identity: %v", err)
	}

	unknownOwner := testsupport.RandomUUID()
	if _, err := ledgerSvc.OpenAccount(ctx, unknownOwner, ledger.AccountTypeWallet, "NGN", 0); err == nil {
		t.Fatal("expected opening an account for an unknown owner to fail the FK constraint")
	}
}

