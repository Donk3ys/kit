// Package apperr defines the classified error type that service and domain
// code returns, and that a transport boundary translates into a response.
//
// It imports nothing outside the standard library, and nothing from it beyond
// errors, maps and log/slog. Describing a failure must not require depending
// on net/http, a router, or a database driver — only the code that writes
// responses needs those, and that lives in package respond.
//
// The intended flow is one-directional: domain code returns an *Error, every
// layer above passes it up unchanged, and exactly one boundary classifies,
// logs and writes it. Nothing below that boundary logs a returned error.
package apperr

import (
	"errors"
	"log/slog"
	"maps"
)

// ExtensionKeyInvalidParams is the problem-details extension member under
// which field-level validation failures are carried.
//
// The name and the element shape below are taken from the worked example in
// RFC 9457 §3 (inherited from RFC 7807). It is illustrative in the spec rather
// than an IANA-registered member, but it is the convention clients and
// tooling expect, so it is worth matching exactly instead of inventing one.
const ExtensionKeyInvalidParams = "invalid-params"

// Error is the canonical classified error.
//
// Cause is preserved for errors.Is/errors.As and for logging, but is never
// serialised to a client: only Code, SafeDetail and Extensions cross that
// line. Keeping the split explicit in the type is what stops an internal
// message leaking into a response by accident.
type Error struct {
	// Kind classifies the failure. It is the only field a caller must get
	// right; the boundary derives status and log level from it.
	Kind Kind
	// Code is a stable, machine-readable identifier such as
	// "PROFILE_NOT_FOUND". It is API contract surface: clients branch on it,
	// so treat a change to one as a breaking change.
	Code string
	// SafeDetail is human-readable text safe to return to the client.
	SafeDetail string
	// Cause is the underlying error, if any. Never serialised.
	Cause error
	// Severity overrides the level this error is logged at. Constructors set
	// it from Kind; the zero value means "derive from Kind", so a struct
	// literal that omits it still behaves.
	Severity Severity
	// Attrs are structured log attributes describing this failure. They reach
	// the log line, never the response body.
	Attrs []slog.Attr
	// Extensions are RFC 9457 extension members merged into the problem
	// response. Client-visible: never put an internal detail here.
	Extensions map[string]any
}

// InvalidParam is one field-level validation failure, in the shape RFC 9457's
// example defines. The json tags are API contract surface.
//
// Name identifies the offending input to the client. The spec does not fix a
// convention for nested bodies; the app decides between a bare field name, a
// dotted path, or an RFC 6901 JSON Pointer, and must apply it consistently —
// clients map this value back to a form control.
type InvalidParam struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Error implements the error interface. It joins SafeDetail and Cause so a
// log line carries both halves; the response body is built from the fields
// directly, never from this string.
func (e *Error) Error() string {
	switch {
	case e == nil:
		return "<nil>"
	case e.Cause == nil:
		return e.SafeDetail
	case e.SafeDetail == "":
		return e.Cause.Error()
	default:
		return e.SafeDetail + ": " + e.Cause.Error()
	}
}

// Unwrap exposes Cause so known domain outcomes stay identifiable through
// errors.Is and errors.As.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// LogSeverity returns the severity this error should be logged at, resolving
// the zero value against its Kind.
func (e *Error) LogSeverity() Severity {
	if e == nil {
		return SeverityLow
	}
	if e.Severity == "" {
		return DefaultSeverity(e.Kind)
	}
	return e.Severity
}

// The builders below copy rather than mutate. A package-level sentinel error
// decorated at one call site would otherwise be permanently altered for every
// other caller — a bug that only shows up under concurrency, and the reason
// these do not simply assign and return the receiver.

// WithAttrs returns a copy of e with additional structured log attributes.
func (e *Error) WithAttrs(attrs ...slog.Attr) *Error {
	if e == nil {
		return nil
	}
	c := *e
	c.Attrs = make([]slog.Attr, 0, len(e.Attrs)+len(attrs))
	c.Attrs = append(c.Attrs, e.Attrs...)
	c.Attrs = append(c.Attrs, attrs...)
	return &c
}

// WithExtensions returns a copy of e with the given RFC 9457 extension
// members merged in. Keys already present are overwritten.
func (e *Error) WithExtensions(ext map[string]any) *Error {
	if e == nil {
		return nil
	}
	c := *e
	c.Extensions = make(map[string]any, len(e.Extensions)+len(ext))
	maps.Copy(c.Extensions, e.Extensions)
	maps.Copy(c.Extensions, ext)
	return &c
}

