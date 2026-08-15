// Package rails simulates external payment rails (ACH-equivalent, wire,
// instant/RTP-style, local schemes) behind a common interface
// (docs/banking-backend-spec.md §1, §6: "stubbed external rails behind
// interfaces, so simulation and real integration are swappable without
// touching core logic"). Real rail integrations are a non-goal for this
// build (§1); SimulatedRail stands in for all of them.
package rails

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
)

type Direction string

const (
	DirectionOutbound Direction = "outbound"
	DirectionInbound  Direction = "inbound"
)

type Status string

const (
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

// PaymentRequest describes one rail call. ClientReference is the caller's
// idempotency key for the call: SimulatedRail replays the cached result for
// a repeated ClientReference instead of re-executing, the same way a real
// rail's own idempotency-key support works — and it's what makes the
// in-flight resolver's "retry with the same key" strategy safe.
type PaymentRequest struct {
	Direction       Direction
	CounterpartyRef string
	AmountMinor     int64
	Currency        string
	ClientReference string
}

// RailResult is a definitive outcome from the rail. A returned error from
// Send is different in kind: it means the outcome is *unknown* (network
// failure, or the call's context deadline expired before the rail
// responded — a simulated timeout), not that the rail rejected the payment.
type RailResult struct {
	Status        Status
	RailReference string
	FailureReason string
}

// Rail is the common interface every rail adapter implements.
type Rail interface {
	Name() string
	Send(ctx context.Context, req PaymentRequest) (RailResult, error)
}

// failMarker in a counterparty reference deterministically fails that
// call, the same "marker string in input" pattern used by
// kyc.StubProvider and compliance.StubScreener.
const failMarker = "RAILFAIL"

// SimulatedRail is a configurable stand-in for a real rail: it takes
// latency to "respond" and can be made to fail deterministically, so tests
// can exercise the success, failure, and timeout paths without a real
// external dependency.
type SimulatedRail struct {
	name    string
	latency time.Duration

	mu    sync.Mutex
	cache map[string]cachedResult
}

type cachedResult struct {
	result RailResult
	err    error
}

// NewSimulatedRail constructs a rail that takes latency to respond to
// every call.
func NewSimulatedRail(name string, latency time.Duration) *SimulatedRail {
	return &SimulatedRail{name: name, latency: latency, cache: make(map[string]cachedResult)}
}

func (r *SimulatedRail) Name() string { return r.name }

func (r *SimulatedRail) Send(ctx context.Context, req PaymentRequest) (RailResult, error) {
	if req.ClientReference != "" {
		r.mu.Lock()
		cached, ok := r.cache[req.ClientReference]
		r.mu.Unlock()
		if ok {
			return cached.result, cached.err
		}
	}

	select {
	case <-time.After(r.latency):
	case <-ctx.Done():
		// Simulated timeout: the rail never got to "respond" within the
		// caller's deadline. The outcome is genuinely unknown to the
		// caller — this is exactly the ambiguity the in-flight resolver
		// exists to clean up.
		return RailResult{}, ctx.Err()
	}

	result := RailResult{RailReference: generateReference()}
	if strings.Contains(req.CounterpartyRef, failMarker) {
		result.Status = StatusFailed
		result.FailureReason = "rail rejected the payment (simulated)"
	} else {
		result.Status = StatusSucceeded
	}

	if req.ClientReference != "" {
		r.mu.Lock()
		r.cache[req.ClientReference] = cachedResult{result: result}
		r.mu.Unlock()
	}

	return result, nil
}

func generateReference() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("rail-%s", hex.EncodeToString(b))
}
