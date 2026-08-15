// Package risk implements real-time transaction monitoring
// (docs/banking-backend-spec.md §3.8): a rules engine that scores every
// external_transfers row before internal/externalpayments finalizes it,
// combining sanctions screening, a fraud-signal stand-in, velocity limits,
// and a large-amount check into one allow/hold/block decision. It adds no
// new money-movement primitives: a block reuses ledger.Service.ReleaseHold
// exactly as the rail-failure path in internal/externalpayments/process.go
// already does, and a hold simply leaves external_transfers.risk_status
// non-clear so ProcessPending's claim query skips it until an investigator
// resolves it via Approve or Block.
package risk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/kyc"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
)

// fraudMarker in a counterparty ref deterministically fires the
// anomaly/fraud rule, standing in for a real ML model the same way
// compliance.StubScreener's sanctionedMarker stands in for a real
// watchlist provider — a controllable simulation, not a production check.
const fraudMarker = "FRAUDSCORE"

// Velocity and large-amount thresholds. Deliberately generous enough that
// normal test fixtures don't trip them by accident, but low enough to
// exercise deterministically within a test's own transaction volume.
const (
	velocityWindow      = 10 * time.Minute
	velocityMaxCount    = 5
	velocityMaxSumMinor = 5_000_000_00
	largeAmountMinor    = 10_000_000_00
)

type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionHold  Decision = "hold"
	DecisionBlock Decision = "block"
)

// Screener matches internal/compliance.Screener's shape — reused as-is
// rather than redefined, so risk can screen a transaction counterparty
// the same way onboarding screens an applicant's legal name.
type Screener interface {
	Screen(ctx context.Context, name string) ([]kyc.CheckOutcome, error)
}

type Assessment struct {
	SourceID string
	Score    int
	Decision Decision
	Reasons  []string
}

type Service struct {
	pool      *pgxpool.Pool
	ledgerSvc *ledger.Service
	screener  Screener
	logger    *slog.Logger
}

func NewService(pool *pgxpool.Pool, ledgerSvc *ledger.Service, screener Screener, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{pool: pool, ledgerSvc: ledgerSvc, screener: screener, logger: logger}
}

type transferFields struct {
	AccountID       string
	CounterpartyRef string
	AmountMinor     int64
	HoldID          *string
}

// Assessed reports whether transferID has already been scored. Used by
// internal/externalpayments to tell "never assessed" apart from
// "held, then released by an investigator" — both look identical as
// risk_status = 'clear', but only the former should be scored by Assess;
// a released transfer must go straight to finalize instead of being
// re-scored back into the same stale hold decision.
func (s *Service) Assessed(ctx context.Context, transferID string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM risk_assessments WHERE source_type = 'external_transfer' AND source_id = $1)
	`, transferID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("risk: check assessed: %w", err)
	}
	return exists, nil
}

// Assess scores the external_transfers row transferID and records the
// decision. Idempotent: a second call for the same transferID returns the
// existing assessment (via risk_assessments' UNIQUE(source_type,
// source_id)) instead of re-scoring or double-releasing a hold.
func (s *Service) Assess(ctx context.Context, transferID string) (Assessment, error) {
	if existing, ok, err := s.getExisting(ctx, transferID); err != nil {
		return Assessment{}, err
	} else if ok {
		return existing, nil
	}

	tf, err := s.loadTransfer(ctx, transferID)
	if err != nil {
		return Assessment{}, err
	}

	assessment, err := s.evaluate(ctx, transferID, tf)
	if err != nil {
		return Assessment{}, err
	}

	if err := s.record(ctx, assessment); err != nil {
		return Assessment{}, err
	}

	switch assessment.Decision {
	case DecisionBlock:
		if err := s.failAndRelease(ctx, transferID, tf.HoldID, "blocked by risk assessment"); err != nil {
			return Assessment{}, err
		}
	case DecisionHold:
		if _, err := s.pool.Exec(ctx, `UPDATE external_transfers SET risk_status = 'held' WHERE id = $1`, transferID); err != nil {
			return Assessment{}, fmt.Errorf("risk: mark held: %w", err)
		}
	}

	return assessment, nil
}

func (s *Service) evaluate(ctx context.Context, transferID string, tf transferFields) (Assessment, error) {
	outcomes, err := s.screener.Screen(ctx, tf.CounterpartyRef)
	if err != nil {
		return Assessment{}, fmt.Errorf("risk: screen: %w", err)
	}
	for _, o := range outcomes {
		if o.CheckType == kyc.CheckSanctions && o.Result == kyc.ResultFail {
			return Assessment{SourceID: transferID, Score: 100, Decision: DecisionBlock, Reasons: []string{"sanctions_hit"}}, nil
		}
	}

	if strings.Contains(tf.CounterpartyRef, fraudMarker) {
		return Assessment{SourceID: transferID, Score: 100, Decision: DecisionBlock, Reasons: []string{"fraud_signal"}}, nil
	}

	count, sum, err := s.velocity(ctx, tf.AccountID)
	if err != nil {
		return Assessment{}, err
	}
	if count >= velocityMaxCount || sum >= velocityMaxSumMinor {
		return Assessment{SourceID: transferID, Score: 70, Decision: DecisionHold, Reasons: []string{"velocity_exceeded"}}, nil
	}

	if tf.AmountMinor >= largeAmountMinor {
		return Assessment{SourceID: transferID, Score: 50, Decision: DecisionHold, Reasons: []string{"large_amount"}}, nil
	}

	score := int(tf.AmountMinor * 20 / largeAmountMinor)
	if score > 49 {
		score = 49
	}
	return Assessment{SourceID: transferID, Score: score, Decision: DecisionAllow, Reasons: nil}, nil
}

// velocity counts and sums accountID's external_transfers created within
// the trailing velocityWindow, including the just-claimed row itself.
func (s *Service) velocity(ctx context.Context, accountID string) (count int, sumMinor int64, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(amount_minor), 0)
		FROM external_transfers
		WHERE account_id = $1 AND created_at >= now() - make_interval(secs => $2)
	`, accountID, velocityWindow.Seconds()).Scan(&count, &sumMinor)
	if err != nil {
		return 0, 0, fmt.Errorf("risk: velocity: %w", err)
	}
	return count, sumMinor, nil
}

