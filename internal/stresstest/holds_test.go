//go:build stress

package stresstest

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/testsupport"
)

// TestHoldCaptureReleaseRace races captures against releases on the same
// hold. "Protection on" is the shipped hold row lock; "off" is the same
// code built with -tags stress_nolock, which drops only that lock
// (internal/ledger/holdlock_nolock.go).
//
// Capturing more than was held can't be tested: CaptureHold takes no
// amount and always captures exactly the held amount.
func TestHoldCaptureReleaseRace(t *testing.T) {
	const (
		holds      = 200
		perOp      = 5 // concurrent captures and, separately, releases per hold
		balance    = 1000
		holdAmount = 300
	)

	p := requirePool(t)
	svc := ledger.NewService(p)
	for run := 1; run <= runs(); run++ {
		type target struct {
			account ledger.Account
			hold    ledger.Hold
		}
		targets := make([]target, holds)
		for i := range targets {
			acc := newFundedAccount(t, p, svc, balance)
			h, err := svc.PlaceHold(context.Background(), acc.ID, ngn(holdAmount))
			if err != nil {
				t.Fatalf("place hold: %v", err)
			}
			targets[i] = target{account: acc, hold: h}
		}

		captures := make([]atomic.Int64, holds)
		releases := make([]atomic.Int64, holds)
		var otherErrs atomic.Int64
		startTogether(holds*perOp*2, func(i int) {
			h := i % holds
			ctx := context.Background()
			var err error
			if (i/holds)%2 == 0 {
				_, err = svc.CaptureHold(ctx, targets[h].hold.ID, testsupport.RandomKey())
				if err == nil {
					captures[h].Add(1)
				}
			} else {
				err = svc.ReleaseHold(ctx, targets[h].hold.ID)
				if err == nil {
					releases[h].Add(1)
				}
			}
			if err != nil && !errors.Is(err, ledger.ErrHoldNotActive) {
				otherErrs.Add(1)
				t.Logf("hold %d: %v", h, err)
			}
		})

		var (
			invalidTransitions int64 // successful state changes beyond the first, per hold
			holdsMultiResolved int
			moneyDiscrepancy   int64 // |actual debit - debit implied by final hold status|, summed
			captured, released int
			stillActive        int
		)
		for i, tg := range targets {
			c, r := captures[i].Load(), releases[i].Load()
			if c+r > 1 {
				invalidTransitions += c + r - 1
				holdsMultiResolved++
			}
			var status string
			if err := p.QueryRow(context.Background(), `SELECT status FROM holds WHERE id = $1`, tg.hold.ID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			var expectedDebit int64
			switch status {
			case "captured":
				captured++
				expectedDebit = holdAmount
			case "released":
				released++
			default:
				stillActive++
			}
			actualDebit := balance - ledgerBalance(t, p, tg.account.ID)
			d := actualDebit - expectedDebit
			if d < 0 {
				d = -d
			}
			moneyDiscrepancy += d
		}

		record(t, result{
			Test: "holds", Scenario: "200 holds x (5 captures + 5 releases) concurrent", Protection: "hold_row_lock_" + lockMode, Run: run,
			Metrics: map[string]any{
				"holds": holds, "ops": holds * perOp * 2,
				"invalid_state_changes": invalidTransitions, "holds_resolved_more_than_once": holdsMultiResolved,
				"final_captured": captured, "final_released": released, "final_still_active": stillActive,
				"money_created_or_lost_minor": moneyDiscrepancy, "other_errors": otherErrs.Load(),
			},
		})
		if lockMode == "on" && (invalidTransitions != 0 || moneyDiscrepancy != 0) {
			t.Errorf("run %d: %d invalid transitions, %d minor units unaccounted", run, invalidTransitions, moneyDiscrepancy)
		}
	}
}

// TestHoldCaptureFullAmount captures holds that reserve more than half the
// balance. A hold already reserves its amount, so capturing it should
// always succeed.
func TestHoldCaptureFullAmount(t *testing.T) {
	const (
		balance = 1000
		trials  = 50
	)
	p := requirePool(t)
	svc := ledger.NewService(p)
	for _, amount := range []int64{300, 500, 501, 600, 1000} {
		var ok, insufficient, other int
		for i := 0; i < trials; i++ {
			acc := newFundedAccount(t, p, svc, balance)
			h, err := svc.PlaceHold(context.Background(), acc.ID, ngn(amount))
			if err != nil {
				t.Fatalf("place hold: %v", err)
			}
			_, err = svc.CaptureHold(context.Background(), h.ID, testsupport.RandomKey())
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ledger.ErrInsufficientBalance):
				insufficient++
			default:
				other++
			}
		}
		record(t, result{
			Test: "holds", Scenario: "capture a hold on balance 1000", Protection: "as_shipped", Run: 1,
			Metrics: map[string]any{
				"hold_amount": amount, "trials": trials, "captured": ok,
				"rejected_insufficient_balance": insufficient, "other_errors": other,
			},
		})
	}
}
