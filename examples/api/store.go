package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/Donk3ys/kit/obs"
	"github.com/Donk3ys/kit/pg"
)

// Storage failures the service classifies. These are plain sentinels, not
// apperr values, on purpose: a repository reports what happened, and the layer
// above decides what it means to a client. That is the same reason kit does
// not map pgx.ErrNoRows to a 404 for you — only this layer knows whether a
// missing row is an expected outcome or a bug.
var (
	errWidgetNotFound = errors.New("widget not found")
	errNameTaken      = errors.New("widget name taken")
)

// widgetStore is the seam that lets the service be exercised end to end
// without a database — see main_test.go.
type widgetStore interface {
	ByID(ctx context.Context, id uuid.UUID) (Widget, error)
	// Create inserts w, reporting errNameTaken if the name is in use. The
	// check and the insert must be atomic.
	Create(ctx context.Context, w Widget) error
	Delete(ctx context.Context, id uuid.UUID) error
}

// pgStore is the production implementation.
type pgStore struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// querySpan opens a span around one database call.
//
// Two ways to get query spans, and this is the manual one — useful when you
// want a span per *unit of work* with attributes you choose. For blanket
// coverage of every query, set pg.Config.QueryTracer to otelpgx.NewTracer()
// instead and delete code like this; do not write your own pgx.QueryTracer.
//
// The attribute names follow OTel database semantic conventions, which is what
// lets a backend recognise these as database spans without configuration.
func querySpan(ctx context.Context, op, table string) (context.Context, trace.Span) {
	ctx, span := obs.Tracer("kit/examples/api").Start(ctx, op+" "+table,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.DBSystemNamePostgreSQL,
			semconv.DBOperationName(op),
			semconv.DBCollectionName(table),
		),
	)
	return ctx, span
}

// endQuerySpan closes a query span, marking it errored only for genuine
// faults. "No rows" is an answer, not a failure — reddening those spans would
// make a database error-rate panel track normal traffic.
func endQuerySpan(span trace.Span, err error) {
	defer span.End()
	if err == nil || errors.Is(err, errWidgetNotFound) || errors.Is(err, errNameTaken) {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, "query failed")
}

func (s *pgStore) ByID(ctx context.Context, id uuid.UUID) (w Widget, err error) {
	ctx, span := querySpan(ctx, "SELECT", "widgets")
	defer func() { endQuerySpan(span, err) }()

	err = s.pool.QueryRow(ctx,
		`SELECT id, name, quantity FROM widgets WHERE id = $1`, id,
	).Scan(&w.ID, &w.Name, &w.Quantity)

	if errors.Is(err, pgx.ErrNoRows) {
		return Widget{}, errWidgetNotFound
	}
	return w, err
}

// Create shows pg.InTx: the uniqueness check and the insert belong in one
// transaction. Returning a non-nil error from the function rolls it back.
//
// The SELECT is not what makes the name unique, and it would be a bug to read
// it that way. Under READ COMMITTED two concurrent creates can both see "no
// such name" and both proceed; the UNIQUE constraint in schema.sql is what
// actually holds, and PostgreSQL says so explicitly — checking first does not
// eliminate a later unique violation at any isolation level. So the constraint
// violation is translated here too. Without that translation the loser of the
// race got a 503 from the storage branch in main.go rather than the documented
// 409, which is the same answer arriving under a different name.
//
// The SELECT stays because it is the cheap path and it produces errNameTaken
// without burning a transaction id on a doomed insert; the 23505 branch is the
// one that has to be right.
func (s *pgStore) Create(ctx context.Context, w Widget) (err error) {
	ctx, span := querySpan(ctx, "INSERT", "widgets")
	defer func() { endQuerySpan(span, err) }()

	err = pg.InTx(ctx, s.pool, "create_widget", func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM widgets WHERE name = $1)`, w.Name,
		).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return errNameTaken
		}

		_, err := tx.Exec(ctx,
			`INSERT INTO widgets (id, name, quantity) VALUES ($1, $2, $3)`,
			w.ID, w.Name, w.Quantity)
		if isUniqueViolation(err, "widgets_name_key") {
			return errNameTaken
		}
		return err
	})
	if err != nil {
		return err
	}

	// This is a component-specific success log. Failures are returned to the
	// service and ultimately logged once by the HTTP boundary.
	s.logger.InfoContext(ctx, "widget persisted",
		slog.String("widget_id", w.ID.String()),
	)
	return nil
}

// isUniqueViolation reports whether err is PostgreSQL's unique_violation for a
// named constraint. The name is checked rather than the SQLSTATE alone: a table
// with two unique constraints would otherwise report every one of them as the
// same conflict, and a client acting on that is being misled.
//
// This lives in the app, not in kit. respond cannot do it — 23505 means "some
// uniqueness was violated", and only the code that wrote the constraint knows
// which of its columns that is and whether the collision is expected control
// flow or a bug.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == pgerrcode.UniqueViolation &&
		pgErr.ConstraintName == constraint
}

func (s *pgStore) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM widgets WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errWidgetNotFound
	}
	return nil
}
