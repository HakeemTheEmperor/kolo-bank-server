package ledger

import "time"

type HoldStatus string

const (
	HoldStatusActive   HoldStatus = "active"
	HoldStatusReleased HoldStatus = "released"
	HoldStatusCaptured HoldStatus = "captured"
)

type Hold struct {
	ID        string
	AccountID string
	Amount    Money
	Status    HoldStatus
	CreatedAt time.Time
	ExpiresAt *time.Time
}

// Balance holds the three distinct balance views a customer or the ledger
// itself needs (docs/banking-backend-spec.md §3.3). All three are derived
// from ledger_entries/holds, never stored as mutable state.
type Balance struct {
	// Ledger is the sum of all posted entries for the account.
	Ledger Money
	// Pending is the sum of active holds against the account.
	Pending Money
	// Available is Ledger minus Pending: what can actually be spent.
	Available Money
}
