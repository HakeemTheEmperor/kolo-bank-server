package auth_test

import (
	"encoding/base32"
	"testing"
	"time"

	"github.com/toluwalase/kolo-bank-server/internal/auth"
)

// TestTOTPRFC6238Vectors checks our implementation against RFC 6238
// Appendix B's official SHA-1 test vectors. The RFC's vectors are 8-digit;
// our implementation is fixed at 6 digits, which is exactly the 8-digit
// value mod 10^6 (HOTP truncation is digit-count-agnostic before the final
// modulo), so the expected values here are each vector's last 6 digits.
func TestTOTPRFC6238Vectors(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))

	cases := []struct {
		unixSeconds int64
		want8Digit  string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
	}

	for _, c := range cases {
		want := c.want8Digit[len(c.want8Digit)-6:]
		got, err := auth.GenerateTOTPCode(secret, time.Unix(c.unixSeconds, 0).UTC())
		if err != nil {
			t.Fatalf("GenerateTOTPCode(%d): %v", c.unixSeconds, err)
		}
		if got != want {
			t.Errorf("GenerateTOTPCode(%d) = %s, want %s", c.unixSeconds, got, want)
		}
	}
}

func TestTOTPValidateRoundTrip(t *testing.T) {
	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("generate secret: %v", err)
	}

	now := time.Now()
	code, err := auth.GenerateTOTPCode(secret, now)
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}

	ok, err := auth.ValidateTOTPCode(secret, code, now)
	if err != nil {
		t.Fatalf("validate code: %v", err)
	}
	if !ok {
		t.Fatal("expected freshly generated code to validate")
	}
}

func TestTOTPRejectsWrongCode(t *testing.T) {
	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("generate secret: %v", err)
	}

	ok, err := auth.ValidateTOTPCode(secret, "000000", time.Now())
	if err != nil {
		t.Fatalf("validate code: %v", err)
	}
	if ok {
		// Astronomically unlikely to be the real code, but guard against
		// flakiness rather than assert false outright.
		real, _ := auth.GenerateTOTPCode(secret, time.Now())
		if real == "000000" {
			t.Skip("generated code coincidentally was 000000")
		}
		t.Fatal("expected wrong code to be rejected")
	}
}

func TestTOTPRejectsStaleCode(t *testing.T) {
	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("generate secret: %v", err)
	}

	old := time.Now().Add(-1 * time.Hour)
	code, err := auth.GenerateTOTPCode(secret, old)
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}

	ok, err := auth.ValidateTOTPCode(secret, code, time.Now())
	if err != nil {
		t.Fatalf("validate code: %v", err)
	}
	if ok {
		t.Fatal("expected code from an hour ago to be rejected")
	}
}
