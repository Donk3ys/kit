package pgtest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Donk3ys/kit/apperr"
	"github.com/Donk3ys/kit/pg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pg_test.go proves PoolConfig writes "30000" into RuntimeParams. Only a
// server proves PostgreSQL accepts that parameter name at startup and reads
// the value as 30 seconds — a unit test cannot tell a correct translation from
// a plausible one.
//
// SHOW takes no parameters, so the setting name is concatenated; every name
// here is a constant in this file.
func TestNewPoolAppliesSettingsTheServerAgreesWith(t *testing.T) {
	pool := newPool(t, pg.Config{
		ApplicationName:                 "kit-pgtest",
		StatementTimeout:                30 * time.Second,
		LockTimeout:                     5 * time.Second,
		IdleInTransactionSessionTimeout: 15 * time.Second,
	})

	// PostgreSQL echoes a duration back in the largest unit that represents it
	// exactly, which is why these are not the millisecond strings that went in.
	want := map[string]string{
		"application_name":                    "kit-pgtest",
		"statement_timeout":                   "30s",
		"lock_timeout":                        "5s",
		"idle_in_transaction_session_timeout": "15s",
	}

	for setting, want := range want {
		var got string
		if err := pool.QueryRow(t.Context(), "SHOW "+setting).Scan(&got); err != nil {
			t.Errorf("SHOW %s: %v", setting, err)
			continue
		}
		if got != want {
			t.Errorf("SHOW %s = %q, want %q", setting, got, want)
		}
	}
}

// Zero means "keep the server default". pg_test.go asserts the parameter is
// absent from the map; this asserts what that absence actually produces, which
// is the claim the Config doc comment makes.
func TestNewPoolLeavesUnsetTimeoutsAtTheServerDefault(t *testing.T) {
	pool := newPool(t, pg.Config{})

	for _, setting := range []string{
		"statement_timeout", "lock_timeout", "idle_in_transaction_session_timeout",
	} {
		var got string
		if err := pool.QueryRow(t.Context(), "SHOW "+setting).Scan(&got); err != nil {
			t.Errorf("SHOW %s: %v", setting, err)
			continue
		}
		if got != "0" {
			t.Errorf("SHOW %s = %q, want %q — kit invented a timeout the app did not ask for",
				setting, got, "0")
		}
	}
}

// The reason StatementTimeout is recommended at all: one runaway query must not
// pin a connection until somebody notices.
func TestStatementTimeoutCancelsARunawayQuery(t *testing.T) {
	pool := newPool(t, pg.Config{StatementTimeout: 250 * time.Millisecond})

	// Long enough to outlast the timeout by a wide margin, short enough that a
	// broken timeout fails this test in seconds rather than stalling the suite.
	start := time.Now()
	_, err := pool.Exec(t.Context(), "SELECT pg_sleep(5)")
	elapsed := time.Since(start)

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("Exec returned %v, want a *pgconn.PgError", err)
	}
	// 57014 is query_canceled: the server stopped it, rather than the client
	// giving up, which is the whole difference between this and a context
	// deadline.
	if pgErr.Code != "57014" {
		t.Errorf("SQLSTATE = %s (%s), want 57014 query_canceled", pgErr.Code, pgErr.Message)
	}
	if elapsed > 2*time.Second {
		t.Errorf("query ran for %v against a 250ms statement_timeout", elapsed)
	}
}

// NewPool's doc comment says the ping is there because pgxpool connects
// lazily, so without it an unreachable server is discovered by the first user
// request rather than at startup. This asserts both halves of that: the raw
// constructor really does succeed, and NewPool really does not.
func TestNewPoolFailsClosedWhenTheServerIsUnreachable(t *testing.T) {
	// Nothing listens on port 1, so this is refused immediately rather than
	// waiting out a timeout.
	const unreachable = "postgres://kit:hunter2@127.0.0.1:1/kit_pgtest?sslmode=disable"
	cfg := pg.Config{DSN: unreachable, ConnectTimeout: 2 * time.Second}

	t.Run("pgxpool alone would have reported success", func(t *testing.T) {
		poolCfg, err := pg.PoolConfig(cfg)
		if err != nil {
			t.Fatalf("PoolConfig returned %v", err)
		}
		pool, err := pgxpool.NewWithConfig(t.Context(), poolCfg)
		if err != nil {
			t.Skipf("pgxpool now connects eagerly (%v); NewPool's ping may be redundant", err)
		}
		pool.Close()
	})

	t.Run("NewPool does not", func(t *testing.T) {
		pool, err := pg.NewPool(t.Context(), cfg)
		if err == nil {
			pool.Close()
			t.Fatal("NewPool returned a working pool for an unreachable server")
		}

		appErr, ok := apperr.From(err)
		if !ok {
			t.Fatalf("error was not an *apperr.Error: %v", err)
		}
		if appErr.Kind != apperr.KindExternal {
			t.Errorf("Kind = %v, want %v", appErr.Kind, apperr.KindExternal)
		}
		if appErr.Code != "DATABASE_UNAVAILABLE" {
			t.Errorf("Code = %q, want DATABASE_UNAVAILABLE", appErr.Code)
		}
		// The DSN carries the password. Only the wrapped cause may hold it.
		if strings.Contains(appErr.SafeDetail, "hunter2") {
			t.Errorf("SafeDetail leaked the DSN password: %q", appErr.SafeDetail)
		}
	})
}

// recordingTracer records the SQL it is handed. Concurrent, because a pool
// warms MinConns connections in the background.
type recordingTracer struct {
	mu  sync.Mutex
	sql []string
}

func (r *recordingTracer) TraceQueryStart(
	ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData,
) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sql = append(r.sql, data.SQL)
	return ctx
}

func (r *recordingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (r *recordingTracer) saw(want string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.sql {
		if s == want {
			return true
		}
	}
	return false
}

// The QueryTracer field exists because there is otherwise no way to reach
// ConnConfig.Tracer through this constructor. pg_test.go proves NewPool stores
// it; this proves pgx then actually calls it, which is the only reason storing
// it is worth anything.
func TestQueryTracerSeesQueriesRunThroughThePool(t *testing.T) {
	tracer := &recordingTracer{}
	pool := newPool(t, pg.Config{QueryTracer: tracer})

	const query = "SELECT 1 -- traced"
	if _, err := pool.Exec(t.Context(), query); err != nil {
		t.Fatalf("Exec returned %v", err)
	}

	if !tracer.saw(query) {
		t.Errorf("tracer recorded %q, want it to include %q", tracer.sql, query)
	}
}
