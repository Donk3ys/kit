package respond

import (
	"context"

	"github.com/Donk3ys/kit/apperr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// recordSpan annotates the active span with a failed request's classification.
//
// This belongs at the boundary rather than in apperr's constructors. Marking a
// span when the error is *built* is wrong twice over: an error that is then
// wrapped, retried past, or discarded would still have reddened a trace, and
// building one would require a context it has no other use for. Here the error
// has actually become a response, so the annotation is a fact.
//
// It is a no-op when nothing is recording, which is the case whenever tracing
// is disabled — so this costs nothing in development.
func recordSpan(ctx context.Context, e *apperr.Error, status int) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}

	// Attributes go on every failure, including expected ones: being able to
	// ask "which traces hit WIDGET_NOT_FOUND?" is useful even when nothing
	// went wrong.
	span.SetAttributes(
		attribute.String("app.error.code", e.Code),
		attribute.String("app.error.kind", string(e.Kind)),
	)

	// Only server-side faults mark the span itself as errored, following the
	// same rule OTel's HTTP semantic conventions use: 5xx is an error, 4xx is
	// not. A 404 is the API working as designed, and reddening those traces
	// makes an error-rate panel useless — real faults drown in expected ones.
	//
	// Note this threshold is status-based while the log threshold is
	// severity-based, so a Forbidden warns in the log but leaves its span
	// unset. That is deliberate: dashboards are built against the OTel
	// convention, and diverging from it to match our own would be the
	// surprising choice.
	if status < 500 {
		return
	}

	if e.Cause != nil {
		span.RecordError(e.Cause)
	}
	span.SetStatus(codes.Error, e.Code)
}
