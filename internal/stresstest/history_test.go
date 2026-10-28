//go:build stress

package stresstest

import (
	"context"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/toluwalase/kolo-bank-server/internal/testsupport"
)

// TestHistoryGrowth measures how one sender's transfer latency changes as
// that sender's ledger history grows. Balances are never stored: the
// balance trigger, the daily-limit check and the payee check each sum
// over the sender's entries on every transfer, so cost should grow with
// history. Requests are sequential, so this is per-request cost, not
// throughput.
func TestHistoryGrowth(t *testing.T) {
	const samples = 200
	p := requirePool(t)
	env := newHTTPEnv(t, p)
	ctx := context.Background()
	client := &http.Client{}

	from := newCustomer(t, env, 1_000_000_00)
	to := newCustomer(t, env, 0)
	if _, err := env.ledger.Transfer(ctx, from.accountID, to.accountID, ngn(1), testsupport.RandomKey()); err != nil {
		t.Fatal(err)
	}

	measure := func(fn func() error) (p50, p95 time.Duration) {
		d := make([]time.Duration, 0, samples)
		for i := 0; i < samples; i++ {
			start := time.Now()
			if err := fn(); err != nil {
				t.Fatalf("sample: %v", err)
			}
			d = append(d, time.Since(start))
		}
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		return d[samples/2], d[samples*95/100]
	}

	for _, target := range []int{0, 1_000, 5_000, 10_000, 20_000} {
		// Grow the sender's history to target entries with direct ledger
		// transfers (16 at a time; they serialize on the sender's row).
		for {
			n := entryCount(t, from.accountID)
			if n >= target {
				break
			}
			batch := target - n
			if batch > 16 {
				batch = 16
			}
			startTogether(batch, func(int) {
				if _, err := env.ledger.Transfer(ctx, from.accountID, to.accountID, ngn(1), testsupport.RandomKey()); err != nil {
					t.Errorf("grow history: %v", err)
				}
			})
		}
		history := entryCount(t, from.accountID)

		ledgerP50, ledgerP95 := measure(func() error {
			_, err := env.ledger.Transfer(ctx, from.accountID, to.accountID, ngn(1), testsupport.RandomKey())
			return err
		})
		httpP50, httpP95 := measure(func() error {
			code, err := postTransfer(client, env.server.URL, from, to, 1, testsupport.RandomKey())
			if err == nil && code != http.StatusCreated {
				t.Fatalf("POST /v1/me/transfers: status %d", code)
			}
			return err
		})
		record(t, result{Test: "history_growth", Scenario: "sequential transfers from one sender", Protection: "as_shipped", Run: 1,
			Metrics: map[string]any{
				"sender_entries": history, "samples": samples,
				"ledger_transfer_p50_ms": ms(ledgerP50), "ledger_transfer_p95_ms": ms(ledgerP95),
				"http_transfer_p50_ms": ms(httpP50), "http_transfer_p95_ms": ms(httpP95),
			}})
	}
}

func entryCount(t *testing.T, accountID string) int {
	t.Helper()
	var n int
	if err := requirePool(t).QueryRow(context.Background(),
		`SELECT count(*) FROM ledger_entries WHERE account_id = $1`, accountID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
