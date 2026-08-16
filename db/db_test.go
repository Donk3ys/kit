package db_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Donk3ys/kit/apperr"
	"github.com/Donk3ys/kit/db"
	"github.com/jackc/pgx/v5"
)

const testDSN = "postgres://user:pw@localhost:5432/appdb"

func TestPoolConfigDefaultsAndOverrides(t *testing.T) {
	t.Run("connect timeout defaults rather than staying unbounded", func(t *testing.T) {
		cfg, err := db.PoolConfig(db.Config{DSN: testDSN})
		if err != nil {
			t.Fatalf("PoolConfig returned %v", err)
		}
		if cfg.ConnConfig.ConnectTimeout != db.DefaultConnectTimeout {
			t.Errorf("ConnectTimeout = %v, want %v",
				cfg.ConnConfig.ConnectTimeout, db.DefaultConnectTimeout)
		}
	})

	t.Run("pool sizing is applied", func(t *testing.T) {
		cfg, err := db.PoolConfig(db.Config{
			DSN:             testDSN,
			MaxConns:        25,
			MinConns:        5,
			MaxConnLifetime: time.Hour,
			MaxConnIdleTime: 10 * time.Minute,
			ConnectTimeout:  2 * time.Second,
		})
		if err != nil {
			t.Fatalf("PoolConfig returned %v", err)
		}
		if cfg.MaxConns != 25 || cfg.MinConns != 5 {
			t.Errorf("conns = %d/%d, want 25/5", cfg.MaxConns, cfg.MinConns)
		}
		if cfg.MaxConnLifetime != time.Hour || cfg.MaxConnIdleTime != 10*time.Minute {
			t.Errorf("lifetimes = %v/%v", cfg.MaxConnLifetime, cfg.MaxConnIdleTime)
		}
		if cfg.ConnConfig.ConnectTimeout != 2*time.Second {
			t.Errorf("ConnectTimeout = %v, want 2s", cfg.ConnConfig.ConnectTimeout)
		}
	})
}

// PostgreSQL expresses these as bare milliseconds; a Go duration written
// verbatim would be silently rejected at connection time.
func TestPoolConfigWritesTimeoutsAsMilliseconds(t *testing.T) {
	cfg, err := db.PoolConfig(db.Config{
		DSN:                             testDSN,
		ApplicationName:                 "ftc-api",
		StatementTimeout:                30 * time.Second,
		LockTimeout:                     5 * time.Second,
		IdleInTransactionSessionTimeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("PoolConfig returned %v", err)
	}

	want := map[string]string{
		"application_name":                    "ftc-api",
		"statement_timeout":                   "30000",
		"lock_timeout":                        "5000",
		"idle_in_transaction_session_timeout": "15000",
	}
	for k, v := range want {
		if got := cfg.ConnConfig.RuntimeParams[k]; got != v {
			t.Errorf("RuntimeParams[%q] = %q, want %q", k, got, v)
		}
	}
}

// Zero means "keep the server default" — this package must not invent a
// statement timeout that could kill a legitimate long-running query.
func TestPoolConfigLeavesUnsetTimeoutsAlone(t *testing.T) {
	cfg, err := db.PoolConfig(db.Config{DSN: testDSN})
	if err != nil {
		t.Fatalf("PoolConfig returned %v", err)
	}
	for _, k := range []string{
		"statement_timeout", "lock_timeout", "idle_in_transaction_session_timeout",
		"application_name",
	} {
		if got, present := cfg.ConnConfig.RuntimeParams[k]; present {
			t.Errorf("RuntimeParams[%q] = %q, want it unset", k, got)
		}
	}
}

func TestPoolConfigRejectsBadInputWithoutEchoingTheDSN(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
	}{
		{"empty", ""},
		{"unparseable", "postgres://user:hunter2@%%%/db"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := db.PoolConfig(db.Config{DSN: tt.dsn})
			if err == nil {
				t.Fatal("PoolConfig accepted an invalid DSN")
			}
			appErr, ok := apperr.From(err)
			if !ok {
				t.Fatalf("error was not an *apperr.Error: %v", err)
			}
			// The DSN carries the password; only the wrapped cause may hold it.
			if appErr.SafeDetail != "An unexpected error occurred." {
				t.Errorf("SafeDetail = %q, want the generic message", appErr.SafeDetail)
			}
		})
	}
}

