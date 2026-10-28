//go:build stress

package stresstest

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/testsupport"
)

// TestConcurrentDebits fires many more concurrent transfers at one
// account than its balance covers. "Protection on" is the shipped
// ledger_check_available_balance trigger, which locks the account row
// (SELECT ... FOR UPDATE) before summing its entries. "Protection off"
// swaps in the same function with only that row lock removed.
func TestConcurrentDebits(t *testing.T) {
	const (
		attempts   = 500
		affordable = 10
		amount     = 100
	)

	for _, mode := range []string{"on", "off"} {
		t.Run("row_lock_"+mode, func(t *testing.T) {
			p := requirePool(t)
			svc := ledger.NewService(p)
			if mode == "off" {
				swapFunction(t, p, "ledger_check_available_balance", func(def string) string {
					return strings.Replace(def, "FOR UPDATE", "", 1)
				})
			}

			for run := 1; run <= runs(); run++ {
				from := newFundedAccount(t, p, svc, affordable*amount)
				to := newFundedAccount(t, p, svc, 0)

				var ok, insufficient, other atomic.Int64
				elapsed := startTogether(attempts, func(int) {
					_, err := svc.Transfer(context.Background(), from.ID, to.ID, ngn(amount), testsupport.RandomKey())
					switch {
					case err == nil:
						ok.Add(1)
					case errors.Is(err, ledger.ErrInsufficientBalance):
						insufficient.Add(1)
					default:
						other.Add(1)
						t.Logf("unexpected error: %v", err)
					}
				})

				final := ledgerBalance(t, p, from.ID)
				overdrafts := ok.Load() - affordable
				if overdrafts < 0 {
					overdrafts = 0
				}
				record(t, result{
					Test: "concurrent_debits", Scenario: "500 transfers of 100 against balance 1000",
					Protection: "row_lock_" + mode, Run: run,
					Metrics: map[string]any{
						"attempts":              attempts,
						"succeeded":             ok.Load(),
						"rejected_insufficient": insufficient.Load(),
						"other_errors":          other.Load(),
						"final_balance":         final,
						"overdrafts":            overdrafts,
						"elapsed_ms":            elapsed.Milliseconds(),
						"pool_max_conns":        p.Config().MaxConns,
					},
				})
				if mode == "on" && (overdrafts != 0 || final < 0) {
					t.Errorf("run %d: %d overdrafts, final balance %d", run, overdrafts, final)
				}
			}
		})
	}
}
