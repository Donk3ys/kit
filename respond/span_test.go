package respond_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Donk3ys/kit/apperr"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// recordedSpan runs fn inside a real recording span and returns what was
// captured. A real SDK provider is used rather than a fake so the assertions
// are about actual OTel behaviour.
func recordedSpan(t *testing.T, fn func(ctx context.Context)) tracetest.SpanStub {
	t.Helper()

	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	ctx, span := provider.Tracer("test").Start(context.Background(), "request")
	fn(ctx)
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	return spans[0]
}

func errorOnSpan(t *testing.T, err error) tracetest.SpanStub {
	t.Helper()
	b, _ := newTestBoundary()
	return recordedSpan(t, func(ctx context.Context) {
		r := httptest.NewRequest(http.MethodGet, "/widgets/42", nil).WithContext(ctx)
		b.Error(httptest.NewRecorder(), r, err)
	})
}

// A server fault must be findable: without this, every trace looks identical
// whether the request succeeded or returned a 500.
func TestServerFaultsMarkTheSpanErrored(t *testing.T) {
	cause := errors.New("connection reset")
	span := errorOnSpan(t, apperr.NewExternal("DB_DOWN", "Try again shortly.", "postgres", cause))

	if span.Status.Code != codes.Error {
		t.Errorf("span status = %v, want Error", span.Status.Code)
	}
	if span.Status.Description != "DB_DOWN" {
		t.Errorf("status description = %q, want the error code", span.Status.Description)
	}

	var recorded bool
	for _, e := range span.Events {
		if e.Name == "exception" {
			recorded = true
		}
	}
	if !recorded {
		t.Error("the cause was not recorded as an exception event")
	}
}

// Expected client outcomes must not redden a trace. Marking every 404 as an
// error makes an error-rate panel useless: real faults drown in normal traffic.
func TestExpectedClientOutcomesLeaveTheSpanUnset(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"not found", apperr.NewNotFound("WIDGET_NOT_FOUND", "No widget.", nil)},
		{"validation", apperr.NewValidation("MALFORMED_JSON", "Bad body.", nil)},
		{"forbidden", apperr.NewForbidden("FORBIDDEN", "Not permitted.", nil)},
		{"conflict", apperr.NewConflict("TAKEN", "Already exists.", nil)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			span := errorOnSpan(t, tt.err)
			if span.Status.Code != codes.Unset {
				t.Errorf("span status = %v, want Unset for a %s", span.Status.Code, tt.name)
			}
			for _, e := range span.Events {
				if e.Name == "exception" {
					t.Error("an expected client outcome recorded an exception event")
				}
			}
		})
	}
}

// The code and kind go on every failure, so "which traces hit this code?" is
// answerable even for outcomes that are not faults.
func TestClassificationIsAlwaysAttachedToTheSpan(t *testing.T) {
	span := errorOnSpan(t, apperr.NewNotFound("WIDGET_NOT_FOUND", "No widget.", nil))

	attrs := map[string]string{}
	for _, a := range span.Attributes {
		attrs[string(a.Key)] = a.Value.AsString()
	}
	if attrs["app.error.code"] != "WIDGET_NOT_FOUND" {
		t.Errorf("app.error.code = %q", attrs["app.error.code"])
	}
	if attrs["app.error.kind"] != "not_found" {
		t.Errorf("app.error.kind = %q", attrs["app.error.kind"])
	}
}

// Tracing is off in development, and the boundary must not care.
func TestSpanRecordingIsANoOpWithoutATracer(t *testing.T) {
	b, _ := newTestBoundary()
	w := httptest.NewRecorder()

	b.Error(w, httptest.NewRequest(http.MethodGet, "/widgets/42", nil),
		apperr.NewInternal("BOOM", "An unexpected error occurred.", "op", errors.New("x")))

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 — the response must not depend on tracing", w.Code)
	}
}
