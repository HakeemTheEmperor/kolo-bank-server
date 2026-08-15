package auth_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/auth"
	"github.com/toluwalase/kolo-bank-server/internal/compliance"
	"github.com/toluwalase/kolo-bank-server/internal/identity"
	"github.com/toluwalase/kolo-bank-server/internal/kyc"
	"github.com/toluwalase/kolo-bank-server/internal/onboarding"
	"github.com/toluwalase/kolo-bank-server/internal/secrets"
	"github.com/toluwalase/kolo-bank-server/internal/testsupport"
)

// The session/MFA tests exercise secrets.LocalKeyProvider, which requires a
// base64-encoded 32-byte dev key per docs/banking-backend-spec.md's
// KeyProvider seam. Set one here so `go test` needs no extra configuration.
func init() {
	const envVar = "KOLO_KEY_mfa-totp"
	if os.Getenv(envVar) == "" {
		key := make([]byte, 32)
		_, _ = rand.Read(key)
		_ = os.Setenv(envVar, base64.StdEncoding.EncodeToString(key))
	}
}

const testPassword = "correct horse battery staple"

func newActiveIdentity(t *testing.T, pool *pgxpool.Pool) identity.Identity {
	t.Helper()
	ctx := context.Background()

	onboardingSvc := onboarding.NewService(pool, kyc.NewStubProvider(), compliance.NewStubScreener())
	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}

	id, err := onboardingSvc.RegisterIndividual(ctx, testsupport.RandomKey()+"@example.com", nil, hash, "Auth Test User", "1 Main St")
	if err != nil {
		t.Fatalf("register identity: %v", err)
	}
	if id.Status != identity.StatusActive {
		t.Fatalf("identity status = %s, want active", id.Status)
	}
	return id
}

func newAuthService(pool *pgxpool.Pool) *auth.Service {
	return auth.NewService(pool, identity.NewService(pool), secrets.NewLocalKeyProvider())
}

func TestLoginSuccessWithoutMFA(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	id := newActiveIdentity(t, pool)
	authSvc := newAuthService(pool)
	ctx := context.Background()

	token, sess, err := authSvc.Login(ctx, id.Email, testPassword, "device-1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if token == "" {
		t.Fatal("expected a non-empty bearer token")
	}
	if !sess.MFAVerified {
		t.Fatal("expected session to be fully verified when no MFA is enrolled")
	}

	got, err := authSvc.ValidateSessionToken(ctx, token)
	if err != nil {
		t.Fatalf("validate token: %v", err)
	}
	if got.ID != sess.ID {
		t.Fatalf("validated session id = %s, want %s", got.ID, sess.ID)
	}
}

func TestLoginWrongPasswordRejected(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	id := newActiveIdentity(t, pool)
	authSvc := newAuthService(pool)
	ctx := context.Background()

	_, _, err := authSvc.Login(ctx, id.Email, "wrong password entirely", "device-1")
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("login with wrong password: got %v, want ErrInvalidCredentials", err)
	}
}

func TestLoginRequiresMFAWhenEnrolled(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	id := newActiveIdentity(t, pool)
	authSvc := newAuthService(pool)
	kp := secrets.NewLocalKeyProvider()
	ctx := context.Background()

	secret, err := auth.EnrollTOTP(ctx, pool, kp, id.ID)
	if err != nil {
		t.Fatalf("enroll totp: %v", err)
	}
	code, err := auth.GenerateTOTPCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate totp code: %v", err)
	}
	if err := auth.ConfirmTOTP(ctx, pool, kp, id.ID, code); err != nil {
		t.Fatalf("confirm totp: %v", err)
	}

	_, sess, err := authSvc.Login(ctx, id.Email, testPassword, "device-1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if sess.MFAVerified {
		t.Fatal("expected session to require MFA verification")
	}

	loginCode, err := auth.GenerateTOTPCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate totp code: %v", err)
	}
	if err := authSvc.VerifyTOTP(ctx, sess.ID, loginCode); err != nil {
		t.Fatalf("verify totp: %v", err)
	}
}

