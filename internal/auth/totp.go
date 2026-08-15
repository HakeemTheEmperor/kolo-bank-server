// TOTP implements RFC 6238 (time-based one-time passwords) on top of RFC
// 4226 HOTP, using SHA-1/6-digit/30-second defaults for compatibility with
// any standard authenticator app. Hand-rolled from stdlib rather than a
// third-party TOTP library, consistent with docs/banking-backend-spec.md
// §2's minimal-dependency rationale.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238's specified default MAC for broad authenticator-app compatibility.
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"time"
)

const (
	totpDigits    = 6
	totpStepSecs  = 30
	totpSecretLen = 20 // 160 bits, matches SHA-1's block strength
	totpWindow    = 1  // tolerate ±1 step of clock drift
)

// GenerateTOTPSecret returns a new random base32-encoded TOTP secret.
func GenerateTOTPSecret() (string, error) {
	raw := make([]byte, totpSecretLen)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: generate totp secret: %w", err)
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw), nil
}

// GenerateTOTPCode computes the TOTP code for secret at time t.
func GenerateTOTPCode(secret string, t time.Time) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		return "", fmt.Errorf("auth: decode totp secret: %w", err)
	}
	counter := uint64(t.Unix()) / totpStepSecs
	return hotp(key, counter), nil
}

// ValidateTOTPCode checks code against secret, tolerating ±totpWindow steps
// of clock drift.
func ValidateTOTPCode(secret, code string, t time.Time) (bool, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		return false, fmt.Errorf("auth: decode totp secret: %w", err)
	}
	counter := uint64(t.Unix()) / totpStepSecs

	for delta := -totpWindow; delta <= totpWindow; delta++ {
		c := counter
		if delta < 0 {
			c -= uint64(-delta)
		} else {
			c += uint64(delta)
		}
		if hotp(key, c) == code {
			return true, nil
		}
	}
	return false, nil
}

func hotp(key []byte, counter uint64) string {
	var counterBytes [8]byte
	binary.BigEndian.PutUint64(counterBytes[:], counter)

	mac := hmac.New(sha1.New, key)
	mac.Write(counterBytes[:])
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	binCode := (uint32(sum[offset])&0x7f)<<24 |
		uint32(sum[offset+1])<<16 |
		uint32(sum[offset+2])<<8 |
		uint32(sum[offset+3])

	code := binCode % 1_000_000

	return fmt.Sprintf("%0*d", totpDigits, code)
}
