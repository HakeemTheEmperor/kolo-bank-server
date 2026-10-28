//go:build stress

package stresstest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/apikeys"
	"github.com/toluwalase/kolo-bank-server/internal/auth"
	"github.com/toluwalase/kolo-bank-server/internal/cards"
	"github.com/toluwalase/kolo-bank-server/internal/charges"
	"github.com/toluwalase/kolo-bank-server/internal/checkout"
	"github.com/toluwalase/kolo-bank-server/internal/compliance"
	"github.com/toluwalase/kolo-bank-server/internal/consent"
	"github.com/toluwalase/kolo-bank-server/internal/coolingoff"
	"github.com/toluwalase/kolo-bank-server/internal/disputes"
	"github.com/toluwalase/kolo-bank-server/internal/externalpayments"
	"github.com/toluwalase/kolo-bank-server/internal/identity"
	"github.com/toluwalase/kolo-bank-server/internal/kyc"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/payments"
	"github.com/toluwalase/kolo-bank-server/internal/payouts"
	"github.com/toluwalase/kolo-bank-server/internal/publicapi"
	"github.com/toluwalase/kolo-bank-server/internal/rails"
	"github.com/toluwalase/kolo-bank-server/internal/recovery"
	"github.com/toluwalase/kolo-bank-server/internal/resilience"
	"github.com/toluwalase/kolo-bank-server/internal/risk"
	"github.com/toluwalase/kolo-bank-server/internal/secrets"
	"github.com/toluwalase/kolo-bank-server/internal/testsupport"
	"github.com/toluwalase/kolo-bank-server/internal/tokens"
	"github.com/toluwalase/kolo-bank-server/internal/webhooks"
)

const idemCopies = 100

// keyFor returns the idempotency key copy i sends. With idempotency on,
// every copy reuses one key (a client retrying the same request); off,
// each copy gets its own (a client that doesn't send keys, or a server
// that ignores them).
func keyFor(mode, shared string, i int) string {
	if mode == "on" {
		return shared
	}
	return fmt.Sprintf("%s-%d", shared, i)
}

// TestIdempotencyLedgerTransfer sends 100 concurrent copies of one
// ledger transfer.
func TestIdempotencyLedgerTransfer(t *testing.T) {
	for _, mode := range []string{"on", "off"} {
		t.Run("idempotency_"+mode, func(t *testing.T) {
			p := requirePool(t)
			svc := ledger.NewService(p)
			for run := 1; run <= runs(); run++ {
				from := newFundedAccount(t, p, svc, idemCopies*100)
				to := newFundedAccount(t, p, svc, 0)
				shared := testsupport.RandomKey()

				var errs atomic.Int64
				ids := sync.Map{}
				startTogether(idemCopies, func(i int) {
					txn, err := svc.Transfer(context.Background(), from.ID, to.ID, ngn(100), keyFor(mode, shared, i))
					if err != nil {
						errs.Add(1)
						t.Logf("copy %d: %v", i, err)
						return
					}
					ids.Store(txn.ID, true)
				})

				distinct := 0
				ids.Range(func(any, any) bool { distinct++; return true })
				postings := countPostingsTo(t, p, to.ID)
				record(t, result{
					Test: "idempotency", Scenario: "ledger.Transfer x100 concurrent", Protection: "idempotency_" + mode, Run: run,
					Metrics: map[string]any{
						"copies": idemCopies, "postings_created": postings, "distinct_txn_ids_returned": distinct,
						"errors": errs.Load(), "amount_moved": ledgerBalance(t, p, to.ID),
					},
				})
				if mode == "on" && postings != 1 {
					t.Errorf("run %d: %d postings, want 1", run, postings)
				}
			}
		})
	}
}

func countPostingsTo(t *testing.T, p *pgxpool.Pool, accountID string) int {
	t.Helper()
	var n int
	if err := p.QueryRow(context.Background(),
		`SELECT count(DISTINCT transaction_id) FROM ledger_entries WHERE account_id = $1`, accountID,
	).Scan(&n); err != nil {
		t.Fatalf("count postings: %v", err)
	}
	return n
}

// holdStats reports the holds on accountID: how many exist, how many are
// still active, and how much money the active ones reserve.
func holdStats(t *testing.T, p *pgxpool.Pool, accountID string) (total, active int, reserved int64) {
	t.Helper()
	if err := p.QueryRow(context.Background(), `
		SELECT count(*), count(*) FILTER (WHERE status = 'active'),
		       COALESCE(SUM(amount_minor) FILTER (WHERE status = 'active'), 0)
		FROM holds WHERE account_id = $1
	`, accountID).Scan(&total, &active, &reserved); err != nil {
		t.Fatalf("hold stats: %v", err)
	}
	return
}

