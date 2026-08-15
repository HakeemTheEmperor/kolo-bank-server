// Package tokens implements tokenization of payment instruments
// (docs/banking-backend-spec.md §3.6). Raw card data is never stored: a
// masked PAN and a deterministic will_fail flag are derived once at
// creation from reserved test card numbers (Stripe-style convention —
// e.g. a number ending "0002" always declines), the same
// marker-string-in-input stub pattern kyc/compliance/rails already use.
package tokens

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/apikeys"
)

var ErrInvalidCardNumber = errors.New("tokens: invalid card number")

// declineSuffix marks a reserved test card number as always-declining,
// mirroring Stripe's well-known test card conventions.
const declineSuffix = "0002"

var digitsOnly = regexp.MustCompile(`^\d{12,19}$`)

type Token struct {
	ID             string
	MerchantID     string
	Mode           apikeys.Mode
	MaskedPAN      string
	CardBrand      string
	WillFail       bool
	IdempotencyKey string
	CreatedAt      time.Time
}

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// Create tokenizes a card. cardNumber is never persisted — only its last 4
// digits (masked) and the derived will_fail outcome are.
func (s *Service) Create(ctx context.Context, merchantID string, mode apikeys.Mode, cardNumber string, idempotencyKey string) (Token, error) {
	if existing, ok, err := s.getByIdempotencyKey(ctx, merchantID, idempotencyKey); err != nil {
		return Token{}, err
	} else if ok {
		return existing, nil
	}

	if !digitsOnly.MatchString(cardNumber) {
		return Token{}, ErrInvalidCardNumber
	}

	last4 := cardNumber[len(cardNumber)-4:]
	masked := "**** **** **** " + last4
	willFail := last4 == declineSuffix
	brand := detectBrand(cardNumber)

	var tok Token
	err := s.pool.QueryRow(ctx, `
		INSERT INTO payment_instrument_tokens (merchant_id, mode, masked_pan, card_brand, will_fail, idempotency_key)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id::text, merchant_id::text, mode, masked_pan, card_brand, will_fail, idempotency_key, created_at
	`, merchantID, mode, masked, brand, willFail, idempotencyKey).Scan(
		&tok.ID, &tok.MerchantID, &tok.Mode, &tok.MaskedPAN, &tok.CardBrand, &tok.WillFail, &tok.IdempotencyKey, &tok.CreatedAt,
	)
	if err != nil {
		return Token{}, fmt.Errorf("tokens: create: %w", err)
	}
	return tok, nil
}

func (s *Service) Get(ctx context.Context, tokenID string) (Token, error) {
	var tok Token
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, merchant_id::text, mode, masked_pan, card_brand, will_fail, idempotency_key, created_at
		FROM payment_instrument_tokens WHERE id = $1
	`, tokenID).Scan(&tok.ID, &tok.MerchantID, &tok.Mode, &tok.MaskedPAN, &tok.CardBrand, &tok.WillFail, &tok.IdempotencyKey, &tok.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Token{}, fmt.Errorf("tokens: %s: %w", tokenID, pgx.ErrNoRows)
		}
		return Token{}, fmt.Errorf("tokens: get: %w", err)
	}
	return tok, nil
}

func (s *Service) getByIdempotencyKey(ctx context.Context, merchantID, idempotencyKey string) (Token, bool, error) {
	var tok Token
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, merchant_id::text, mode, masked_pan, card_brand, will_fail, idempotency_key, created_at
		FROM payment_instrument_tokens WHERE merchant_id = $1 AND idempotency_key = $2
	`, merchantID, idempotencyKey).Scan(&tok.ID, &tok.MerchantID, &tok.Mode, &tok.MaskedPAN, &tok.CardBrand, &tok.WillFail, &tok.IdempotencyKey, &tok.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Token{}, false, nil
		}
		return Token{}, false, fmt.Errorf("tokens: lookup by idempotency key: %w", err)
	}
	return tok, true, nil
}

func detectBrand(cardNumber string) string {
	switch {
	case len(cardNumber) > 0 && cardNumber[0] == '4':
		return "visa"
	case len(cardNumber) > 1 && (cardNumber[:2] >= "51" && cardNumber[:2] <= "55"):
		return "mastercard"
	default:
		return "unknown"
	}
}
