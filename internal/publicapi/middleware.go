package publicapi

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/time/rate"

	"github.com/toluwalase/kolo-bank-server/internal/apikeys"
	"github.com/toluwalase/kolo-bank-server/internal/auth"
)

// rateLimiter enforces per-API-key rate limiting (docs/banking-backend-spec.md
// §3.6). An in-memory token bucket per key is sufficient for the project's
// current single-instance deployment model (§10); a distributed limiter
// would be needed if that changes.
type rateLimiter struct {
	mu       sync.Mutex
	limiters map[string]*rate.Limiter
	rps      rate.Limit
	burst    int
}

func newRateLimiter(rps float64, burst int) *rateLimiter {
	return &rateLimiter{limiters: make(map[string]*rate.Limiter), rps: rate.Limit(rps), burst: burst}
}

func (rl *rateLimiter) allow(keyID string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	lim, ok := rl.limiters[keyID]
	if !ok {
		lim = rate.NewLimiter(rl.rps, rl.burst)
		rl.limiters[keyID] = lim
	}
	return lim.Allow()
}

// ipRateLimit bounds abuse on public, unauthenticated routes (account
// recovery, docs/banking-backend-spec.md §5.5) — there's no API key or
// session to key the existing rateLimiter off of before the caller has
// proven anything, so this keys off remote address instead.
func ipRateLimit(limiter *rateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host = r.RemoteAddr
			}
			if !limiter.allow(host) {
				writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(h, prefix)), true
}

// apiKeyAuth authenticates integration routes via "Authorization: Bearer
// <api key>" and enforces per-key rate limiting.
func apiKeyAuth(svc *apikeys.Service, limiter *rateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerToken(r)
			if !ok {
				writeError(w, http.StatusUnauthorized, "unauthorized", "missing API key")
				return
			}

			key, err := svc.Authenticate(r.Context(), raw)
			if err != nil {
				if errors.Is(err, apikeys.ErrInvalidKey) {
					writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or revoked API key")
					return
				}
				writeError(w, http.StatusInternalServerError, "internal_error", "authentication failed")
				return
			}

			if !limiter.allow(key.ID) {
				writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests")
				return
			}

			next.ServeHTTP(w, r.WithContext(withAPIKey(r.Context(), key)))
		})
	}
}

// requireScope rejects requests whose API key lacks scope.
func requireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !apiKeyFromContext(r).HasScope(scope) {
				writeError(w, http.StatusForbidden, "forbidden", "API key missing required scope: "+scope)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// sessionAuth authenticates dashboard routes via a merchant's own login
// session (auth.Service, internal/auth/session.go, Phase 2) rather than an
// API key — a merchant logs into their dashboard to manage the API keys
// and webhook endpoints that the integration routes then authenticate
// with.
func sessionAuth(svc *auth.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerToken(r)
			if !ok {
				writeError(w, http.StatusUnauthorized, "unauthorized", "missing session token")
				return
			}

			sess, err := svc.ValidateSessionToken(r.Context(), raw)
			if err != nil {
				if errors.Is(err, auth.ErrSessionNotFound) {
					writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or expired session")
					return
				}
				writeError(w, http.StatusInternalServerError, "internal_error", "authentication failed")
				return
			}
			if !sess.MFAVerified {
				writeError(w, http.StatusUnauthorized, "unauthorized", "session requires MFA verification")
				return
			}

			next.ServeHTTP(w, r.WithContext(withMerchantID(r.Context(), sess.IdentityID)))
		})
	}
}

// requireIdempotencyKey enforces docs/banking-backend-spec.md §3.6's
// "idempotency keys required on every mutating endpoint." The header's
// value is what each service's UNIQUE(merchant_id, idempotency_key)
// constraint then dedupes against.
func requireIdempotencyKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimSpace(r.Header.Get("Idempotency-Key")) == "" {
			writeError(w, http.StatusBadRequest, "missing_idempotency_key", "Idempotency-Key header is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// chain applies middleware factories left-to-right (chain(a, b)(h) = a(b(h))).
func chain(mws ...func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		for i := len(mws) - 1; i >= 0; i-- {
			h = mws[i](h)
		}
		return h
	}
}