func (s *Service) loadTransfer(ctx context.Context, transferID string) (transferFields, error) {
	var tf transferFields
	err := s.pool.QueryRow(ctx, `
		SELECT account_id::text, counterparty_ref, amount_minor, hold_id::text
		FROM external_transfers WHERE id = $1
	`, transferID).Scan(&tf.AccountID, &tf.CounterpartyRef, &tf.AmountMinor, &tf.HoldID)
	if err != nil {
		return transferFields{}, fmt.Errorf("risk: load transfer %s: %w", transferID, err)
	}
	return tf, nil
}

func (s *Service) record(ctx context.Context, a Assessment) error {
	reasons, err := json.Marshal(a.Reasons)
	if err != nil {
		return fmt.Errorf("risk: marshal reasons: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO risk_assessments (source_type, source_id, score, decision, reasons)
		VALUES ('external_transfer', $1, $2, $3, $4)
	`, a.SourceID, a.Score, a.Decision, reasons)
	if err != nil {
		return fmt.Errorf("risk: record assessment: %w", err)
	}
	return nil
}

func (s *Service) getExisting(ctx context.Context, transferID string) (Assessment, bool, error) {
	var a Assessment
	var reasons []byte
	err := s.pool.QueryRow(ctx, `
		SELECT source_id::text, score, decision, reasons FROM risk_assessments
		WHERE source_type = 'external_transfer' AND source_id = $1
	`, transferID).Scan(&a.SourceID, &a.Score, &a.Decision, &reasons)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Assessment{}, false, nil
		}
		return Assessment{}, false, fmt.Errorf("risk: lookup assessment: %w", err)
	}
	if err := json.Unmarshal(reasons, &a.Reasons); err != nil {
		return Assessment{}, false, fmt.Errorf("risk: unmarshal reasons: %w", err)
	}
	return a, true, nil
}

// failAndRelease marks transferID failed and releases any placed hold —
// the identical shape to the rail-failure path in
// internal/externalpayments/process.go's finalize.
func (s *Service) failAndRelease(ctx context.Context, transferID string, holdID *string, reason string) error {
	if holdID != nil {
		if err := s.ledgerSvc.ReleaseHold(ctx, *holdID); err != nil {
			return fmt.Errorf("risk: release hold: %w", err)
		}
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE external_transfers SET status = 'failed', risk_status = 'blocked', error = $2, completed_at = now() WHERE id = $1
	`, transferID, reason)
	if err != nil {
		return fmt.Errorf("risk: mark blocked: %w", err)
	}
	return nil
}
