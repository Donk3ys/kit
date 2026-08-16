package respond

import (
	"context"
	"errors"

	"github.com/Donk3ys/kit/apperr"
	"github.com/jackc/pgx/v5/pgconn"
)

// GenericInternalDetail is the only thing a client is ever told about an
// unexpected failure. The cause reaches the log, never the response.
//
// Exported so that middleware constructing its own internal errors — the
// panic recoverer, for one — says the same thing this package would.
const GenericInternalDetail = "An unexpected error occurred."

// classify reduces any error to an *apperr.Error. An error that arrives here
// unclassified is treated as an internal failure — failing closed, so a
// forgotten classification shows up as a logged 500 rather than leaking a
// driver message to a client.
func classify(err error) *apperr.Error {
	// Checked first, and ahead of any *apperr.Error in the chain: a failed
	// transaction rollback must not be reported as the domain error that
	// triggered it. The data may be in an unknown state, and that is the more
	// serious fact.
	var cleanup interface{ UnexpectedCleanupFailure() bool }
	if errors.As(err, &cleanup) && cleanup.UnexpectedCleanupFailure() {
		return apperr.NewInternal(
			"TRANSACTION_CLEANUP_FAILED", GenericInternalDetail, "transaction_cleanup", err)
	}

	if appErr, ok := apperr.From(err); ok {
		if appErr.SafeDetail != "" {
			return appErr
		}
		// Never send an empty detail: fall back to the status text rather
		// than a blank field the client has to guess at.
		filled := *appErr
		filled.SafeDetail = statusTitle(StatusFor(appErr.Kind))
		return &filled
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return apperr.NewTimeout(
			"REQUEST_TIMEOUT", "The request exceeded its time limit.", err)
	}

	// A cancelled request context normally means the client hung up. Nobody
	// will read the response, so the status barely matters — but logging it
	// at critical would drown real failures in noise, hence the demotion.
	if errors.Is(err, context.Canceled) {
		return apperr.NewTimeout("REQUEST_CANCELED", "The request was canceled.", err).
			WithSeverity(apperr.SeverityLow)
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && isTimeoutSQLState(pgErr.Code) {
		return apperr.NewExternal(
			"DATABASE_TIMEOUT",
			"The database operation timed out; retry the request.",
			"postgres", err)
	}

	return apperr.NewInternal("INTERNAL_ERROR", GenericInternalDetail, "unclassified", err)
}

// isTimeoutSQLState reports whether a PostgreSQL SQLSTATE means "we ran out of
// time" rather than "your request was wrong". These surface as 503 with a
// retry hint, not 500: the request may well succeed next time.
//
// Deliberately not mapped: pgx.ErrNoRows. Whether a missing row is a 404 or a
// bug is a decision only the repository has the context to make, and guessing
// here would turn genuine internal failures into quiet 404s.
func isTimeoutSQLState(code string) bool {
	switch code {
	case "57014", // query_canceled, including statement_timeout
		"55P03", // lock_not_available, including lock_timeout
		"25P03", // idle_in_transaction_session_timeout
		"57P05": // idle_session_timeout
		return true
	default:
		return false
	}
}
