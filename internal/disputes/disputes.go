// Package disputes implements dispute and case management for back-office
// investigators (docs/banking-backend-spec.md §3.8). A dispute moves
// through a fixed state machine — open -> investigating -> resolved or
// rejected — with every transition recorded as an append-only
// dispute_events row, the same "record what happened, never overwrite"
// approach as internal/audit.
package disputes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Status string

const (
	StatusOpen          Status = "open"
	StatusInvestigating Status = "investigating"
	StatusResolved      Status = "resolved"
	StatusRejected      Status = "rejected"
)

// ErrInvalidTransition is returned when Advance is asked to move a
// dispute to a status that isn't reachable from its current one.
var ErrInvalidTransition = errors.New("disputes: invalid status transition")

// validTransitions enumerates the case workflow: open can move to
// investigating or be rejected outright; investigating resolves either way;
// resolved and rejected are terminal.
var validTransitions = map[Status][]Status{
	StatusOpen:          {StatusInvestigating, StatusRejected},
	StatusInvestigating: {StatusResolved, StatusRejected},
}

type Dispute struct {
	ID         string
	IdentityID string
	SourceType string
	SourceID   string
	Reason     string
	Status     Status
	Resolution *string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type Service struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

func NewService(pool *pgxpool.Pool, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{pool: pool, logger: logger}
}

// Create opens a new dispute for identityID against the given source
// record (a charge, payout, or external transfer).
func (s *Service) Create(ctx context.Context, identityID, sourceType, sourceID, reason string) (Dispute, error) {
	var d Dispute
	err := s.pool.QueryRow(ctx, `
		INSERT INTO disputes (identity_id, source_type, source_id, reason)
		VALUES ($1, $2, $3, $4)
		RETURNING id::text, identity_id::text, source_type, source_id::text, reason, status, resolution, created_at, updated_at
	`, identityID, sourceType, sourceID, reason).Scan(
		&d.ID, &d.IdentityID, &d.SourceType, &d.SourceID, &d.Reason, &d.Status, &d.Resolution, &d.CreatedAt, &d.UpdatedAt,
	)
	if err != nil {
		return Dispute{}, fmt.Errorf("disputes: create: %w", err)
	}
	return d, nil
}

// Advance moves disputeID to newStatus, recording a dispute_events row for
// the transition. resolution is stored only when newStatus is a terminal
// status (resolved/rejected); it's ignored otherwise.
func (s *Service) Advance(ctx context.Context, disputeID string, newStatus Status, note string) (Dispute, error) {
	current, err := s.Get(ctx, disputeID)
	if err != nil {
		return Dispute{}, err
	}

	if !isValidTransition(current.Status, newStatus) {
		return Dispute{}, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, current.Status, newStatus)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Dispute{}, fmt.Errorf("disputes: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var d Dispute
	if newStatus == StatusResolved || newStatus == StatusRejected {
		err = tx.QueryRow(ctx, `
			UPDATE disputes SET status = $2, resolution = $3 WHERE id = $1
			RETURNING id::text, identity_id::text, source_type, source_id::text, reason, status, resolution, created_at, updated_at
		`, disputeID, newStatus, note).Scan(
			&d.ID, &d.IdentityID, &d.SourceType, &d.SourceID, &d.Reason, &d.Status, &d.Resolution, &d.CreatedAt, &d.UpdatedAt,
		)
	} else {
		err = tx.QueryRow(ctx, `
			UPDATE disputes SET status = $2 WHERE id = $1
			RETURNING id::text, identity_id::text, source_type, source_id::text, reason, status, resolution, created_at, updated_at
		`, disputeID, newStatus).Scan(
			&d.ID, &d.IdentityID, &d.SourceType, &d.SourceID, &d.Reason, &d.Status, &d.Resolution, &d.CreatedAt, &d.UpdatedAt,
		)
	}
	if err != nil {
		return Dispute{}, fmt.Errorf("disputes: update status: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO dispute_events (dispute_id, from_status, to_status, note)
		VALUES ($1, $2, $3, $4)
	`, disputeID, current.Status, newStatus, note); err != nil {
		return Dispute{}, fmt.Errorf("disputes: record event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Dispute{}, fmt.Errorf("disputes: commit: %w", err)
	}
	return d, nil
}

func (s *Service) Get(ctx context.Context, disputeID string) (Dispute, error) {
	var d Dispute
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, identity_id::text, source_type, source_id::text, reason, status, resolution, created_at, updated_at
		FROM disputes WHERE id = $1
	`, disputeID).Scan(
		&d.ID, &d.IdentityID, &d.SourceType, &d.SourceID, &d.Reason, &d.Status, &d.Resolution, &d.CreatedAt, &d.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Dispute{}, fmt.Errorf("disputes: %s: %w", disputeID, pgx.ErrNoRows)
		}
		return Dispute{}, fmt.Errorf("disputes: get: %w", err)
	}
	return d, nil
}

// ListByIdentity returns identityID's disputes, newest first.
func (s *Service) ListByIdentity(ctx context.Context, identityID string, limit int) ([]Dispute, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, identity_id::text, source_type, source_id::text, reason, status, resolution, created_at, updated_at
		FROM disputes WHERE identity_id = $1
		ORDER BY created_at DESC
		LIMIT $2
	`, identityID, limit)
	if err != nil {
		return nil, fmt.Errorf("disputes: list: %w", err)
	}
	defer rows.Close()

	var out []Dispute
	for rows.Next() {
		var d Dispute
		if err := rows.Scan(&d.ID, &d.IdentityID, &d.SourceType, &d.SourceID, &d.Reason, &d.Status, &d.Resolution, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("disputes: scan: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func isValidTransition(from, to Status) bool {
	for _, allowed := range validTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}
