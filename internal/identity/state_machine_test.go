package identity_test

import (
	"testing"

	"github.com/toluwalase/kolo-bank-server/internal/identity"
)

func TestIdentityStatusTransitions(t *testing.T) {
	states := []identity.Status{
		identity.StatusPending, identity.StatusActive, identity.StatusRejected,
		identity.StatusSuspended, identity.StatusClosed,
	}
	allowed := map[identity.Status]map[identity.Status]bool{
		identity.StatusPending:   {identity.StatusActive: true, identity.StatusRejected: true},
		identity.StatusActive:    {identity.StatusSuspended: true, identity.StatusClosed: true},
		identity.StatusSuspended: {identity.StatusActive: true, identity.StatusClosed: true},
		identity.StatusRejected:  {},
		identity.StatusClosed:    {},
	}

	for _, from := range states {
		for _, to := range states {
			want := allowed[from][to]
			got := from.CanTransitionTo(to)
			if got != want {
				t.Errorf("Status %s -> %s: got %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestRoleScopes(t *testing.T) {
	if !identity.RoleAdmin.HasScope("identities:write") {
		t.Error("admin should have identities:write")
	}
	if identity.RoleCustomer.HasScope("identities:write") {
		t.Error("customer should not have identities:write")
	}
	if !identity.RoleCustomer.HasScope("accounts:transact") {
		t.Error("customer should have accounts:transact")
	}
}
