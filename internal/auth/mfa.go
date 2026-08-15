package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/secrets"
)

const totpKeyName = "mfa-totp"

var (
	// ErrMFANotEnrolled is returned when confirming/using TOTP before enrollment.
	ErrMFANotEnrolled = errors.New("auth: totp not enrolled")
	// ErrInvalidCode is returned when a TOTP or challenge code fails to validate.
	ErrInvalidCode = errors.New("auth: invalid or expired code")
)

// EnrollTOTP generates a new TOTP secret for identityID, seals it with
// keyProvider, and stores it unconfirmed. Re-enrolling replaces any prior
// (still unconfirmed or confirmed) secret.
func EnrollTOTP(ctx context.Context, pool *pgxpool.Pool, kp secrets.KeyProvider, identityID string) (secret string, err error) {
	secret, err = GenerateTOTPSecret()
	if err != nil {
		return "", err
	}

	sealed, err := kp.Encrypt(ctx, totpKeyName, []byte(secret))
	if err != nil {
		return "", fmt.Errorf("auth: seal totp secret: %w", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO mfa_totp_secrets (identity_id, secret_encrypted, confirmed)
		VALUES ($1, $2, false)
		ON CONFLICT (identity_id) DO UPDATE SET secret_encrypted = EXCLUDED.secret_encrypted, confirmed = false
	`, identityID, sealed); err != nil {
		return "", fmt.Errorf("auth: store totp secret: %w", err)
	}

	return secret, nil
}

// ConfirmTOTP validates code against the enrolled-but-unconfirmed secret
// and marks it confirmed.
func ConfirmTOTP(ctx context.Context, pool *pgxpool.Pool, kp secrets.KeyProvider, identityID, code string) error {
	secret, err := fetchTOTPSecret(ctx, pool, kp, identityID)
	if err != nil {
		return err
	}

	ok, err := ValidateTOTPCode(secret, code, time.Now())
	if err != nil {
		return err
	}
	if !ok {
		return ErrInvalidCode
	}

	if _, err := pool.Exec(ctx, `UPDATE mfa_totp_secrets SET confirmed = true WHERE identity_id = $1`, identityID); err != nil {
		return fmt.Errorf("auth: confirm totp: %w", err)
	}
	return nil
}

// HasConfirmedTOTP reports whether identityID has a confirmed TOTP factor.
func HasConfirmedTOTP(ctx context.Context, pool *pgxpool.Pool, identityID string) (bool, error) {
	var confirmed bool
	err := pool.QueryRow(ctx, `SELECT confirmed FROM mfa_totp_secrets WHERE identity_id = $1`, identityID).Scan(&confirmed)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("auth: check totp enrollment: %w", err)
	}
	return confirmed, nil
}

// VerifyTOTPCode validates code against identityID's confirmed TOTP secret.
func VerifyTOTPCode(ctx context.Context, pool *pgxpool.Pool, kp secrets.KeyProvider, identityID, code string) error {
	secret, err := fetchTOTPSecret(ctx, pool, kp, identityID)
	if err != nil {
		return err
	}
	ok, err := ValidateTOTPCode(secret, code, time.Now())
	if err != nil {
		return err
	}
	if !ok {
		return ErrInvalidCode
	}
	return nil
}

func fetchTOTPSecret(ctx context.Context, pool *pgxpool.Pool, kp secrets.KeyProvider, identityID string) (string, error) {
	var sealed []byte
	err := pool.QueryRow(ctx, `SELECT secret_encrypted FROM mfa_totp_secrets WHERE identity_id = $1`, identityID).Scan(&sealed)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrMFANotEnrolled
		}
		return "", fmt.Errorf("auth: fetch totp secret: %w", err)
	}
	plain, err := kp.Decrypt(ctx, totpKeyName, sealed)
	if err != nil {
		return "", fmt.Errorf("auth: unseal totp secret: %w", err)
	}
	return string(plain), nil
}

// Channel is a stubbed out-of-band MFA delivery channel.
type Channel string

const (
	ChannelSMS  Channel = "sms"
	ChannelPush Channel = "push"
)

const challengeTTL = 5 * time.Minute

// Notifier sends an out-of-band MFA challenge and returns its ID. Real
// SMS/push delivery is a non-goal for this build
// (docs/banking-backend-spec.md §1); StubNotifier stands in.
type Notifier interface {
	Send(ctx context.Context, identityID string, channel Channel) (challengeID string, err error)
}

// StubNotifier "delivers" a challenge by hashing and storing it, then
// logging it, instead of a real SMS/push send. PeekCode exists only so
// tests can retrieve what was "sent" — there is no real inbox to check.
type StubNotifier struct {
	pool *pgxpool.Pool

	mu    sync.Mutex
	codes map[string]string
}

func NewStubNotifier(pool *pgxpool.Pool) *StubNotifier {
	return &StubNotifier{pool: pool, codes: make(map[string]string)}
}

func (n *StubNotifier) Send(ctx context.Context, identityID string, channel Channel) (string, error) {
	code, err := randomDigits(6)
	if err != nil {
		return "", err
	}
	hash := hashCode(code)

	var challengeID string
	err = n.pool.QueryRow(ctx, `
		INSERT INTO mfa_challenges (identity_id, channel, code_hash, expires_at)
		VALUES ($1, $2, $3, now() + $4)
		RETURNING id::text
	`, identityID, channel, hash, challengeTTL).Scan(&challengeID)
	if err != nil {
		return "", fmt.Errorf("auth: create mfa challenge: %w", err)
	}

	n.mu.Lock()
	n.codes[challengeID] = code
	n.mu.Unlock()

	slog.InfoContext(ctx, "mfa challenge sent (stub, not a real send)",
		slog.String("identity_id", identityID), slog.String("channel", string(channel)), slog.String("challenge_id", challengeID))

	return challengeID, nil
}

// PeekCode is a stub-only test helper; no real Notifier could offer this.
func (n *StubNotifier) PeekCode(challengeID string) (string, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	code, ok := n.codes[challengeID]
	return code, ok
}

// VerifyChallenge validates code against a pending SMS/push challenge.
func VerifyChallenge(ctx context.Context, pool *pgxpool.Pool, challengeID, code string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("auth: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		codeHash   string
		expiresAt  time.Time
		consumedAt *time.Time
	)
	err = tx.QueryRow(ctx, `
		SELECT code_hash, expires_at, consumed_at FROM mfa_challenges WHERE id = $1 FOR UPDATE
	`, challengeID).Scan(&codeHash, &expiresAt, &consumedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidCode
		}
		return fmt.Errorf("auth: fetch challenge: %w", err)
	}

	if consumedAt != nil || time.Now().After(expiresAt) || hashCode(code) != codeHash {
		return ErrInvalidCode
	}

	if _, err := tx.Exec(ctx, `UPDATE mfa_challenges SET consumed_at = now() WHERE id = $1`, challengeID); err != nil {
		return fmt.Errorf("auth: consume challenge: %w", err)
	}

	return tx.Commit(ctx)
}

func hashCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

func randomDigits(n int) (string, error) {
	digits := make([]byte, n)
	if _, err := rand.Read(digits); err != nil {
		return "", fmt.Errorf("auth: generate code: %w", err)
	}
	out := make([]byte, n)
	for i, b := range digits {
		out[i] = '0' + b%10
	}
	return string(out), nil
}
