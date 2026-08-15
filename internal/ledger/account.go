package ledger

import "time"

type AccountType string

const (
	AccountTypeCurrent    AccountType = "current"
	AccountTypeSavings    AccountType = "savings"
	AccountTypeWallet     AccountType = "wallet"
	AccountTypeSubAccount AccountType = "sub_account"
	AccountTypeVirtual    AccountType = "virtual"
)

type AccountState string

const (
	AccountStateOpen    AccountState = "open"
	AccountStateFrozen  AccountState = "frozen"
	AccountStateDormant AccountState = "dormant"
	AccountStateClosed  AccountState = "closed"
)

// accountTransitions is the exhaustive table of valid account lifecycle
// transitions (docs/banking-backend-spec.md §3.3, §6). Any transition not
// listed here is invalid.
var accountTransitions = map[AccountState]map[AccountState]bool{
	AccountStateOpen:    {AccountStateFrozen: true, AccountStateDormant: true, AccountStateClosed: true},
	AccountStateFrozen:  {AccountStateOpen: true, AccountStateClosed: true},
	AccountStateDormant: {AccountStateOpen: true, AccountStateClosed: true},
	AccountStateClosed:  {},
}

// CanTransitionTo reports whether moving from s to target is a valid
// account lifecycle transition.
func (s AccountState) CanTransitionTo(target AccountState) bool {
	return accountTransitions[s][target]
}

type Account struct {
	ID                  string
	OwnerID             string
	Type                AccountType
	Currency            string
	State               AccountState
	OverdraftLimitMinor int64
	CreatedAt           time.Time
	UpdatedAt           time.Time
}
