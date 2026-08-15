package ledger_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
)

// testPool is shared by all DB-backed tests in this package. It connects to
// the Postgres instance docker-compose already brings up for `make test`
// (via DATABASE_URL) rather than spinning up a nested container, which
// would require Docker-in-Docker socket access from inside the test
// container. Tests that need it call requireTestPool, which skips
// gracefully when DATABASE_URL is unset so pure unit tests in this package
// still run standalone.
var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn != "" {
		if err := runMigrations(dsn); err != nil {
			fmt.Fprintln(os.Stderr, "ledger: run migrations:", err)
			os.Exit(1)
		}

		pool, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ledger: connect pool:", err)
			os.Exit(1)
		}
		testPool = pool
	}

	code := m.Run()

	if testPool != nil {
		testPool.Close()
	}
	os.Exit(code)
}

func requireTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skip("DATABASE_URL not set; skipping DB-backed test (run via `make test`)")
	}
	return testPool
}

func runMigrations(dsn string) error {
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.Up(sqlDB, migrationsDir())
}

func migrationsDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "db", "migrations")
}

// randomUUID generates an RFC 4122 v4 UUID string without pulling in a
// dependency solely for test fixtures.
func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// randomKey generates a short random token, e.g. for idempotency keys.
func randomKey() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}
