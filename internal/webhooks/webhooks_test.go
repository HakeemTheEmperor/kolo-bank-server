package webhooks_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/apikeys"
	"github.com/toluwalase/kolo-bank-server/internal/charges"
	"github.com/toluwalase/kolo-bank-server/internal/externalpayments"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/rails"
	"github.com/toluwalase/kolo-bank-server/internal/secrets"
	"github.com/toluwalase/kolo-bank-server/internal/testsupport"
	"github.com/toluwalase/kolo-bank-server/internal/tokens"
	"github.com/toluwalase/kolo-bank-server/internal/webhooks"
)

func init() {
	const envVar = "KOLO_KEY_webhook-endpoint-secret"
	if os.Getenv(envVar) == "" {
		key := make([]byte, 32)
		_, _ = rand.Read(key)
		_ = os.Setenv(envVar, base64.StdEncoding.EncodeToString(key))
	}
}

func mustMoney(t *testing.T, minor int64) ledger.Money {
	t.Helper()
	m, err := ledger.NewMoney(minor, "NGN")
	if err != nil {
		t.Fatalf("new money: %v", err)
	}
	return m
}

func newCompletedCharge(t *testing.T, pool *pgxpool.Pool) (merchantID string, chargeID string) {
	t.Helper()
	ctx := context.Background()

	ledgerSvc := ledger.NewService(pool)
	tokensSvc := tokens.NewService(pool)
	externalSvc := externalpayments.NewService(pool, ledgerSvc, rails.NewRegistry(), nil)
	chargesSvc := charges.NewService(pool, tokensSvc, externalSvc)

	err := pool.QueryRow(ctx, `
		INSERT INTO identities (kind, status, email, password_hash, legal_name)
		VALUES ('business', 'active', $1, 'unused', 'Test Merchant')
		RETURNING id::text
	`, testsupport.RandomKey()+"@example.com").Scan(&merchantID)
	if err != nil {
		t.Fatalf("insert merchant: %v", err)
	}
	if _, err := ledgerSvc.OpenAccount(ctx, merchantID, ledger.AccountTypeCurrent, "NGN", 0); err != nil {
		t.Fatalf("open account: %v", err)
	}

	tok, err := tokensSvc.Create(ctx, merchantID, apikeys.ModeSandbox, "4242424242424242", testsupport.RandomKey())
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	ch, err := chargesSvc.Create(ctx, merchantID, apikeys.ModeSandbox, tok.ID, mustMoney(t, 1_000_00), testsupport.RandomKey())
	if err != nil {
		t.Fatalf("create charge: %v", err)
	}
	if err := externalSvc.ProcessPending(ctx); err != nil {
		t.Fatalf("process pending: %v", err)
	}

	return merchantID, ch.ID
}

func TestCreateAndListEndpoints(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := webhooks.NewService(pool, secrets.NewLocalKeyProvider())
	ctx := context.Background()

	merchantID, _ := newCompletedCharge(t, pool)

	secret, ep, err := svc.CreateEndpoint(ctx, merchantID, apikeys.ModeSandbox, "https://merchant.example/webhook")
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	if secret == "" {
		t.Fatal("expected a non-empty signing secret")
	}
	if !ep.Active {
		t.Fatal("expected new endpoint to be active")
	}

	endpoints, err := svc.ListEndpoints(ctx, merchantID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(endpoints) != 1 {
		t.Fatalf("endpoint count = %d, want 1", len(endpoints))
	}
}

func TestNotifyTerminalAndDeliverPendingSucceeds(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := webhooks.NewService(pool, secrets.NewLocalKeyProvider())
	ctx := context.Background()

	merchantID, chargeID := newCompletedCharge(t, pool)

	var mu sync.Mutex
	var receivedBody []byte
	var receivedSig string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		receivedBody = body
		receivedSig = r.Header.Get("X-Kolo-Signature")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	secret, _, err := svc.CreateEndpoint(ctx, merchantID, apikeys.ModeSandbox, server.URL)
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}

	if err := svc.NotifyTerminal(ctx); err != nil {
		t.Fatalf("notify terminal: %v", err)
	}

	var notifiedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT notified_at FROM charges WHERE id = $1`, chargeID).Scan(&notifiedAt); err != nil {
		t.Fatalf("query charge: %v", err)
	}
	if notifiedAt == nil {
		t.Fatal("expected charge.notified_at to be set after NotifyTerminal")
	}

	// Scoped to this test's own merchant: NotifyTerminal scans globally
	// across all merchants, so other tests' leftover unnotified charges
	// (each with their own endpoint) also produce deliveries in the shared
	// test database — a global count would double-count those.
	var deliveryCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM webhook_deliveries d
		JOIN webhook_events e ON e.id = d.webhook_event_id
		WHERE e.merchant_id = $1
	`, merchantID).Scan(&deliveryCount); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if deliveryCount != 1 {
		t.Fatalf("delivery count = %d, want 1", deliveryCount)
	}

	if err := svc.DeliverPending(ctx); err != nil {
		t.Fatalf("deliver pending: %v", err)
	}

	mu.Lock()
	body, sig := receivedBody, receivedSig
	mu.Unlock()
	if len(body) == 0 {
		t.Fatal("expected the endpoint to receive a payload")
	}
	wantSig := "sha256=" + webhooks.Sign(body, secret)
	if sig != wantSig {
		t.Fatalf("signature = %q, want %q", sig, wantSig)
	}

	var status string
	if err := pool.QueryRow(ctx, `
		SELECT d.status FROM webhook_deliveries d JOIN webhook_events e ON e.id = d.webhook_event_id WHERE e.merchant_id = $1
	`, merchantID).Scan(&status); err != nil {
		t.Fatalf("query delivery status: %v", err)
	}
	if status != "succeeded" {
		t.Fatalf("delivery status = %s, want succeeded", status)
	}
}

