# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this
repository. It is this repository's declaration under "The host repository contract"
(`~/.agents/epic/references/foundation.md`).

## What this is

`github.com/Donk3ys/kit` — shared Go plumbing for Postgres + chi HTTP services. It was **extracted
from two working codebases** (`freelance-tax-copilot`, and the archived `nodeb8`) rather than
designed up front, and every line in it ran in production or dogfooding before it landed here.

It is a **library, not a framework**. It never owns `main`, the router, or the dependency graph.
The app composes; kit hands it pieces. If a change here starts owning startup — an `App` type, a
`Run` function, a registry handlers plug into — that is the failure mode this repository exists to
avoid, not a refactor.

Its first consumer is the freelance-tax-copilot rebuild.

## Repository map

| Path | What it is |
| --- | --- |
| `apperr/` | Classified errors. Imports **nothing** beyond `errors`, `maps`, `log/slog` — no `net/http`, no router, no driver. |
| `respond/` | The HTTP boundary: `Kind`→status, RFC 9457 problem details, log-once, `classify`. |
| `httpmw/` | Only the middlewares that must know about the boundary: `Recoverer`, `AccessLog`, `SecurityHeaders`. |
| `httpin/` | Strict JSON decoding, validation, path params. |
| `db/` | pgx pool construction and transaction helpers. |
| `obs/` | slog + OpenTelemetry bootstrap. No middleware, no logger wrapper. |
| `examples/` | **A separate module.** A complete service wiring all six packages, plus `main_test.go`, which drives the whole stack end to end in memory and asserts the exact wire output — including that traces and metrics actually emit. |
| `*/example_test.go` | Go `Example` functions — compiled *and run* by `go test`, with `// Output:` assertions. |
| `README.md` | The composition root, the handler pattern, and a worked problem response. Read it first. |

There are no product docs beyond this file and `README.md`; the package doc comments are the
specification. Judge a change against those and against the reasoning recorded in comments — several
of them exist to stop a plausible-looking "simplification" from reintroducing a fixed bug.

## Verification commands

```bash
make check
```

Which is `gofmt` + `go build` + `go vet` + `go test`, across **both modules**. Use it rather than
running the commands by hand: `examples/` is a nested module, so a bare `go test ./...` from the
root silently skips it and reports success. `make walkthrough` prints the end-to-end example output.

`examples/` is a separate module on purpose — it depends on `otelhttp`, and kit must not, because
kit deliberately does not provide that middleware. Keeping the dependency there means kit's own
`require` block stays an honest statement of what the library needs. Note that `go build ./...`
cannot be used inside `examples/`; it would try to write a binary named `api` over the `api/`
directory, which is why the Makefile uses vet and test there.

**Usage examples are executable, and that is the point.** `examples/api` is compiled by
`go build ./...`, and every `Example*` function is run by `go test` with its `// Output:` block
asserted. A change to a signature or a response body breaks them, which is exactly what a README
snippet cannot do. When behaviour changes, update the examples in the same change rather than
letting them drift — and prefer adding an `Example` over adding a prose snippet to `README.md`.

Per-package examples stay **beside the code they document**. Do not move them into `examples/`:
`go vet` rejects an `Example` name that does not resolve to an identifier in the package under
test (`ExampleNoSuchSymbol refers to unknown identifier`), and pkgsite attaches each one to the
symbol it names. `examples/api` is where they are shown composed.

Coverage is high on purpose — this is the code every service depends on, so a bug here is a bug
everywhere. Do not let it fall without saying why. `db.NewPool` is the one known gap: it needs a
real PostgreSQL and belongs in an integration test. `db.PoolConfig` exists specifically so the
configuration translation is testable without one — keep that split.

## Base branch

`main`. There is no remote yet, and no released tag — cutting `v0.1.0` before the FTC rebuild
imports this module is outstanding, so the first consumer pins a version rather than tracking a
moving branch.

## Core architectural principles

