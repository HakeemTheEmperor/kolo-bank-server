package reconciliation

import (
	"context"
	"fmt"
	"log/slog"
)

type unmatchedLine struct {
	LineID             string
	ReportedMinor      int64
	ExternalTransferID string
	TransferAmountMinor int64
	TransferStatus      string
}

// RunReconciliation matches unmatched statement lines against the
// transfers they describe. A transfer still in flight ("pending" or
// "processing") is a benign timing difference, left for the next run.
// A completed transfer whose reported amount doesn't match, or a failed
// transfer with a nonzero report, is a genuine break — routed to the
// escalation queue rather than silently absorbed.
func (s *Service) RunReconciliation(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `
		SELECT l.id::text, l.reported_amount_minor, et.id::text, et.amount_minor, et.status
		FROM reconciliation_statement_lines l
		JOIN external_transfers et ON et.id = l.external_transfer_id
		WHERE l.status = 'unmatched'
		LIMIT $1
	`, batchSize)
	if err != nil {
		return fmt.Errorf("reconciliation: find unmatched lines: %w", err)
	}

	var lines []unmatchedLine
	for rows.Next() {
		var l unmatchedLine
		if err := rows.Scan(&l.LineID, &l.ReportedMinor, &l.ExternalTransferID, &l.TransferAmountMinor, &l.TransferStatus); err != nil {
			rows.Close()
			return fmt.Errorf("scan unmatched line: %w", err)
		}
		lines = append(lines, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, l := range lines {
		if err := s.resolveLine(ctx, l); err != nil {
			s.logger.ErrorContext(ctx, "reconciliation: resolve line failed", slog.String("line_id", l.LineID), slog.Any("error", err))
		}
	}
	return nil
}

func (s *Service) resolveLine(ctx context.Context, l unmatchedLine) error {
	switch l.TransferStatus {
	case "pending", "processing":
		// Benign timing difference: our side hasn't caught up yet. Leave
		// unmatched for the next run rather than flagging a break.
		return nil

	case "completed":
		if l.ReportedMinor == l.TransferAmountMinor {
			return s.markMatched(ctx, l.LineID)
		}
		return s.raiseBreak(ctx, l, "amount_mismatch", l.TransferAmountMinor)

	case "failed":
		if l.ReportedMinor == 0 {
			return s.markMatched(ctx, l.LineID)
		}
		return s.raiseBreak(ctx, l, "unexpected_settlement", 0)

	default:
		return fmt.Errorf("reconciliation: unknown transfer status %q", l.TransferStatus)
	}
}

func (s *Service) markMatched(ctx context.Context, lineID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE reconciliation_statement_lines SET status = 'matched' WHERE id = $1`, lineID)
	if err != nil {
		return fmt.Errorf("mark matched: %w", err)
	}
	return nil
}

func (s *Service) raiseBreak(ctx context.Context, l unmatchedLine, reason string, expectedMinor int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO reconciliation_breaks (statement_line_id, external_transfer_id, reason, expected_amount_minor, reported_amount_minor)
		VALUES ($1, $2, $3, $4, $5)
	`, l.LineID, l.ExternalTransferID, reason, expectedMinor, l.ReportedMinor); err != nil {
		return fmt.Errorf("insert break: %w", err)
	}

	if _, err := tx.Exec(ctx, `UPDATE reconciliation_statement_lines SET status = 'break' WHERE id = $1`, l.LineID); err != nil {
		return fmt.Errorf("mark break: %w", err)
	}

	return tx.Commit(ctx)
}
