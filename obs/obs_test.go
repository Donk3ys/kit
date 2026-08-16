package obs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Donk3ys/kit/apperr"
	"github.com/Donk3ys/kit/obs"
	"go.opentelemetry.io/otel/trace"
)

// spanContext builds a valid, sampled span context without needing a provider.
func spanContext(t *testing.T) context.Context {
	t.Helper()
	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	if err != nil {
		t.Fatalf("parsing trace id: %v", err)
	}
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	if err != nil {
		t.Fatalf("parsing span id: %v", err)
	}
	return trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    traceID,
			SpanID:     spanID,
			TraceFlags: trace.FlagsSampled,
		}))
}

func decodeLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log line was not valid JSON: %v (%s)", err, buf.String())
	}
	return rec
}

func TestLoggerCorrelatesWithTheActiveSpan(t *testing.T) {
	var buf bytes.Buffer
	logger := obs.NewLogger(obs.LogConfig{Output: &buf})

	logger.InfoContext(spanContext(t), "profile loaded")

	rec := decodeLine(t, &buf)
	if rec["trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("trace_id = %v, want the active trace", rec["trace_id"])
	}
	if rec["span_id"] != "00f067aa0ba902b7" {
		t.Errorf("span_id = %v, want the active span", rec["span_id"])
	}
}

func TestLoggerOmitsCorrelationOutsideASpan(t *testing.T) {
	var buf bytes.Buffer
	logger := obs.NewLogger(obs.LogConfig{Output: &buf})

	logger.InfoContext(context.Background(), "startup")

	rec := decodeLine(t, &buf)
	if _, present := rec["trace_id"]; present {
		t.Errorf("trace_id was emitted without a span: %v", rec["trace_id"])
	}
}

// The regression this guards: if WithAttrs does not re-wrap, correlation is
// lost the first time anyone derives a logger — which every service does in
// its composition root, so the loss would be total rather than partial.
func TestCorrelationSurvivesDerivedLoggers(t *testing.T) {
	ctx := spanContext(t)

	t.Run("With", func(t *testing.T) {
		var buf bytes.Buffer
		logger := obs.NewLogger(obs.LogConfig{Output: &buf}).With("component", "profiles")

		logger.InfoContext(ctx, "loaded")

		rec := decodeLine(t, &buf)
		if rec["trace_id"] == nil {
			t.Error("trace_id was lost after With()")
		}
		if rec["component"] != "profiles" {
			t.Errorf("component = %v, want the derived attribute kept", rec["component"])
		}
	})

	t.Run("WithGroup", func(t *testing.T) {
		var buf bytes.Buffer
		logger := obs.NewLogger(obs.LogConfig{Output: &buf}).WithGroup("db")

		logger.InfoContext(ctx, "query")

		// Documented caveat: attributes added to the record land inside an
		// open group, so correlation nests rather than sitting at the top.
		rec := decodeLine(t, &buf)
		group, ok := rec["db"].(map[string]any)
		if !ok {
			t.Fatalf("expected a db group, got %v", rec)
		}
		if group["trace_id"] == nil {
			t.Error("trace_id was lost after WithGroup()")
		}
	})

	t.Run("chained", func(t *testing.T) {
		var buf bytes.Buffer
		logger := obs.NewLogger(obs.LogConfig{Output: &buf}).
			With("a", 1).With("b", 2)

		logger.InfoContext(ctx, "chained")

		if decodeLine(t, &buf)["trace_id"] == nil {
			t.Error("trace_id was lost after chained With() calls")
		}
	})
}

func TestNewLoggerFormatAndLevel(t *testing.T) {
	t.Run("defaults to JSON", func(t *testing.T) {
		var buf bytes.Buffer
		obs.NewLogger(obs.LogConfig{Output: &buf}).Info("hello")
		if !strings.HasPrefix(strings.TrimSpace(buf.String()), "{") {
			t.Errorf("output = %q, want JSON", buf.String())
		}
	})

	t.Run("an unrecognised format stays JSON rather than downgrading", func(t *testing.T) {
		var buf bytes.Buffer
		obs.NewLogger(obs.LogConfig{Output: &buf, Format: "jsn"}).Info("hello")
		if !strings.HasPrefix(strings.TrimSpace(buf.String()), "{") {
			t.Errorf("output = %q, want JSON for an unrecognised format", buf.String())
		}
	})

	t.Run("text when asked", func(t *testing.T) {
		var buf bytes.Buffer
		obs.NewLogger(obs.LogConfig{Output: &buf, Format: "TEXT"}).Info("hello")
		if !strings.Contains(buf.String(), "msg=hello") {
			t.Errorf("output = %q, want text format", buf.String())
		}
	})

	t.Run("level filters", func(t *testing.T) {
		var buf bytes.Buffer
		logger := obs.NewLogger(obs.LogConfig{Output: &buf, Level: slog.LevelWarn})
		logger.Info("suppressed")
		if buf.Len() != 0 {
			t.Errorf("Info was emitted at Warn level: %q", buf.String())
		}
		logger.Warn("kept")
		if buf.Len() == 0 {
			t.Error("Warn was suppressed at Warn level")
		}
	})
}

