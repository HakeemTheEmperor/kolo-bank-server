// Package idempotency makes mutating ledger operations safe to retry.
//
// Execute relies on the idempotency_keys table's PRIMARY KEY: when two
// callers race on the same key, Postgres blocks the second INSERT until the
// first transaction resolves. If the first commits, the second observes a
// unique_violation and fetches the now-committed response. If the first
// rolls back, the second's INSERT succeeds and it becomes the one true
// executor. This guarantees at most one execution per key without a
// separate locking step, and the key row is written atomically in the same
// database transaction as the caller's own writes (see fn's tx parameter).
package idempotency

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const uniqueViolationCode = "23505"

// ErrKeyReused is returned when an idempotency key is reused with a
// different request payload than the one it was first associated with.
var ErrKeyReused = errors.New("idempotency: key reused with a different request")

// Execute runs fn at most once for the given key. fn receives the open
// transaction so its writes commit atomically with the idempotency record.
// Concurrent or repeated calls with the same key and requestHash return the
// first call's stored response without re-running fn.
func Execute(
	ctx context.Context,
	pool *pgxpool.Pool,
	key string,
	requestHash string,
	ttl time.Duration,
	fn func(ctx context.Context, tx pgx.Tx) ([]byte, error),
) ([]byte, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("idempotency: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx,
		`INSERT INTO idempotency_keys (key, request_hash, status, expires_at)
		 VALUES ($1, $2, 'in_progress', now() + $3)`,
		key, requestHash, ttl,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolationCode {
			// Another execution won the race and has since committed
			// (our blocked INSERT only proceeds after it resolves). Release
			// our own failed transaction's connection before acquiring
			// another for fetchCompleted, or concurrent losers can starve
			// the pool waiting on each other's still-held connections.
			_ = tx.Rollback(ctx)
			return fetchCompleted(ctx, pool, key, requestHash)
		}
		return nil, fmt.Errorf("idempotency: reserve key: %w", err)
	}

	resp, err := fn(ctx, tx)
	if err != nil {
		return nil, err
	}

	if _, err := tx.Exec(ctx,
		`UPDATE idempotency_keys SET status = 'completed', response_body = $2 WHERE key = $1`,
		key, resp,
	); err != nil {
		return nil, fmt.Errorf("idempotency: record completion: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("idempotency: commit: %w", err)
	}

	return resp, nil
}

func fetchCompleted(ctx context.Context, pool *pgxpool.Pool, key, requestHash string) ([]byte, error) {
	var (
		storedHash string
		status     string
		response   []byte
	)

	err := pool.QueryRow(ctx,
		`SELECT request_hash, status, response_body FROM idempotency_keys WHERE key = $1`,
		key,
	).Scan(&storedHash, &status, &response)
	if err != nil {
		return nil, fmt.Errorf("idempotency: fetch existing key: %w", err)
	}

	if storedHash != requestHash {
		return nil, ErrKeyReused
	}
	if status != "completed" {
		return nil, fmt.Errorf("idempotency: key %q is still in progress, retry later", key)
	}

	return response, nil
}
