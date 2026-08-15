package recovery_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/auth"
	"github.com/toluwalase/kolo-bank-server/internal/compliance"
	"github.com/toluwalase/kolo-bank-server/internal/identity"
	"github.com/toluwalase/kolo-bank-server/internal/kyc"
	"github.com/toluwalase/kolo-bank-server/internal/onboarding"
	"github.com/toluwalase/kolo-bank-server/internal/recovery"
	"github.com/toluwalase/kolo-bank-server/internal/secrets"
	"github.com/toluwalase/kolo-bank-server/internal/testsupport"
)

const testPassword = "correct horse battery staple"

type harness struct {
	pool        *pgxpool.Pool
	identitySvc *identity.Service
	authSvc     *auth.Service
	svc         *recovery.Service
}

func newHarness(t *testing.T) harness {
	t.Helper()
	pool := testsupport.RequireTestPool(t)
	identitySvc := identity.NewService(pool)
	authSvc := auth.NewService(pool, identitySvc, secrets.NewLocalKeyProvider())
	return harness{
		pool: pool, identitySvc: identitySvc, authSvc: authSvc,
		svc: recovery.NewService(pool, identitySvc, authSvc, kyc.NewStubProvider()),
	}
}

// newActiveIdentity registers a real active individual via the same
// onboarding path Phase 2's own tests use, so legalName is genuinely on
// record for KYC re-verification to check against.
func (h harness) newActiveIdentity(t *testing.T, legalName string) (identity.Identity, string) {
	t.Helper()
	ctx := context.Background()
	onboardingSvc := onboarding.NewService(h.pool, kyc.NewStubProvider(), compliance.NewStubScreener())
	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	id, err := onboardingSvc.RegisterIndividual(ctx, testsupport.RandomKey()+"@example.com", nil, hash, legalName, "1 Main St")
	if err != nil {
		t.Fatalf("register identity: %v", err)
	}
	return id, id.Email
}

func TestInitiateRejectsOnKYCFailure(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	id, email := h.newActiveIdentity(t, "Recovery Test User")

	req, err := h.svc.Initiate(ctx, email, "device-1", "KYCFAIL Name", "1 Main St")
	if !errors.Is(err, recovery.ErrKYCFailed) {
		t.Fatalf("err = %v, want ErrKYCFailed", err)
	}
	if req.Status != recovery.StatusRejected {
		t.Fatalf("status = %s, want rejected", req.Status)
	}
	if req.IdentityID != id.ID {
		t.Fatalf("identity id = %s, want %s", req.IdentityID, id.ID)
	}
}

func TestInitiateTrustedDeviceGetsShorterWindow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	id, email := h.newActiveIdentity(t, "Recovery Test User")

	deviceID, _, err := auth.UpsertDevice(ctx, h.pool, id.ID, "trusted-device")
	if err != nil {
		t.Fatalf("upsert device: %v", err)
	}
	if err := auth.TrustDevice(ctx, h.pool, deviceID); err != nil {
		t.Fatalf("trust device: %v", err)
	}

	trusted, err := h.svc.Initiate(ctx, email, "trusted-device", "Recovery Test User", "1 Main St")
	if err != nil {
		t.Fatalf("initiate (trusted device): %v", err)
	}

	unrecognized, err := h.svc.Initiate(ctx, email, "unrecognized-device", "Recovery Test User", "1 Main St")
	if err != nil {
		t.Fatalf("initiate (unrecognized device): %v", err)
	}

	if !trusted.EligibleAt.Before(unrecognized.EligibleAt) {
		t.Fatalf("trusted-device eligible_at (%v) should be before unrecognized-device's (%v)", trusted.EligibleAt, unrecognized.EligibleAt)
	}
}

func TestCompleteBeforeEligibleIsRejected(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, email := h.newActiveIdentity(t, "Recovery Test User")

	req, err := h.svc.Initiate(ctx, email, "device-1", "Recovery Test User", "1 Main St")
	if err != nil {
		t.Fatalf("initiate: %v", err)
	}

	if err := h.svc.Complete(ctx, req.ID, "a new password"); !errors.Is(err, recovery.ErrNotEligible) {
		t.Fatalf("complete before eligible err = %v, want ErrNotEligible", err)
	}
}

func TestCompleteAfterEligibleResetsPasswordAndRevokesSessions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, email := h.newActiveIdentity(t, "Recovery Test User")

	rawToken, _, err := h.authSvc.Login(ctx, email, testPassword, "device-1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	req, err := h.svc.Initiate(ctx, email, "device-1", "Recovery Test User", "1 Main St")
	if err != nil {
		t.Fatalf("initiate: %v", err)
	}
	if _, err := h.pool.Exec(ctx, `UPDATE account_recovery_requests SET eligible_at = now() - interval '1 minute' WHERE id = $1`, req.ID); err != nil {
		t.Fatalf("backdate eligible_at: %v", err)
	}

	const newPassword = "a completely different passphrase"
	if err := h.svc.Complete(ctx, req.ID, newPassword); err != nil {
		t.Fatalf("complete: %v", err)
	}

	if _, err := h.authSvc.ValidateSessionToken(ctx, rawToken); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatalf("old session validate err = %v, want ErrSessionNotFound (revoked)", err)
	}

	if _, _, err := h.authSvc.Login(ctx, email, testPassword, "device-1"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("login with old password err = %v, want ErrInvalidCredentials", err)
	}
	if _, _, err := h.authSvc.Login(ctx, email, newPassword, "device-1"); err != nil {
		t.Fatalf("login with new password: %v", err)
	}

	updated, err := h.svc.Get(ctx, req.ID)
	if err != nil {
		t.Fatalf("get request: %v", err)
	}
	if updated.Status != recovery.StatusCompleted {
		t.Fatalf("status = %s, want completed", updated.Status)
	}
}
