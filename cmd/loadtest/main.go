// Command loadtest drives load/soak testing against a running Kolo Bank API
// instance (docs/banking-backend-spec.md §Phase 10). It seeds its own test
// identities directly against Postgres (the same way internal/publicapi's
// tests do), then drives a mixed read/write workload against the real,
// running HTTP API over a fixed duration and concurrency, reporting
// p50/p95/p99 latency and error rate per scenario. Exits non-zero if the
// configured thresholds are breached, so a CI/ops pipeline can gate on it
// rather than eyeballing the output.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/auth"
	"github.com/toluwalase/kolo-bank-server/internal/identity"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/secrets"
)

const loadtestPassword = "loadtest-password-not-for-humans"

type account struct {
	sessionToken string
	toEmail      string
}

func main() {
	baseURL := flag.String("base-url", "http://localhost:8080", "API base URL")
	databaseURL := flag.String("database-url", os.Getenv("DATABASE_URL"), "Postgres DSN used only to seed test identities")
	concurrency := flag.Int("concurrency", 20, "number of concurrent workers")
	duration := flag.Duration("duration", 30*time.Second, "how long to run the load phase")
	seedAccounts := flag.Int("seed-accounts", 20, "number of test identities to create")
	maxErrorRate := flag.Float64("max-error-rate", 0.01, "fail if any scenario's error rate exceeds this fraction")
	maxP99Read := flag.Duration("max-p99-read", 500*time.Millisecond, "fail if read p99 exceeds this")
	maxP99Write := flag.Duration("max-p99-write", time.Second, "fail if write p99 exceeds this")
	flag.Parse()

	if *databaseURL == "" {
		log.Fatal("loadtest: -database-url (or DATABASE_URL) is required for seeding")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, *databaseURL)
	if err != nil {
		log.Fatalf("loadtest: connect to postgres: %v", err)
	}
	defer pool.Close()

	fmt.Printf("== seeding %d test accounts ==\n", *seedAccounts)
	accounts, err := seed(ctx, pool, *seedAccounts)
	if err != nil {
		log.Fatalf("loadtest: seed: %v", err)
	}
	fmt.Printf("seeded %d accounts\n", len(accounts))

	fmt.Printf("== running load: concurrency=%d duration=%s ==\n", *concurrency, *duration)
	results := run(*baseURL, accounts, *concurrency, *duration)

	ok := report(results, *maxErrorRate, *maxP99Read, *maxP99Write)
	if !ok {
		os.Exit(1)
	}
}

// seed creates N funded individual identities with open sessions, paired up
// so each account has another seeded account to send small transfers to.
func seed(ctx context.Context, pool *pgxpool.Pool, n int) ([]account, error) {
	identitySvc := identity.NewService(pool)
	ledgerSvc := ledger.NewService(pool)
	authSvc := auth.NewService(pool, identitySvc, secrets.NewLocalKeyProvider())

	passwordHash, err := auth.HashPassword(loadtestPassword)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	type seeded struct {
		identityID string
		email      string
		token      string
	}
	all := make([]seeded, 0, n)

	for i := 0; i < n; i++ {
		email := fmt.Sprintf("loadtest-%s@example.com", randomHex(8))
		var identityID string
		err := pool.QueryRow(ctx, `
			INSERT INTO identities (kind, status, kyc_tier, email, password_hash, legal_name)
			VALUES ('individual', 'active', 2, $1, $2, 'Load Test User')
			RETURNING id::text
		`, email, passwordHash).Scan(&identityID)
		if err != nil {
			return nil, fmt.Errorf("insert identity %d: %w", i, err)
		}

		acc, err := ledgerSvc.OpenAccount(ctx, identityID, ledger.AccountTypeWallet, "NGN", 0)
		if err != nil {
			return nil, fmt.Errorf("open account %d: %w", i, err)
		}
		if _, err := ledgerSvc.Credit(ctx, acc.ID, ledger.Money{Minor: 100_000_00, Currency: "NGN"}, randomHex(12)); err != nil {
			return nil, fmt.Errorf("fund account %d: %w", i, err)
		}

		token, _, err := authSvc.Login(ctx, email, loadtestPassword, "loadtest-device")
		if err != nil {
			return nil, fmt.Errorf("login %d: %w", i, err)
		}

		all = append(all, seeded{identityID: identityID, email: email, token: token})
	}

	accounts := make([]account, 0, n)
	for i, s := range all {
		partner := all[(i+1)%len(all)]
		accounts = append(accounts, account{sessionToken: s.token, toEmail: partner.email})
	}
	return accounts, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

type sample struct {
	scenario string
	latency  time.Duration
	ok       bool
}

func run(baseURL string, accounts []account, concurrency int, duration time.Duration) []sample {
	samples := make(chan sample, 4096)
	stop := time.Now().Add(duration)
	client := &http.Client{Timeout: 5 * time.Second}

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			acc := accounts[workerID%len(accounts)]
			for time.Now().Before(stop) {
				// 80% reads, 20% writes — a realistic-enough skew for a
				// banking API where balance/history checks dominate.
				if workerID%5 == 0 {
					samples <- doWrite(client, baseURL, acc)
				} else {
					samples <- doRead(client, baseURL, acc)
				}
			}
		}(i)
	}

	go func() { wg.Wait(); close(samples) }()

	var out []sample
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	lastReport := time.Now()
	for s := range samples {
		out = append(out, s)
		if time.Since(lastReport) > 5*time.Second {
			fmt.Printf("... %d requests so far\n", len(out))
			lastReport = time.Now()
		}
	}
	return out
}

