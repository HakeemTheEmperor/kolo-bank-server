package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/audit"
	"github.com/toluwalase/kolo-bank-server/internal/identity"
	"github.com/toluwalase/kolo-bank-server/internal/secrets"
)

const sessionTTL = 24 * time.Hour

var (
	// ErrInvalidCredentials covers both unknown email and wrong password;
	// the two are never distinguished to callers, to avoid leaking which
	// emails are registered.
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	// ErrIdentityNotActive is returned when a not-yet-activated, rejected,
	// suspended, or closed identity attempts to log in.
	ErrIdentityNotActive = errors.New("auth: identity is not active")
	// ErrSessionNotFound covers missing, expired, and revoked sessions
	// uniformly.
	ErrSessionNotFound = errors.New("auth: session not found or expired")
)

type Session struct {
	ID          string
	IdentityID  string
	DeviceID    *string
	MFAVerified bool
	StepUpAt    *time.Time
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

// Service implements login, MFA verification, step-up, and session
// validation (docs/banking-backend-spec.md §3.2).
type Service struct {
	pool        *pgxpool.Pool
	identitySvc *identity.Service
	kp          secrets.KeyProvider
}

func NewService(pool *pgxpool.Pool, identitySvc *identity.Service, kp secrets.KeyProvider) *Service {
	return &Service{pool: pool, identitySvc: identitySvc, kp: kp}
}

// Login verifies credentials and opens a session. If the identity has a
// confirmed TOTP factor, the session starts unverified (MFAVerified=false)
// and the caller must call VerifyTOTP before treating it as authenticated.
// The raw bearer token is returned once and never stored; only its hash is.
func (s *Service) Login(ctx context.Context, email, password, deviceFingerprint string) (token string, sess Session, err error) {
	identityID, hash, err := s.identitySvc.PasswordHash(ctx, email)
	if err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			return "", Session{}, ErrInvalidCredentials
		}
		return "", Session{}, err
	}

	if verr := VerifyPassword(password, hash); verr != nil {
		_ = audit.RecordNow(ctx, s.pool, &identityID, "auth.login_failed", "identity", identityID, map[string]any{"reason": "invalid_password"})
		return "", Session{}, ErrInvalidCredentials
	}

	id, err := s.identitySvc.GetByID(ctx, identityID)
	if err != nil {
		return "", Session{}, err
	}
	if id.Status != identity.StatusActive {
		_ = audit.RecordNow(ctx, s.pool, &identityID, "auth.login_failed", "identity", identityID, map[string]any{"reason": "not_active", "status": string(id.Status)})
		return "", Session{}, ErrIdentityNotActive
	}

	deviceID, isNewDevice, err := UpsertDevice(ctx, s.pool, identityID, deviceFingerprint)
	if err != nil {
		return "", Session{}, err
	}

	hasMFA, err := HasConfirmedTOTP(ctx, s.pool, identityID)
	if err != nil {
		return "", Session{}, err
	}

	rawToken, tokenHash, err := newSessionToken()
	if err != nil {
		return "", Session{}, err
	}

	err = s.pool.QueryRow(ctx, `
		INSERT INTO sessions (identity_id, device_id, token_hash, mfa_verified, expires_at)
		VALUES ($1, $2, $3, $4, now() + $5)
		RETURNING id::text, identity_id::text, device_id::text, mfa_verified, created_at, expires_at
	`, identityID, deviceID, tokenHash, !hasMFA, sessionTTL).Scan(
		&sess.ID, &sess.IdentityID, &sess.DeviceID, &sess.MFAVerified, &sess.CreatedAt, &sess.ExpiresAt,
	)
	if err != nil {
		return "", Session{}, fmt.Errorf("auth: create session: %w", err)
	}

	_ = audit.RecordNow(ctx, s.pool, &identityID, "auth.login_succeeded", "identity", identityID, map[string]any{"mfa_required": hasMFA})
	if isNewDevice {
		_ = audit.RecordNow(ctx, s.pool, &identityID, "auth.new_device_login", "device", deviceID, nil)
	}

	return rawToken, sess, nil
}

