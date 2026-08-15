package rails

import (
	"fmt"
	"time"
)

// Registry names each rail's simulated latency, distinguishing them the
// way it actually matters for simulation: instant/RTP-style rails respond
// fast, ACH-equivalent and wire settle more slowly, local schemes and
// biller connections are somewhere in between.
type Registry struct {
	rails map[string]Rail
}

// NewRegistry builds the standard set of simulated rails.
func NewRegistry() *Registry {
	return &Registry{
		rails: map[string]Rail{
			"instant": NewSimulatedRail("instant", 100*time.Millisecond),
			"ach":     NewSimulatedRail("ach", 800*time.Millisecond),
			"wire":    NewSimulatedRail("wire", 1200*time.Millisecond),
			"local":   NewSimulatedRail("local", 400*time.Millisecond),
			"billpay": NewSimulatedRail("billpay", 300*time.Millisecond),
		},
	}
}

func (r *Registry) Get(name string) (Rail, error) {
	rail, ok := r.rails[name]
	if !ok {
		return nil, fmt.Errorf("rails: no rail registered for %q", name)
	}
	return rail, nil
}
