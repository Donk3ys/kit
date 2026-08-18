package pgtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Donk3ys/kit/apperr"
	"github.com/Donk3ys/kit/pg"
	"github.com/jackc/pgx/v5"
)

func TestInTxCommitsAndRollsBackRealRows(t *testing.T) {
	pool := newPool(t, pg.Config{})
	newTable(t, pool, "intx_rows", "id int PRIMARY KEY")

	t.Run("a returning-nil function has its writes committed", func(t *testing.T) {
		err := pg.InTx(t.Context(), pool, "insert", func(tx pgx.Tx) error {
			_, err := tx.Exec(t.Context(), "INSERT INTO intx_rows (id) VALUES (1)")
			return err
		})
		if err != nil {
			t.Fatalf("InTx returned %v", err)
		}
		if n := countRows(t, pool, "intx_rows"); n != 1 {
			t.Errorf("row count = %d, want 1 — the commit did not land", n)
		}
	})

	t.Run("a failing function has its writes discarded", func(t *testing.T) {
		opErr := apperr.NewConflict("ROLLED_BACK", "Conflict.", nil)
		// Read the count rather than assuming the subtest above ran, so this
		// still means something under `-run .../a_failing`.
		before := countRows(t, pool, "intx_rows")

		err := pg.InTx(t.Context(), pool, "insert", func(tx pgx.Tx) error {
			if _, err := tx.Exec(t.Context(), "INSERT INTO intx_rows (id) VALUES (2)"); err != nil {
				return err
			}
			return opErr
		})

		if !errors.Is(err, opErr) {
			t.Errorf("InTx returned %v, want the operation error unchanged", err)
		}
		if after := countRows(t, pool, "intx_rows"); after != before {
			t.Errorf("row count went %d -> %d; the failed write was not rolled back", before, after)
		}
	})
}

// TestPanickingCallbackReturnsItsConnectionToThePool is the assertion a stub
// cannot make. pg_test.go proves InTx calls Rollback when fn panics; only a
// real pool proves that rollback is what hands the connection back, because
// pgxpool releases a transaction's connection from Commit and Rollback and
// nowhere else, and nothing reaps one that is still checked out.
//
// MaxConns is 1 so a leak is unambiguous: the follow-up query has no second
// connection to fall back on. Its own short deadline is what turns the failure
// into a message instead of a hung test binary.
func TestPanickingCallbackReturnsItsConnectionToThePool(t *testing.T) {
	pool := newPool(t, pg.Config{MaxConns: 1})
	newTable(t, pool, "intx_panic", "id int PRIMARY KEY")

	func() {
		defer func() {
			if recover() == nil {
				t.Error("InTx swallowed the panic")
			}
		}()
		_ = pg.InTx(t.Context(), pool, "panicking", func(tx pgx.Tx) error {
			if _, err := tx.Exec(t.Context(), "INSERT INTO intx_panic (id) VALUES (1)"); err != nil {
				t.Errorf("insert: %v", err)
			}
			panic("handler bug")
		})
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM intx_panic").Scan(&n); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("the pool is exhausted: the panicking transaction never " +
				"returned its connection, and with MaxConns=1 nothing else can run")
		}
		t.Fatalf("counting rows: %v", err)
	}
	if n != 0 {
		t.Errorf("row count = %d, want 0 — the panicking transaction's write was committed", n)
	}
}

