package rails_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/toluwalase/kolo-bank-server/internal/rails"
)

func TestSimulatedRailSucceeds(t *testing.T) {
	r := rails.NewSimulatedRail("test", 10*time.Millisecond)
	result, err := r.Send(context.Background(), rails.PaymentRequest{
		Direction: rails.DirectionOutbound, CounterpartyRef: "acct-123", AmountMinor: 1000, Currency: "NGN", ClientReference: "req-1",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if result.Status != rails.StatusSucceeded {
		t.Fatalf("status = %s, want succeeded", result.Status)
	}
	if result.RailReference == "" {
		t.Fatal("expected a non-empty rail reference")
	}
}

func TestSimulatedRailDeterministicFailure(t *testing.T) {
	r := rails.NewSimulatedRail("test", 10*time.Millisecond)
	result, err := r.Send(context.Background(), rails.PaymentRequest{
		CounterpartyRef: "acct-RAILFAIL-123", ClientReference: "req-2",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if result.Status != rails.StatusFailed {
		t.Fatalf("status = %s, want failed", result.Status)
	}
	if result.FailureReason == "" {
		t.Fatal("expected a failure reason")
	}
}

func TestSimulatedRailTimeout(t *testing.T) {
	r := rails.NewSimulatedRail("slow", 200*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := r.Send(ctx, rails.PaymentRequest{CounterpartyRef: "acct-123", ClientReference: "req-3"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("send: got err %v, want context.DeadlineExceeded", err)
	}
}

func TestSimulatedRailReplaysCachedResultForSameClientReference(t *testing.T) {
	r := rails.NewSimulatedRail("test", 10*time.Millisecond)
	req := rails.PaymentRequest{CounterpartyRef: "acct-123", ClientReference: "req-4"}

	first, err := r.Send(context.Background(), req)
	if err != nil {
		t.Fatalf("first send: %v", err)
	}

	second, err := r.Send(context.Background(), req)
	if err != nil {
		t.Fatalf("second send: %v", err)
	}

	if first.RailReference != second.RailReference {
		t.Fatalf("replayed send produced a different rail reference: %s != %s", first.RailReference, second.RailReference)
	}
}

func TestRegistryKnownRails(t *testing.T) {
	reg := rails.NewRegistry()
	for _, name := range []string{"instant", "ach", "wire", "local", "billpay"} {
		if _, err := reg.Get(name); err != nil {
			t.Errorf("expected rail %q to be registered: %v", name, err)
		}
	}
	if _, err := reg.Get("nonexistent"); err == nil {
		t.Error("expected an error for an unregistered rail name")
	}
}
