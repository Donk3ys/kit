package obs

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/Donk3ys/kit/apperr"
)

// TraceConfig configures InitTracing.
type TraceConfig struct {
	// ServiceName is required. Everything a backend shows is grouped by it.
	ServiceName    string
	ServiceVersion string
	Environment    string

	// Endpoint is the OTLP gRPC collector address, "host:port".
	//
	// Empty disables tracing entirely: no provider is installed, so OTel's
	// no-op default stays in place and instrumented code keeps working with
	// zero cost. That is the point — a developer with no collector running
	// should not have to configure anything, or watch export failures scroll
	// past.
	Endpoint string
	// Insecure sends plaintext. Correct for a collector on a private network;
	// wrong for anything crossing one.
	Insecure bool

	// SampleRatio is the fraction of root traces recorded, from 0 to 1. Zero
	// means the default of 1 (record everything), which is right in
	// development and expensive in production.
	SampleRatio float64
}

// InitTracing installs a global tracer provider and the W3C trace-context
// propagator, returning a function that flushes and stops it.
//
// When Endpoint is empty it installs nothing and returns a no-op shutdown, so
// the caller's code path is identical either way.
func InitTracing(ctx context.Context, cfg TraceConfig) (ShutdownFunc, error) {
	if cfg.ServiceName == "" {
		return noopShutdown, apperr.NewInternal("TRACING_CONFIG_INVALID",
			"An unexpected error occurred.", "init_tracing",
			errUnnamedService)
	}
	if cfg.Endpoint == "" {
		return noopShutdown, nil
	}

	res, err := buildResource(cfg.ServiceName, cfg.ServiceVersion, cfg.Environment)
	if err != nil {
		return noopShutdown, apperr.NewInternal("TRACING_CONFIG_INVALID",
			"An unexpected error occurred.", "build_resource", err)
	}

	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return noopShutdown, apperr.NewExternal("TRACING_EXPORTER_FAILED",
			"An unexpected error occurred.", "otlp", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler(cfg.SampleRatio)),
	)

	otel.SetTracerProvider(provider)
	// Without a propagator, every service starts its own trace and a request
	// crossing a boundary appears as two unrelated traces. W3C tracecontext is
	// what otelhttp reads and writes; baggage carries app-level correlation.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return provider.Shutdown, nil
}

// Tracer returns a named tracer from the global provider. Before InitTracing
// runs — or when it is deliberately disabled — this is a no-op tracer, so
// package-level tracer variables and unconditional span creation are safe.
//
// The twin of Meter. Instrumentation calls this rather than otel.Tracer only
// so that a service imports one observability package, not two.
func Tracer(name string) trace.Tracer { return otel.Tracer(name) }

// sampler resolves the configured ratio into a sampler.
//
// ParentBased matters more than the ratio: it makes a child span follow the
// decision its caller already made, so a sampled trace stays whole instead of
// losing every second hop to an independent coin flip.
func sampler(ratio float64) sdktrace.Sampler {
	// Zero means "unset", which defaults to recording everything; a ratio at
	// or above 1 asks for the same thing. Both collapse to AlwaysSample rather
	// than TraceIDRatioBased(1), which would do the same work by arithmetic.
	if ratio <= 0 || ratio >= 1 {
		return sdktrace.ParentBased(sdktrace.AlwaysSample())
	}
	return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
}

var errUnnamedService = errors.New("service name is required")
