//go:build stress

package stresstest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/toluwalase/kolo-bank-server/internal/apikeys"
	"github.com/toluwalase/kolo-bank-server/internal/secrets"
	"github.com/toluwalase/kolo-bank-server/internal/testsupport"
	"github.com/toluwalase/kolo-bank-server/internal/webhooks"
)

// receiverBehaviour is what the fake merchant endpoint does with one event.
type receiverBehaviour int

const (
	healthy    receiverBehaviour = iota // 200 every time
	failTwice                           // 500 on attempts 1-2, then 200
	slowThenOK                          // attempt 1 processes the event but answers after the 5s client timeout
	hangOnce                            // attempt 1 hangs past the timeout without processing
	alwaysDown                          // 500 forever
)

// behaviourFor spreads events deterministically: 60% healthy, 20% fail
// twice, 10% slow, 5% hang once, 5% permanently down.
func behaviourFor(eventID string) receiverBehaviour {
	h := fnv.New32a()
	h.Write([]byte(eventID))
	switch n := h.Sum32() % 100; {
	case n < 60:
		return healthy
	case n < 80:
		return failTwice
	case n < 90:
		return slowThenOK
	case n < 95:
		return hangOnce
	default:
		return alwaysDown
	}
}

// TestWebhookRetries delivers webhook events to an endpoint that fails or
// times out for some of them, with four concurrent delivery workers (as
// four API instances would run). The production backoff is 1m/5m/30m/2h;
// between rounds the test moves next_attempt_at to now() so the retry
// schedule plays out in seconds. That's the only thing it changes.
func TestWebhookRetries(t *testing.T) {
	const (
		events  = 400
		workers = 4
	)
	p := requirePool(t)
	ctx := context.Background()
	const envVar = "KOLO_KEY_webhook-endpoint-secret"
	if os.Getenv(envVar) == "" {
		t.Setenv(envVar, base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	}
	svc := webhooks.NewService(p, secrets.NewLocalKeyProvider())

	for run := 1; run <= runs(); run++ {
		var mu sync.Mutex
		attempts := map[string]int{}  // HTTP requests seen per event
		processed := map[string]int{} // times the merchant acted on the event
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var payload struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(body, &payload)
			mu.Lock()
			attempts[payload.ID]++
			n := attempts[payload.ID]
			mu.Unlock()
			process := func() {
				mu.Lock()
				processed[payload.ID]++
				mu.Unlock()
			}
			switch behaviourFor(payload.ID) {
			case failTwice:
				if n <= 2 {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
			case slowThenOK:
				if n == 1 {
					process()
					time.Sleep(6 * time.Second)
					return
				}
			case hangOnce:
				if n == 1 {
					time.Sleep(6 * time.Second)
					return
				}
			case alwaysDown:
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			process()
			w.WriteHeader(http.StatusOK)
		}))

		merchant := newOwner(t, p, 2)
		if _, err := p.Exec(ctx, `UPDATE identities SET kind = 'business' WHERE id = $1`, merchant); err != nil {
			t.Fatal(err)
		}
		_, ep, err := svc.CreateEndpoint(ctx, merchant, apikeys.ModeLive, server.URL)
		if err != nil {
			t.Fatalf("create endpoint: %v", err)
		}
		var deliveryIDs []string
		want := map[receiverBehaviour]int{}
		for i := 0; i < events; i++ {
			eventKey := fmt.Sprintf("evt-%s-%d", testsupport.RandomKey(), i)
			want[behaviourFor(eventKey)]++
			payload, _ := json.Marshal(map[string]any{"id": eventKey, "amount_minor": 100, "currency": "NGN", "status": "succeeded"})
			var eventID, deliveryID string
			if err := p.QueryRow(ctx, `INSERT INTO webhook_events (merchant_id, mode, event_type, payload) VALUES ($1, 'live', 'charge.succeeded', $2) RETURNING id::text`,
				merchant, payload).Scan(&eventID); err != nil {
				t.Fatal(err)
			}
			if err := p.QueryRow(ctx, `INSERT INTO webhook_deliveries (webhook_event_id, webhook_endpoint_id) VALUES ($1, $2) RETURNING id::text`,
				eventID, ep.ID).Scan(&deliveryID); err != nil {
				t.Fatal(err)
			}
			deliveryIDs = append(deliveryIDs, deliveryID)
		}

		started := time.Now()
		rounds := 0
		for ; rounds < 30; rounds++ {
			var pending int
			if err := p.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries WHERE id = ANY($1) AND status = 'pending'`, deliveryIDs).Scan(&pending); err != nil {
				t.Fatal(err)
			}
			if pending == 0 {
				break
			}
			// Fast-forward the backoff, then let four workers drain it.
			if _, err := p.Exec(ctx, `UPDATE webhook_deliveries SET next_attempt_at = now() WHERE id = ANY($1) AND status = 'pending'`, deliveryIDs); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			for w := 0; w < workers; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; i < events/20; i++ {
						if err := svc.DeliverPending(ctx); err != nil {
							t.Errorf("deliver: %v", err)
							return
						}
					}
				}()
			}
			wg.Wait()
		}
		server.Close()

		var succeeded, failed, pending, firstTry int
		if err := p.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE status = 'succeeded'), count(*) FILTER (WHERE status = 'failed'),
			       count(*) FILTER (WHERE status = 'pending'), count(*) FILTER (WHERE status = 'succeeded' AND attempt_count = 0)
			FROM webhook_deliveries WHERE id = ANY($1)
		`, deliveryIDs).Scan(&succeeded, &failed, &pending, &firstTry); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		dupes, totalRequests := 0, 0
		for _, n := range processed {
			if n > 1 {
				dupes++
			}
		}
		for _, n := range attempts {
			totalRequests += n
		}
		mu.Unlock()

		reachable := events - want[alwaysDown]
		record(t, result{Test: "webhooks", Scenario: "400 events, 40% with failing/slow receiver, 4 workers", Protection: "retries_on", Run: run,
			Metrics: map[string]any{
				"events": events, "receiver_mix": map[string]int{"healthy": want[healthy], "fail_twice": want[failTwice], "slow_then_ok": want[slowThenOK], "hang_once": want[hangOnce], "always_down": want[alwaysDown]},
				"delivered": succeeded, "gave_up": failed, "still_pending": pending,
				"delivery_rate": float64(succeeded) / events, "delivery_rate_of_reachable": float64(succeeded) / float64(reachable),
				"duplicate_deliveries_processed_by_receiver": dupes, "http_requests": totalRequests,
				"retry_rounds": rounds, "wall_seconds": time.Since(started).Seconds(),
			}})
		// "Retries off" is read from the same run: what would have been
		// delivered if the first attempt were the only one.
		record(t, result{Test: "webhooks", Scenario: "400 events, 40% with failing/slow receiver, 4 workers", Protection: "retries_off_first_attempt_only", Run: run,
			Metrics: map[string]any{"events": events, "delivered": firstTry, "delivery_rate": float64(firstTry) / events}})
	}
}
