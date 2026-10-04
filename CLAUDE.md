# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this
repository. It is this repository's declaration under "The host repository contract"
(`~/.agents/epic/references/foundation.md`).

## What this is

`github.com/Donk3ys/kit` — shared Go plumbing for Postgres + chi HTTP services. It was **extracted
from two working codebases** rather than designed up front, and every line in it ran in production
or dogfooding before it landed here.

It is a **library, not a framework**. It never owns `main`, the router, or the dependency graph.
The app composes; kit hands it pieces. If a change here starts owning startup — an `App` type, a
`Run` function, a registry handlers plug into — that is the failure mode this repository exists to
avoid, not a refactor.

## Repository map

| Path | What it is |
| --- | --- |
| `apperr/` | Classified errors. Imports **nothing** beyond `errors`, `maps`, `log/slog` — no `net/http`, no router, no driver. |
| `respond/` | The HTTP boundary: `Kind`→status, RFC 9457 problem details, log-once, `classify`. |
| `httpmw/` | Only the middlewares that must know about the boundary: `Recoverer`, `AccessLog`, `SecurityHeaders`. |
| `httpin/` | Strict JSON decoding, validation, path params. |
| `pg/` | pgx pool construction and transaction helpers. |
| `obs/` | slog + OpenTelemetry bootstrap. No middleware, no logger wrapper. |
| `examples/` | **A separate module.** A complete service wiring all six packages, plus `main_test.go`, which drives the whole stack end to end in memory and asserts the exact wire output — including that traces and metrics actually emit. |
| `pgtest/` | **A separate module.** `pg` against a real PostgreSQL, started by testcontainers. Holds only what a server can prove. |
| `*/example_test.go` | Go `Example` functions — compiled *and run* by `go test`, with `// Output:` assertions. |
| `README.md` | The composition root, the handler pattern, and a worked problem response. Read it first. |

There are no product docs beyond this file and `README.md`; the package doc comments are the
specification. Judge a change against those and against the reasoning recorded in comments — several
of them exist to stop a plausible-looking "simplification" from reintroducing a fixed bug.

## Verification commands

```bash
make check
```

Which is `gofmt` + `go build` + `go vet` + `go test`, across **all three modules**. Use it rather
than running the commands by hand: `examples/` and `pgtest/` are nested modules, so a bare `go test
./...` from the root silently skips both and reports success. `make walkthrough` prints the
end-to-end example output.

`make check` **needs a running Docker daemon**, because `pgtest` starts a throwaway PostgreSQL.
`make check-short` is the opt-out and passes `-short`, which that module honours by skipping with a
message rather than silently covering less than it appears to. Prefer fixing Docker over reaching
for it.

`examples/infra/local` runs the example against real infrastructure — Postgres plus Grafana,
Tempo, Loki, Prometheus and Alloy — via `make docker-obs`, `make demo`, `make demo-requests`. It is
adapted from a working local stack rather than invented, and trimmed: no valkey, nats or smtp4dev,
because kit provides no cache, event bus or email and the stack should not imply otherwise. Two
things there are load-bearing and easy to break. `grafana-datasources.yaml` must keep the filename
the `grafana/otel-lgtm` image uses, or it lands beside the image's own file and collides on
datasource uids instead of replacing it. And the Makefile creates `KIT_LOG_DIR` before `docker
compose` runs, because Docker creates a missing bind-mount source itself, as root, after which the
demo cannot write its logs.

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

**The two kinds of example have different jobs, and neither replaces the other.**

- A **per-package `Example`** documents one symbol's semantics at its edges: what a builder
  returns, what an unclassified error becomes, what a config value translates to. It renders on
  that symbol on pkgsite and under `go doc`, which is where a reader who already knows what they
  want looks. Add one when the behaviour is a property of the value or the function rather than of
  a request travelling through a chain.
- **`examples/api`** documents composition: middleware ordering, the `Wrap` pattern, otelhttp
  outermost, and the wire bytes a client actually receives. `httpmw` and `obs` have no
  `example_test.go` at all, and that is the rule working rather than an omission — a middleware
  needs a chain and observability needs a live app, so both can only be demonstrated composed.

Two consequences to know before editing either. **`pg` is compiled but never executed by
`examples/api`** — `main_test.go` swaps in `memStore`, so `run()` and `pgStore` are typechecked and
nothing more, which makes `pg/example_test.go` the only *example* asserting that package's
behaviour; `pgtest/` asserts the rest against a real server.
And a per-package example that merely re-demonstrates something `Example_endToEnd` already asserts
byte-for-byte is duplication: delete it, leave a comment saying where the behaviour is now shown,
and keep a named unit test pinning it. Three were removed this way — extension members and
`TypeBaseURI` from `respond`, unknown-field rejection from `httpin`.

Coverage is high on purpose — this is the code every service depends on, so a bug here is a bug
everywhere. Do not let it fall without saying why. `pg.PoolConfig` exists specifically so the
configuration translation is testable without a server — keep that split.

`pg` is the one package whose number is split across modules: `go test ./pg/...` reports what can be
asserted without a server, and `make cover-pg` reports `pgtest`'s contribution. Merged they are
97.0%, with `pg.NewPool` at 91.7%. The remaining `NewPool` branch is `pgxpool.NewWithConfig`
returning an error, which is unreachable through `pg.Config` — puddle rejects only `MaxConns <= 0`,
and `PoolConfig` assigns `MaxConns` solely when it is positive, leaving `ParseConfig`'s default
otherwise. It is defensive code; leave it, and do not contort a test to reach it.

**What belongs in `pgtest` is what a server can prove and a stub cannot.** Not coverage for its own
sake — every test there fails if the behaviour it names is removed, which was checked by making the
edit and watching it fail, not assumed. Four of them exist to hold an assumption about pgx to
account: that a failed commit leaves a transaction pgx reports as `ErrTxClosed`, that `Beginner`
taking a `pgx.Tx` really yields savepoint semantics, that `PreserveRollbackError`'s
`context.WithoutCancel` is what lets a rollback run at all once the request is gone, and that
`Rollback` is the *only* thing that hands a panicking transaction's connection back — which is why
that one runs against a `MaxConns: 1` pool, where a leak is a hang rather than a statistic. Against
a stub those are restatements of the code; against PostgreSQL they are tests.

## Base branch

`main`, published at `github.com/Donk3ys/kit`. Consumers pin a tagged release rather than tracking
a moving branch, so a change a consumer needs ships as a new tag. `v0.1.0` is the first.

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

- **Storage packages are named for their engine, and never share an interface.** `pg` is called
  that because every exported symbol takes or returns a pgx type and its timeouts are PostgreSQL's
  own parameter names; it was renamed from `db`, which promised a generality it does not have. If a
  second database is ever needed it is a sibling package — `mongo`, say — with its own vocabulary.
  Do **not** introduce a `Store`, `DB` or `Repository` interface spanning them: Mongo has no
  savepoints, so `Beginner` and `InTx` cannot mean there what they mean here, and the shared
  interface would be the intersection of two engines, useful to neither. The same reasoning rules
  out a `Cache` interface over in-memory and Redis — a map lookup cannot fail and a network call
  can, and that difference is the one worth keeping.

- **Classification fails closed.** An unrecognised error becomes a logged internal 500 with a
  generic detail. Never let a driver message reach a response body.

- **Cleanup runs on a live context.** `pg.PreserveRollbackError` uses `context.WithoutCancel` — the
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
