package ledger_test

import (
	"context"
	"testing"

	"pgregory.net/rapid"

	"github.com/toluwalase/kolo-bank-server/internal/ledger"
)

// TestPropertyLedgerAlwaysBalances runs random sequences of credits, debits,
// and transfers across a handful of accounts and checks, after every single
// operation, that no account's available balance goes negative and that the
// ledger as a whole always sums to zero (docs/banking-backend-spec.md §6:
// "a transaction never unbalances the ledger; available balance never goes
// negative without an authorized overdraft").
func TestPropertyLedgerAlwaysBalances(t *testing.T) {
	pool := requireTestPool(t)
	ctx := context.Background()
	svc := ledger.NewService(pool)

	rapid.Check(t, func(rt *rapid.T) {
		numAccounts := rapid.IntRange(2, 4).Draw(rt, "numAccounts")
		accounts := make([]ledger.Account, numAccounts)
		for i := range accounts {
			accounts[i] = newTestAccount(t, svc, 0)
		}

		numOps := rapid.IntRange(1, 15).Draw(rt, "numOps")
		for i := 0; i < numOps; i++ {
			amount := int64(rapid.IntRange(1, 5000).Draw(rt, "amount"))

			switch rapid.SampledFrom([]string{"credit", "debit", "transfer"}).Draw(rt, "op") {
			case "credit":
				acc := accounts[rapid.IntRange(0, numAccounts-1).Draw(rt, "acc")]
				_, _ = svc.Credit(ctx, acc.ID, mustMoney(t, amount), randomKey())
			case "debit":
				acc := accounts[rapid.IntRange(0, numAccounts-1).Draw(rt, "acc")]
				_, _ = svc.Debit(ctx, acc.ID, mustMoney(t, amount), randomKey())
			case "transfer":
				from := accounts[rapid.IntRange(0, numAccounts-1).Draw(rt, "from")]
				to := accounts[rapid.IntRange(0, numAccounts-1).Draw(rt, "to")]
				if from.ID != to.ID {
					_, _ = svc.Transfer(ctx, from.ID, to.ID, mustMoney(t, amount), randomKey())
				}
			}

			for _, acc := range accounts {
				bal, err := svc.GetBalance(ctx, acc.ID)
				if err != nil {
					rt.Fatalf("get balance: %v", err)
				}
				if bal.Available.Minor < 0 {
					rt.Fatalf("account %s available balance went negative: %d", acc.ID, bal.Available.Minor)
				}
			}
		}

		var globalSum int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount_minor), 0) FROM ledger_entries`).Scan(&globalSum); err != nil {
			rt.Fatalf("sum all ledger entries: %v", err)
		}
		if globalSum != 0 {
			rt.Fatalf("global ledger sum = %d, want 0 (double-entry violated)", globalSum)
		}
	})
}
