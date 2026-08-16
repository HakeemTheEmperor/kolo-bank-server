package resilience_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/toluwalase/kolo-bank-server/internal/resilience"
	"github.com/toluwalase/kolo-bank-server/internal/testsupport"
)

// System-mode is a genuine global singleton — unlike every other resource
// in this test suite, it can't be namespaced per test. Every test that
// flips it back to read-only resets it via t.Cleanup immediately after its
// assertion, to keep the window other concurrently-running packages could
// observe it in as small as possible.

func TestCheck_AllowsWhenClear(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := resilience.NewService(pool)

	if err := svc.Check(context.Background(), resilience.Feature("test-clear-"+testsupport.RandomKey())); err != nil {
		t.Fatalf("Check() = %v, want nil", err)
	}
}

func TestCheck_BlocksOnReadOnly(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := resilience.NewService(pool)
	ctx := context.Background()

	if _, err := svc.SetReadOnly(ctx, true, "incident drill", "test"); err != nil {
		t.Fatalf("SetReadOnly(true): %v", err)
	}
	t.Cleanup(func() {
		if _, err := svc.SetReadOnly(context.Background(), false, "", "test-cleanup"); err != nil {
			t.Errorf("cleanup SetReadOnly(false): %v", err)
		}
	})

	err := svc.Check(ctx, resilience.Feature("anything"))
	if !errors.Is(err, resilience.ErrReadOnly) {
		t.Fatalf("Check() = %v, want ErrReadOnly", err)
	}
}

func TestCheck_BlocksOnMatchingKillSwitch(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := resilience.NewService(pool)
	ctx := context.Background()

	tripped := resilience.Feature("test-trip-" + testsupport.RandomKey())
	untouched := resilience.Feature("test-untouched-" + testsupport.RandomKey())

	if _, err := svc.SetKillSwitch(ctx, tripped, false, "maintenance", "test"); err != nil {
		t.Fatalf("SetKillSwitch: %v", err)
	}

	err := svc.Check(ctx, tripped)
	var killErr *resilience.ErrKillSwitchTripped
	if !errors.As(err, &killErr) {
		t.Fatalf("Check(tripped) = %v, want *ErrKillSwitchTripped", err)
	}
	if killErr.Scope != tripped {
		t.Fatalf("tripped scope = %+v, want %+v", killErr.Scope, tripped)
	}

	// Scope isolation: tripping one scope must not block an unrelated one.
	if err := svc.Check(ctx, untouched); err != nil {
		t.Fatalf("Check(untouched) = %v, want nil", err)
	}
}

func TestCheck_MultipleScopes_AnyTrippedBlocks(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := resilience.NewService(pool)
	ctx := context.Background()

	ok := resilience.Feature("test-ok-" + testsupport.RandomKey())
	blocked := resilience.Merchant("test-merchant-" + testsupport.RandomKey())

	if _, err := svc.SetKillSwitch(ctx, blocked, false, "", "test"); err != nil {
		t.Fatalf("SetKillSwitch: %v", err)
	}

	err := svc.Check(ctx, ok, blocked)
	var killErr *resilience.ErrKillSwitchTripped
	if !errors.As(err, &killErr) {
		t.Fatalf("Check(ok, blocked) = %v, want *ErrKillSwitchTripped", err)
	}
}

func TestSetKillSwitch_InvalidatesCacheImmediately(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := resilience.NewService(pool)
	ctx := context.Background()

	scope := resilience.Feature("test-immediate-" + testsupport.RandomKey())

	// Prime the cache with a "not tripped" snapshot.
	if err := svc.Check(ctx, scope); err != nil {
		t.Fatalf("Check() before trip = %v, want nil", err)
	}

	if _, err := svc.SetKillSwitch(ctx, scope, false, "", "test"); err != nil {
		t.Fatalf("SetKillSwitch: %v", err)
	}

	// Without invalidation this would still read the pre-trip cache for up
	// to the TTL — SetKillSwitch must make it visible immediately.
	var killErr *resilience.ErrKillSwitchTripped
	if err := svc.Check(ctx, scope); !errors.As(err, &killErr) {
		t.Fatalf("Check() after trip = %v, want *ErrKillSwitchTripped", err)
	}
}