// Level is set above the record so the branch is exercised without spraying
// output into the test run.
func TestNewLoggerDefaultsToStdout(t *testing.T) {
	logger := obs.NewLogger(obs.LogConfig{Level: slog.LevelError})
	if logger == nil {
		t.Fatal("NewLogger returned nil")
	}
	logger.Info("suppressed, and not written to the test's stdout")
}

func TestWithTraceContextOnNil(t *testing.T) {
	if obs.WithTraceContext(nil) != nil {
		t.Error("WithTraceContext(nil) returned non-nil")
	}
}

// A developer with no collector running should not have to configure
// anything, nor watch export failures scroll past.
func TestInitTracingWithoutAnEndpointInstallsNothing(t *testing.T) {
	shutdown, err := obs.InitTracing(context.Background(),
		obs.TraceConfig{ServiceName: "kit-test"})
	if err != nil {
		t.Fatalf("InitTracing returned %v", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown was nil; callers must be able to defer it unconditionally")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("no-op shutdown returned %v", err)
	}
}

func TestInitTracingRequiresAServiceName(t *testing.T) {
	shutdown, err := obs.InitTracing(context.Background(), obs.TraceConfig{Endpoint: "localhost:4317"})
	if !apperr.IsKind(err, apperr.KindInternal) {
		t.Errorf("error = %v, want an internal configuration error", err)
	}
	if shutdown == nil {
		t.Error("shutdown was nil on the error path; deferring it would panic")
	}
}

func TestInitTracingInstallsAProviderWhenConfigured(t *testing.T) {
	ctx := context.Background()

	shutdown, err := obs.InitTracing(ctx, obs.TraceConfig{
		ServiceName:    "kit-test",
		ServiceVersion: "1.2.3",
		Environment:    "test",
		Endpoint:       "localhost:4317",
		Insecure:       true,
		SampleRatio:    0.25,
	})
	if err != nil {
		t.Fatalf("InitTracing returned %v", err)
	}

	// The exporter connects lazily, so nothing here needs a live collector.
	shutdownCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := shutdown(shutdownCtx); err != nil {
		t.Errorf("shutdown returned %v", err)
	}
}

// A child span must follow the decision its caller already made, or a sampled
// trace loses every second hop to an independent coin flip.
func TestSamplerIsAlwaysParentBased(t *testing.T) {
	for _, ratio := range []float64{-1, 0, 0.5, 1, 2} {
		desc := obs.Sampler(ratio).Description()
		if !strings.HasPrefix(desc, "ParentBased") {
			t.Errorf("sampler(%v) = %q, want a ParentBased sampler", ratio, desc)
		}
	}
}

// Zero means "unset", and unset records everything — surprising enough to pin.
func TestZeroAndFullRatiosRecordEverything(t *testing.T) {
	always := obs.Sampler(1).Description()
	for _, ratio := range []float64{-1, 0, 1, 2} {
		if got := obs.Sampler(ratio).Description(); got != always {
			t.Errorf("sampler(%v) = %q, want %q", ratio, got, always)
		}
	}
	if partial := obs.Sampler(0.5).Description(); partial == always {
		t.Errorf("sampler(0.5) = %q, want a ratio-based sampler", partial)
	}
}

func TestInitMetricsRequiresAServiceName(t *testing.T) {
	_, shutdown, err := obs.InitMetrics(obs.MetricConfig{})
	if !apperr.IsKind(err, apperr.KindInternal) {
		t.Errorf("error = %v, want an internal configuration error", err)
	}
	if shutdown == nil {
		t.Error("shutdown was nil on the error path; deferring it would panic")
	}
}

func TestInitMetricsExportsInstrumentsAndRuntimeMetrics(t *testing.T) {
	ctx := context.Background()

	registry, shutdown, err := obs.InitMetrics(obs.MetricConfig{
		ServiceName:    "kit-test",
		ServiceVersion: "1.2.3",
		Environment:    "test",
	})
	if err != nil {
		t.Fatalf("InitMetrics returned %v", err)
	}
	defer func() {
		if err := shutdown(ctx); err != nil {
			t.Errorf("shutdown returned %v", err)
		}
	}()

	counter, err := obs.Meter("kit/obs/test").Int64Counter("widgets_made")
	if err != nil {
		t.Fatalf("creating counter: %v", err)
	}
	counter.Add(ctx, 3)

	w := httptest.NewRecorder()
	obs.MetricsHandler(registry).ServeHTTP(w,
		httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("scrape status = %d, want 200", w.Code)
	}
	body := w.Body.String()

	// An instrument recorded through the OTel API must reach Prometheus…
	if !strings.Contains(body, "widgets_made") {
		t.Error("the OTel instrument did not reach the Prometheus registry")
	}
	// …and the runtime collectors must be registered alongside it.
	if !strings.Contains(body, "go_goroutines") {
		t.Error("Go runtime metrics are missing from the registry")
	}
	if !strings.Contains(body, `service_name="kit-test"`) &&
		!strings.Contains(body, "kit-test") {
		t.Error("the resource attributes did not reach the exposition")
	}
}

func TestInitMetricsNamespacePrefixesInstruments(t *testing.T) {
	ctx := context.Background()

	registry, shutdown, err := obs.InitMetrics(obs.MetricConfig{
		ServiceName: "kit-test",
		Namespace:   "ftc",
	})
	if err != nil {
		t.Fatalf("InitMetrics returned %v", err)
	}
	defer shutdown(ctx)

	counter, err := obs.Meter("kit/obs/test").Int64Counter("gadgets_made")
	if err != nil {
		t.Fatalf("creating counter: %v", err)
	}
	counter.Add(ctx, 1)

	w := httptest.NewRecorder()
	obs.MetricsHandler(registry).ServeHTTP(w,
		httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if !strings.Contains(w.Body.String(), "ftc_gadgets_made") {
		t.Errorf("namespace was not applied; body did not contain ftc_gadgets_made")
	}
}

// Registries are explicit rather than prometheus.DefaultRegisterer precisely
// so a second call cannot panic on duplicate registration.
func TestInitMetricsCanBeCalledRepeatedly(t *testing.T) {
	ctx := context.Background()
	for range 3 {
		registry, shutdown, err := obs.InitMetrics(obs.MetricConfig{ServiceName: "kit-test"})
		if err != nil {
			t.Fatalf("InitMetrics returned %v", err)
		}
		if registry == nil {
			t.Fatal("registry was nil")
		}
		if err := shutdown(ctx); err != nil {
			t.Errorf("shutdown returned %v", err)
		}
	}
}

func TestCombineShutdown(t *testing.T) {
	ctx := context.Background()

	t.Run("runs every function in order", func(t *testing.T) {
		var order []string
		err := obs.CombineShutdown(
			func(context.Context) error { order = append(order, "a"); return nil },
			nil, // a provider that was never installed
			func(context.Context) error { order = append(order, "b"); return nil },
		)(ctx)

		if err != nil {
			t.Errorf("returned %v, want nil", err)
		}
		if strings.Join(order, ",") != "a,b" {
			t.Errorf("order = %v, want a,b", order)
		}
	})

	// A tracer that cannot flush must not stop the meter from shutting down.
	t.Run("a failure does not stop the rest", func(t *testing.T) {
		first := errors.New("tracer flush failed")
		second := errors.New("meter flush failed")
		ran := false

		err := obs.CombineShutdown(
			func(context.Context) error { return first },
			func(context.Context) error { ran = true; return second },
		)(ctx)

		if !ran {
			t.Error("a later shutdown was skipped after an earlier failure")
		}
		if !errors.Is(err, first) || !errors.Is(err, second) {
			t.Errorf("err = %v, want both failures joined", err)
		}
	})
}

// Tracer and Meter are safe before Init* runs: both fall back to the no-op
// global, so package-level instrumentation variables cannot panic at init.
func TestTracerAndMeterAreSafeBeforeInit(t *testing.T) {
	ctx, span := obs.Tracer("kit/obs/test").Start(context.Background(), "unit-of-work")
	span.End()
	if ctx == nil {
		t.Error("Tracer returned a nil context")
	}

	if _, err := obs.Meter("kit/obs/test").Int64Counter("safe_before_init"); err != nil {
		t.Errorf("Meter counter creation failed: %v", err)
	}
}
