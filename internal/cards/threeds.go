package cards

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// threeDSThresholdMinor is the amount at/above which Authorize requires a
// 3-D Secure challenge before a hold is placed
// (docs/banking-backend-spec.md §3.5).
const threeDSThresholdMinor = 100_000_00

const threeDSChallengeTTL = 5 * time.Minute

// threeDSCodes is a process-local stash of "sent" challenge codes, styled
// on internal/auth/mfa.go's StubNotifier — there is no real inbox to
// check, so tests read the code back directly rather than through a
// notification channel.
var (
	threeDSCodesMu sync.Mutex
	threeDSCodes   = make(map[string]string)
)

// issueThreeDSChallenge generates a 6-digit code, records its hash and
// expiry on authID's row, and stashes the raw code for PeekThreeDSCode.
func (s *Service) issueThreeDSChallenge(ctx context.Context, authID string) error {
	code, err := randomDigits(6)
	if err != nil {
		return err
	}
	hash := hashThreeDSCode(code)

	if _, err := s.pool.Exec(ctx, `
		UPDATE card_authorizations SET threeds_code_hash = $2, threeds_expires_at = now() + make_interval(secs => $3)
		WHERE id = $1
	`, authID, hash, threeDSChallengeTTL.Seconds()); err != nil {
		return fmt.Errorf("cards: issue 3ds challenge: %w", err)
	}

	threeDSCodesMu.Lock()
	threeDSCodes[authID] = code
	threeDSCodesMu.Unlock()
	return nil
}

// PeekThreeDSCode is a stub-only test helper; no real 3DS provider could
// offer this.
func PeekThreeDSCode(authID string) (string, bool) {
	threeDSCodesMu.Lock()
	defer threeDSCodesMu.Unlock()
	code, ok := threeDSCodes[authID]
	return code, ok
}

func hashThreeDSCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

func randomDigits(n int) (string, error) {
	digits := make([]byte, n)
	if _, err := rand.Read(digits); err != nil {
		return "", fmt.Errorf("cards: generate code: %w", err)
	}
	out := make([]byte, n)
	for i, b := range digits {
		out[i] = '0' + b%10
	}
	return string(out), nil
}

func verifyThreeDSCode(storedHash, code string) bool {
	given := hashThreeDSCode(code)
	return subtle.ConstantTimeCompare([]byte(given), []byte(storedHash)) == 1
}
