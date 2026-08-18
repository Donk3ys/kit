// Command api is a complete, runnable service wired with every kit package.
//
// Two things keep it honest. `go build ./...` compiles it, so the wiring here
// cannot drift from the library the way a README snippet can — and
// main_test.go drives the whole stack end to end in memory, so you can watch
// the packages work together without a database:
//
//	go test ./examples/api -run Example -v
//
// Running the real thing needs a PostgreSQL:
//
//	createdb kitdemo
//	psql kitdemo -c 'CREATE TABLE widgets (
//	    id UUID PRIMARY KEY, name TEXT NOT NULL UNIQUE, quantity INT NOT NULL)'
//	DATABASE_URL=postgres://localhost:5432/kitdemo go run ./examples/api
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
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

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
		// The only place this service writes to stderr: the process failed to
		// start, which is not ordinary operation.
		fmt.Fprintln(os.Stderr, "startup failed:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Configuration is the app's business, not kit's. These env lookups stand
	// in for whatever your config package does.
	var (
		addr         = envOr("ADDR", ":8080")
		databaseURL  = envOr("DATABASE_URL", "postgres://localhost:5432/kitdemo")
		otlpEndpoint = os.Getenv("OTLP_ENDPOINT") // empty disables tracing entirely
		environment  = envOr("ENVIRONMENT", "development")
	)

	logger := obs.NewLogger(obs.LogConfig{Level: slog.LevelInfo, Format: "json"}).With(
		slog.String("service.name", serviceName),
		slog.String("deployment.environment.name", environment),
	)

	shutdownTraces, err := obs.InitTracing(ctx, obs.TraceConfig{
		ServiceName: serviceName,
		Environment: environment,
		Endpoint:    otlpEndpoint,
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
		// WithoutCancel: ctx is already done by the time this runs, and a
		// cancelled context cannot flush anything.
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

	store := &pgStore{
		pool:   pool,
		logger: logger.With(slog.String("component", "widget-store")),
	}
	svc, err := newWidgetService(store,
		logger.With(slog.String("component", "widget-service")))
	if err != nil {
		return fmt.Errorf("instrumentation: %w", err)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           newRouter(logger, registry, svc),
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

// newRouter builds the whole HTTP surface. It takes its dependencies rather
// than reaching for globals, which is what lets main_test.go run the identical
// chain against an in-memory store.
func newRouter(logger *slog.Logger, registry *prometheus.Registry, svc *widgetService) http.Handler {
	boundary := respond.New(logger)
	boundary.TypeBaseURI = "https://api.example.com/problems"

	r := chi.NewRouter()
	// Order matters. RequestID first — both the access log and the problem
	// body's instance member read it. AccessLog outside Recoverer, so a
	// panicking request still gets an access line carrying the status the
	// recoverer settled on.
	r.Use(chimw.RequestID)
	r.Use(httpmw.AccessLog(logger, "/healthz", "/metrics"))
	r.Use(httpmw.Recoverer(boundary))
	r.Use(httpmw.SecurityHeaders(httpmw.SecurityHeadersConfig{}))
	r.Use(chimw.RequestSize(1 << 20))
	r.Use(chimw.Timeout(30 * time.Second))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	if registry != nil {
		r.Handle("/metrics", obs.MetricsHandler(registry))
	}
	r.Mount("/api/v1", widgetRoutes(boundary, svc))

	// This is why kit's httpmw has no tracing or metrics middleware:
	// otelhttp already emits a server span and the http.server.* metrics under
	// OTel semantic conventions, which is what makes stock dashboards work
	// without bespoke queries. It goes outermost so it observes the real
	// status, including one the recoverer produced.
	//
	// WithSpanNameFormatter uses the chi route pattern rather than the raw
	// path, so span names stay bounded instead of one per widget id.
	return otelhttp.NewHandler(r, "api",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + respond.RouteTemplate(r)
		}),
	)
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

	// Deliberately present: proves the recoverer turns a panic into the same
	// problem+json shape a returned error produces, rather than chi's
	// plain-text 500.
	r.Get("/boom", b.Wrap(func(http.ResponseWriter, *http.Request) error {
		panic("something unrecoverable")
	}))

	return r
}

// --- service ---------------------------------------------------------------

// widgetService turns storage outcomes into classified errors. It leaves
// failures for the boundary to log once, but may log app-specific successful
// outcomes such as a completed create.
type widgetService struct {
	store  widgetStore
	logger *slog.Logger
	nextID func() uuid.UUID // injectable so the end-to-end example is deterministic

	tracer   trace.Tracer
	created  metric.Int64Counter
	rejected metric.Int64Counter
}

// newWidgetService wires app-level instrumentation. Both accessors are safe to
// call before InitTracing/InitMetrics run — they fall back to no-op providers —
// so this works identically with observability switched off.
func newWidgetService(store widgetStore, logger *slog.Logger) (*widgetService, error) {
	meter := obs.Meter("kit/examples/api")

	created, err := meter.Int64Counter("widgets.created",
		metric.WithDescription("Widgets successfully created."))
	if err != nil {
		return nil, err
	}
	rejected, err := meter.Int64Counter("widgets.rejected",
		metric.WithDescription("Widget creations rejected, by reason."))
	if err != nil {
		return nil, err
	}

	return &widgetService{
		store:    store,
		logger:   logger,
		tracer:   obs.Tracer("kit/examples/api"),
		created:  created,
		rejected: rejected,
	}, nil
}

func (s *widgetService) newID() uuid.UUID {
	if s.nextID != nil {
		return s.nextID()
	}
	return uuid.New()
}

func (s *widgetService) byID(ctx context.Context, id uuid.UUID) (*Widget, error) {
	widget, err := s.store.ByID(ctx, id)
	switch {
	case errors.Is(err, errWidgetNotFound):
		return nil, apperr.NewNotFound("WIDGET_NOT_FOUND", "No widget with that id.", err)
	case err != nil:
		return nil, apperr.NewExternal("WIDGET_LOAD_FAILED", "Try again shortly.", "postgres", err)
	}
	return &widget, nil
}

func (s *widgetService) create(ctx context.Context, in CreateWidget) (*Widget, error) {
	// A child of otelhttp's server span, so a trace shows the request and the
	// work inside it. Ending it in a defer means it closes on every path.
	ctx, span := s.tracer.Start(ctx, "widgetService.create")
	defer span.End()
	span.SetAttributes(attribute.Int("widget.quantity", in.Quantity))

	widget := Widget{ID: s.newID(), Name: in.Name, Quantity: in.Quantity}

	err := s.store.Create(ctx, widget)
	switch {
	case errors.Is(err, errNameTaken):
		s.rejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "name_taken")))
		return nil, apperr.NewConflict("WIDGET_NAME_TAKEN",
			"A widget with that name already exists.", err).
			WithExtensions(map[string]any{"conflictingName": in.Name})
	case err != nil:
		s.rejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "storage")))
		return nil, apperr.NewExternal("WIDGET_INSERT_FAILED", "Try again shortly.", "postgres", err)
	}

	s.created.Add(ctx, 1)
	span.SetAttributes(attribute.String("widget.id", widget.ID.String()))
	s.logger.InfoContext(ctx, "widget created",
		slog.String("widget_id", widget.ID.String()),
		slog.Int("quantity", widget.Quantity),
	)
	return &widget, nil
}

func (s *widgetService) delete(ctx context.Context, id uuid.UUID) error {
	err := s.store.Delete(ctx, id)
	switch {
	case errors.Is(err, errWidgetNotFound):
		return apperr.NewNotFound("WIDGET_NOT_FOUND", "No widget with that id.", err)
	case err != nil:
		return apperr.NewExternal("WIDGET_DELETE_FAILED", "Try again shortly.", "postgres", err)
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
