package ledger

import "time"

type TransactionType string

const (
	TransactionTypeCredit   TransactionType = "credit"
	TransactionTypeDebit    TransactionType = "debit"
	TransactionTypeTransfer TransactionType = "transfer"
	TransactionTypeReversal TransactionType = "reversal"
)

type TransactionState string

const (
	TransactionStatePending  TransactionState = "pending"
	TransactionStatePosted   TransactionState = "posted"
	TransactionStateFailed   TransactionState = "failed"
	TransactionStateReversed TransactionState = "reversed"
)

// transactionTransitions is the exhaustive transaction state machine
// (docs/banking-backend-spec.md §3.4, §6). Every movement runs through this
// machine; the Phase 3 in-flight resolver drives transitions out of
// "pending" for anything left stuck there.
var transactionTransitions = map[TransactionState]map[TransactionState]bool{
	TransactionStatePending:  {TransactionStatePosted: true, TransactionStateFailed: true},
	TransactionStatePosted:   {TransactionStateReversed: true},
	TransactionStateFailed:   {},
	TransactionStateReversed: {},
}

// CanTransitionTo reports whether moving from s to target is a valid
// transaction state transition.
func (s TransactionState) CanTransitionTo(target TransactionState) bool {
	return transactionTransitions[s][target]
}

type Transaction struct {
	ID             string
	IdempotencyKey string
	Type           TransactionType
	State          TransactionState
	CreatedAt      time.Time
	UpdatedAt      time.Time
}