func doRead(client *http.Client, baseURL string, acc account) sample {
	start := time.Now()
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/v1/me/transfers/pending", nil)
	req.Header.Set("Authorization", "Bearer "+acc.sessionToken)
	resp, err := client.Do(req)
	lat := time.Since(start)
	if err != nil {
		return sample{"read", lat, false}
	}
	defer resp.Body.Close()
	return sample{"read", lat, resp.StatusCode < 400}
}

func doWrite(client *http.Client, baseURL string, acc account) sample {
	body, _ := json.Marshal(map[string]any{
		"recipient_email": acc.toEmail,
		"recipient_name":  "Load Test User",
		"amount_minor":    100,
		"currency":        "NGN",
	})
	start := time.Now()
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/me/transfers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+acc.sessionToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", randomHex(16))
	resp, err := client.Do(req)
	lat := time.Since(start)
	if err != nil {
		return sample{"write", lat, false}
	}
	defer resp.Body.Close()
	// 409 (payee-mismatch block) is a well-formed business response, not
	// an infrastructure failure, so treat < 500 as "ok" for writes.
	return sample{"write", lat, resp.StatusCode < 500}
}

func report(samples []sample, maxErrorRate float64, maxP99Read, maxP99Write time.Duration) bool {
	byScenario := map[string][]sample{}
	for _, s := range samples {
		byScenario[s.scenario] = append(byScenario[s.scenario], s)
	}

	ok := true
	fmt.Println("\n== results ==")
	for _, scenario := range []string{"read", "write"} {
		group := byScenario[scenario]
		if len(group) == 0 {
			continue
		}
		lat := make([]time.Duration, len(group))
		errCount := 0
		for i, s := range group {
			lat[i] = s.latency
			if !s.ok {
				errCount++
			}
		}
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		p50 := percentile(lat, 0.50)
		p95 := percentile(lat, 0.95)
		p99 := percentile(lat, 0.99)
		errRate := float64(errCount) / float64(len(group))

		maxP99 := maxP99Read
		if scenario == "write" {
			maxP99 = maxP99Write
		}

		fmt.Printf("%-6s n=%-6d p50=%-10s p95=%-10s p99=%-10s errors=%d (%.2f%%)\n",
			scenario, len(group), p50, p95, p99, errCount, errRate*100)

		if errRate > maxErrorRate {
			fmt.Printf("  FAIL: %s error rate %.2f%% exceeds max %.2f%%\n", scenario, errRate*100, maxErrorRate*100)
			ok = false
		}
		if p99 > maxP99 {
			fmt.Printf("  FAIL: %s p99 %s exceeds max %s\n", scenario, p99, maxP99)
			ok = false
		}
	}

	if ok {
		fmt.Println("\nPASS: all scenarios within target")
	} else {
		fmt.Println("\nFAIL: one or more scenarios missed target")
	}
	return ok
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)))
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
