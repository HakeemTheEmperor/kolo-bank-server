// Package bills implements bill validation and payment
// (docs/banking-backend-spec.md §3.7): a stubbed biller directory, bill
// validation, and bill/airtime/data payments including recurring ones.
// PayBill delegates the actual money movement to
// externalpayments.Service.SendOutbound, so bills gets the whole
// pending/processing/in-flight-resolver pipeline for free and needs no
// resolver of its own.
package bills

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/externalpayments"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/resilience"
)

// invalidMarker in a reference deterministically fails validation, the
// same "marker string in input" pattern as kyc.StubProvider.
const invalidMarker = "INVALID"

var (
	// ErrInvalidReference is returned when a bill reference fails
	// validation (bad account/meter number).
	ErrInvalidReference = errors.New("bills: invalid reference")
	// ErrBillerNotFound is returned for an unknown or inactive biller.
	ErrBillerNotFound = errors.New("bills: biller not found or inactive")
)

type Interval string

const (
	IntervalDaily   Interval = "daily"
	IntervalWeekly  Interval = "weekly"
	IntervalMonthly Interval = "monthly"
)

type Biller struct {
	ID       string
	Name     string
	Category string
	Code     string
	Active   bool
}

type BillPayment struct {
	ID                 string
	BillerID           string
	AccountID          string
	Reference          string
	Amount             ledger.Money
	IdempotencyKey     string
	ExternalTransferID string
	CreatedAt          time.Time
}

type RecurringBillPayment struct {
	ID        string
	BillerID  string
	AccountID string
	Reference string
	Amount    ledger.Money
	Interval  Interval
	NextRunAt time.Time
	Status    string
}

type Service struct {
	pool          *pgxpool.Pool
	externalSvc   *externalpayments.Service
	resilienceSvc *resilience.Service
}

func NewService(pool *pgxpool.Pool, externalSvc *externalpayments.Service, resilienceSvc *resilience.Service) *Service {
	return &Service{pool: pool, externalSvc: externalSvc, resilienceSvc: resilienceSvc}
}

// ValidateReference is a stub validator: any reference containing
// invalidMarker fails, everything else passes with a synthesized name.
func (s *Service) ValidateReference(ctx context.Context, billerID, reference string) (valid bool, customerName string, err error) {
	var biller Biller
	err = s.pool.QueryRow(ctx, `SELECT name, active FROM billers WHERE id = $1`, billerID).Scan(&biller.Name, &biller.Active)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, "", ErrBillerNotFound
		}
		return false, "", fmt.Errorf("bills: lookup biller: %w", err)
	}
	if !biller.Active {
		return false, "", ErrBillerNotFound
	}

	if strings.Contains(reference, invalidMarker) {
		return false, "", nil
	}
	return true, "Customer " + reference, nil
}

func (s *Service) getBillerCode(ctx context.Context, billerID string) (string, error) {
	var code string
	err := s.pool.QueryRow(ctx, `SELECT code FROM billers WHERE id = $1 AND active`, billerID).Scan(&code)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrBillerNotFound
		}
		return "", fmt.Errorf("bills: lookup biller code: %w", err)
	}
	return code, nil
}
