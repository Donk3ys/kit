// Command api is a complete, runnable service wired with every kit package.
//
// It exists to be compiled: `go build ./...` covers it, so the wiring shown
// here cannot drift from the library the way a README snippet can. Running it
// needs a PostgreSQL; compiling it needs nothing.
//
//	createdb kitdemo
//	psql kitdemo -c 'CREATE TABLE widgets (
//	    id UUID PRIMARY KEY, name TEXT NOT NULL, quantity INT NOT NULL)'
//	DATABASE_URL=postgres://localhost:5432/kitdemo go run ./examples/api
//
// Then:
//
//	curl -s localhost:8080/api/v1/widgets/nope | jq
//	curl -s -X POST localhost:8080/api/v1/widgets \
//	     -H 'content-type: application/json' -d '{"name":"","quantity":-1}' | jq
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Donk3ys/kit/apperr"
	"github.com/Donk3ys/kit/db"
	"github.com/Donk3ys/kit/httpin"
	"github.com/Donk3ys/kit/httpmw"
	"github.com/Donk3ys/kit/obs"
	"github.com/Donk3ys/kit/respond"
)

const serviceName = "kit-example-api"

func main() {
	if err := run(); err != nil {
		// The only place in the service that writes to stderr: the process
		// failed to start, which is not ordinary operation.
		fmt.Fprintln(os.Stderr, "startup failed:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Configuration is the app's business, not kit's. Env lookups here stand
	// in for whatever your config package does.
	var (
		addr        = envOr("ADDR", ":8080")
		databaseURL = envOr("DATABASE_URL", "postgres://localhost:5432/kitdemo")
		otlpEndoint = os.Getenv("OTLP_ENDPOINT") // empty disables tracing entirely
		environment = envOr("ENVIRONMENT", "development")
	)

	logger := obs.NewLogger(obs.LogConfig{Level: slog.LevelInfo, Format: "json"})

	shutdownTraces, err := obs.InitTracing(ctx, obs.TraceConfig{
		ServiceName: serviceName,
		Environment: environment,
		Endpoint:    otlpEndoint,
		Insecure:    true,
		SampleRatio: 1,
	})
	if err != nil {
		return fmt.Errorf("tracing: %w", err)
	}

	registry, shutdownMetrics, err := obs.InitMetrics(obs.MetricConfig{
		ServiceName: serviceName,
		Environment: environment,
	})
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}

	shutdownObs := obs.CombineShutdown(shutdownTraces, shutdownMetrics)
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := shutdownObs(flushCtx); err != nil {
			logger.ErrorContext(flushCtx, "observability shutdown failed", slog.Any("error", err))
		}
	}()

	pool, err := db.NewPool(ctx, db.Config{
		DSN:                             databaseURL,
		ApplicationName:                 serviceName,
		MaxConns:                        25,
		StatementTimeout:                30 * time.Second,
		LockTimeout:                     5 * time.Second,
		IdleInTransactionSessionTimeout: 15 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer pool.Close()

	boundary := respond.New(logger)
	boundary.TypeBaseURI = "https://api.example.com/problems"

	svc := &widgetService{pool: pool}

	r := chi.NewRouter()
	// Order matters. RequestID must come first — both the access log and the
	// problem body's instance member read it. AccessLog sits outside Recoverer
	// so a panicking request still produces an access line, carrying the
	// status the recoverer settled on.
	r.Use(chimw.RequestID)
	r.Use(httpmw.AccessLog(logger, "/healthz", "/metrics"))
	r.Use(httpmw.Recoverer(boundary))
	r.Use(httpmw.SecurityHeaders(httpmw.SecurityHeadersConfig{}))
	r.Use(chimw.RequestSize(1 << 20))
	r.Use(chimw.Timeout(30 * time.Second))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	r.Handle("/metrics", obs.MetricsHandler(registry))
	r.Mount("/api/v1", widgetRoutes(boundary, svc))

	// In production, wrap the whole router: otelhttp.NewHandler(r, "api")
	// emits request spans and metrics under OTel semantic conventions. It is
	// omitted here only to keep this example free of extra dependencies.
	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	logger.InfoContext(ctx, "listening", slog.String("addr", addr))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// --- transport -------------------------------------------------------------

// CreateWidget is the request body. The json names are what clients send and
// what validation failures are reported against.
type CreateWidget struct {
	Name     string `json:"name"     validate:"required,max=60"`
	Quantity int    `json:"quantity" validate:"gte=0,lte=1000"`
}

// Widget is the response body.
type Widget struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Quantity int       `json:"quantity"`
}

func widgetRoutes(b *respond.Boundary, svc *widgetService) http.Handler {
	r := chi.NewRouter()

	// Handlers return errors rather than writing them. Wrap is the only place
	// a failed request is written, so "log exactly once per failure" is a
	// property of the structure, not a rule each handler must remember.
	r.Get("/widgets/{id}", b.Wrap(func(w http.ResponseWriter, r *http.Request) error {
		id, err := httpin.UUIDParam(chi.URLParam(r, "id"), "id")
		if err != nil {
			return err
		}
		widget, err := svc.byID(r.Context(), id)
		if err != nil {
			return err
		}
		return respond.JSON(w, http.StatusOK, widget)
	}))

	r.Post("/widgets", b.Wrap(func(w http.ResponseWriter, r *http.Request) error {
		in, err := httpin.DecodeAndValidate[CreateWidget](w, r, 0)
		if err != nil {
			return err
		}
		widget, err := svc.create(r.Context(), in)
		if err != nil {
			return err
		}
		return respond.JSON(w, http.StatusCreated, widget)
	}))

	r.Delete("/widgets/{id}", b.Wrap(func(w http.ResponseWriter, r *http.Request) error {
		id, err := httpin.UUIDParam(chi.URLParam(r, "id"), "id")
		if err != nil {
			return err
		}
		if err := svc.delete(r.Context(), id); err != nil {
			return err
		}
		return respond.NoContent(w)
	}))

	return r
}

// --- service ---------------------------------------------------------------

type widgetService struct{ pool *pgxpool.Pool }

// byID shows the shape every service method takes: name the failure, return
// it, log nothing. The boundary decides the status and writes the one log line.
func (s *widgetService) byID(ctx context.Context, id uuid.UUID) (*Widget, error) {
	var widget Widget
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, quantity FROM widgets WHERE id = $1`, id,
	).Scan(&widget.ID, &widget.Name, &widget.Quantity)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Mapped here, not in kit: whether a missing row is a 404 or a bug is
		// a decision only this layer has the context to make.
		return nil, apperr.NewNotFound("WIDGET_NOT_FOUND", "No widget with that id.", err)
	case err != nil:
		return nil, apperr.NewExternal("WIDGET_LOAD_FAILED",
			"Try again shortly.", "postgres", err)
	}
	return &widget, nil
}

// create shows a transaction. InTx commits when the function returns nil and
// rolls back otherwise; a rollback that itself fails outranks whatever error
// triggered it.
func (s *widgetService) create(ctx context.Context, in CreateWidget) (*Widget, error) {
	widget := Widget{ID: uuid.New(), Name: in.Name, Quantity: in.Quantity}

	err := db.InTx(ctx, s.pool, "create_widget", func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM widgets WHERE name = $1)`, in.Name,
		).Scan(&exists); err != nil {
			return apperr.NewExternal("WIDGET_CHECK_FAILED",
				"Try again shortly.", "postgres", err)
		}
		if exists {
			// Returning a non-nil error rolls the transaction back. The
			// classification survives to the boundary unchanged.
			return apperr.NewConflict("WIDGET_NAME_TAKEN",
				"A widget with that name already exists.", nil)
		}

		_, err := tx.Exec(ctx,
			`INSERT INTO widgets (id, name, quantity) VALUES ($1, $2, $3)`,
			widget.ID, widget.Name, widget.Quantity)
		if err != nil {
			return apperr.NewExternal("WIDGET_INSERT_FAILED",
				"Try again shortly.", "postgres", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &widget, nil
}

func (s *widgetService) delete(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM widgets WHERE id = $1`, id)
	if err != nil {
		return apperr.NewExternal("WIDGET_DELETE_FAILED",
			"Try again shortly.", "postgres", err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.NewNotFound("WIDGET_NOT_FOUND", "No widget with that id.", nil)
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
