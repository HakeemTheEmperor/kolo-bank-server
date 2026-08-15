package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/toluwalase/kolo-bank-server/internal/apikeys"
)

const (
	maxDeliveryAttempts = 5
	// claimLease is how long a claimed delivery is pushed out by, both to
	// give the HTTP attempt room to complete and as an implicit
	// crash-recovery net: if the process dies mid-delivery, the row
	// naturally becomes due again after this window rather than needing a
	// separate resolver, unlike externalpayments' two-state claim
	// (internal/externalpayments/process.go) — deliveries only have
	// pending/succeeded/failed, so reusing next_attempt_at as the
	// in-flight marker avoids a fourth status.
	claimLease   = time.Minute
	httpTimeout  = 5 * time.Second
	claimBatch   = 20
)

// NotifyTerminal enqueues a webhook event (and one delivery per active
// endpoint) for every charge/payout whose linked external transfer just
// reached a terminal state and hasn't been notified yet.
func (s *Service) NotifyTerminal(ctx context.Context) error {
	if err := s.notifyTerminalCharges(ctx); err != nil {
		return fmt.Errorf("webhooks: notify terminal charges: %w", err)
	}
	if err := s.notifyTerminalPayouts(ctx); err != nil {
		return fmt.Errorf("webhooks: notify terminal payouts: %w", err)
	}
	return nil
}

type terminalItem struct {
	ID         string
	MerchantID string
	Mode       apikeys.Mode
	AmountMinor int64
	Currency   string
	Succeeded  bool
}

func (s *Service) notifyTerminalCharges(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `
		SELECT c.id::text, c.merchant_id::text, c.mode, c.amount_minor, c.currency, et.status = 'completed'
		FROM charges c JOIN external_transfers et ON et.id = c.external_transfer_id
		WHERE c.notified_at IS NULL AND et.status IN ('completed', 'failed')
		LIMIT $1
	`, claimBatch)
	if err != nil {
		return err
	}
	items, err := scanTerminalItems(rows)
	if err != nil {
		return err
	}

	for _, item := range items {
		eventType := "charge.failed"
		if item.Succeeded {
			eventType = "charge.succeeded"
		}
		if err := s.enqueueEvent(ctx, item, eventType, "charges"); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) notifyTerminalPayouts(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `
		SELECT p.id::text, p.merchant_id::text, p.mode, p.amount_minor, p.currency, et.status = 'completed'
		FROM payouts p JOIN external_transfers et ON et.id = p.external_transfer_id
		WHERE p.notified_at IS NULL AND et.status IN ('completed', 'failed')
		LIMIT $1
	`, claimBatch)
	if err != nil {
		return err
	}
	items, err := scanTerminalItems(rows)
	if err != nil {
		return err
	}

	for _, item := range items {
		eventType := "payout.failed"
		if item.Succeeded {
			eventType = "payout.succeeded"
		}
		if err := s.enqueueEvent(ctx, item, eventType, "payouts"); err != nil {
			return err
		}
	}
	return nil
}

type rowScanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

