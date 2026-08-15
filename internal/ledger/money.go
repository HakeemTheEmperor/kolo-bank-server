package ledger

import "fmt"

// Money is an amount in integer minor units (e.g. kobo, cents) of a given
// currency. Float64 is never used on the ledger path (see
// docs/banking-backend-spec.md §2, §6); shopspring/decimal is reserved for
// rate calculations (fees, interest, FX) elsewhere, converted back to Money
// before it touches the ledger.
type Money struct {
	Minor    int64
	Currency string
}

// NewMoney constructs a Money value, validating the currency code.
func NewMoney(minor int64, currency string) (Money, error) {
	if len(currency) != 3 {
		return Money{}, fmt.Errorf("ledger: currency must be a 3-letter ISO 4217 code, got %q", currency)
	}
	return Money{Minor: minor, Currency: currency}, nil
}

// Negate returns the additive inverse, used to build the opposite leg of a
// balanced entry pair.
func (m Money) Negate() Money {
	return Money{Minor: -m.Minor, Currency: m.Currency}
}

// IsPositive reports whether the amount is greater than zero.
func (m Money) IsPositive() bool {
	return m.Minor > 0
}
