//go:build stress

// Command k6seed creates the customers scripts/stress/transfers.js sends
// money between, and writes their session tokens to a JSON file k6 reads.
// Each sender gets its own recipient, and one earlier transfer to that
// recipient is posted directly, so the load hits the immediate-transfer
// path of POST /v1/me/transfers rather than the cooling-off path.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toluwalase/kolo-bank-server/internal/auth"
	"github.com/toluwalase/kolo-bank-server/internal/identity"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/secrets"
)

const password = "k6-password-not-for-humans"

type pair struct {
	Token   string `json:"token"`
	ToEmail string `json:"to_email"`
	ToName  string `json:"to_name"`
}

func main() {
	n := flag.Int("senders", 200, "number of sender/recipient pairs")
	out := flag.String("out", "k6-pairs.json", "output file")
	flag.Parse()

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	ledgerSvc := ledger.NewService(pool)
	authSvc := auth.NewService(pool, identity.NewService(pool), secrets.NewLocalKeyProvider())
	hash, err := auth.HashPassword(password)
	if err != nil {
		log.Fatal(err)
	}

	create := func(name string, balance int64) (email, accountID string) {
		email = fmt.Sprintf("k6-%s@example.com", randomHex(8))
		var id string
		if err := pool.QueryRow(ctx, `
			INSERT INTO identities (kind, status, kyc_tier, email, password_hash, legal_name)
			VALUES ('individual', 'active', 2, $1, $2, $3) RETURNING id::text
		`, email, hash, name).Scan(&id); err != nil {
			log.Fatal(err)
		}
		acc, err := ledgerSvc.OpenAccount(ctx, id, ledger.AccountTypeWallet, "NGN", 0)
		if err != nil {
			log.Fatal(err)
		}
		if balance > 0 {
			if _, err := ledgerSvc.Credit(ctx, acc.ID, ledger.Money{Minor: balance, Currency: "NGN"}, randomHex(12)); err != nil {
				log.Fatal(err)
			}
		}
		return email, acc.ID
	}

	pairs := make([]pair, 0, *n)
	for i := 0; i < *n; i++ {
		fromEmail, fromAcc := create("K6 Sender", 1_000_000_00)
		toName := fmt.Sprintf("K6 Recipient %d", i)
		toEmail, toAcc := create(toName, 0)
		if _, err := ledgerSvc.Transfer(ctx, fromAcc, toAcc, ledger.Money{Minor: 1, Currency: "NGN"}, randomHex(12)); err != nil {
			log.Fatal(err)
		}
		token, _, err := authSvc.Login(ctx, fromEmail, password, "k6")
		if err != nil {
			log.Fatal(err)
		}
		pairs = append(pairs, pair{Token: token, ToEmail: toEmail, ToName: toName})
	}
	b, _ := json.Marshal(pairs)
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %d pairs to %s\n", len(pairs), *out)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}