- **Do not rebuild what the ecosystem ships.** This is the rule that shaped the current package
  list. Take from upstream:

  | Concern | Use |
  | --- | --- |
  | Request ID, timeout, body limit, compression, response recording | `chi/middleware` |
  | CORS | `go-chi/cors` |
  | HTTP tracing and metrics | `otelhttp.NewHandler` (OTel semantic conventions) |
  | Contextual logging | `*slog.Logger` — it has had `InfoContext` since Go 1.21 |

  Before adding anything to `httpmw` or `obs`, check whether chi or OTel already provides it. An
  earlier draft of this module rebuilt six chi middlewares, `WrapResponseWriter`, and both
  otelhttp's tracing and its metrics. `examples/api` demonstrates the upstream pieces in place, and
  `Example_metrics` asserts the `http.server.*` series actually arrive — an example that only
  *initialises* observability without emitting anything is worse than none, because it looks wired.

- **Errors are values.** Constructing one has no side effects — no span recording, no logging — so
  building an error you then wrap or discard cannot pollute a trace. Do not add a `ctx` parameter to
  a constructor.

- **Builders copy, never mutate.** `WithAttrs`/`WithExtensions`/`WithSeverity`/`WithCause` return
  copies so decorating a package-level sentinel at one call site cannot alter it for every other
  caller. Two tests pin this; they are not ceremony.

- **Exactly one place logs a failed request.** `respond.Boundary.Wrap` is that place. Nothing below
  the boundary logs an error it also returns. This is why the earlier `claimFinalErrorLog` atomic
  coordination could be deleted — keep it structural.

- **Transport lives in `respond`, not `apperr`.** `Kind` is semantic (`not_found`); `404` is one
  transport's opinion. Do not move `StatusFor` onto the Kind type, and do not let `apperr` grow a
  `net/http` import.

- **Classification fails closed.** An unrecognised error becomes a logged internal 500 with a
  generic detail. Never let a driver message reach a response body.

- **Cleanup runs on a live context.** `db.PreserveRollbackError` uses `context.WithoutCancel` — the
  request context is usually already cancelled by the time cleanup runs, often because that
  cancellation caused the failure. Rolling back on it leaks the transaction.

## Contract surface

These are read by web and Flutter clients. A change to any of them is a breaking API change, not a
refactor:

- `apperr.Error.Code` strings, wherever they are constructed.
- The problem-details member names: `type`, `title`, `status`, `detail`, `instance`, `code`.
- `invalid-params` with `name`/`reason`, which is the shape from RFC 9457 §3's own example —
  matched deliberately rather than invented. Two tests pin the serialised JSON byte-for-byte.
- `httpin`'s dotted field paths (`employment[1].months`), which clients map back to form controls.

## What must not enter kit

**Extraction is safe; addition is what needs discipline.** With one live consumer there is nothing
pushing back on a bad abstraction, so:

- Nothing enters that is not already working in a real service. New ideas live in the app's own
  `pkg/` first and are promoted only once a second consumer wants them.
- Deliberately excluded, and not oversights: app configuration, auth and session modelling
  (including the cookie helper — it has one consumer), rate limiting, caching, email, event bus,
  dependency injection.
- If a change adds an extension point, a plugin registry, or a config knob whose only caller is
  hypothetical, delete it instead.

## Conventions

- Prose in this file wraps at roughly 100 columns. Go code follows `gofmt`; comments wrap at 80.
- Comments say **why**, not what. A comment explaining a non-obvious choice is load-bearing — if a
  change makes one wrong, update it in the same change.
- Tests are named for the behaviour they protect, and regression tests say what regressed.
- Doc comments cite prior art where a decision diverges from an established convention (see
  `apperr.Kind` on gRPC codes / AIP-193). Record the reason, so the next reader knows it was a
  decision.
- Dependency versions are pinned for a reason in at least one place: `obs` imports semconv
  `v1.43.0` to match the SDK's own, because a mismatch makes `resource.Merge` fail at runtime.
  Check that pairing when bumping OTel.
