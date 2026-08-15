// Package kyc implements the KYC pipeline behind a provider interface
// (docs/banking-backend-spec.md §3.1): ID document, liveness, and address
// checks, feeding the identity's tiered verification level. Real providers
// are a non-goal for this build (§1); StubProvider stands in.
package kyc

import (
	"context"
	"strings"
)

type CheckType string

const (
	CheckIDDocument CheckType = "id_document"
	CheckLiveness   CheckType = "liveness"
	CheckAddress    CheckType = "address"
	CheckSanctions  CheckType = "sanctions"
	CheckPEP        CheckType = "pep"
)

type Result string

const (
	ResultPass   Result = "pass"
	ResultFail   Result = "fail"
	ResultReview Result = "review"
)

// Applicant is the data submitted for KYC verification.
type Applicant struct {
	LegalName string
	Address   string
}

// CheckOutcome is one provider call's result, ready to be recorded as a
// kyc_checks row.
type CheckOutcome struct {
	CheckType CheckType
	Provider  string
	Result    Result
	RawResult map[string]any
}

// Provider runs the KYC checks for an applicant.
type Provider interface {
	Verify(ctx context.Context, applicant Applicant) ([]CheckOutcome, error)
}

// failMarker in a legal name or address deterministically fails that check,
// so tests can exercise both the pass and fail paths without a real provider.
const failMarker = "KYCFAIL"

// StubProvider is a deterministic stand-in for a real KYC vendor.
type StubProvider struct{}

func NewStubProvider() *StubProvider {
	return &StubProvider{}
}

func (p *StubProvider) Verify(_ context.Context, applicant Applicant) ([]CheckOutcome, error) {
	docResult := ResultPass
	if strings.Contains(applicant.LegalName, failMarker) {
		docResult = ResultFail
	}

	addrResult := ResultPass
	if strings.Contains(applicant.Address, failMarker) {
		addrResult = ResultFail
	}

	return []CheckOutcome{
		{CheckType: CheckIDDocument, Provider: "stub", Result: docResult, RawResult: map[string]any{"legal_name": applicant.LegalName}},
		{CheckType: CheckLiveness, Provider: "stub", Result: ResultPass, RawResult: map[string]any{}},
		{CheckType: CheckAddress, Provider: "stub", Result: addrResult, RawResult: map[string]any{"address": applicant.Address}},
	}, nil
}
