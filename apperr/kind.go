package apperr

// Kind classifies a failure semantically — what went wrong, not how any
// particular transport should report it. The HTTP status mapping lives in
// package respond, so the same taxonomy can be reused by a queue consumer or
// a CLI without either one depending on net/http.
//
// The established prior art here is gRPC's codes package, mirrored by
// Connect's connect.Code, and the error model in Google's AIP-193. This is a
// deliberate divergence rather than an unaware one: those taxonomies are
// shaped for RPC and collapse "malformed request" and "well-formed but
// semantically invalid" into a single InvalidArgument. Over HTTP that
// distinction is the difference between 400 and 422, and it is worth keeping
// for an HTTP-first API. Adopt gRPC's codes instead if a gRPC or Connect
// surface is ever added — the code-to-status mapping is well-trodden.
type Kind string

const (
	// KindValidation is a malformed request: bad syntax or wrong shape. The
	// client sent something the server could not parse or bind.
	KindValidation Kind = "validation"
	// KindUnprocessable is a well-formed request that is semantically
	// invalid — the shape parsed, the meaning did not hold.
	KindUnprocessable Kind = "unprocessable"
	// KindUnauthorized means the caller is not authenticated.
	KindUnauthorized Kind = "unauthorized"
	// KindForbidden means the caller is authenticated but not permitted.
	KindForbidden Kind = "forbidden"
	// KindNotFound means the addressed resource does not exist, or does not
	// exist for this caller. Prefer it over KindForbidden for resources owned
	// by someone else, so an opaque ID cannot be probed for existence.
	KindNotFound Kind = "not_found"
	// KindConflict means the request collided with existing state.
	KindConflict Kind = "conflict"
	// KindRateLimited means the caller exceeded a quota.
	KindRateLimited Kind = "rate_limited"
	// KindPayloadTooLarge means the request body exceeded a named limit.
	KindPayloadTooLarge Kind = "payload_too_large"
	// KindUnsupportedMedia means the request carried a Content-Type this
	// endpoint does not accept. Rejecting anything but JSON is also a CSRF
	// control: a cross-origin request cannot set application/json without
	// triggering a preflight the browser will refuse to skip.
	KindUnsupportedMedia Kind = "unsupported_media"
	// KindTimeout means the operation exceeded its deadline.
	KindTimeout Kind = "timeout"
	// KindExternal means a dependency (database, provider, queue) failed or
	// was unavailable. The bug, if any, is not here.
	KindExternal Kind = "external"
	// KindInternal means an unexpected failure in this service: a bug, or a
	// state that should not be reachable.
	KindInternal Kind = "internal"
)

// Severity drives log level at the boundary. It defaults from Kind and is
// overridden only when a specific failure deserves more or less attention
// than its Kind implies — an external dependency you can degrade past is not
// the same 3am problem as one you cannot.
type Severity string

const (
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

// DefaultSeverity returns the severity a Kind carries unless overridden.
// Expected client outcomes are low: they are the API working as designed.
func DefaultSeverity(k Kind) Severity {
	switch k {
	case KindForbidden:
		// Distinguished from the other 4xx kinds: a permission failure is
		// either a client bug or someone probing, and both are worth seeing.
		return SeverityMedium
	case KindTimeout, KindExternal:
		return SeverityHigh
	case KindInternal:
		return SeverityCritical
	default:
		return SeverityLow
	}
}