func newExternalService(p *pgxpool.Pool, ledgerSvc *ledger.Service) *externalpayments.Service {
	return externalpayments.NewService(p, ledgerSvc, rails.NewRegistry(),
		risk.NewService(p, ledgerSvc, compliance.NewStubScreener(), nil), resilience.NewService(p), nil)
}

// TestIdempotencyExternalOutbound sends 100 concurrent copies of one
// outbound external transfer.
func TestIdempotencyExternalOutbound(t *testing.T) {
	for _, mode := range []string{"on", "off"} {
		t.Run("idempotency_"+mode, func(t *testing.T) {
			p := requirePool(t)
			ledgerSvc := ledger.NewService(p)
			ext := newExternalService(p, ledgerSvc)
			for run := 1; run <= runs(); run++ {
				acc := newFundedAccount(t, p, ledgerSvc, idemCopies*100)
				shared := testsupport.RandomKey()

				var errs atomic.Int64
				startTogether(idemCopies, func(i int) {
					if _, err := ext.SendOutbound(context.Background(), acc.ID, "instant", "ACCT-123", ngn(100), keyFor(mode, shared, i)); err != nil {
						errs.Add(1)
					}
				})

				var rows int
				if err := p.QueryRow(context.Background(),
					`SELECT count(*) FROM external_transfers WHERE account_id = $1`, acc.ID).Scan(&rows); err != nil {
					t.Fatal(err)
				}
				total, active, reserved := holdStats(t, p, acc.ID)
				orphans, orphanAmount := orphanHolds(t, p, acc.ID)
				record(t, result{
					Test: "idempotency", Scenario: "externalpayments.SendOutbound x100 concurrent", Protection: "idempotency_" + mode, Run: run,
					Metrics: map[string]any{
						"copies": idemCopies, "transfer_rows_created": rows, "holds_created": total, "active_holds": active,
						"reserved_minor": reserved, "orphan_holds": orphans, "orphan_reserved_minor": orphanAmount, "errors": errs.Load(),
					},
				})
			}
		})
	}
}

// orphanHolds counts active holds on accountID that no external transfer,
// cooling-off transfer or card authorization points at: money reserved
// that nothing will ever capture or release.
func orphanHolds(t *testing.T, p *pgxpool.Pool, accountID string) (int, int64) {
	t.Helper()
	var n int
	var amount int64
	if err := p.QueryRow(context.Background(), `
		SELECT count(*), COALESCE(SUM(h.amount_minor), 0) FROM holds h
		WHERE h.account_id = $1 AND h.status = 'active'
		  AND NOT EXISTS (SELECT 1 FROM external_transfers et WHERE et.hold_id = h.id)
		  AND NOT EXISTS (SELECT 1 FROM cooling_off_transfers c WHERE c.hold_id = h.id)
		  AND NOT EXISTS (SELECT 1 FROM card_authorizations ca WHERE ca.hold_id = h.id)
	`, accountID).Scan(&n, &amount); err != nil {
		t.Fatalf("orphan holds: %v", err)
	}
	return n, amount
}

// httpEnv is the real HTTP handler stack, wired the same way cmd/api
// wires it, served on a real listener.
type httpEnv struct {
	server  *httptest.Server
	pool    *pgxpool.Pool
	ledger  *ledger.Service
	authSvc *auth.Service
}

