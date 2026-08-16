package bills

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/resilience"
)

// PayBill validates reference, then routes the payment through the
// "billpay" rail via externalpayments.Service.SendOutbound. Idempotent:
// retrying with the same idempotencyKey (including two concurrent
// RunDueBills ticks racing the same recurring firing) returns the original
// bill_payments row rather than paying twice.
func (s *Service) PayBill(ctx context.Context, accountID, billerID, reference string, amount ledger.Money, idempotencyKey string) (BillPayment, error) {
	if existing, ok, err := s.getByIdempotencyKey(ctx, idempotencyKey); err != nil {
		return BillPayment{}, err
	} else if ok {
		return existing, nil
	}

	if err := s.resilienceSvc.Check(ctx, resilience.Feature("bill_payment")); err != nil {
		return BillPayment{}, err
	}

	valid, _, err := s.ValidateReference(ctx, billerID, reference)
	if err != nil {
		return BillPayment{}, err
	}
	if !valid {
		return BillPayment{}, ErrInvalidReference
	}

	billerCode, err := s.getBillerCode(ctx, billerID)
	if err != nil {
		return BillPayment{}, err
	}

	et, err := s.externalSvc.SendOutbound(ctx, accountID, "billpay", billerCode+":"+reference, amount, idempotencyKey)
	if err != nil {
		return BillPayment{}, fmt.Errorf("bills: send payment: %w", err)
	}

	var bp BillPayment
	err = s.pool.QueryRow(ctx, `
		INSERT INTO bill_payments (biller_id, account_id, reference, amount_minor, currency, idempotency_key, external_transfer_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (idempotency_key) DO UPDATE SET idempotency_key = EXCLUDED.idempotency_key
		RETURNING id::text, biller_id::text, account_id::text, reference, amount_minor, currency, idempotency_key, external_transfer_id::text, created_at
	`, billerID, accountID, reference, amount.Minor, amount.Currency, idempotencyKey, et.ID).Scan(
		&bp.ID, &bp.BillerID, &bp.AccountID, &bp.Reference, &bp.Amount.Minor, &bp.Amount.Currency,
		&bp.IdempotencyKey, &bp.ExternalTransferID, &bp.CreatedAt,
	)
	if err != nil {
		return BillPayment{}, fmt.Errorf("bills: record payment: %w", err)
	}
	return bp, nil
}

func (s *Service) getByIdempotencyKey(ctx context.Context, idempotencyKey string) (BillPayment, bool, error) {
	var bp BillPayment
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, biller_id::text, account_id::text, reference, amount_minor, currency, idempotency_key, external_transfer_id::text, created_at
		FROM bill_payments WHERE idempotency_key = $1
	`, idempotencyKey).Scan(
		&bp.ID, &bp.BillerID, &bp.AccountID, &bp.Reference, &bp.Amount.Minor, &bp.Amount.Currency,
		&bp.IdempotencyKey, &bp.ExternalTransferID, &bp.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return BillPayment{}, false, nil
		}
		return BillPayment{}, false, fmt.Errorf("bills: lookup by idempotency key: %w", err)
	}
	return bp, true, nil
}

// CreateRecurring schedules a recurring bill payment.
func (s *Service) CreateRecurring(ctx context.Context, accountID, billerID, reference string, amount ledger.Money, interval Interval, firstRunAt time.Time) (RecurringBillPayment, error) {
	var r RecurringBillPayment
	err := s.pool.QueryRow(ctx, `
		INSERT INTO recurring_bill_payments (biller_id, account_id, reference, amount_minor, currency, interval, next_run_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id::text, biller_id::text, account_id::text, reference, amount_minor, currency, interval, next_run_at, status
	`, billerID, accountID, reference, amount.Minor, amount.Currency, interval, firstRunAt).Scan(
		&r.ID, &r.BillerID, &r.AccountID, &r.Reference, &r.Amount.Minor, &r.Amount.Currency, &r.Interval, &r.NextRunAt, &r.Status,
	)
	if err != nil {
		return RecurringBillPayment{}, fmt.Errorf("bills: create recurring: %w", err)
	}
	return r, nil
}

// RunDueBills fires every active recurring bill payment whose next_run_at
// has arrived. Concurrency safety comes from bill_payments.idempotency_key
// being unique, not from claiming rows here — PayBill itself is what
// guards against double-firing.
func (s *Service) RunDueBills(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, biller_id::text, account_id::text, reference, amount_minor, currency, interval, next_run_at
		FROM recurring_bill_payments
		WHERE status = 'active' AND next_run_at <= now()
		ORDER BY next_run_at
		LIMIT 100
	`)
	if err != nil {
		return fmt.Errorf("bills: find due recurring payments: %w", err)
	}

	var due []RecurringBillPayment
	for rows.Next() {
		var r RecurringBillPayment
		if err := rows.Scan(&r.ID, &r.BillerID, &r.AccountID, &r.Reference, &r.Amount.Minor, &r.Amount.Currency, &r.Interval, &r.NextRunAt); err != nil {
			rows.Close()
			return fmt.Errorf("bills: scan due recurring payment: %w", err)
		}
		due = append(due, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("bills: iterate due recurring payments: %w", err)
	}

	for _, r := range due {
		idemKey := fmt.Sprintf("recurring-bill:%s:%d", r.ID, r.NextRunAt.Unix())
		if _, err := s.PayBill(ctx, r.AccountID, r.BillerID, r.Reference, r.Amount, idemKey); err != nil {
			slog.ErrorContext(ctx, "bills: recurring payment failed, will retry next cycle", slog.String("recurring_id", r.ID), slog.Any("error", err))
		}

		next, err := advance(r.NextRunAt, r.Interval)
		if err != nil {
			slog.ErrorContext(ctx, "bills: advance schedule failed", slog.String("recurring_id", r.ID), slog.Any("error", err))
			continue
		}
		if _, err := s.pool.Exec(ctx, `UPDATE recurring_bill_payments SET next_run_at = $2 WHERE id = $1`, r.ID, next); err != nil {
			slog.ErrorContext(ctx, "bills: persist advanced schedule failed", slog.String("recurring_id", r.ID), slog.Any("error", err))
		}
	}
	return nil
}

func advance(from time.Time, interval Interval) (time.Time, error) {
	switch interval {
	case IntervalDaily:
		return from.AddDate(0, 0, 1), nil
	case IntervalWeekly:
		return from.AddDate(0, 0, 7), nil
	case IntervalMonthly:
		return from.AddDate(0, 1, 0), nil
	default:
		return time.Time{}, fmt.Errorf("bills: unknown interval %q", interval)
	}
}
