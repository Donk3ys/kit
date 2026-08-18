package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/Donk3ys/kit/obs"
)

// memStore is an in-memory widgetStore, so the whole stack can be exercised
// without a database. It is the only thing swapped out — the router, the
// middleware chain, the boundary and the service are the production ones.
type memStore struct {
	byName map[string]uuid.UUID
	byUUID map[uuid.UUID]Widget
	logger *slog.Logger
}

func newMemStore(logger *slog.Logger) *memStore {
	return &memStore{
		byName: map[string]uuid.UUID{},
		byUUID: map[uuid.UUID]Widget{},
		logger: logger,
	}
}

func (m *memStore) ByID(_ context.Context, id uuid.UUID) (Widget, error) {
	w, ok := m.byUUID[id]
	if !ok {
		return Widget{}, errWidgetNotFound
	}
	return w, nil
}

func (m *memStore) Create(ctx context.Context, w Widget) error {
	if _, taken := m.byName[w.Name]; taken {
		return errNameTaken
	}
	m.byName[w.Name] = w.ID
	m.byUUID[w.ID] = w
	m.logger.InfoContext(ctx, "widget persisted",
		slog.String("widget_id", w.ID.String()),
	)
	return nil
}

func (m *memStore) Delete(_ context.Context, id uuid.UUID) error {
	w, ok := m.byUUID[id]
	if !ok {
		return errWidgetNotFound
	}
	delete(m.byUUID, id)
	delete(m.byName, w.Name)
	return nil
}

// instanceRE redacts the request ID so example output is deterministic. The
// real value is a fresh ULID per request — that is the point of it.
var instanceRE = regexp.MustCompile(`"instance":"[^"]*"`)

// fixedID makes created widgets deterministic for the same reason.
const fixedID = "0f8c9a1e-2b3d-4c5e-8f90-1a2b3c4d5e6f"

func newTestRouter() http.Handler {
	h, _ := newTestRouterWithMetrics(nil)
	return h
}

// newTestRouterWithMetrics builds the production router over an in-memory
// store. Only storage is swapped — the middleware chain, boundary, handlers
// and instrumentation are the same ones main() wires up.
func newTestRouterWithMetrics(registry *prometheus.Registry) (http.Handler, *widgetService) {
	// Example output is asserted from stdout, so opt-in logs use stderr: this
	// keeps the wire walkthrough deterministic while making the production log
	// path observable with KIT_EXAMPLE_LOGS=1 make walkthrough.
	logOutput := io.Writer(io.Discard)
	if os.Getenv("KIT_EXAMPLE_LOGS") != "" {
		logOutput = os.Stderr
	}
	logger := obs.NewLogger(obs.LogConfig{
		Level:  slog.LevelInfo,
		Format: "text",
		Output: logOutput,
	}).With(
		slog.String("service.name", serviceName),
		slog.String("deployment.environment.name", "test"),
	)

	store := newMemStore(logger.With(slog.String("component", "widget-store")))
	svc, err := newWidgetService(store,
		logger.With(slog.String("component", "widget-service")))
	if err != nil {
		panic(err)
	}
	svc.nextID = func() uuid.UUID { return uuid.MustParse(fixedID) }

	return newRouter(logger, registry, svc), svc
}

// call performs a request and prints what a client would actually see.
func call(h http.Handler, method, path, body string) {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	// TrimRight because a 204 carries no Content-Type, and a dangling space
	// would make this example's output whitespace-sensitive.
	fmt.Println(strings.TrimRight(
		fmt.Sprintf("%s %s -> %d %s", method, path, w.Code, w.Header().Get("Content-Type")), " "))
	if w.Body.Len() > 0 {
		fmt.Println(" ", instanceRE.ReplaceAllString(strings.TrimSpace(w.Body.String()),
			`"instance":"<request-id>"`))
	}
}

