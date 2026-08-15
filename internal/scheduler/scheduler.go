// Package scheduler implements scheduled and recurring transfers / standing
// orders (docs/banking-backend-spec.md §3.4) and the in-flight resolver
// (§4.4) that guarantees a firing is never left permanently unresolved.
//
// Each firing claims a scheduled_transfer_runs row (unique per
// (schedule, occurrence), so concurrent claims can't double-fire), calls
// payments.Transfer with a deterministic idempotency key derived from the
// schedule id and occurrence time, then records the outcome and advances
// the schedule — all as two separate commits. The gap between them is a
// real crash window: ResolveStuck finds runs still "processing" past a
// timeout and retries the same deterministic key, which either executes
// for the first time or replays the already-committed result via the
// ledger's idempotency layer (internal/idempotency) — either way producing
// a definitive outcome instead of a permanently unknown one.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/payments"
)

type ScheduleType string

const (
	ScheduleOnce      ScheduleType = "once"
	ScheduleRecurring ScheduleType = "recurring"
)

type Interval string

const (
	IntervalDaily   Interval = "daily"
	IntervalWeekly  Interval = "weekly"
	IntervalMonthly Interval = "monthly"
)

type Status string

const (
	StatusActive    Status = "active"
	StatusPaused    Status = "paused"
	StatusCompleted Status = "completed"
	StatusCancelled Status = "cancelled"
)

// stuckTimeout is how long a run may sit in "processing" before
// ResolveStuck treats it as crashed and retries it.
const stuckTimeout = 2 * time.Minute

type ScheduledTransfer struct {
	ID            string
	FromAccountID string
	ToAccountID   string
	Amount        ledger.Money
	ScheduleType  ScheduleType
	Interval      *Interval
	NextRunAt     time.Time
	Status        Status
}

type Service struct {
	pool        *pgxpool.Pool
	paymentsSvc *payments.Service
	logger      *slog.Logger
}

func NewService(pool *pgxpool.Pool, paymentsSvc *payments.Service, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{pool: pool, paymentsSvc: paymentsSvc, logger: logger}
}

// Create schedules a one-off or recurring transfer. For "once", interval
// must be nil; for "recurring", it must be set.
func (s *Service) Create(ctx context.Context, fromAccountID, toAccountID string, amount ledger.Money, scheduleType ScheduleType, interval *Interval, firstRunAt time.Time) (ScheduledTransfer, error) {
	var st ScheduledTransfer
	err := s.pool.QueryRow(ctx, `
		INSERT INTO scheduled_transfers (from_account_id, to_account_id, amount_minor, currency, schedule_type, interval, next_run_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id::text, from_account_id::text, to_account_id::text, amount_minor, currency, schedule_type, interval, next_run_at, status
	`, fromAccountID, toAccountID, amount.Minor, amount.Currency, scheduleType, interval, firstRunAt).Scan(
		&st.ID, &st.FromAccountID, &st.ToAccountID, &st.Amount.Minor, &st.Amount.Currency,
		&st.ScheduleType, &st.Interval, &st.NextRunAt, &st.Status,
	)
	if err != nil {
		return ScheduledTransfer{}, fmt.Errorf("scheduler: create: %w", err)
	}
	return st, nil
}

// RunDue executes every active schedule whose next_run_at has arrived.
func (s *Service) RunDue(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, from_account_id::text, to_account_id::text, amount_minor, currency, schedule_type, interval, next_run_at, status
		FROM scheduled_transfers
		WHERE status = 'active' AND next_run_at <= now()
		ORDER BY next_run_at
		LIMIT 100
	`)
	if err != nil {
		return fmt.Errorf("scheduler: find due schedules: %w", err)
	}

	var due []ScheduledTransfer
	for rows.Next() {
		var st ScheduledTransfer
		if err := rows.Scan(&st.ID, &st.FromAccountID, &st.ToAccountID, &st.Amount.Minor, &st.Amount.Currency,
			&st.ScheduleType, &st.Interval, &st.NextRunAt, &st.Status); err != nil {
			rows.Close()
			return fmt.Errorf("scheduler: scan due schedule: %w", err)
		}
		due = append(due, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("scheduler: iterate due schedules: %w", err)
	}

	for _, st := range due {
		if err := s.fireOnce(ctx, st, st.NextRunAt); err != nil {
			s.logger.ErrorContext(ctx, "scheduler: fire failed", slog.String("scheduled_transfer_id", st.ID), slog.Any("error", err))
		}
	}
	return nil
}

// fireOnce claims the run for (st, scheduledFor); if another caller already
// claimed it, it's a no-op.
func (s *Service) fireOnce(ctx context.Context, st ScheduledTransfer, scheduledFor time.Time) error {
	var runID string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO scheduled_transfer_runs (scheduled_transfer_id, scheduled_for, status)
		VALUES ($1, $2, 'processing')
		ON CONFLICT (scheduled_transfer_id, scheduled_for) DO NOTHING
		RETURNING id::text
	`, st.ID, scheduledFor).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // already claimed by a concurrent caller
	}
	if err != nil {
		return fmt.Errorf("scheduler: claim run: %w", err)
	}

	return s.executeAndFinalize(ctx, runID, st, scheduledFor)
}

