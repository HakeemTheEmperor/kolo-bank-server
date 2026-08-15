// Package recovery implements secure self-service account recovery via
// independent factors (docs/banking-backend-spec.md §5.5): KYC
// re-verification and liveness (reusing the same internal/kyc.Provider
// stub used at onboarding), known-device history
// (internal/auth's devices table), and a waiting period before a
// high-risk credential change takes effect. Trusted contacts (the fifth
// factor the spec names) is out of scope — it would need a standalone
// contact-management/confirmation flow with no existing primitive to
// build on, and the other four factors already meet the exit criterion
// of self-service recovery without support intervention.
package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/auth"
	"github.com/toluwalase/kolo-bank-server/internal/identity"
	"github.com/toluwalase/kolo-bank-server/internal/kyc"
)

var (
	// ErrKYCFailed is returned when re-verification fails at Initiate —
	// the request is rejected outright rather than left pending.
	ErrKYCFailed = errors.New("recovery: kyc re-verification failed")
	// ErrNotEligible is returned when Complete is called before the
	// waiting period has elapsed, or on a request that was rejected or
	// already completed.
	ErrNotEligible = errors.New("recovery: request is not yet eligible")
)

// Waiting periods are demo-scale stand-ins for the real-world 24-72h
// high-risk-change delay (§5.5); a recognized device shortens it, an
// unrecognized one lengthens it.
const (
	trustedDeviceWindow      = 1 * time.Minute
	unrecognizedDeviceWindow = 5 * time.Minute
)

type Status string

const (
	StatusPending       Status = "pending"
	StatusRejected      Status = "rejected"
	StatusWaitingPeriod Status = "waiting_period"
	StatusEligible      Status = "eligible"
	StatusCompleted     Status = "completed"
)

type Request struct {
	ID          string
	IdentityID  string
	Status      Status
	EligibleAt  time.Time
	RequestedAt time.Time
	CompletedAt *time.Time
}

type Service struct {
	pool        *pgxpool.Pool
	identitySvc *identity.Service
	authSvc     *auth.Service
	kycProvider kyc.Provider
}

func NewService(pool *pgxpool.Pool, identitySvc *identity.Service, authSvc *auth.Service, kycProvider kyc.Provider) *Service {
	return &Service{pool: pool, identitySvc: identitySvc, authSvc: authSvc, kycProvider: kycProvider}
}

// Initiate starts a recovery request for email. KYC re-verification runs
// immediately: a failure rejects the request outright, protecting against
// an attacker who can't pass it. Otherwise the waiting period is set from
// whether deviceFingerprint is already a trusted device for this identity.
func (s *Service) Initiate(ctx context.Context, email, deviceFingerprint, legalName, address string) (Request, error) {
	id, err := s.identitySvc.GetByEmail(ctx, email)
	if err != nil {
		return Request{}, err
	}

	outcomes, err := s.kycProvider.Verify(ctx, kyc.Applicant{LegalName: legalName, Address: address})
	if err != nil {
		return Request{}, fmt.Errorf("recovery: kyc verify: %w", err)
	}
	kycJSON, err := json.Marshal(outcomes)
	if err != nil {
		return Request{}, fmt.Errorf("recovery: marshal kyc result: %w", err)
	}

	status := StatusWaitingPeriod
	for _, o := range outcomes {
		if o.Result == kyc.ResultFail {
			status = StatusRejected
			break
		}
	}

	window := unrecognizedDeviceWindow
	if status != StatusRejected {
		trusted, err := s.isTrustedDevice(ctx, id.ID, deviceFingerprint)
		if err != nil {
			return Request{}, err
		}
		if trusted {
			window = trustedDeviceWindow
		}
	}

	var req Request
	err = s.pool.QueryRow(ctx, `
		INSERT INTO account_recovery_requests (identity_id, device_fingerprint, kyc_result, status, eligible_at)
		VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5))
		RETURNING id::text, identity_id::text, status, eligible_at, requested_at, completed_at
	`, id.ID, deviceFingerprint, kycJSON, status, window.Seconds()).Scan(
		&req.ID, &req.IdentityID, &req.Status, &req.EligibleAt, &req.RequestedAt, &req.CompletedAt,
	)
	if err != nil {
		return Request{}, fmt.Errorf("recovery: create request: %w", err)
	}

	if status == StatusRejected {
		return req, ErrKYCFailed
	}
	return req, nil
}

func (s *Service) isTrustedDevice(ctx context.Context, identityID, fingerprint string) (bool, error) {
	var trusted bool
	err := s.pool.QueryRow(ctx, `
		SELECT trusted FROM devices WHERE identity_id = $1 AND fingerprint = $2
	`, identityID, fingerprint).Scan(&trusted)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("recovery: check device trust: %w", err)
	}
	return trusted, nil
}

// Complete resets the identity's password once the waiting period has
// elapsed, and revokes every active session — the credentials an attacker
// (or the customer's lost device) held are cut off the moment recovery
// completes.
func (s *Service) Complete(ctx context.Context, requestID, newPassword string) error {
	var identityID string
	var status Status
	var eligibleAt time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT identity_id::text, status, eligible_at FROM account_recovery_requests WHERE id = $1
	`, requestID).Scan(&identityID, &status, &eligibleAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotEligible
		}
		return fmt.Errorf("recovery: look up request: %w", err)
	}

	if status != StatusWaitingPeriod || time.Now().Before(eligibleAt) {
		return ErrNotEligible
	}

	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		return fmt.Errorf("recovery: hash password: %w", err)
	}

	if _, err := s.pool.Exec(ctx, `UPDATE identities SET password_hash = $2 WHERE id = $1`, identityID, hash); err != nil {
		return fmt.Errorf("recovery: update password: %w", err)
	}

	if err := s.revokeAllSessions(ctx, identityID); err != nil {
		return err
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE account_recovery_requests SET status = 'completed', completed_at = now()
		WHERE id = $1 AND status = 'waiting_period'
	`, requestID)
	if err != nil {
		return fmt.Errorf("recovery: mark completed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotEligible
	}
	return nil
}

func (s *Service) revokeAllSessions(ctx context.Context, identityID string) error {
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM sessions WHERE identity_id = $1 AND revoked_at IS NULL`, identityID)
	if err != nil {
		return fmt.Errorf("recovery: list active sessions: %w", err)
	}
	var sessionIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("recovery: scan session id: %w", err)
		}
		sessionIDs = append(sessionIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, id := range sessionIDs {
		if err := s.authSvc.Logout(ctx, id); err != nil {
			return fmt.Errorf("recovery: revoke session %s: %w", id, err)
		}
	}
	return nil
}

// Get returns a recovery request by id.
func (s *Service) Get(ctx context.Context, requestID string) (Request, error) {
	var req Request
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, identity_id::text, status, eligible_at, requested_at, completed_at
		FROM account_recovery_requests WHERE id = $1
	`, requestID).Scan(&req.ID, &req.IdentityID, &req.Status, &req.EligibleAt, &req.RequestedAt, &req.CompletedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Request{}, fmt.Errorf("recovery: %s: %w", requestID, pgx.ErrNoRows)
		}
		return Request{}, fmt.Errorf("recovery: get: %w", err)
	}
	return req, nil
}