// WithSeverity returns a copy of e logged at the given severity instead of
// its Kind's default.
func (e *Error) WithSeverity(s Severity) *Error {
	if e == nil {
		return nil
	}
	c := *e
	c.Severity = s
	return &c
}

// WithCause returns a copy of e wrapping the given cause. Useful when a
// package-level sentinel describes the outcome and the call site has the
// underlying failure.
func (e *Error) WithCause(cause error) *Error {
	if e == nil {
		return nil
	}
	c := *e
	c.Cause = cause
	return &c
}

// From extracts an *Error from anywhere in err's chain.
func From(err error) (*Error, bool) {
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr, true
	}
	return nil, false
}

// IsKind reports whether err's chain contains an *Error of the given Kind.
// Prefer it over comparing strings at call sites that branch on failure type.
func IsKind(err error, k Kind) bool {
	appErr, ok := From(err)
	return ok && appErr.Kind == k
}

func newError(kind Kind, code, safeDetail string, cause error) *Error {
	return &Error{
		Kind:       kind,
		Code:       code,
		SafeDetail: safeDetail,
		Cause:      cause,
		Severity:   DefaultSeverity(kind),
	}
}

// Constructors. Prefer these over struct literals so Kind, Code and Severity
// stay consistent with one another.

// NewValidation reports a malformed request — bad syntax or wrong shape.
func NewValidation(code, safeDetail string, cause error) *Error {
	return newError(KindValidation, code, safeDetail, cause)
}

// NewUnprocessable reports a well-formed but semantically invalid request.
func NewUnprocessable(code, safeDetail string, cause error) *Error {
	return newError(KindUnprocessable, code, safeDetail, cause)
}

// NewUnauthorized reports an unauthenticated caller.
func NewUnauthorized(code, safeDetail string, cause error) *Error {
	return newError(KindUnauthorized, code, safeDetail, cause)
}

// NewForbidden reports an authenticated but unpermitted caller.
func NewForbidden(code, safeDetail string, cause error) *Error {
	return newError(KindForbidden, code, safeDetail, cause)
}

// NewNotFound reports a missing resource.
func NewNotFound(code, safeDetail string, cause error) *Error {
	return newError(KindNotFound, code, safeDetail, cause)
}

// NewConflict reports a collision with existing state.
func NewConflict(code, safeDetail string, cause error) *Error {
	return newError(KindConflict, code, safeDetail, cause)
}

// NewRateLimited reports an exceeded quota.
func NewRateLimited(code, safeDetail string, cause error) *Error {
	return newError(KindRateLimited, code, safeDetail, cause)
}

// NewPayloadTooLarge reports a body over a named limit.
func NewPayloadTooLarge(code, safeDetail string, cause error) *Error {
	return newError(KindPayloadTooLarge, code, safeDetail, cause)
}

// NewUnsupportedMedia reports a Content-Type this endpoint does not accept.
func NewUnsupportedMedia(code, safeDetail string, cause error) *Error {
	return newError(KindUnsupportedMedia, code, safeDetail, cause)
}

// NewTimeout reports an operation that exceeded its deadline.
func NewTimeout(code, safeDetail string, cause error) *Error {
	return newError(KindTimeout, code, safeDetail, cause)
}

// NewExternal reports a failed or unavailable dependency. service names the
// dependency and is attached as a log attribute, not returned to the client.
func NewExternal(code, safeDetail, service string, cause error) *Error {
	return newError(KindExternal, code, safeDetail, cause).
		WithAttrs(slog.String("external_service", service))
}

// NewInternal reports an unexpected failure in this service. operation names
// what was being attempted and is attached as a log attribute, not returned
// to the client.
func NewInternal(code, safeDetail, operation string, cause error) *Error {
	return newError(KindInternal, code, safeDetail, cause).
		WithAttrs(slog.String("operation", operation))
}

// NewInvalidParams reports field-level validation failures. They are carried
// to the client under ExtensionKeyInvalidParams so a form can mark individual
// controls, rather than being flattened into one message.
func NewInvalidParams(code, safeDetail string, params ...InvalidParam) *Error {
	return newError(KindUnprocessable, code, safeDetail, nil).
		WithExtensions(map[string]any{ExtensionKeyInvalidParams: params})
}