func scanTerminalItems(rows rowScanner) ([]terminalItem, error) {
	defer rows.Close()
	var out []terminalItem
	for rows.Next() {
		var it terminalItem
		if err := rows.Scan(&it.ID, &it.MerchantID, &it.Mode, &it.AmountMinor, &it.Currency, &it.Succeeded); err != nil {
			return nil, fmt.Errorf("scan terminal item: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (s *Service) enqueueEvent(ctx context.Context, item terminalItem, eventType, sourceTable string) error {
	status := "failed"
	if item.Succeeded {
		status = "succeeded"
	}
	payload, err := json.Marshal(map[string]any{
		"id":           item.ID,
		"amount_minor": item.AmountMinor,
		"currency":     item.Currency,
		"status":       status,
	})
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	endpointIDs, err := s.activeEndpointIDs(ctx, item.MerchantID, item.Mode)
	if err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var eventID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO webhook_events (merchant_id, mode, event_type, payload) VALUES ($1, $2, $3, $4) RETURNING id::text
	`, item.MerchantID, item.Mode, eventType, payload).Scan(&eventID); err != nil {
		return fmt.Errorf("insert event: %w", err)
	}

	for _, epID := range endpointIDs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO webhook_deliveries (webhook_event_id, webhook_endpoint_id) VALUES ($1, $2)
		`, eventID, epID); err != nil {
			return fmt.Errorf("insert delivery: %w", err)
		}
	}

	if _, err := tx.Exec(ctx, `UPDATE `+sourceTable+` SET notified_at = now() WHERE id = $1`, item.ID); err != nil {
		return fmt.Errorf("mark notified: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *Service) activeEndpointIDs(ctx context.Context, merchantID string, mode apikeys.Mode) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text FROM webhook_endpoints WHERE merchant_id = $1 AND mode = $2 AND active
	`, merchantID, mode)
	if err != nil {
		return nil, fmt.Errorf("find active endpoints: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan endpoint id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

type claimedDelivery struct {
	ID            string
	AttemptCount  int
	EventID       string
	EndpointID    string
}

// DeliverPending claims due deliveries and POSTs the signed payload to
// each endpoint, retrying failures with exponential backoff up to
// maxDeliveryAttempts.
func (s *Service) DeliverPending(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `
		WITH claimed AS (
			SELECT id FROM webhook_deliveries WHERE status = 'pending' AND next_attempt_at <= now()
			ORDER BY next_attempt_at LIMIT $1 FOR UPDATE SKIP LOCKED
		)
		UPDATE webhook_deliveries SET next_attempt_at = now() + $2
		WHERE id IN (SELECT id FROM claimed)
		RETURNING id::text, attempt_count, webhook_event_id::text, webhook_endpoint_id::text
	`, claimBatch, claimLease)
	if err != nil {
		return fmt.Errorf("webhooks: claim deliveries: %w", err)
	}

	var claimed []claimedDelivery
	for rows.Next() {
		var d claimedDelivery
		if err := rows.Scan(&d.ID, &d.AttemptCount, &d.EventID, &d.EndpointID); err != nil {
			rows.Close()
			return fmt.Errorf("webhooks: scan claimed delivery: %w", err)
		}
		claimed = append(claimed, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, d := range claimed {
		s.deliverOne(ctx, d)
	}
	return nil
}

func (s *Service) deliverOne(ctx context.Context, d claimedDelivery) {
	var eventType string
	var payload []byte
	if err := s.pool.QueryRow(ctx, `SELECT event_type, payload FROM webhook_events WHERE id = $1`, d.EventID).Scan(&eventType, &payload); err != nil {
		s.recordFailure(ctx, d, fmt.Sprintf("load event: %v", err))
		return
	}

	var url string
	var sealedSecret []byte
	if err := s.pool.QueryRow(ctx, `SELECT url, secret_encrypted FROM webhook_endpoints WHERE id = $1`, d.EndpointID).Scan(&url, &sealedSecret); err != nil {
		s.recordFailure(ctx, d, fmt.Sprintf("load endpoint: %v", err))
		return
	}

	secret, err := s.unseal(ctx, sealedSecret)
	if err != nil {
		s.recordFailure(ctx, d, err.Error())
		return
	}

	if err := s.send(ctx, url, secret, eventType, payload); err != nil {
		s.recordFailure(ctx, d, err.Error())
		return
	}

	if _, err := s.pool.Exec(ctx, `
		UPDATE webhook_deliveries SET status = 'succeeded', delivered_at = now() WHERE id = $1
	`, d.ID); err != nil {
		// The delivery itself succeeded; a failure to record that is
		// logged by the caller's ticker, not treated as a send failure.
		_ = err
	}
}

func (s *Service) send(ctx context.Context, url, secret, eventType string, payload []byte) error {
	sendCtx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(sendCtx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kolo-Event", eventType)
	req.Header.Set("X-Kolo-Signature", "sha256="+Sign(payload, secret))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("endpoint responded %d", resp.StatusCode)
	}
	return nil
}

func (s *Service) recordFailure(ctx context.Context, d claimedDelivery, errMsg string) {
	attempt := d.AttemptCount + 1
	if attempt >= maxDeliveryAttempts {
		_, _ = s.pool.Exec(ctx, `
			UPDATE webhook_deliveries SET status = 'failed', attempt_count = $2, last_error = $3 WHERE id = $1
		`, d.ID, attempt, errMsg)
		return
	}
	_, _ = s.pool.Exec(ctx, `
		UPDATE webhook_deliveries SET attempt_count = $2, next_attempt_at = now() + $3, last_error = $4 WHERE id = $1
	`, d.ID, attempt, backoff(attempt), errMsg)
}

// backoff grows 1m, 5m, 30m, 2h for attempts 1-4 (attempt 5 exhausts
// maxDeliveryAttempts and gives up instead of scheduling another).
func backoff(attempt int) time.Duration {
	schedule := []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour}
	if attempt-1 < len(schedule) {
		return schedule[attempt-1]
	}
	return schedule[len(schedule)-1]
}

// Sign computes the hex-encoded HMAC-SHA256 of payload using secret — the
// same computation a merchant's receiver must perform to verify
// X-Kolo-Signature.
func Sign(payload []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}