// stubTx implements just enough of pgx.Tx to exercise the helpers without a
// database. Any method not overridden panics, which is the intended signal
// that a test reached code it was not written for.
type stubTx struct {
	pgx.Tx
	commitErr   error
	rollbackErr error
	committed   bool
	rolledBack  bool
	// rollbackCtxHadDeadline records whether cleanup ran on a live context,
	// which is what proves the cancelled-request path works.
	rollbackCtxErr error
}

func (s *stubTx) Commit(context.Context) error { s.committed = true; return s.commitErr }
func (s *stubTx) Rollback(ctx context.Context) error {
	s.rolledBack = true
	s.rollbackCtxErr = ctx.Err()
	return s.rollbackErr
}

type stubBeginner struct {
	tx       *stubTx
	beginErr error
}

func (s *stubBeginner) Begin(context.Context) (pgx.Tx, error) {
	if s.beginErr != nil {
		return nil, s.beginErr
	}
	return s.tx, nil
}

func TestPreserveRollbackErrorReturnsNilWhenNothingFailed(t *testing.T) {
	tx := &stubTx{}
	if err := db.PreserveRollbackError(context.Background(), tx, nil, "scope"); err != nil {
		t.Errorf("returned %v, want nil", err)
	}
	if tx.rolledBack {
		t.Error("rolled back a transaction that had not failed")
	}
}

func TestPreserveRollbackErrorReturnsTheOperationErrorOnCleanRollback(t *testing.T) {
	tx := &stubTx{}
	opErr := apperr.NewConflict("EMAIL_TAKEN", "Already registered.", nil)

	err := db.PreserveRollbackError(context.Background(), tx, opErr, "register")
	if !tx.rolledBack {
		t.Error("did not roll back after a failed operation")
	}
	if !errors.Is(err, opErr) {
		t.Errorf("returned %v, want the original operation error", err)
	}
	var rbErr *db.RollbackError
	if errors.As(err, &rbErr) {
		t.Error("a clean rollback was reported as a RollbackError")
	}
}

// The rollback failure outranks the operation error: the data is in an
// unknown state, which matters more than what went wrong first.
func TestFailedRollbackOutranksTheOperationError(t *testing.T) {
	tx := &stubTx{rollbackErr: errors.New("connection reset")}
	opErr := apperr.NewNotFound("PROFILE_NOT_FOUND", "Not found.", nil)

	err := db.PreserveRollbackError(context.Background(), tx, opErr, "load")

	var rbErr *db.RollbackError
	if !errors.As(err, &rbErr) {
		t.Fatalf("returned %T, want a *RollbackError", err)
	}
	if !rbErr.UnexpectedCleanupFailure() {
		t.Error("UnexpectedCleanupFailure() = false; respond's classifier keys off it")
	}
	if rbErr.Scope != "load" {
		t.Errorf("Scope = %q, want %q", rbErr.Scope, "load")
	}
	// Both halves stay reachable through the multi-error Unwrap.
	if !errors.Is(err, opErr) {
		t.Error("the operation error is no longer reachable through errors.Is")
	}
	if !errors.Is(err, rbErr.Cause) {
		t.Error("the rollback failure is not reachable through errors.Is")
	}
}

// A transaction already closed by a failed commit is not a cleanup failure.
func TestClosedTransactionIsNotACleanupFailure(t *testing.T) {
	tx := &stubTx{rollbackErr: pgx.ErrTxClosed}
	opErr := errors.New("commit failed")

	err := db.PreserveRollbackError(context.Background(), tx, opErr, "commit")

	var rbErr *db.RollbackError
	if errors.As(err, &rbErr) {
		t.Fatal("ErrTxClosed was reported as a cleanup failure")
	}
	if !errors.Is(err, opErr) {
		t.Errorf("returned %v, want the operation error", err)
	}
}