func TestListKillSwitches_ReturnsAllRows(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := resilience.NewService(pool)
	ctx := context.Background()

	scope := resilience.Integration("test-rail-" + testsupport.RandomKey())
	created, err := svc.SetKillSwitch(ctx, scope, false, "rail outage", "ops")
	if err != nil {
		t.Fatalf("SetKillSwitch: %v", err)
	}

	list, err := svc.ListKillSwitches(ctx)
	if err != nil {
		t.Fatalf("ListKillSwitches: %v", err)
	}

	var found bool
	for _, ks := range list {
		if ks.ID == created.ID {
			found = true
			if ks.Enabled {
				t.Fatalf("listed switch Enabled = true, want false")
			}
			if ks.Reason != "rail outage" {
				t.Fatalf("listed switch Reason = %q, want %q", ks.Reason, "rail outage")
			}
		}
	}
	if !found {
		t.Fatalf("ListKillSwitches did not include the switch just created (id=%s)", created.ID)
	}
}

// TestGetSystemMode_HandlesNullReasonAndUpdatedBy covers the row's state
// exactly as migration 00044 leaves it (INSERT INTO system_mode (id,
// read_only) VALUES (true, false) — reason and updated_by both NULL,
// never set) — regression test for a real bug caught only by a genuine
// end-to-end run against a freshly migrated database, not by `go test`
// alone: other tests in this package running first happen to leave
// updated_by non-NULL, masking a "cannot scan NULL into *string" panic
// that a truly first-ever GetSystemMode call would hit.
func TestGetSystemMode_HandlesNullReasonAndUpdatedBy(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := resilience.NewService(pool)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `UPDATE system_mode SET reason = NULL, updated_by = NULL WHERE id = true`); err != nil {
		t.Fatalf("reset system_mode to NULL: %v", err)
	}
	t.Cleanup(func() {
		if _, err := svc.SetReadOnly(context.Background(), false, "", "test-cleanup"); err != nil {
			t.Errorf("cleanup SetReadOnly(false): %v", err)
		}
	})

	mode, err := svc.GetSystemMode(ctx)
	if err != nil {
		t.Fatalf("GetSystemMode with NULL reason/updated_by: %v", err)
	}
	if mode.Reason != "" || mode.UpdatedBy != "" {
		t.Fatalf("mode = %+v, want empty Reason/UpdatedBy for NULL columns", mode)
	}
}

func TestSetReadOnly_RoundTrip(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := resilience.NewService(pool)
	ctx := context.Background()

	before, err := svc.GetSystemMode(ctx)
	if err != nil {
		t.Fatalf("GetSystemMode: %v", err)
	}
	if before.ReadOnly {
		t.Fatalf("system started in read-only mode; a prior test may have leaked state")
	}

	entered, err := svc.SetReadOnly(ctx, true, "drill", "test")
	if err != nil {
		t.Fatalf("SetReadOnly(true): %v", err)
	}
	if !entered.ReadOnly || entered.Reason != "drill" {
		t.Fatalf("entered = %+v, want ReadOnly=true Reason=drill", entered)
	}

	exited, err := svc.SetReadOnly(ctx, false, "", "test")
	if err != nil {
		t.Fatalf("SetReadOnly(false): %v", err)
	}
	if exited.ReadOnly {
		t.Fatalf("exited.ReadOnly = true, want false")
	}
}

func TestCache_TTLRefresh(t *testing.T) {
	pool := testsupport.RequireTestPool(t)
	svc := resilience.NewService(pool)
	ctx := context.Background()

	scope := resilience.Feature("test-ttl-" + testsupport.RandomKey())

	// Prime the cache before the switch exists.
	if err := svc.Check(ctx, scope); err != nil {
		t.Fatalf("Check() before trip = %v, want nil", err)
	}

	// Bypass invalidation to simulate a second process flipping the switch
	// without this process's cache knowing yet.
	if _, err := pool.Exec(ctx, `
		INSERT INTO kill_switches (scope_type, scope_value, enabled, updated_by)
		VALUES ($1, $2, false, 'test')
	`, string(scope.Type), scope.Value); err != nil {
		t.Fatalf("insert kill switch directly: %v", err)
	}

	// Still within the 2s TTL: stale cache, not yet blocked.
	if err := svc.Check(ctx, scope); err != nil {
		t.Fatalf("Check() within TTL = %v, want nil (stale cache)", err)
	}

	time.Sleep(2100 * time.Millisecond)

	var killErr *resilience.ErrKillSwitchTripped
	if err := svc.Check(ctx, scope); !errors.As(err, &killErr) {
		t.Fatalf("Check() after TTL = %v, want *ErrKillSwitchTripped", err)
	}
}
