# kit

Shared Go plumbing for Postgres + chi HTTP services. Extracted from two working
codebases rather than designed up front.

```
apperr/    classified errors — no net/http, no router, no driver
respond/   the HTTP boundary: RFC 9457 problem details, log-once, classify
httpmw/    the three middlewares that must know about the boundary
httpin/    strict JSON decoding and validation
db/        pgx pool construction and transaction helpers
obs/       slog + OpenTelemetry bootstrap
```

## What this is not

It is a library, not a framework. It never owns `main`, the router, or the
dependency graph — the app composes, kit provides pieces.

It also does not rebuild what the ecosystem already ships. Take these from
upstream:

| Concern | Use |
| --- | --- |
| Request ID, timeout, body limit, compression, response recording | `chi/middleware` |
| CORS | `go-chi/cors` |
| HTTP tracing and metrics | `otelhttp.NewHandler` (OTel semantic conventions) |
| Contextual logging | `*slog.Logger` — it has had `InfoContext` since Go 1.21 |

## Examples

Everything below is also available as compiled, executed code — so it cannot drift from the library
the way a README snippet can:

| Where | What |
| --- | --- |
| `examples/api/` | A complete service — composition root, middleware chain, handlers, a service layer returning `apperr`, and a transaction. Compiled by `go build ./...`. |
| `examples/api/main_test.go` | **The whole stack working together**, end to end, in memory. Drives the production router through nine requests and asserts the exact wire output of each, plus panic recovery, security headers, and that traces and metrics really emit. No database needed. |
| `apperr/example_test.go` | Constructing and classifying errors; copy-on-write builders. |
| `respond/example_test.go` | The handler pattern, and the exact problem body a client receives. |
| `httpin/example_test.go` | Decoding, per-field validation failures, content-type enforcement. |
| `db/example_test.go` | Pool construction, `InTx`, nested transactions, rollback semantics. |
| `examples/infra/local/` | Docker Compose: Postgres and a full LGTM stack, so the service can be run for real and its telemetry actually looked at. |

```bash
make walkthrough              # watch the whole stack respond
make check                    # both modules: fmt, build, vet, test
go doc ./respond              # read the examples alongside the API
```

`examples/` is a **separate module**, so a bare `go test ./...` from the root skips it — use
`make check`. It is separate because it depends on `otelhttp` and kit must not: kit deliberately
does not provide that middleware, and its `require` block should say only what the library needs.

**Start with `examples/api`.** It is the front door: one service wiring all six packages, and the
only place middleware ordering, the boundary and observability are shown working together.

The per-package examples live beside the code they document, not in `examples/`, because Go
attaches `ExampleNewNotFound` to `apperr.NewNotFound` on pkgsite and in IDEs — and `go vet` fails
an `Example` name that does not resolve to a real identifier in that package. They answer a
narrower question than `examples/api` does: what one symbol does at its edges — what a builder
returns, what an unclassified error becomes, what a config value translates to. Behaviour that only
shows up in a request travelling through a chain belongs in `examples/api` instead, which is why
`httpmw` and `obs` have no `example_test.go`: a middleware needs a chain and observability needs a
live app.

### Running it for real

`main_test.go` proves the wire output but swaps the database for an in-memory
store, so `db` is only ever typechecked there. `examples/infra/local` runs the
real thing — Postgres, plus Grafana with Tempo, Loki, Prometheus and Alloy:

```bash
make docker-obs     # database + the observability stack
make demo           # the service, on the host, wired to both
make demo-requests  # in a second terminal: drive it and print where to look
```

Then open Grafana at <http://localhost:3001>:

| Pillar | Where | What kit is doing |
| --- | --- | --- |
| Traces | Explore → Tempo | `otelhttp`'s server span, with `widgetService.create` and `INSERT widgets` nested under it |
| Logs | Explore → Loki, `{service_name="kit-example-api"}` | `obs.NewLogger`'s JSON; expand a line and click **TraceID** to jump to that request's trace |
| Metrics | Explore → Prometheus | `http_server_request_duration_seconds` from otelhttp, `widgets_created_total` from the service |

The log→trace jump is the one worth doing by hand: it works because
`obs.WithTraceContext` puts `trace_id` on every record logged with a context
inside a recording span, which is the entire reason that function exists.

Metrics are **scraped** while traces are **pushed** — `obs.InitMetrics` builds a
Prometheus exporter, `obs.InitTracing` an OTLP client. That is why the stack has
a Prometheus alongside the all-in-one image, and why the service runs on the
host but is still reachable at `host.docker.internal`.

Ports avoid freelance-tax-copilot's (Postgres on 5433, the API on 8081); see
`examples/infra/local/.env.example` to change them. `make docker-reset` drops
the volumes, which is required after editing `schema.sql` because Postgres runs
`initdb` scripts only against an empty data directory.

## Composition root

