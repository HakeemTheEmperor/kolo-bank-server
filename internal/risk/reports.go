package risk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type ReportType string

const (
	ReportSAR ReportType = "sar"
	ReportCTR ReportType = "ctr"
)

type Report struct {
	ID          string
	ReportType  ReportType
	PeriodStart time.Time
	PeriodEnd   time.Time
	GeneratedAt time.Time
	Payload     json.RawMessage
}

type sarLine struct {
	TransferID string   `json:"transfer_id"`
	Score      int      `json:"score"`
	Reasons    []string `json:"reasons"`
}

type ctrLine struct {
	TransferID  string `json:"transfer_id"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

// GenerateSARReport aggregates risk_assessments blocked within
// [periodStart, periodEnd) into one suspicious-activity-report-equivalent
// row (docs/banking-backend-spec.md §3.8: "AML transaction reporting
// (SAR/CTR-equivalent)"). Idempotent via regulatory_reports'
// UNIQUE(report_type, period_start, period_end): regenerating for the same
// period returns the existing report instead of duplicating.
func (s *Service) GenerateSARReport(ctx context.Context, periodStart, periodEnd time.Time) (Report, error) {
	if existing, ok, err := s.getExistingReport(ctx, ReportSAR, periodStart, periodEnd); err != nil {
		return Report{}, err
	} else if ok {
		return existing, nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT source_id::text, score, reasons FROM risk_assessments
		WHERE decision = 'block' AND created_at >= $1 AND created_at < $2
		ORDER BY created_at
	`, periodStart, periodEnd)
	if err != nil {
		return Report{}, fmt.Errorf("risk: query blocked assessments: %w", err)
	}
	defer rows.Close()

	var lines []sarLine
	for rows.Next() {
		var l sarLine
		var reasons []byte
		if err := rows.Scan(&l.TransferID, &l.Score, &reasons); err != nil {
			return Report{}, fmt.Errorf("risk: scan sar line: %w", err)
		}
		if err := json.Unmarshal(reasons, &l.Reasons); err != nil {
			return Report{}, fmt.Errorf("risk: unmarshal reasons: %w", err)
		}
		lines = append(lines, l)
	}
	if err := rows.Err(); err != nil {
		return Report{}, err
	}

	return s.insertReport(ctx, ReportSAR, periodStart, periodEnd, map[string]any{"flagged_transfers": lines})
}

// GenerateCTRReport aggregates external_transfers at or above
// largeAmountMinor within the period into a currency-transaction-report
// equivalent.
func (s *Service) GenerateCTRReport(ctx context.Context, periodStart, periodEnd time.Time) (Report, error) {
	if existing, ok, err := s.getExistingReport(ctx, ReportCTR, periodStart, periodEnd); err != nil {
		return Report{}, err
	} else if ok {
		return existing, nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id::text, amount_minor, currency FROM external_transfers
		WHERE amount_minor >= $1 AND created_at >= $2 AND created_at < $3
		ORDER BY created_at
	`, largeAmountMinor, periodStart, periodEnd)
	if err != nil {
		return Report{}, fmt.Errorf("risk: query large transfers: %w", err)
	}
	defer rows.Close()

	var lines []ctrLine
	for rows.Next() {
		var l ctrLine
		if err := rows.Scan(&l.TransferID, &l.AmountMinor, &l.Currency); err != nil {
			return Report{}, fmt.Errorf("risk: scan ctr line: %w", err)
		}
		lines = append(lines, l)
	}
	if err := rows.Err(); err != nil {
		return Report{}, err
	}

	return s.insertReport(ctx, ReportCTR, periodStart, periodEnd, map[string]any{"large_transfers": lines})
}

func (s *Service) insertReport(ctx context.Context, reportType ReportType, periodStart, periodEnd time.Time, payload any) (Report, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return Report{}, fmt.Errorf("risk: marshal report payload: %w", err)
	}

	var r Report
	err = s.pool.QueryRow(ctx, `
		INSERT INTO regulatory_reports (report_type, period_start, period_end, payload)
		VALUES ($1, $2, $3, $4)
		RETURNING id::text, report_type, period_start, period_end, generated_at, payload
	`, reportType, periodStart, periodEnd, body).Scan(
		&r.ID, &r.ReportType, &r.PeriodStart, &r.PeriodEnd, &r.GeneratedAt, &r.Payload,
	)
	if err != nil {
		return Report{}, fmt.Errorf("risk: insert report: %w", err)
	}
	return r, nil
}

func (s *Service) getExistingReport(ctx context.Context, reportType ReportType, periodStart, periodEnd time.Time) (Report, bool, error) {
	var r Report
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, report_type, period_start, period_end, generated_at, payload FROM regulatory_reports
		WHERE report_type = $1 AND period_start = $2 AND period_end = $3
	`, reportType, periodStart, periodEnd).Scan(
		&r.ID, &r.ReportType, &r.PeriodStart, &r.PeriodEnd, &r.GeneratedAt, &r.Payload,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Report{}, false, nil
		}
		return Report{}, false, fmt.Errorf("risk: lookup existing report: %w", err)
	}
	return r, true, nil
}
