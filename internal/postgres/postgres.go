// Package postgres provides the database connection pool and repositories
// backing the ledger. PostgreSQL is the ledger's system of record (see
// docs/banking-backend-spec.md §10): ACID transactions let a debit and its
// matching credit commit atomically, and invariants are enforced as real
// database constraints rather than only in application code.
package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool wraps a pgx connection pool.
type Pool struct {
	*pgxpool.Pool
}

// Connect opens a connection pool against dsn.
func Connect(ctx context.Context, dsn string) (*Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	return &Pool{pool}, nil
}

// Ping satisfies httpserver.Pinger for readiness checks.
func (p *Pool) Ping(ctx context.Context) error {
	return p.Pool.Ping(ctx)
}