func TestDeliverPendingRetriesWithBackoffThenGivesUp(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := webhooks.NewService(pool, secrets.NewLocalKeyProvider())
	ctx := context.Background()

	merchantID, _ := newCompletedCharge(t, pool)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	if _, _, err := svc.CreateEndpoint(ctx, merchantID, apikeys.ModeSandbox, server.URL); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	if err := svc.NotifyTerminal(ctx); err != nil {
		t.Fatalf("notify terminal: %v", err)
	}

	if err := svc.DeliverPending(ctx); err != nil {
		t.Fatalf("deliver pending (attempt 1): %v", err)
	}

	var deliveryID, status string
	var attemptCount int
	var nextAttemptAt time.Time
	if err := pool.QueryRow(ctx, `
		SELECT d.id::text, d.status, d.attempt_count, d.next_attempt_at
		FROM webhook_deliveries d JOIN webhook_events e ON e.id = d.webhook_event_id WHERE e.merchant_id = $1
	`, merchantID).Scan(&deliveryID, &status, &attemptCount, &nextAttemptAt); err != nil {
		t.Fatalf("query delivery: %v", err)
	}
	if status != "pending" {
		t.Fatalf("status after first failure = %s, want pending (retry scheduled)", status)
	}
	if attemptCount != 1 {
		t.Fatalf("attempt count = %d, want 1", attemptCount)
	}
	if !nextAttemptAt.After(time.Now()) {
		t.Fatal("expected next_attempt_at to be scheduled in the future (backoff)")
	}

	// Force the delivery to its final attempt and simulate the backoff
	// window having elapsed, then let it exhaust retries.
	if _, err := pool.Exec(ctx, `
		UPDATE webhook_deliveries SET attempt_count = 4, next_attempt_at = now() - interval '1 minute' WHERE id = $1
	`, deliveryID); err != nil {
		t.Fatalf("fast-forward delivery: %v", err)
	}

	if err := svc.DeliverPending(ctx); err != nil {
		t.Fatalf("deliver pending (final attempt): %v", err)
	}

	if err := pool.QueryRow(ctx, `SELECT status FROM webhook_deliveries WHERE id = $1`, deliveryID).Scan(&status); err != nil {
		t.Fatalf("query final status: %v", err)
	}
	if status != "failed" {
		t.Fatalf("final status = %s, want failed (exhausted retries)", status)
	}
}

func TestSignIsDeterministic(t *testing.T) {
	payload := []byte(`{"id":"abc"}`)
	a := webhooks.Sign(payload, "secret1")
	b := webhooks.Sign(payload, "secret1")
	if a != b {
		t.Fatal("expected the same payload+secret to produce the same signature")
	}
	c := webhooks.Sign(payload, "secret2")
	if a == c {
		t.Fatal("expected a different secret to produce a different signature")
	}
}
