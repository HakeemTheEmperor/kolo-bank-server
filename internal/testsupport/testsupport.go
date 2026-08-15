// Package testsupport provides a shared real-Postgres test harness for the
// Phase 2 packages (identity, kyc, compliance, auth, onboarding, audit).
// It connects to whatever docker-compose already brings up for `make test`
// via DATABASE_URL, the same approach internal/ledger's own self-contained
// harness uses (see internal/ledger/testdb_test.go), factored out here
// because five packages now need it instead of one.
package testsupport

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
)

var (
	once     sync.Once
	pool     *pgxpool.Pool
	initErr  error
)

// RequireTestPool returns a pgxpool connected to the test database, running
// migrations on first use. It skips the calling test when DATABASE_URL is
// unset, so pure unit tests in the same package still run standalone (e.g.
// via `go test` on a dev machine with no Postgres running).
func RequireTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping DB-backed test (run via `make test`)")
	}

	once.Do(func() {
		if err := runMigrations(dsn); err != nil {
			initErr = fmt.Errorf("testsupport: run migrations: %w", err)
			return
		}
		p, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			initErr = fmt.Errorf("testsupport: connect pool: %w", err)
			return
		}
		pool = p
	})

	if initErr != nil {
		t.Fatalf("testsupport: %v", initErr)
	}

	return pool
}

// migrationLockKey is an arbitrary fixed key for a Postgres session-level
// advisory lock. `go test ./...` runs each package as its own OS process,
// so the in-process `once` above only serializes within a package — every
// package still races to run goose.Up against the same fresh database at
// once. Concurrent CREATE TABLE/CREATE TYPE from two racing goose runs can
// hit Postgres catalog unique-constraint errors, so the migration step
// itself is additionally serialized across processes with this lock.
const migrationLockKey = 8825172

func runMigrations(dsn string) error {
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	// Advisory locks are session-scoped: pinning this *sql.DB to exactly
	// one physical connection guarantees the lock/unlock below and the
	// goose.Up in between all run on the same Postgres session.
	sqlDB.SetMaxOpenConns(1)

	if _, err := sqlDB.Exec(`SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() { _, _ = sqlDB.Exec(`SELECT pg_advisory_unlock($1)`, migrationLockKey) }()

	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.Up(sqlDB, migrationsDir())
}

func migrationsDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "db", "migrations")
}

// RandomUUID generates an RFC 4122 v4 UUID string without pulling in a
// dependency solely for test fixtures.
func RandomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// RandomKey generates a short random token, e.g. for idempotency keys or
// test email local-parts.
func RandomKey() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}
