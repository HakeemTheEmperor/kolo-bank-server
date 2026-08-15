package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrNotFound is returned when an identity or beneficial owner lookup
	// finds nothing.
	ErrNotFound = errors.New("identity: not found")
	// ErrInvalidTransition is returned when a status change is not allowed
	// by the state machine.
	ErrInvalidTransition = errors.New("identity: invalid status transition")
	// ErrEmailTaken is returned when registering with an email already in use.
	ErrEmailTaken = errors.New("identity: email already registered")
)

const identityColumns = `id::text, kind, status, kyc_tier, email, phone, legal_name, role, created_at, updated_at`

// Service reads identities via its own pool. Writes that must commit
// atomically with other work (onboarding, KYC checks, audit log) take an
// explicit pgx.Tx instead, so callers control the transaction boundary.
type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// InsertIndividual creates a pending individual identity within tx.
func InsertIndividual(ctx context.Context, tx pgx.Tx, email string, phone *string, passwordHash, legalName string) (Identity, error) {
	return insert(ctx, tx, KindIndividual, email, phone, passwordHash, legalName)
}

// InsertBusiness creates a pending business identity within tx.
func InsertBusiness(ctx context.Context, tx pgx.Tx, email string, phone *string, passwordHash, legalName string) (Identity, error) {
	return insert(ctx, tx, KindBusiness, email, phone, passwordHash, legalName)
}

func insert(ctx context.Context, tx pgx.Tx, kind Kind, email string, phone *string, passwordHash, legalName string) (Identity, error) {
	var id Identity
	err := tx.QueryRow(ctx, `
		INSERT INTO identities (kind, email, phone, password_hash, legal_name)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+identityColumns,
		kind, email, phone, passwordHash, legalName,
	).Scan(&id.ID, &id.Kind, &id.Status, &id.KYCTier, &id.Email, &id.Phone, &id.LegalName, &id.Role, &id.CreatedAt, &id.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return Identity{}, ErrEmailTaken
		}
		return Identity{}, fmt.Errorf("identity: insert: %w", err)
	}
	return id, nil
}

// InsertBeneficialOwner records one beneficial owner for a business
// identity within tx.
func InsertBeneficialOwner(ctx context.Context, tx pgx.Tx, businessIdentityID, fullName string, ownershipPercent float64) (BeneficialOwner, error) {
	var bo BeneficialOwner
	err := tx.QueryRow(ctx, `
		INSERT INTO beneficial_owners (business_identity_id, full_name, ownership_percent)
		VALUES ($1, $2, $3)
		RETURNING id::text, business_identity_id::text, full_name, ownership_percent, created_at
	`, businessIdentityID, fullName, ownershipPercent).Scan(
		&bo.ID, &bo.BusinessIdentityID, &bo.FullName, &bo.OwnershipPercent, &bo.CreatedAt,
	)
	if err != nil {
		return BeneficialOwner{}, fmt.Errorf("identity: insert beneficial owner: %w", err)
	}
	return bo, nil
}

// SetStatusAndTier updates status and kyc_tier within tx, enforcing the
// status state machine.
func SetStatusAndTier(ctx context.Context, tx pgx.Tx, identityID string, target Status, kycTier int) error {
	var current Status
	if err := tx.QueryRow(ctx, `SELECT status FROM identities WHERE id = $1 FOR UPDATE`, identityID).Scan(&current); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("identity: lock identity: %w", err)
	}

	if current != target && !current.CanTransitionTo(target) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, current, target)
	}

	if _, err := tx.Exec(ctx, `UPDATE identities SET status = $2, kyc_tier = $3 WHERE id = $1`, identityID, target, kycTier); err != nil {
		return fmt.Errorf("identity: update status: %w", err)
	}
	return nil
}

func (s *Service) GetByID(ctx context.Context, id string) (Identity, error) {
	return s.get(ctx, `id = $1`, id)
}

func (s *Service) GetByEmail(ctx context.Context, email string) (Identity, error) {
	return s.get(ctx, `email = $1`, email)
}

func (s *Service) get(ctx context.Context, where string, arg any) (Identity, error) {
	var id Identity
	err := s.pool.QueryRow(ctx, `SELECT `+identityColumns+` FROM identities WHERE `+where, arg).Scan(
		&id.ID, &id.Kind, &id.Status, &id.KYCTier, &id.Email, &id.Phone, &id.LegalName, &id.Role, &id.CreatedAt, &id.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Identity{}, ErrNotFound
		}
		return Identity{}, fmt.Errorf("identity: get: %w", err)
	}
	return id, nil
}

// PasswordHash fetches the password hash for email, used only by
// internal/auth's login flow.
func (s *Service) PasswordHash(ctx context.Context, email string) (identityID, hash string, err error) {
	err = s.pool.QueryRow(ctx, `SELECT id::text, password_hash FROM identities WHERE email = $1`, email).Scan(&identityID, &hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", ErrNotFound
		}
		return "", "", fmt.Errorf("identity: get password hash: %w", err)
	}
	return identityID, hash, nil
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}