func TestStepUp(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	id := newActiveIdentity(t, pool)
	authSvc := newAuthService(pool)
	kp := secrets.NewLocalKeyProvider()
	ctx := context.Background()

	secret, err := auth.EnrollTOTP(ctx, pool, kp, id.ID)
	if err != nil {
		t.Fatalf("enroll totp: %v", err)
	}
	code, _ := auth.GenerateTOTPCode(secret, time.Now())
	if err := auth.ConfirmTOTP(ctx, pool, kp, id.ID, code); err != nil {
		t.Fatalf("confirm totp: %v", err)
	}

	token, sess, err := authSvc.Login(ctx, id.Email, testPassword, "device-1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if auth.RequireStepUp(sess, 5*time.Minute) {
		t.Fatal("expected a freshly logged-in session to have no step-up yet")
	}

	stepUpCode, _ := auth.GenerateTOTPCode(secret, time.Now())
	if err := authSvc.StepUp(ctx, sess.ID, stepUpCode); err != nil {
		t.Fatalf("step up: %v", err)
	}

	refreshed, err := authSvc.ValidateSessionToken(ctx, token)
	if err != nil {
		t.Fatalf("validate token: %v", err)
	}
	if !auth.RequireStepUp(refreshed, 5*time.Minute) {
		t.Fatal("expected step-up to be recorded and within the freshness window")
	}
}

func TestNewDeviceLoginIsAudited(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	id := newActiveIdentity(t, pool)
	authSvc := newAuthService(pool)
	ctx := context.Background()

	if _, _, err := authSvc.Login(ctx, id.Email, testPassword, "brand-new-device"); err != nil {
		t.Fatalf("login: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM audit_log WHERE actor_identity_id = $1 AND action = 'auth.new_device_login'
	`, id.ID).Scan(&count); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("new_device_login audit rows = %d, want 1", count)
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	id := newActiveIdentity(t, pool)
	authSvc := newAuthService(pool)
	ctx := context.Background()

	token, sess, err := authSvc.Login(ctx, id.Email, testPassword, "device-1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	if err := authSvc.Logout(ctx, sess.ID); err != nil {
		t.Fatalf("logout: %v", err)
	}

	_, err = authSvc.ValidateSessionToken(ctx, token)
	if !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatalf("validate revoked token: got %v, want ErrSessionNotFound", err)
	}
}

func TestSMSChallengeSendAndVerify(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	id := newActiveIdentity(t, pool)
	ctx := context.Background()

	notifier := auth.NewStubNotifier(pool)
	challengeID, err := notifier.Send(ctx, id.ID, auth.ChannelSMS)
	if err != nil {
		t.Fatalf("send challenge: %v", err)
	}

	code, ok := notifier.PeekCode(challengeID)
	if !ok {
		t.Fatal("expected stub notifier to retain the sent code")
	}

	if err := auth.VerifyChallenge(ctx, pool, challengeID, code); err != nil {
		t.Fatalf("verify challenge: %v", err)
	}

	// Replaying a consumed challenge must fail.
	if err := auth.VerifyChallenge(ctx, pool, challengeID, code); !errors.Is(err, auth.ErrInvalidCode) {
		t.Fatalf("replay challenge: got %v, want ErrInvalidCode", err)
	}
}

func TestDeviceTrust(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	id := newActiveIdentity(t, pool)
	ctx := context.Background()

	deviceID, isNew, err := auth.UpsertDevice(ctx, pool, id.ID, "trusted-device")
	if err != nil {
		t.Fatalf("upsert device: %v", err)
	}
	if !isNew {
		t.Fatal("expected first sighting to be new")
	}

	_, isNewAgain, err := auth.UpsertDevice(ctx, pool, id.ID, "trusted-device")
	if err != nil {
		t.Fatalf("upsert device again: %v", err)
	}
	if isNewAgain {
		t.Fatal("expected second sighting to not be new")
	}

	if err := auth.TrustDevice(ctx, pool, deviceID); err != nil {
		t.Fatalf("trust device: %v", err)
	}

	var trusted bool
	if err := pool.QueryRow(ctx, `SELECT trusted FROM devices WHERE id = $1`, deviceID).Scan(&trusted); err != nil {
		t.Fatalf("query device: %v", err)
	}
	if !trusted {
		t.Fatal("expected device to be trusted after TrustDevice")
	}
}
