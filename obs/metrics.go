package obs

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/Donk3ys/kit/apperr"
)

// MetricConfig configures InitMetrics.
type MetricConfig struct {
	// ServiceName is required.
	ServiceName    string
	ServiceVersion string
	Environment    string
	// Namespace prefixes every metric name, e.g. "ftc". Optional, and best
	// left empty unless one Prometheus scrapes several services whose metric
	// names would otherwise collide.
	Namespace string
}

// InitMetrics installs a global meter provider that exports to Prometheus,
// returning the registry to serve and a shutdown function.
//
// The registry is created here rather than using prometheus.DefaultRegisterer.
// The global registerer panics on duplicate registration, which is why code
// that uses it grows defensive register-or-reuse helpers and why the same
// binary cannot easily be exercised twice in one test process. An explicit
// registry has neither problem.
//
// Instrumentation goes through the OTel metric API — otelhttp's HTTP metrics
// arrive here automatically — so nothing needs to know it lands in Prometheus.
func InitMetrics(cfg MetricConfig) (*prometheus.Registry, ShutdownFunc, error) {
	if cfg.ServiceName == "" {
		return nil, noopShutdown, apperr.NewInternal("METRICS_CONFIG_INVALID",
			"An unexpected error occurred.", "init_metrics", errUnnamedService)
	}

	res, err := buildResource(cfg.ServiceName, cfg.ServiceVersion, cfg.Environment)
	if err != nil {
		return nil, noopShutdown, apperr.NewInternal("METRICS_CONFIG_INVALID",
			"An unexpected error occurred.", "build_resource", err)
	}

	registry := prometheus.NewRegistry()
	// Go runtime and process metrics: heap, goroutines, GC pauses, open file
	// descriptors. They cost nothing and are the first thing anyone asks for
	// when a service misbehaves without failing.
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	opts := []otelprom.Option{otelprom.WithRegisterer(registry)}
	if cfg.Namespace != "" {
		opts = append(opts, otelprom.WithNamespace(cfg.Namespace))
	}
	exporter, err := otelprom.New(opts...)
	if err != nil {
		return nil, noopShutdown, apperr.NewInternal("METRICS_EXPORTER_FAILED",
			"An unexpected error occurred.", "prometheus_exporter", err)
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(exporter),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(provider)

	return registry, provider.Shutdown, nil
}

// MetricsHandler serves a registry in the Prometheus exposition format.
//
// Mount it somewhere unauthenticated scrapers can reach but the public cannot:
// metric names and label values leak internal structure, and cardinality makes
// it a cheap thing to hammer.
func MetricsHandler(registry *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		// A failed scrape should be visible in the scrape itself, not only in
		// logs the person debugging the gap may not be reading.
		ErrorHandling: promhttp.HTTPErrorOnError,
	})
}

// Meter returns a named meter from the global provider. Before InitMetrics
// runs this is a no-op meter, so package-level instrument declarations are
// safe.
func Meter(name string) metric.Meter { return otel.Meter(name) }
