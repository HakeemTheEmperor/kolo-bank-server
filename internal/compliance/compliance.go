// Package compliance implements sanctions/PEP screening behind a provider
// interface (docs/banking-backend-spec.md §3.8: "real-time sanctions
// screening"). Real watchlist providers are a non-goal for this build
// (§1); StubScreener stands in.
package compliance

import (
	"context"
	"strings"

	"github.com/toluwalase/kolo-bank-server/internal/kyc"
)

// sanctionedMarker in a legal name deterministically fails sanctions
// screening, so tests can exercise both the pass and hit paths without a
// real watchlist provider.
const sanctionedMarker = "SANCTIONED"

// Screener screens an applicant against sanctions/PEP watchlists.
type Screener interface {
	Screen(ctx context.Context, legalName string) ([]kyc.CheckOutcome, error)
}

// StubScreener is a deterministic stand-in for a real watchlist provider.
type StubScreener struct{}

func NewStubScreener() *StubScreener {
	return &StubScreener{}
}

func (s *StubScreener) Screen(_ context.Context, legalName string) ([]kyc.CheckOutcome, error) {
	sanctionsResult := kyc.ResultPass
	if strings.Contains(legalName, sanctionedMarker) {
		sanctionsResult = kyc.ResultFail
	}

	return []kyc.CheckOutcome{
		{
			CheckType: kyc.CheckSanctions,
			Provider:  "stub",
			Result:    sanctionsResult,
			RawResult: map[string]any{"legal_name": legalName},
		},
		{
			CheckType: kyc.CheckPEP,
			Provider:  "stub",
			Result:    kyc.ResultPass,
			RawResult: map[string]any{},
		},
	}, nil
}