// The request context is usually already cancelled by the time cleanup runs —
// often that cancellation is why the operation failed. Rolling back on it
// would fail instantly and leak the transaction.
func TestRollbackRunsEvenWhenTheRequestContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tx := &stubTx{}
	opErr := errors.New("query cancelled")

	err := db.PreserveRollbackError(ctx, tx, opErr, "scope")

	if !tx.rolledBack {
		t.Fatal("no rollback was attempted on a cancelled context")
	}
	if tx.rollbackCtxErr != nil {
		t.Errorf("rollback ran on an already-dead context: %v", tx.rollbackCtxErr)
	}
	if !errors.Is(err, opErr) {
		t.Errorf("returned %v, want the operation error", err)
	}
}

// Values must survive so the cleanup's own logging still carries trace and
// request IDs.
func TestRollbackContextKeepsRequestValues(t *testing.T) {
	type ctxKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, "req-7"))
	cancel()

	var seen any
	tx := &stubTx{}
	db.PreserveRollbackError(ctx, valueCapturingRollbacker{tx, func(c context.Context) {
		seen = c.Value(ctxKey{})
	}}, errors.New("failed"), "scope")

	if seen != "req-7" {
		t.Errorf("rollback context value = %v, want it preserved", seen)
	}
}

type valueCapturingRollbacker struct {
	*stubTx
	capture func(context.Context)
}

func (v valueCapturingRollbacker) Rollback(ctx context.Context) error {
	v.capture(ctx)
	return v.stubTx.Rollback(ctx)
}

func TestInTxCommitsOnSuccess(t *testing.T) {
	tx := &stubTx{}
	b := &stubBeginner{tx: tx}

	err := db.InTx(context.Background(), b, "scope", func(pgx.Tx) error { return nil })
	if err != nil {
		t.Fatalf("InTx returned %v", err)
	}
	if !tx.committed {
		t.Error("did not commit")
	}
	if tx.rolledBack {
		t.Error("rolled back a successful transaction")
	}
}

func TestInTxRollsBackOnFailure(t *testing.T) {
	tx := &stubTx{}
	b := &stubBeginner{tx: tx}
	opErr := apperr.NewConflict("CONFLICT", "Conflict.", nil)

	err := db.InTx(context.Background(), b, "scope", func(pgx.Tx) error { return opErr })

	if tx.committed {
		t.Error("committed a failed transaction")
	}
	if !tx.rolledBack {
		t.Error("did not roll back")
	}
	if !errors.Is(err, opErr) {
		t.Errorf("returned %v, want the operation error unchanged", err)
	}
}

func TestInTxReportsBeginAndCommitFailuresAsExternal(t *testing.T) {
	t.Run("begin", func(t *testing.T) {
		b := &stubBeginner{beginErr: errors.New("pool exhausted")}
		err := db.InTx(context.Background(), b, "scope", func(pgx.Tx) error { return nil })
		if !apperr.IsKind(err, apperr.KindExternal) {
			t.Errorf("error = %v, want an external failure", err)
		}
	})

	t.Run("commit", func(t *testing.T) {
		tx := &stubTx{commitErr: errors.New("serialization failure")}
		b := &stubBeginner{tx: tx}
		err := db.InTx(context.Background(), b, "scope", func(pgx.Tx) error { return nil })
		if !apperr.IsKind(err, apperr.KindExternal) {
			t.Errorf("error = %v, want an external failure", err)
		}
		appErr, _ := apperr.From(err)
		if appErr.Code != "TRANSACTION_COMMIT_FAILED" {
			t.Errorf("code = %q, want TRANSACTION_COMMIT_FAILED", appErr.Code)
		}
	})
}

func TestRollbackErrorMessageNamesBothFailures(t *testing.T) {
	err := &db.RollbackError{
		Scope:     "register",
		Cause:     errors.New("connection reset"),
		Operation: errors.New("duplicate key"),
	}

	msg := err.Error()
	for _, want := range []string{"register", "connection reset", "duplicate key"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() = %q, want it to mention %q", msg, want)
		}
	}
}
