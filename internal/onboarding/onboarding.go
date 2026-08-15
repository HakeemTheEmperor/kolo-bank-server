// Package onboarding orchestrates identity registration, KYC, and
// compliance screening as one atomic flow
// (docs/banking-backend-spec.md §3.1, Phase 2 exit criteria: "a user
// completes onboarding through KYC to an active, tiered account").
package onboarding

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/audit"
	"github.com/toluwalase/kolo-bank-server/internal/compliance"
	"github.com/toluwalase/kolo-bank-server/internal/identity"
	"github.com/toluwalase/kolo-bank-server/internal/kyc"
)

type BeneficialOwnerInput struct {
	FullName         string
	OwnershipPercent float64
}

type Service struct {
	pool     *pgxpool.Pool
	kycProv  kyc.Provider
	screener compliance.Screener
}

func NewService(pool *pgxpool.Pool, kycProv kyc.Provider, screener compliance.Screener) *Service {
	return &Service{pool: pool, kycProv: kycProv, screener: screener}
}

// RegisterIndividual creates an identity, runs KYC and compliance checks,
// and activates or rejects it, all in one transaction.
func (s *Service) RegisterIndividual(ctx context.Context, email string, phone *string, passwordHash, legalName, address string) (identity.Identity, error) {
	return s.register(ctx, func(ctx context.Context, tx pgx.Tx) (identity.Identity, error) {
		return identity.InsertIndividual(ctx, tx, email, phone, passwordHash, legalName)
	}, legalName, address, nil)
}

// RegisterBusiness creates a business identity plus its beneficial owners,
// runs KYC and compliance checks, and activates or rejects it, all in one
// transaction.
func (s *Service) RegisterBusiness(ctx context.Context, email string, phone *string, passwordHash, legalName, address string, owners []BeneficialOwnerInput) (identity.Identity, error) {
	return s.register(ctx, func(ctx context.Context, tx pgx.Tx) (identity.Identity, error) {
		return identity.InsertBusiness(ctx, tx, email, phone, passwordHash, legalName)
	}, legalName, address, owners)
}

func (s *Service) register(
	ctx context.Context,
	insert func(ctx context.Context, tx pgx.Tx) (identity.Identity, error),
	legalName, address string,
	owners []BeneficialOwnerInput,
) (identity.Identity, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return identity.Identity{}, fmt.Errorf("onboarding: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	id, err := insert(ctx, tx)
	if err != nil {
		return identity.Identity{}, err
	}

	for _, o := range owners {
		if _, err := identity.InsertBeneficialOwner(ctx, tx, id.ID, o.FullName, o.OwnershipPercent); err != nil {
			return identity.Identity{}, err
		}
	}

	if err := audit.Record(ctx, tx, &id.ID, "identity.registered", "identity", id.ID, map[string]any{
		"kind": string(id.Kind),
	}); err != nil {
		return identity.Identity{}, err
	}

	kycOutcomes, err := s.kycProv.Verify(ctx, kyc.Applicant{LegalName: legalName, Address: address})
	if err != nil {
		return identity.Identity{}, fmt.Errorf("onboarding: run kyc: %w", err)
	}
	if err := kyc.RecordChecks(ctx, tx, id.ID, kycOutcomes); err != nil {
		return identity.Identity{}, err
	}

	screenOutcomes, err := s.screener.Screen(ctx, legalName)
	if err != nil {
		return identity.Identity{}, fmt.Errorf("onboarding: run compliance screen: %w", err)
	}
	if err := kyc.RecordChecks(ctx, tx, id.ID, screenOutcomes); err != nil {
		return identity.Identity{}, err
	}

	allOutcomes := append(append([]kyc.CheckOutcome{}, kycOutcomes...), screenOutcomes...)
	tier, overall := kyc.Evaluate(allOutcomes)

	var (
		targetStatus identity.Status
		auditAction  string
	)
	switch overall {
	case kyc.ResultPass:
		targetStatus, auditAction = identity.StatusActive, "identity.activated"
	case kyc.ResultFail:
		targetStatus, auditAction = identity.StatusRejected, "identity.rejected"
	default: // review: leave pending for manual review (Phase 11 back office)
		targetStatus, auditAction, tier = identity.StatusPending, "identity.pending_review", 0
	}

	if targetStatus != identity.StatusPending {
		if err := identity.SetStatusAndTier(ctx, tx, id.ID, targetStatus, tier); err != nil {
			return identity.Identity{}, err
		}
		id.Status = targetStatus
		id.KYCTier = tier
	}

	if err := audit.Record(ctx, tx, &id.ID, auditAction, "identity", id.ID, map[string]any{
		"tier":    tier,
		"overall": string(overall),
	}); err != nil {
		return identity.Identity{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return identity.Identity{}, fmt.Errorf("onboarding: commit: %w", err)
	}

	return id, nil
}
