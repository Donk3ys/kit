// Package pgtest exercises kit's pg package against a real PostgreSQL.
//
// pg/pg_test.go covers everything that can be asserted without a server, and
// pg.PoolConfig exists precisely so the configuration translation is one of
// those things. What is left needs a live server to mean anything: that
// PostgreSQL accepts the runtime parameters PoolConfig writes and reads them
// as the durations intended, that NewPool's ping is what turns an unreachable
// database into a startup failure, and that pgx really behaves the way the
// stubs in pg_test.go assume it does.
//
// These tests need a Docker daemon. `go test -short` is the explicit opt-out;
// a missing or broken daemon fails loudly rather than skipping, because a
// silent skip here would report coverage this module does not have.
package pgtest

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Donk3ys/kit/pg"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// Pinned rather than :latest so a server-side default changing upstream is a
// deliberate bump here, not a test that starts failing on an unrelated day.
const postgresImage = "postgres:17-alpine"

// containerDSN points at the one PostgreSQL shared by every test in this
// package. Each test builds its own pool over it, since the pool configuration
// is usually the thing under test.
var containerDSN string

func TestMain(m *testing.M) {
	// testing.Short panics before the flags are parsed, and TestMain runs
	// before the testing package parses them itself.
	flag.Parse()
	if testing.Short() {
		fmt.Fprintln(os.Stderr, "pgtest: skipped, -short is set (these tests need Docker)")
		os.Exit(0)
	}

	code, err := runWithPostgres(m)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgtest: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

// runWithPostgres exists so the container is terminated on every path.
// os.Exit in TestMain does not run deferred functions.
func runWithPostgres(m *testing.M) (int, error) {
	ctx := context.Background()

	container, err := postgres.Run(ctx, postgresImage,
		postgres.WithDatabase("kit_pgtest"),
		postgres.WithUsername("kit"),
		postgres.WithPassword("secret"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		return 0, fmt.Errorf("starting %s: %w\n"+
			"These tests need a running Docker daemon. Use `go test -short` to skip them.",
			postgresImage, err)
	}
	defer func() {
		// A leaked container outlives the test run and holds a port, so this
		// failure is worth printing even though the run itself is over.
		if err := container.Terminate(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "pgtest: terminating container: %v\n", err)
		}
	}()

	// sslmode=disable is stated rather than left to pgx's negotiation, for the
	// same reason the Makefile states it: a throwaway container has no TLS to
	// negotiate and the handshake would only be somewhere to stall.
	containerDSN, err = container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return 0, fmt.Errorf("connection string: %w", err)
	}

	return m.Run(), nil
}

// newPool builds a pool through pg.NewPool — the constructor under test — and
// closes it when the test ends. A zero DSN means the shared container.
func newPool(t *testing.T, cfg pg.Config) *pgxpool.Pool {
	t.Helper()
	if cfg.DSN == "" {
		cfg.DSN = containerDSN
	}

	pool, err := pg.NewPool(t.Context(), cfg)
	if err != nil {
		t.Fatalf("pg.NewPool returned %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newTable creates a table for one test and drops it afterwards, so tests
// sharing the container cannot see each other's rows.
func newTable(t *testing.T, pool *pgxpool.Pool, name, columns string) {
	t.Helper()

	if _, err := pool.Exec(t.Context(), "CREATE TABLE "+name+" ("+columns+")"); err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}
	t.Cleanup(func() {
		// t.Context is already cancelled by the time cleanup runs, which is
		// the same reason pg.PreserveRollbackError does not use it either.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
			t.Errorf("dropping %s: %v", name, err)
		}
	})
}

// countRows is the standard question these tests ask: did the write survive?
func countRows(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()

	var n int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return n
}
