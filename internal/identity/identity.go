// Package identity is the customer/business record that owns ledger
// accounts and authenticates against the API
// (docs/banking-backend-spec.md §3.1, §3.2).
package identity

import "time"

type Kind string

const (
	KindIndividual Kind = "individual"
	KindBusiness   Kind = "business"
	KindSystem     Kind = "system"
)

type Status string

const (
	StatusPending   Status = "pending"
	StatusActive    Status = "active"
	StatusRejected  Status = "rejected"
	StatusSuspended Status = "suspended"
	StatusClosed    Status = "closed"
)

// statusTransitions is the exhaustive onboarding/lifecycle state machine,
// same pattern as ledger.AccountState (internal/ledger/account.go).
var statusTransitions = map[Status]map[Status]bool{
	StatusPending:   {StatusActive: true, StatusRejected: true},
	StatusActive:    {StatusSuspended: true, StatusClosed: true},
	StatusSuspended: {StatusActive: true, StatusClosed: true},
	StatusRejected:  {},
	StatusClosed:    {},
}

// CanTransitionTo reports whether moving from s to target is a valid
// identity lifecycle transition.
func (s Status) CanTransitionTo(target Status) bool {
	return statusTransitions[s][target]
}

type Role string

const (
	RoleCustomer     Role = "customer"
	RoleSupportAgent Role = "support_agent"
	RoleAdmin        Role = "admin"
)

// roleScopes is the starter RBAC catalog; it grows as later phases add
// payments, disputes, and back-office functionality.
var roleScopes = map[Role]map[string]bool{
	RoleCustomer: {
		"accounts:read":     true,
		"accounts:transact": true,
	},
	RoleSupportAgent: {
		"accounts:read":   true,
		"identities:read": true,
	},
	RoleAdmin: {
		"accounts:read":     true,
		"accounts:transact": true,
		"identities:read":   true,
		"identities:write":  true,
		"kyc:review":        true,
	},
}

// HasScope reports whether r is authorized for the given scope.
func (r Role) HasScope(scope string) bool {
	return roleScopes[r][scope]
}

type Identity struct {
	ID        string
	Kind      Kind
	Status    Status
	KYCTier   int
	Email     string
	Phone     *string
	LegalName string
	Role      Role
	CreatedAt time.Time
	UpdatedAt time.Time
}

type BeneficialOwner struct {
	ID                 string
	BusinessIdentityID string
	FullName           string
	OwnershipPercent   float64
	CreatedAt          time.Time
}
