package ledger_test

import (
	"testing"

	"github.com/toluwalase/kolo-bank-server/internal/ledger"
)

func TestAccountStateTransitions(t *testing.T) {
	states := []ledger.AccountState{
		ledger.AccountStateOpen, ledger.AccountStateFrozen, ledger.AccountStateDormant, ledger.AccountStateClosed,
	}
	allowed := map[ledger.AccountState]map[ledger.AccountState]bool{
		ledger.AccountStateOpen:    {ledger.AccountStateFrozen: true, ledger.AccountStateDormant: true, ledger.AccountStateClosed: true},
		ledger.AccountStateFrozen:  {ledger.AccountStateOpen: true, ledger.AccountStateClosed: true},
		ledger.AccountStateDormant: {ledger.AccountStateOpen: true, ledger.AccountStateClosed: true},
		ledger.AccountStateClosed:  {},
	}

	for _, from := range states {
		for _, to := range states {
			want := allowed[from][to]
			got := from.CanTransitionTo(to)
			if got != want {
				t.Errorf("AccountState %s -> %s: got %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestTransactionStateTransitions(t *testing.T) {
	states := []ledger.TransactionState{
		ledger.TransactionStatePending, ledger.TransactionStatePosted,
		ledger.TransactionStateFailed, ledger.TransactionStateReversed,
	}
	allowed := map[ledger.TransactionState]map[ledger.TransactionState]bool{
		ledger.TransactionStatePending:  {ledger.TransactionStatePosted: true, ledger.TransactionStateFailed: true},
		ledger.TransactionStatePosted:   {ledger.TransactionStateReversed: true},
		ledger.TransactionStateFailed:   {},
		ledger.TransactionStateReversed: {},
	}

	for _, from := range states {
		for _, to := range states {
			want := allowed[from][to]
			got := from.CanTransitionTo(to)
			if got != want {
				t.Errorf("TransactionState %s -> %s: got %v, want %v", from, to, got, want)
			}
		}
	}
}
