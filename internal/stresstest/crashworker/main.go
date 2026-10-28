//go:build stress

// Command crashworker is the process TestCrashRecovery kills. It does
// what cmd/api's external-payments goroutines do: submit outbound
// transfers (as a client retrying with fixed idempotency keys would), and
// run ProcessPending and ResolveStuck on a loop. The test SIGKILLs it at
// random moments and starts it again. Ticks are faster than cmd/api's (3s
// and 60s) so a run finishes in minutes; stuckTimeout itself is the real
// 2-minute production value.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/compliance"
	"github.com/toluwalase/kolo-bank-server/internal/externalpayments"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/rails"
	"github.com/toluwalase/kolo-bank-server/internal/resilience"
	"github.com/toluwalase/kolo-bank-server/internal/risk"
)

func main() {
	accountsFile := flag.String("accounts", "", "file with one account id per line")
	perAccount := flag.Int("per-account", 4, "transfers to submit per account")
	runID := flag.String("run-id", "", "prefix for idempotency keys")
	flag.Parse()

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ledgerSvc := ledger.NewService(pool)
	ext := externalpayments.NewService(pool, ledgerSvc, rails.NewRegistry(),
		risk.NewService(pool, ledgerSvc, compliance.NewStubScreener(), logger), resilience.NewService(pool), logger)

	accounts, err := readLines(*accountsFile)
	if err != nil {
		log.Fatal(err)
	}

	go func() {
		for i := 0; i < len(accounts)*(*perAccount); i++ {
			acc := accounts[i%len(accounts)]
			ref := "ACCT-OK"
			if i%4 == 0 {
				ref = "ACCT-RAILFAIL" // a quarter of transfers take the failure path
			}
			key := fmt.Sprintf("crash-%s-%d", *runID, i)
			if _, err := ext.SendOutbound(ctx, acc, "instant", ref, ledger.Money{Minor: int64(100 + i%7), Currency: "NGN"}, key); err != nil {
				logger.Error("submit", "key", key, "error", err)
			}
			time.Sleep(5 * time.Millisecond)
		}
		fmt.Println("SUBMITTED")
	}()

	pending := time.NewTicker(100 * time.Millisecond)
	stuck := time.NewTicker(500 * time.Millisecond)
	for {
		select {
		case <-pending.C:
			_ = ext.ProcessPending(ctx)
		case <-stuck.C:
			_ = ext.ResolveStuck(ctx)
		}
	}
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if sc.Text() != "" {
			out = append(out, sc.Text())
		}
	}
	return out, sc.Err()
}
