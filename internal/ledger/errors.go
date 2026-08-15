package ledger

import "errors"

var (
	// ErrInvalidTransition is returned when an account or transaction state
	// change is not allowed by the state machine.
	ErrInvalidTransition = errors.New("ledger: invalid state transition")
	// ErrAccountNotFound is returned when an account ID does not exist.
	ErrAccountNotFound = errors.New("ledger: account not found")
	// ErrHoldNotFound is returned when a hold ID does not exist.
	ErrHoldNotFound = errors.New("ledger: hold not found")
	// ErrHoldNotActive is returned when releasing or capturing a hold that
	// is not in the active state.
	ErrHoldNotActive = errors.New("ledger: hold is not active")
	// ErrInsufficientBalance surfaces the database's own overdraft-limit
	// rejection (see the ledger_check_available_balance trigger) as a typed
	// application error instead of a raw SQL error.
	ErrInsufficientBalance = errors.New("ledger: insufficient available balance")
	// ErrCurrencyMismatch is returned when an operation mixes currencies
	// that must match (e.g. a transfer's two legs).
	ErrCurrencyMismatch = errors.New("ledger: currency mismatch")
)