// Example_endToEnd drives every kit package through one router: chi routing
// and request IDs, httpmw's access log, recoverer and security headers,
// httpin's decoding and validation, the service's classification, and
// respond's problem-details boundary.
//
// Read it top to bottom as the story of what a client sees.
func Example_endToEnd() {
	h := newTestRouter()

	// A path parameter that is not an identifier at all — a different mistake
	// from asking for one that does not exist, so 422 rather than 404.
	call(h, http.MethodGet, "/api/v1/widgets/banana", "")

	// Well-formed id, no such widget. The service maps its store's sentinel;
	// kit never guesses that a missing row means 404.
	call(h, http.MethodGet, "/api/v1/widgets/11111111-1111-1111-1111-111111111111", "")

	// Two field failures at once, named as the client sent them.
	call(h, http.MethodPost, "/api/v1/widgets", `{"name":"","quantity":-4}`)

	// Strict decoding: a misspelled field is rejected, not ignored.
	call(h, http.MethodPost, "/api/v1/widgets", `{"name":"bolt","quantity":1,"qty":2}`)

	// The happy path.
	call(h, http.MethodPost, "/api/v1/widgets", `{"name":"bolt","quantity":12}`)

	// The same name again: a conflict, carrying an extension member.
	call(h, http.MethodPost, "/api/v1/widgets", `{"name":"bolt","quantity":3}`)

	// Read it back, then remove it.
	call(h, http.MethodGet, "/api/v1/widgets/"+fixedID, "")
	call(h, http.MethodDelete, "/api/v1/widgets/"+fixedID, "")
	call(h, http.MethodGet, "/api/v1/widgets/"+fixedID, "")

	// Output:
	// GET /api/v1/widgets/banana -> 422 application/problem+json
	//   {"code":"VALIDATION_ERROR","detail":"A path parameter is not a valid identifier.","instance":"<request-id>","invalid-params":[{"name":"id","reason":"must be a valid UUID"}],"status":422,"title":"Unprocessable Entity","type":"https://api.example.com/problems/validation-error"}
	// GET /api/v1/widgets/11111111-1111-1111-1111-111111111111 -> 404 application/problem+json
	//   {"code":"WIDGET_NOT_FOUND","detail":"No widget with that id.","instance":"<request-id>","status":404,"title":"Not Found","type":"https://api.example.com/problems/widget-not-found"}
	// POST /api/v1/widgets -> 422 application/problem+json
	//   {"code":"VALIDATION_ERROR","detail":"Some fields need attention.","instance":"<request-id>","invalid-params":[{"name":"name","reason":"is required"},{"name":"quantity","reason":"must be 0 or greater"}],"status":422,"title":"Unprocessable Entity","type":"https://api.example.com/problems/validation-error"}
	// POST /api/v1/widgets -> 422 application/problem+json
	//   {"code":"VALIDATION_ERROR","detail":"The request contains an unknown field.","instance":"<request-id>","invalid-params":[{"name":"qty","reason":"is not a recognised field"}],"status":422,"title":"Unprocessable Entity","type":"https://api.example.com/problems/validation-error"}
	// POST /api/v1/widgets -> 201 application/json
	//   {"id":"0f8c9a1e-2b3d-4c5e-8f90-1a2b3c4d5e6f","name":"bolt","quantity":12}
	// POST /api/v1/widgets -> 409 application/problem+json
	//   {"code":"WIDGET_NAME_TAKEN","conflictingName":"bolt","detail":"A widget with that name already exists.","instance":"<request-id>","status":409,"title":"Conflict","type":"https://api.example.com/problems/widget-name-taken"}
	// GET /api/v1/widgets/0f8c9a1e-2b3d-4c5e-8f90-1a2b3c4d5e6f -> 200 application/json
	//   {"id":"0f8c9a1e-2b3d-4c5e-8f90-1a2b3c4d5e6f","name":"bolt","quantity":12}
	// DELETE /api/v1/widgets/0f8c9a1e-2b3d-4c5e-8f90-1a2b3c4d5e6f -> 204
	// GET /api/v1/widgets/0f8c9a1e-2b3d-4c5e-8f90-1a2b3c4d5e6f -> 404 application/problem+json
	//   {"code":"WIDGET_NOT_FOUND","detail":"No widget with that id.","instance":"<request-id>","status":404,"title":"Not Found","type":"https://api.example.com/problems/widget-not-found"}
}

// A panic produces the same problem+json shape a returned error does, rather
// than chi's plain-text 500 — and the panic value never reaches the client.
func Example_panicRecovery() {
	h := newTestRouter()
	call(h, http.MethodGet, "/api/v1/boom", "")

	// Output:
	// GET /api/v1/boom -> 500 application/problem+json
	//   {"code":"PANIC","detail":"An unexpected error occurred.","instance":"<request-id>","status":500,"title":"Internal Server Error","type":"https://api.example.com/problems/panic"}
}

