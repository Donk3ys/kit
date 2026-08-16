// Package obs bootstraps observability: a slog logger that correlates with
// traces, an OpenTelemetry tracer provider, and a metric provider that exports
// to Prometheus.
//
// It deliberately provides no HTTP middleware and no logger wrapper.
// otelhttp.NewHandler already emits request spans and metrics under OTel
// semantic conventions — which is what makes stock dashboards and vendor
// integrations work without bespoke queries — and slog has had InfoContext,
// WarnContext and ErrorContext since Go 1.21, so wrapping *slog.Logger to add
// them would be re-typing the standard library.
//
// What is left is genuinely missing upstream: the twenty lines of provider
// construction that every service writes identically, and a slog.Handler that
// puts the current trace and span IDs on every log line.
//
// A typical startup:
//
//	logger := obs.NewLogger(obs.LogConfig{Level: slog.LevelInfo, Format: "json"})
//
//	shutdownTraces, err := obs.InitTracing(ctx, obs.TraceConfig{
//	        ServiceName: "ftc-api", Endpoint: cfg.OTLPEndpoint, SampleRatio: 0.1,
//	})
//	registry, shutdownMetrics, err := obs.InitMetrics(obs.MetricConfig{ServiceName: "ftc-api"})
//	defer obs.CombineShutdown(shutdownTraces, shutdownMetrics)(ctx)
//
//	handler := otelhttp.NewHandler(router, "api")
//	mux.Handle("/metrics", obs.MetricsHandler(registry))
package obs

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// ShutdownFunc flushes and releases a provider. Call it before the process
// exits, or buffered spans and metrics from the final seconds — usually the
// interesting ones — are lost.
type ShutdownFunc func(context.Context) error

// CombineShutdown returns a ShutdownFunc that runs each of fns, in order,
// joining their errors. Every function runs even if an earlier one fails: a
// tracer that cannot flush must not prevent the meter from shutting down.
func CombineShutdown(fns ...ShutdownFunc) ShutdownFunc {
	return func(ctx context.Context) error {
		var errs []error
		for _, fn := range fns {
			if fn == nil {
				continue
			}
			if err := fn(ctx); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
}

// noopShutdown is returned when a provider was deliberately not installed, so
// callers never have to nil-check before deferring.
func noopShutdown(context.Context) error { return nil }

// buildResource describes this service to the backend. service.name is what
// every trace and metric is grouped by, so it is the one required field.
//
// resource.Default carries the same semconv schema URL as the version imported
// here (both v1.43.0 in the SDK we build against); a mismatch would make Merge
// fail, which is why the import is pinned rather than tracking latest blindly.
func buildResource(serviceName, version, environment string) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{semconv.ServiceName(serviceName)}
	if version != "" {
		attrs = append(attrs, semconv.ServiceVersion(version))
	}
	if environment != "" {
		// No generated helper for this one, unlike ServiceName/ServiceVersion,
		// so the Key is used directly.
		attrs = append(attrs, semconv.DeploymentEnvironmentNameKey.String(environment))
	}

	return resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(semconv.SchemaURL, attrs...),
	)
}
