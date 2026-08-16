// Package resilience implements kill switches and system-wide read-only
// mode (docs/banking-backend-spec.md §Phase 10): per-integration,
// per-merchant, and per-feature circuit breakers flippable without a
// deploy, plus a singleton read-only-mode flag that pauses new money
// movement while balances and history stay readable.
//
// Check is called at the top of higher-level service entry points
// (payments.Transfer, coolingoff.Send, externalpayments.SendOutbound/
// SendInbound/CreateBatch, cards.Authorize, charges.Create, payouts.Create,
// bills' due-payment send path) — never inside internal/ledger itself.
// ledger.Credit/Debit/Transfer/PlaceHold/ReleaseHold/CaptureHold/
// ReverseTransaction are also used for internal bookkeeping (fee sweeps,
// ticker-driven resolution of already-approved holds, dispute-driven
// reversals) that must never be gated by a business kill switch; only the
// caller knows which scope (a specific rail, merchant, or feature) applies.
//
// The rule of thumb for call sites: guard *initiation* of new money
// movement, never *resolution* of work already approved. So
// coolingoff.Cancel and cards.Settle/Void/Chargeback are deliberately not
// guarded — releasing, completing, or reversing something already approved
// is de-risking, not new exposure, and read-only mode's promise ("money
// movement pauses") is about pausing new debits/credits a customer or
// merchant triggers, not freezing the system's ability to safely unwind or
// complete things already in flight.
package resilience

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrReadOnly is returned by Check when the system is in read-only mode.
var ErrReadOnly = errors.New("resilience: system is in read-only mode")

// ErrKillSwitchTripped is returned by Check when one of the given scopes
// has been disabled. The Scope field lets callers/tests identify which one.
type ErrKillSwitchTripped struct {
	Scope Scope
}

func (e *ErrKillSwitchTripped) Error() string {
	return fmt.Sprintf("resilience: kill switch tripped for %s:%s", e.Scope.Type, e.Scope.Value)
}

type ScopeType string

const (
	ScopeIntegration ScopeType = "integration"
	ScopeMerchant    ScopeType = "merchant"
	ScopeFeature     ScopeType = "feature"
)

// Scope identifies what a kill switch gates — a specific rail
// (ScopeIntegration), a specific merchant (ScopeMerchant), or a whole
// class of operation (ScopeFeature, e.g. "transfer", "card_authorize").
type Scope struct {
	Type  ScopeType
	Value string
}

func Integration(v string) Scope { return Scope{ScopeIntegration, v} }
func Merchant(v string) Scope    { return Scope{ScopeMerchant, v} }
func Feature(v string) Scope     { return Scope{ScopeFeature, v} }

func (s Scope) key() string { return string(s.Type) + ":" + s.Value }

// KillSwitch is a single scope's on/off state, as exposed to the admin
// surface.
type KillSwitch struct {
	ID        string
	Scope     Scope
	Enabled   bool
	Reason    string
	UpdatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SystemMode is the singleton read-only-mode state.
type SystemMode struct {
	ReadOnly  bool
	Reason    string
	UpdatedBy string
	UpdatedAt time.Time
}

// defaultCacheTTL bounds how stale an in-process snapshot of resilience
// state can be — a kill switch flipped by an operator takes effect
// everywhere within this window. Short enough that "contained with a
// targeted kill switch" is a real, near-immediate guarantee; long enough
// that Check, called on every money-moving request, doesn't add a database
// round trip to the hot path.
const defaultCacheTTL = 2 * time.Second

type cacheState struct {
	readOnly  bool
	tripped   map[string]bool
	fetchedAt time.Time
}

type Service struct {
	pool *pgxpool.Pool

	mu       sync.RWMutex
	cache    cacheState
	cacheTTL time.Duration
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, cacheTTL: defaultCacheTTL}
}

// Check returns ErrReadOnly, or an *ErrKillSwitchTripped for the first
// scope that's disabled, if any of the given scopes currently block money
// movement. A nil result means proceed.
func (s *Service) Check(ctx context.Context, scopes ...Scope) error {
	state, err := s.snapshot(ctx)
	if err != nil {
		// Fail closed: silently no-op-ing on the resilience check's own DB
		// hiccup would defeat its purpose, and Postgres being unreachable
		// is about to fail the underlying ledger call moments later
		// anyway — this just fails faster with a clearer error.
		return err
	}
	if state.readOnly {
		return ErrReadOnly
	}
	for _, sc := range scopes {
		if state.tripped[sc.key()] {
			return &ErrKillSwitchTripped{Scope: sc}
		}
	}
	return nil
}

// snapshot returns the cached resilience state, refreshing it from
// Postgres if the cache has expired.
func (s *Service) snapshot(ctx context.Context) (cacheState, error) {
	s.mu.RLock()
	cached := s.cache
	fresh := time.Since(cached.fetchedAt) < s.cacheTTL
	s.mu.RUnlock()
	if fresh {
		return cached, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Another goroutine may have refreshed while we waited for the lock.
	if time.Since(s.cache.fetchedAt) < s.cacheTTL {
		return s.cache, nil
	}

	readOnly, err := s.readSystemModeRaw(ctx)
	if err != nil {
		return cacheState{}, err
	}
	tripped, err := s.readTrippedRaw(ctx)
	if err != nil {
		return cacheState{}, err
	}

	s.cache = cacheState{readOnly: readOnly, tripped: tripped, fetchedAt: time.Now()}
	return s.cache, nil
}

// invalidate clears the cache so the next Check call refreshes from
// Postgres immediately, rather than waiting out cacheTTL — called after
// SetKillSwitch/SetReadOnly so an admin's own change is visible right away
// in the same process.
func (s *Service) invalidate() {
	s.mu.Lock()
	s.cache.fetchedAt = time.Time{}
	s.mu.Unlock()
}

func (s *Service) readSystemModeRaw(ctx context.Context) (bool, error) {
	var readOnly bool
	if err := s.pool.QueryRow(ctx, `SELECT read_only FROM system_mode WHERE id = true`).Scan(&readOnly); err != nil {
		return false, fmt.Errorf("resilience: read system mode: %w", err)
	}
	return readOnly, nil
}

func (s *Service) readTrippedRaw(ctx context.Context) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT scope_type, scope_value FROM kill_switches WHERE enabled = false`)
	if err != nil {
		return nil, fmt.Errorf("resilience: read kill switches: %w", err)
	}
	defer rows.Close()

	tripped := make(map[string]bool)
	for rows.Next() {
		var scopeType, scopeValue string
		if err := rows.Scan(&scopeType, &scopeValue); err != nil {
			return nil, fmt.Errorf("resilience: scan kill switch: %w", err)
		}
		tripped[scopeType+":"+scopeValue] = true
	}
	return tripped, rows.Err()
}
