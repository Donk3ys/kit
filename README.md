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
| `examples/api/main_test.go` | **The whole stack working together**, end to end, in memory. Drives the production router through nine requests and asserts the exact wire output of each. No database needed. |
| `apperr/example_test.go` | Constructing and classifying errors; copy-on-write builders. |
| `respond/example_test.go` | The handler pattern, and the exact problem body a client receives. |
| `httpin/example_test.go` | Decoding, per-field validation failures, content-type enforcement. |
| `db/example_test.go` | Pool construction, `InTx`, nested transactions, rollback semantics. |

```bash
go test ./examples/api -run Example -v   # watch the whole stack respond
go test ./... -run Example -v            # every example, output asserted
go doc ./respond                         # read them alongside the API
```

The per-package examples live beside the code they document, not in `examples/`, because Go
attaches `ExampleNewNotFound` to `apperr.NewNotFound` on pkgsite and in IDEs — and `go vet` fails
an `Example` name that does not resolve to a real identifier in that package. `examples/api` is
where they are shown composed.

To run the example service:

```bash
createdb kitdemo
psql kitdemo -c 'CREATE TABLE widgets (id UUID PRIMARY KEY, name TEXT NOT NULL, quantity INT NOT NULL)'
DATABASE_URL=postgres://localhost:5432/kitdemo go run ./examples/api
```

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