// Security headers are applied to every response, including error responses.
func Example_securityHeaders() {
	h := newTestRouter()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	for _, name := range []string{
		"X-Content-Type-Options",
		"X-Frame-Options",
		"Referrer-Policy",
		"Content-Security-Policy",
	} {
		fmt.Printf("%s: %s\n", name, w.Header().Get(name))
	}

	// Output:
	// X-Content-Type-Options: nosniff
	// X-Frame-Options: DENY
	// Referrer-Policy: no-referrer
	// Content-Security-Policy: default-src 'none'; frame-ancestors 'none'; base-uri 'none'
}

// Example_metrics proves the observability wiring actually emits, rather than
// merely being initialised. otelhttp contributes the http.server.* series
// under OTel semantic conventions; the service contributes its own counters.
// Neither needs a collector — the Prometheus exporter is scraped in-process.
func Example_metrics() {
	registry, shutdown, err := obs.InitMetrics(obs.MetricConfig{ServiceName: "kit-example-api"})
	if err != nil {
		fmt.Println("init:", err)
		return
	}
	defer shutdown(context.Background())

	h, _ := newTestRouterWithMetrics(registry)

	// One success and one conflict, so both counters move.
	silent(h, http.MethodPost, "/api/v1/widgets", `{"name":"bolt","quantity":12}`)
	silent(h, http.MethodPost, "/api/v1/widgets", `{"name":"bolt","quantity":3}`)

	w := httptest.NewRecorder()
	obs.MetricsHandler(registry).ServeHTTP(w,
		httptest.NewRequest(http.MethodGet, "/metrics", nil))

	// Report presence rather than values: histogram buckets and runtime gauges
	// are not stable enough to assert on, but the series names are.
	for _, name := range []string{
		"http_server_request_duration_seconds", // otelhttp, via semconv
		"widgets_created_total",                // app instrumentation
		"widgets_rejected_total",
		"go_goroutines", // runtime collector registered by InitMetrics
	} {
		fmt.Printf("%s: %t\n", name, strings.Contains(w.Body.String(), name))
	}

	// Output:
	// http_server_request_duration_seconds: true
	// widgets_created_total: true
	// widgets_rejected_total: true
	// go_goroutines: true
}

// silent performs a request without printing it.
func silent(h http.Handler, method, path, body string) {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(httptest.NewRecorder(), r)
}

// Example_traces answers "what does the tracer do on success versus error?".
//
// It installs a recording provider, issues four requests, and prints the
// resulting spans with their status. Two rules to notice.
//
// A 4xx leaves its span Unset because the API worked as designed, while a 5xx
// is marked Error and carries an exception event. Reddening expected outcomes
// is how an error-rate panel becomes useless.
//
// And span names are route patterns, not paths: the read below is of a real
// widget id but the span is named "GET /api/v1/widgets/{id}". Naming it after
// the path would mint a new span name per widget, which is what makes a
// service's span-name list unusable. That case is here deliberately — an
// earlier version of this example exercised only fixed paths, where template
// and path are identical, and so kept asserting the right output while the
// router had in fact been naming spans after raw paths all along.
func Example_traces() {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(provider)
	defer func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(noop.NewTracerProvider())
	}()

	h, _ := newTestRouterWithMetrics(nil)

	silent(h, http.MethodPost, "/api/v1/widgets", `{"name":"bolt","quantity":12}`) // 201
	silent(h, http.MethodPost, "/api/v1/widgets", `{"name":"bolt","quantity":3}`)  // 409
	silent(h, http.MethodGet, "/api/v1/widgets/"+fixedID, "")                      // 200, parameterised
	silent(h, http.MethodGet, "/api/v1/boom", "")                                  // 500, panic

	for _, s := range exporter.GetSpans() {
		exception := ""
		for _, e := range s.Events {
			if e.Name == "exception" {
				exception = " +exception"
			}
		}
		code := ""
		for _, a := range s.Attributes {
			if a.Key == "app.error.code" {
				code = " " + a.Value.AsString()
			}
		}
		fmt.Printf("%-28s %-5s%s%s\n", s.Name, s.Status.Code, code, exception)
	}

	// Output:
	// widgetService.create         Unset
	// POST /api/v1/widgets         Unset
	// widgetService.create         Unset
	// POST /api/v1/widgets         Unset WIDGET_NAME_TAKEN
	// GET /api/v1/widgets/{id}     Unset
	// GET /api/v1/boom             Error PANIC +exception
}
