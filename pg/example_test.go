package pg_test

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Donk3ys/kit/apperr"
	"github.com/Donk3ys/kit/pg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pool stands in for the one built at startup. The examples below that touch a
// database have no Output comment, so they are compiled but never run — which
// is what keeps them honest without needing a live PostgreSQL.
var pool *pgxpool.Pool

func ExampleNewPool() {
	ctx := context.Background()

	pool, err := pg.NewPool(ctx, pg.Config{
		DSN:             "postgres://user:pw@localhost:5432/appdb",
		ApplicationName: "shop-api",
		MaxConns:        25,
		// Strongly recommended, and deliberately not defaulted: the right
		// value is a product decision, and a guess would silently kill a
		// legitimate long-running report.
		StatementTimeout:                30 * time.Second,
		LockTimeout:                     5 * time.Second,
		IdleInTransactionSessionTimeout: 15 * time.Second,
	})
	if err != nil {
		return
	}
	defer pool.Close()
}

// InTx commits when fn returns nil and rolls back otherwise. fn must not
// commit or roll back itself.
func ExampleInTx() {
	ctx := context.Background()

	err := pg.InTx(ctx, pool, "attach_payslip", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE ledger SET payslip_id = $1 WHERE id = $2`, 7, 42); err != nil {
			return apperr.NewExternal("LEDGER_UPDATE_FAILED",
				"Try again shortly.", "postgres", err)
		}
		_, err := tx.Exec(ctx, `UPDATE payslips SET attached = true WHERE id = $1`, 7)
		return err
	})
	if err != nil {
		// Return it unchanged; the HTTP boundary classifies and logs it.
		return
	}
}

// Beginner is satisfied by pgx.Tx as well as *pgxpool.Pool, so a nested call
// opens a savepoint rather than needing a second helper.
func ExampleInTx_nested() {
	ctx := context.Background()

	_ = pg.InTx(ctx, pool, "outer", func(tx pgx.Tx) error {
		return pg.InTx(ctx, tx, "inner", func(pgx.Tx) error {
			return nil
		})
	})
}

// A rollback that itself fails outranks the error that triggered it: the data
// is in an unknown state, which matters more than what went wrong first. The
// original error stays reachable through errors.Is either way.
func ExamplePreserveRollbackError() {
	ctx := context.Background()
	opErr := apperr.NewConflict("EMAIL_TAKEN", "Already registered.", nil)

	clean := pg.PreserveRollbackError(ctx, &stubTx{}, opErr, "register")
	fmt.Println(errors.Is(clean, opErr), isCleanupFailure(clean))

	broken := &stubTx{rollbackErr: errors.New("connection reset")}
	failed := pg.PreserveRollbackError(ctx, broken, opErr, "register")
	fmt.Println(errors.Is(failed, opErr), isCleanupFailure(failed))

	// Output:
	// true false
	// true true
}

// isCleanupFailure reports what respond's classifier looks for when deciding
// that a failure outranks whatever it wraps.
func isCleanupFailure(err error) bool {
	var cleanup interface{ UnexpectedCleanupFailure() bool }
	return errors.As(err, &cleanup) && cleanup.UnexpectedCleanupFailure()
}

// PoolConfig exists separately from NewPool so the configuration translation
// is inspectable, and testable, without a database.
func ExamplePoolConfig() {
	cfg, err := pg.PoolConfig(pg.Config{
		DSN:              "postgres://user:pw@localhost:5432/appdb",
		ApplicationName:  "shop-api",
		StatementTimeout: 30 * time.Second,
	})
	if err != nil {
		return
	}

	fmt.Println(cfg.ConnConfig.ConnectTimeout)
	// PostgreSQL expresses durations as bare milliseconds.
	fmt.Println(cfg.ConnConfig.RuntimeParams["statement_timeout"])
	fmt.Println(cfg.ConnConfig.RuntimeParams["application_name"])

	// Output:
	// 5s
	// 30000
	// shop-api
}
