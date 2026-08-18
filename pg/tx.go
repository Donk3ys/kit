package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Donk3ys/kit/apperr"
	"github.com/jackc/pgx/v5"
)

// RollbackTimeout bounds a rollback attempted after the request context has
// already expired. Short, because at this point we are trying to release a
// connection, not to succeed at anything.
const RollbackTimeout = 2 * time.Second

// Beginner is anything that can start a transaction: *pgxpool.Pool, and also
// pgx.Tx itself, which opens a savepoint. Taking the interface means nested
// transactions work without a second helper, and that these functions can be
// tested without a database.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Rollbacker is the part of pgx.Tx that PreserveRollbackError needs.
type Rollbacker interface {
	Rollback(ctx context.Context) error
}

// RollbackError reports that an operation failed and its rollback then failed
// too. That second failure outranks the first: the data is in an unknown
// state, which matters more than whatever the original error was.
//
// UnexpectedCleanupFailure is the marker respond's classifier looks for, which
// is what forces this to a logged 500 no matter what it wraps.
type RollbackError struct {
	Scope string
	// Cause is the rollback failure.
	Cause error
	// Operation is the error that triggered the rollback, kept so it stays
	// reachable through errors.Is and errors.As.
	Operation error
}

func (e *RollbackError) Error() string {
	return fmt.Sprintf("%s: rollback failed after %v: %v", e.Scope, e.Operation, e.Cause)
}

// Unwrap returns both errors so errors.Is finds either the rollback failure or
// the operation that caused it.
func (e *RollbackError) Unwrap() []error { return []error{e.Cause, e.Operation} }

// UnexpectedCleanupFailure marks this as a cleanup failure for respond's
// classifier. Declared as a method rather than a sentinel so the coupling
// between the two packages stays structural, not by import.
func (e *RollbackError) UnexpectedCleanupFailure() bool { return true }

// PreserveRollbackError rolls back tx when operationErr is non-nil, returning
// operationErr on success and a *RollbackError when the rollback itself fails.
// It returns nil when operationErr is nil, having done nothing.
func PreserveRollbackError(
	ctx context.Context, tx Rollbacker, operationErr error, scope string,
) error {
	if operationErr == nil {
		return nil
	}

	// The request context is very often already cancelled by the time we get
	// here — that cancellation may be why the operation failed. Rolling back
	// on it would fail instantly and leak the transaction until the pool
	// reaped it. WithoutCancel keeps trace and request IDs for the log while
	// dropping the deadline that would defeat the cleanup.
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), RollbackTimeout)
	defer cancel()

	// A transaction already finished by a failed commit is not a cleanup
	// failure; there is nothing left to roll back.
	if err := tx.Rollback(rollbackCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return &RollbackError{Scope: scope, Cause: err, Operation: operationErr}
	}
	return operationErr
}

// errAbandonedTx is the operation error InTx hands PreserveRollbackError when
// it unwinds without committing or rolling back, which in practice means fn
// panicked. It never escapes InTx.
var errAbandonedTx = errors.New("transaction abandoned without commit or rollback")

// InTx runs fn inside a transaction, committing when it returns nil and
// rolling back otherwise. A panic in fn rolls back before the panic continues
// to unwind.
//
// fn must not commit or roll back the transaction itself, and must not retain
// the pgx.Tx after returning.
func InTx(ctx context.Context, b Beginner, scope string, fn func(pgx.Tx) error) error {
	tx, err := b.Begin(ctx)
	if err != nil {
		return apperr.NewExternal("DATABASE_UNAVAILABLE",
			"The service is temporarily unavailable.", "postgres", err)
	}

	// Installed before fn runs, because a panic unwinding past here would
	// otherwise reach neither the rollback below nor the commit. That leaks
	// more than memory: pgxpool hands a transaction's connection back to the
	// pool only from Commit or Rollback, and nothing reaps one that is still
	// checked out, so the connection and the locks the transaction holds are
	// gone for the life of the process. httpmw.Recoverer keeps that process
	// alive to do it again, so MaxConns panics stop being an error rate and
	// start being a permanent hang in Acquire.
	//
	// Unconditional, with no "already finished" flag, because pgx makes a
	// second Rollback free: both dbTx and the savepoint form return
	// ErrTxClosed from a local field check without a round trip, and
	// PreserveRollbackError already treats that as nothing left to roll back.
	// A flag on three return paths is the version that rots.
	//
	// The rollback error is discarded rather than logged: a panic outranks it,
	// nothing below respond.Boundary logs, and a panicking function has no
	// return value left to carry it.
	defer func() {
		_ = PreserveRollbackError(ctx, tx, errAbandonedTx, scope)
	}()

	if err := fn(tx); err != nil {
		return PreserveRollbackError(ctx, tx, err, scope)
	}

	if err := tx.Commit(ctx); err != nil {
		commitErr := apperr.NewExternal("TRANSACTION_COMMIT_FAILED",
			"The service is temporarily unavailable.", "postgres", err)
		return PreserveRollbackError(ctx, tx, commitErr, scope)
	}
	return nil
}
