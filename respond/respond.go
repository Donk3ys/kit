// Package respond is the HTTP boundary for a kit-based service: it turns an
// error returned by a handler into a logged, classified RFC 9457 problem
// response, and writes ordinary JSON responses.
//
// The design point is that handlers return errors instead of writing them:
//
//	r.Get("/profiles/{id}", boundary.Wrap(func(w http.ResponseWriter, r *http.Request) error {
//	        p, err := svc.Profile(r.Context(), id)
//	        if err != nil {
//	                return err
//	        }
//	        return respond.JSON(w, http.StatusOK, p)
//	}))
//
// Because Wrap is the only place a failed request is written, "log exactly
// once per failure" is a property of the structure rather than a convention
// each handler has to remember. Nothing below this package should log an
// error it also returns.
//
// This package expects chi's RequestID middleware to be installed; without it
// the problem body simply omits its instance member.
package respond

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/Donk3ys/kit/apperr"
	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
)

// Boundary writes responses and logs failures. Construct one at startup and
// share it; its fields are read-only after construction.
type Boundary struct {
	// Logger receives one line per failed request, at a level derived from
	// the error's severity.
	Logger *slog.Logger
	// TypeBaseURI, when set, makes the problem "type" member a dereferenceable
	// documentation URI per code — https://api.example.com/problems, with a
	// PROFILE_NOT_FOUND error, yields …/problems/profile-not-found. Empty
	// leaves every type as "about:blank".
	TypeBaseURI string
}

// New returns a Boundary logging to logger, or to slog.Default if nil.
func New(logger *slog.Logger) *Boundary {
	if logger == nil {
		logger = slog.Default()
	}
	return &Boundary{Logger: logger}
}

// HandlerFunc is an http.HandlerFunc that may return an error instead of
// writing one.
type HandlerFunc func(http.ResponseWriter, *http.Request) error

// Wrap adapts a HandlerFunc into an http.HandlerFunc, routing any returned
// error through Error.
func (b *Boundary) Wrap(fn HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			b.Error(w, r, err)
		}
	}
}

// Error classifies err, logs it if it warrants a line, and writes a problem
// response. Prefer returning the error from a wrapped handler; call this
// directly only from code that cannot (middleware, for instance).
func (b *Boundary) Error(w http.ResponseWriter, r *http.Request, err error) {
	appErr := classify(err)
	status := StatusFor(appErr.Kind)
	requestID := chimw.GetReqID(r.Context())

	body, skipped := b.problemWithExtensions(appErr, status, requestID)
	b.log(r, appErr, status, requestID, skipped)
	// Same classification, three destinations: the response body, one log
	// line, and the active span. Doing all three here is what stops them
	// disagreeing about what happened.
	recordSpan(r.Context(), appErr, status)

	// A handler that wrote a response and then failed leaves us with headers
	// already on the wire. A second status line cannot be sent and a second
	// body would corrupt the first, so the log above is the whole remedy.
	if committed(w) {
		return
	}

	buf, marshalErr := json.Marshal(body)
	if marshalErr != nil {
		// An extension member carried something unserialisable. Send the
		// problem without extensions rather than failing to answer at all.
		b.Logger.LogAttrs(r.Context(), slog.LevelError, "problem extensions could not be serialised",
			slog.String("code", appErr.Code), slog.String("error", marshalErr.Error()))
		buf, marshalErr = json.Marshal(b.baseProblem(appErr, status, requestID))
		if marshalErr != nil {
			http.Error(w, statusTitle(status), status)
			return
		}
	}

	w.Header().Set("Content-Type", ProblemContentType)
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

// JSON writes v as an ordinary JSON response.
//
// It marshals into memory before writing anything, so an encoding failure
// leaves the response untouched and can still be reported as a clean 500 —
// streaming straight to the ResponseWriter would commit a partial body first.
// That makes it a poor fit for genuinely large payloads; stream those.
func JSON(w http.ResponseWriter, status int, v any) error {
	buf, err := json.Marshal(v)
	if err != nil {
		return apperr.NewInternal(
			"RESPONSE_ENCODE_FAILED", GenericInternalDetail, "encode_response", err)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(buf); err != nil {
		// Usually the client hung up mid-response. Worth seeing, not worth
		// paging anyone: the response is already committed either way.
		return apperr.NewInternal(
			"RESPONSE_WRITE_FAILED", GenericInternalDetail, "write_response", err).
			WithSeverity(apperr.SeverityMedium)
	}
	return nil
}

// NoContent writes a 204 with no body.
func NoContent(w http.ResponseWriter) error {
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// log emits at most one line per failed request. Expected client outcomes
// (severity low) are deliberately silent here: they are already visible in
// the access log, and logging them turns a working API into an alert storm.
func (b *Boundary) log(
	r *http.Request, e *apperr.Error, status int, requestID string, skipped []string,
) {
	severity := e.LogSeverity()
	level, ok := levelFor(severity)
	if !ok {
		return
	}

	attrs := make([]slog.Attr, 0, len(e.Attrs)+8)
	attrs = append(attrs,
		slog.String("error", e.Error()),
		slog.String("code", e.Code),
		slog.String("kind", string(e.Kind)),
		// Severity is an attribute rather than a custom slog level so that
		// critical failures stay queryable without every handler in every
		// consumer needing to know how to render a non-standard level.
		slog.String("severity", string(severity)),
		slog.Int("status", status),
		slog.String("method", r.Method),
		slog.String("route", RouteTemplate(r)),
	)
	if requestID != "" {
		attrs = append(attrs, slog.String("request_id", requestID))
	}
	if len(skipped) > 0 {
		attrs = append(attrs, slog.Any("skipped_extensions", skipped))
	}
	attrs = append(attrs, e.Attrs...)

	b.Logger.LogAttrs(r.Context(), level, "request failed", attrs...)
}

// levelFor maps severity to a log level, reporting false for severities that
// should not be logged at all.
func levelFor(s apperr.Severity) (slog.Level, bool) {
	switch s {
	case apperr.SeverityMedium:
		return slog.LevelWarn, true
	case apperr.SeverityHigh, apperr.SeverityCritical:
		return slog.LevelError, true
	default:
		return 0, false
	}
}

// responseState is the part of chi's middleware.WrapResponseWriter this
// package needs. Declaring it locally means any equivalent wrapper satisfies
// it, and an unwrapped ResponseWriter simply reports "not committed".
type responseState interface {
	Status() int
	BytesWritten() int
}

// committed reports whether a response has already been started. It can only
// answer truthfully when a recording ResponseWriter is installed; without one
// it assumes nothing has been written, which is the same assumption a bare
// http.ResponseWriter forces on everyone.
func committed(w http.ResponseWriter) bool {
	rs, ok := w.(responseState)
	if !ok {
		return false
	}
	return rs.Status() != 0 || rs.BytesWritten() > 0
}

// RouteTemplate returns the matched route pattern — "/profiles/{id}" rather
// than "/profiles/0f8c…". Logs and metrics must use bounded labels, or every
// distinct ID becomes its own time series. Falls back to the raw path when no
// pattern is available.
func RouteTemplate(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		if pattern := rctx.RoutePattern(); pattern != "" {
			return pattern
		}
	}
	if r.Pattern != "" { // net/http ServeMux, for services not using chi
		return r.Pattern
	}
	return r.URL.Path
}