// VerifyTOTP checks a TOTP code against the session's identity and, if
// valid, marks the session fully MFA-verified.
func (s *Service) VerifyTOTP(ctx context.Context, sessionID, code string) error {
	sess, err := s.getSession(ctx, sessionID)
	if err != nil {
		return err
	}

	if err := VerifyTOTPCode(ctx, s.pool, s.kp, sess.IdentityID, code); err != nil {
		_ = audit.RecordNow(ctx, s.pool, &sess.IdentityID, "auth.mfa_failed", "session", sessionID, nil)
		return err
	}

	if _, err := s.pool.Exec(ctx, `UPDATE sessions SET mfa_verified = true WHERE id = $1`, sessionID); err != nil {
		return fmt.Errorf("auth: mark session mfa-verified: %w", err)
	}
	_ = audit.RecordNow(ctx, s.pool, &sess.IdentityID, "auth.mfa_verified", "session", sessionID, nil)
	return nil
}

// StepUp re-verifies a TOTP code and records a fresh step_up_at timestamp,
// for gating risky actions that require recent proof of possession.
func (s *Service) StepUp(ctx context.Context, sessionID, code string) error {
	sess, err := s.getSession(ctx, sessionID)
	if err != nil {
		return err
	}

	if err := VerifyTOTPCode(ctx, s.pool, s.kp, sess.IdentityID, code); err != nil {
		_ = audit.RecordNow(ctx, s.pool, &sess.IdentityID, "auth.step_up_failed", "session", sessionID, nil)
		return err
	}

	if _, err := s.pool.Exec(ctx, `UPDATE sessions SET step_up_at = now() WHERE id = $1`, sessionID); err != nil {
		return fmt.Errorf("auth: record step-up: %w", err)
	}
	_ = audit.RecordNow(ctx, s.pool, &sess.IdentityID, "auth.step_up_succeeded", "session", sessionID, nil)
	return nil
}

// RequireStepUp reports whether sess has a step-up recorded within maxAge —
// the primitive later "risky action" call sites gate on.
func RequireStepUp(sess Session, maxAge time.Duration) bool {
	return sess.StepUpAt != nil && time.Since(*sess.StepUpAt) <= maxAge
}

// ValidateSessionToken resolves a raw bearer token to its session, if it
// exists, is unrevoked, and unexpired.
func (s *Service) ValidateSessionToken(ctx context.Context, rawToken string) (Session, error) {
	hash := hashToken(rawToken)
	return s.getSessionByHash(ctx, hash)
}

// Logout revokes a session so its token can no longer be used.
func (s *Service) Logout(ctx context.Context, sessionID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE sessions SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, sessionID)
	if err != nil {
		return fmt.Errorf("auth: revoke session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSessionNotFound
	}
	return nil
}

func (s *Service) getSession(ctx context.Context, sessionID string) (Session, error) {
	var sess Session
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, identity_id::text, device_id::text, mfa_verified, step_up_at, created_at, expires_at
		FROM sessions
		WHERE id = $1 AND revoked_at IS NULL AND expires_at > now()
	`, sessionID).Scan(&sess.ID, &sess.IdentityID, &sess.DeviceID, &sess.MFAVerified, &sess.StepUpAt, &sess.CreatedAt, &sess.ExpiresAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, ErrSessionNotFound
		}
		return Session{}, fmt.Errorf("auth: get session: %w", err)
	}
	return sess, nil
}

func (s *Service) getSessionByHash(ctx context.Context, tokenHash string) (Session, error) {
	var sess Session
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, identity_id::text, device_id::text, mfa_verified, step_up_at, created_at, expires_at
		FROM sessions
		WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now()
	`, tokenHash).Scan(&sess.ID, &sess.IdentityID, &sess.DeviceID, &sess.MFAVerified, &sess.StepUpAt, &sess.CreatedAt, &sess.ExpiresAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, ErrSessionNotFound
		}
		return Session{}, fmt.Errorf("auth: get session: %w", err)
	}
	return sess, nil
}

func newSessionToken() (raw string, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("auth: generate session token: %w", err)
	}
	raw = hex.EncodeToString(b)
	return raw, hashToken(raw), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