```go
func main() {
	ctx := context.Background()
	cfg := config.Load() // app-owned; kit takes no view on configuration

	logger := obs.NewLogger(obs.LogConfig{Level: cfg.LogLevel, Format: "json"})

	shutdownTraces, err := obs.InitTracing(ctx, obs.TraceConfig{
		ServiceName: "ftc-api",
		Environment: cfg.Environment,
		Endpoint:    cfg.OTLPEndpoint, // empty disables tracing entirely
		SampleRatio: cfg.TraceSampleRatio,
	})
	must(err)

	registry, shutdownMetrics, err := obs.InitMetrics(obs.MetricConfig{
		ServiceName: "ftc-api",
		Environment: cfg.Environment,
	})
	must(err)
	defer obs.CombineShutdown(shutdownTraces, shutdownMetrics)(ctx)

	pool, err := db.NewPool(ctx, db.Config{
		DSN:                             cfg.DatabaseURL,
		ApplicationName:                 "ftc-api",
		MaxConns:                        25,
		StatementTimeout:                30 * time.Second,
		LockTimeout:                     5 * time.Second,
		IdleInTransactionSessionTimeout: 15 * time.Second,
	})
	must(err)
	defer pool.Close()

	boundary := respond.New(logger)
	boundary.TypeBaseURI = "https://api.example.com/problems"

	r := chi.NewRouter()
	r.Use(chimw.RequestID) // must precede AccessLog and the boundary
	r.Use(chimw.RealIP)    // ONLY behind a proxy you control
	r.Use(httpmw.AccessLog(logger, "/healthz"))
	r.Use(httpmw.Recoverer(boundary))
	r.Use(httpmw.SecurityHeaders(httpmw.SecurityHeadersConfig{}))
	r.Use(chimw.RequestSize(1 << 20))
	r.Use(chimw.Timeout(30 * time.Second))

	r.Mount("/api/v1", routes(boundary, svc))
	r.Handle("/metrics", obs.MetricsHandler(registry))

	must(http.ListenAndServe(cfg.Addr, otelhttp.NewHandler(r, "api")))
}
```

`AccessLog` sits outside `Recoverer` so a panicking request still produces an
access line, carrying the status the recoverer settled on.

## Handlers return errors

```go
func routes(b *respond.Boundary, svc *profiles.Service) http.Handler {
	r := chi.NewRouter()

	r.Get("/profiles/{id}", b.Wrap(func(w http.ResponseWriter, r *http.Request) error {
		id, err := httpin.UUIDParam(chi.URLParam(r, "id"), "id")
		if err != nil {
			return err
		}
		p, err := svc.Profile(r.Context(), id)
		if err != nil {
			return err
		}
		return respond.JSON(w, http.StatusOK, p)
	}))

	r.Post("/profiles", b.Wrap(func(w http.ResponseWriter, r *http.Request) error {
		in, err := httpin.DecodeAndValidate[CreateProfile](w, r, 0)
		if err != nil {
			return err
		}
		p, err := svc.Create(r.Context(), in)
		if err != nil {
			return err
		}
		return respond.JSON(w, http.StatusCreated, p)
	}))

	return r
}
```

`Wrap` is the only place a failed request is written, which is what makes
"log exactly once per failure" structural rather than a convention every
handler has to remember. **Nothing below the boundary logs an error it also
returns.**

Service code names the failure and passes it up unchanged:

```go
func (s *Service) Profile(ctx context.Context, id uuid.UUID) (*Profile, error) {
	p, err := s.repo.ByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NewNotFound("PROFILE_NOT_FOUND", "No profile for that year.", err)
	}
	if err != nil {
		return nil, apperr.NewExternal("PROFILE_LOAD_FAILED",
			"Try again shortly.", "postgres", err)
	}
	return p, nil
}
```

## What a client sees

```http
HTTP/1.1 422 Unprocessable Entity
Content-Type: application/problem+json
```

```json
{
  "type": "https://api.example.com/problems/validation-error",
  "title": "Unprocessable Entity",
  "status": 422,
  "code": "VALIDATION_ERROR",
  "detail": "Some fields need attention.",
  "instance": "req-01HZY…",
  "invalid-params": [
    { "name": "email", "reason": "must be a valid email address" },
    { "name": "employment[1].months", "reason": "must be at most 12" }
  ]
}
```

`invalid-params` with `name`/`reason` is the shape from RFC 9457 §3's own
example. `instance` carries chi's request ID — the thing a user can quote back
to support. Internal causes never appear here; they go to the log line only.

## Conventions worth knowing

- **Errors are values.** Constructing one has no side effects, so building an
  error you then discard or wrap does not pollute a trace.
- **Builders copy.** `WithAttrs`, `WithExtensions`, `WithSeverity` and
  `WithCause` return copies, so decorating a package-level sentinel at one call
  site cannot alter it for every other caller.
- **Low-severity errors are not logged.** A 404 is the API working; it appears
  in the access log and nowhere else. Forbidden warns; external, timeout and
  internal errors log at Error with a `severity` attribute you can alert on.
- **Classification fails closed.** Anything unrecognised becomes a logged
  internal 500 with a generic detail, never a leaked driver message.
- **`Code` is contract surface.** Clients branch on it, so treat a change to
  one as a breaking change. Declare codes as constants in the app and check
  them against your OpenAPI document — kit cannot enforce that.

## Adding to kit

Extraction is safe; addition is what needs discipline. Nothing enters this
module that is not already working in a real service. New ideas live in the
app's own `pkg/` first and get promoted only once a second consumer wants them.

## Verification

```bash
go build ./... && go vet ./... && gofmt -l . && go test ./... -cover
```

`db.NewPool` is the one function without unit coverage — it needs a real
PostgreSQL, so it belongs in an integration test. `db.PoolConfig` exists
specifically so the configuration translation is testable without one.