func newHTTPEnv(t *testing.T, p *pgxpool.Pool) httpEnv {
	t.Helper()
	identitySvc := identity.NewService(p)
	ledgerSvc := ledger.NewService(p)
	registry := rails.NewRegistry()
	resilienceSvc := resilience.NewService(p)
	externalSvc := externalpayments.NewService(p, ledgerSvc, registry, risk.NewService(p, ledgerSvc, compliance.NewStubScreener(), nil), resilienceSvc, nil)
	kp := secrets.NewLocalKeyProvider()
	authSvc := auth.NewService(p, identitySvc, kp)
	paymentsSvc := payments.NewService(p, ledgerSvc, identitySvc, resilienceSvc)

	handler := publicapi.New(publicapi.Deps{
		ApiKeys:       apikeys.NewService(p),
		Auth:          authSvc,
		Identity:      identitySvc,
		Tokens:        tokens.NewService(p),
		Charges:       charges.NewService(p, tokens.NewService(p), externalSvc, resilienceSvc),
		Payouts:       payouts.NewService(p, externalSvc, registry, resilienceSvc),
		Checkout:      checkout.NewService(p),
		Webhooks:      webhooks.NewService(p, kp),
		CoolingOff:    coolingoff.NewService(p, ledgerSvc, paymentsSvc, identitySvc, resilienceSvc, nil),
		Consent:       consent.NewService(p),
		Disputes:      disputes.NewService(p, nil),
		Recovery:      recovery.NewService(p, identitySvc, authSvc, kyc.NewStubProvider()),
		Cards:         cards.NewService(p, ledgerSvc, resilienceSvc),
		Resilience:    resilienceSvc,
		AdminAPIKey:   "stress-admin-key",
		Pool:          p,
		PublicBaseURL: "http://localhost",
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return httpEnv{server: srv, pool: p, ledger: ledgerSvc, authSvc: authSvc}
}

type customer struct {
	identityID string
	email      string
	name       string
	accountID  string
	token      string
}

const stressPassword = "stress-password-not-for-humans"

func newCustomer(t *testing.T, env httpEnv, balance int64) customer {
	t.Helper()
	ctx := context.Background()
	hash, err := auth.HashPassword(stressPassword)
	if err != nil {
		t.Fatal(err)
	}
	c := customer{email: "stress-" + testsupport.RandomKey() + "@example.com", name: "Ada Stress " + testsupport.RandomKey()[:6]}
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO identities (kind, status, kyc_tier, email, password_hash, legal_name)
		VALUES ('individual', 'active', 2, $1, $2, $3) RETURNING id::text
	`, c.email, hash, c.name).Scan(&c.identityID); err != nil {
		t.Fatalf("insert customer: %v", err)
	}
	acc, err := env.ledger.OpenAccount(ctx, c.identityID, ledger.AccountTypeWallet, "NGN", 0)
	if err != nil {
		t.Fatal(err)
	}
	c.accountID = acc.ID
	if balance > 0 {
		if _, err := env.ledger.Credit(ctx, acc.ID, ngn(balance), testsupport.RandomKey()); err != nil {
			t.Fatal(err)
		}
	}
	c.token, _, err = env.authSvc.Login(ctx, c.email, stressPassword, "stress-device")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return c
}

func postTransfer(client *http.Client, baseURL string, from, to customer, amount int64, key string) (int, error) {
	body, _ := json.Marshal(map[string]any{
		"recipient_email": to.email, "recipient_name": to.name, "amount_minor": amount, "currency": "NGN",
	})
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/me/transfers", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+from.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// TestIdempotencyHTTPTransfer sends 100 concurrent copies of
// POST /v1/me/transfers through the real handler stack. Two paths exist
// behind that endpoint: a repeat payee posts immediately, a first-time
// payee is parked behind a cooling-off hold. Both are measured, and the
// cooling-off path is also retried one copy at a time (a client retrying
// after a timeout) to separate "concurrent" from "any retry".
func TestIdempotencyHTTPTransfer(t *testing.T) {
	for _, path := range []string{"repeat_payee", "first_time_payee", "first_time_payee_sequential"} {
		sequential := path == "first_time_payee_sequential"
		for _, mode := range []string{"on", "off"} {
			t.Run(path+"/idempotency_"+mode, func(t *testing.T) {
				p := requirePool(t)
				env := newHTTPEnv(t, p)
				client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: idemCopies}}
				for run := 1; run <= runs(); run++ {
					from := newCustomer(t, env, idemCopies*100+100)
					to := newCustomer(t, env, 0)
					if path == "repeat_payee" {
						// One earlier posted transfer makes this a known
						// payee, so the request takes the immediate path.
						if _, err := env.ledger.Transfer(context.Background(), from.accountID, to.accountID, ngn(100), testsupport.RandomKey()); err != nil {
							t.Fatal(err)
						}
					}
					postingsBefore := countPostingsTo(t, p, to.accountID)
					shared := testsupport.RandomKey()

					var mu sync.Mutex
					statuses := map[string]int{}
					send := func(i int) {
						code, err := postTransfer(client, env.server.URL, from, to, 100, keyFor(mode, shared, i))
						k := fmt.Sprint(code)
						if err != nil {
							k = "transport_error"
						}
						mu.Lock()
						statuses[k]++
						mu.Unlock()
					}
					if sequential {
						for i := 0; i < idemCopies; i++ {
							send(i)
						}
					} else {
						startTogether(idemCopies, send)
					}

					total, active, reserved := holdStats(t, p, from.accountID)
					orphans, orphanAmount := orphanHolds(t, p, from.accountID)
					var pending int
					if err := p.QueryRow(context.Background(),
						`SELECT count(*) FROM cooling_off_transfers WHERE from_account_id = $1`, from.accountID).Scan(&pending); err != nil {
						t.Fatal(err)
					}
					record(t, result{
						Test: "idempotency", Scenario: "POST /v1/me/transfers x100, " + path, Protection: "idempotency_" + mode, Run: run,
						Metrics: map[string]any{
							"copies": idemCopies, "postings_created": countPostingsTo(t, p, to.accountID) - postingsBefore,
							"cooling_off_rows": pending, "holds_created": total, "active_holds": active, "reserved_minor": reserved,
							"orphan_holds": orphans, "orphan_reserved_minor": orphanAmount, "http_status": statuses,
						},
					})
				}
			})
		}
	}
}