// pg_test.go's TestClosedTransactionIsNotACleanupFailure stubs pgx.ErrTxClosed
// as the thing a rollback returns after a failed commit. That is an assumption
// about a dependency, and this is the test that holds it to account: if pgx
// ever reports something else, PreserveRollbackError starts misclassifying a
// perfectly ordinary constraint violation as an unknown-data-state 500.
//
// A deferred foreign key is the clean way to make COMMIT itself fail — the
// insert passes, and the constraint is only checked at commit time.
func TestFailedCommitIsNotReportedAsACleanupFailure(t *testing.T) {
	pool := newPool(t, pg.Config{})
	newTable(t, pool, "deferred_fk",
		"id int PRIMARY KEY, parent int REFERENCES deferred_fk(id) DEFERRABLE INITIALLY DEFERRED")

	err := pg.InTx(t.Context(), pool, "insert", func(tx pgx.Tx) error {
		// Points at an id that does not exist. Accepted now, rejected at COMMIT.
		_, execErr := tx.Exec(t.Context(), "INSERT INTO deferred_fk (id, parent) VALUES (1, 999)")
		return execErr
	})

	if err == nil {
		t.Fatal("InTx returned nil for a transaction whose commit violated a constraint")
	}

	var rbErr *pg.RollbackError
	if errors.As(err, &rbErr) {
		t.Errorf("a failed commit was reported as a cleanup failure: %v\n"+
			"pgx no longer returns ErrTxClosed after a failed commit; "+
			"pg.PreserveRollbackError's check needs updating", err)
	}

	appErr, ok := apperr.From(err)
	if !ok {
		t.Fatalf("error was not an *apperr.Error: %v", err)
	}
	if appErr.Code != "TRANSACTION_COMMIT_FAILED" {
		t.Errorf("Code = %q, want TRANSACTION_COMMIT_FAILED", appErr.Code)
	}
	if appErr.Kind != apperr.KindExternal {
		t.Errorf("Kind = %v, want %v", appErr.Kind, apperr.KindExternal)
	}
	if n := countRows(t, pool, "deferred_fk"); n != 0 {
		t.Errorf("row count = %d, want 0", n)
	}
}

// The regression this protects is the one PreserveRollbackError's
// context.WithoutCancel comment describes. Against a stub, dropping that call
// only changes which context a recorder sees; against a real server it leaks
// the transaction, because pgx refuses to send the ROLLBACK at all.
func TestRollbackSucceedsAfterTheRequestContextIsCancelled(t *testing.T) {
	pool := newPool(t, pg.Config{})
	newTable(t, pool, "cancelled_rollback", "id int PRIMARY KEY")

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("Begin returned %v", err)
	}
	if _, err := tx.Exec(t.Context(), "INSERT INTO cancelled_rollback (id) VALUES (1)"); err != nil {
		t.Fatalf("Exec returned %v", err)
	}

	// The request is gone — very often that cancellation is why the operation
	// failed in the first place.
	reqCtx, cancel := context.WithCancel(t.Context())
	cancel()

	opErr := errors.New("query cancelled")
	err = pg.PreserveRollbackError(reqCtx, tx, opErr, "scope")

	var rbErr *pg.RollbackError
	if errors.As(err, &rbErr) {
		t.Fatalf("rollback failed on a cancelled request context: %v\n"+
			"the context.WithoutCancel in pg.PreserveRollbackError is what prevents this", err)
	}
	if !errors.Is(err, opErr) {
		t.Errorf("returned %v, want the operation error", err)
	}
	if n := countRows(t, pool, "cancelled_rollback"); n != 0 {
		t.Errorf("row count = %d, want 0 — the transaction was not rolled back", n)
	}
}

// pg.Beginner is satisfied by pgx.Tx as well as the pool, and the doc comment
// claims that gets you a savepoint rather than a second helper. Only a server
// can confirm the claim, since savepoint semantics are the server's.
func TestNestedInTxRollsBackToASavepoint(t *testing.T) {
	pool := newPool(t, pg.Config{})
	newTable(t, pool, "savepoints", "id int PRIMARY KEY")

	inner := apperr.NewConflict("INNER_FAILED", "Conflict.", nil)

	err := pg.InTx(t.Context(), pool, "outer", func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), "INSERT INTO savepoints (id) VALUES (1)"); err != nil {
			return err
		}

		// The inner failure must not take the outer transaction with it.
		nested := pg.InTx(t.Context(), tx, "inner", func(sp pgx.Tx) error {
			if _, err := sp.Exec(t.Context(), "INSERT INTO savepoints (id) VALUES (2)"); err != nil {
				return err
			}
			return inner
		})
		if !errors.Is(nested, inner) {
			t.Errorf("nested InTx returned %v, want the inner error", nested)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("outer InTx returned %v", err)
	}

	rows, err := pool.Query(t.Context(), "SELECT id FROM savepoints ORDER BY id")
	if err != nil {
		t.Fatalf("Query returned %v", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		t.Fatalf("CollectRows returned %v", err)
	}

	if len(ids) != 1 || ids[0] != 1 {
		t.Errorf("ids = %v, want [1] — the outer write should survive and the inner should not", ids)
	}
}