// executeAndFinalize runs the transfer under a deterministic idempotency
// key and records the outcome. Safe to call more than once for the same
// (st, scheduledFor) — that's exactly what ResolveStuck relies on.
func (s *Service) executeAndFinalize(ctx context.Context, runID string, st ScheduledTransfer, scheduledFor time.Time) error {
	runKey := fmt.Sprintf("scheduled:%s:%d", st.ID, scheduledFor.Unix())
	txn, transferErr := s.paymentsSvc.Transfer(ctx, st.FromAccountID, st.ToAccountID, st.Amount, runKey)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("scheduler: begin finalize tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if transferErr == nil {
		if _, err := tx.Exec(ctx, `
			UPDATE scheduled_transfer_runs SET status = 'completed', transaction_id = $2, completed_at = now() WHERE id = $1
		`, runID, txn.ID); err != nil {
			return fmt.Errorf("scheduler: mark run completed: %w", err)
		}
	} else {
		if _, err := tx.Exec(ctx, `
			UPDATE scheduled_transfer_runs SET status = 'failed', error = $2, completed_at = now() WHERE id = $1
		`, runID, transferErr.Error()); err != nil {
			return fmt.Errorf("scheduler: mark run failed: %w", err)
		}
	}

	if st.ScheduleType == ScheduleOnce {
		if _, err := tx.Exec(ctx, `UPDATE scheduled_transfers SET status = 'completed' WHERE id = $1`, st.ID); err != nil {
			return fmt.Errorf("scheduler: complete schedule: %w", err)
		}
	} else {
		next, err := advance(scheduledFor, st.Interval)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE scheduled_transfers SET next_run_at = $2 WHERE id = $1`, st.ID, next); err != nil {
			return fmt.Errorf("scheduler: advance schedule: %w", err)
		}
	}

	return tx.Commit(ctx)
}

// ResolveStuck is the in-flight resolver: it finds runs still "processing"
// past stuckTimeout and retries them to a definitive outcome.
func (s *Service) ResolveStuck(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `
		SELECT r.id::text, r.scheduled_for,
		       st.id::text, st.from_account_id::text, st.to_account_id::text, st.amount_minor, st.currency,
		       st.schedule_type, st.interval, st.next_run_at, st.status
		FROM scheduled_transfer_runs r
		JOIN scheduled_transfers st ON st.id = r.scheduled_transfer_id
		WHERE r.status = 'processing' AND r.attempted_at < now() - make_interval(secs => $1)
	`, stuckTimeout.Seconds())
	if err != nil {
		return fmt.Errorf("scheduler: find stuck runs: %w", err)
	}

	type stuckRun struct {
		runID        string
		scheduledFor time.Time
		st           ScheduledTransfer
	}
	var stuck []stuckRun
	for rows.Next() {
		var r stuckRun
		if err := rows.Scan(&r.runID, &r.scheduledFor,
			&r.st.ID, &r.st.FromAccountID, &r.st.ToAccountID, &r.st.Amount.Minor, &r.st.Amount.Currency,
			&r.st.ScheduleType, &r.st.Interval, &r.st.NextRunAt, &r.st.Status); err != nil {
			rows.Close()
			return fmt.Errorf("scheduler: scan stuck run: %w", err)
		}
		stuck = append(stuck, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("scheduler: iterate stuck runs: %w", err)
	}

	for _, r := range stuck {
		s.logger.WarnContext(ctx, "scheduler: resolving stuck run", slog.String("run_id", r.runID), slog.String("scheduled_transfer_id", r.st.ID))
		if err := s.executeAndFinalize(ctx, r.runID, r.st, r.scheduledFor); err != nil {
			s.logger.ErrorContext(ctx, "scheduler: resolve stuck run failed", slog.String("run_id", r.runID), slog.Any("error", err))
		}
	}
	return nil
}

func advance(from time.Time, interval *Interval) (time.Time, error) {
	if interval == nil {
		return time.Time{}, fmt.Errorf("scheduler: recurring schedule missing interval")
	}
	switch *interval {
	case IntervalDaily:
		return from.AddDate(0, 0, 1), nil
	case IntervalWeekly:
		return from.AddDate(0, 0, 7), nil
	case IntervalMonthly:
		return from.AddDate(0, 1, 0), nil
	default:
		return time.Time{}, fmt.Errorf("scheduler: unknown interval %q", *interval)
	}
}
