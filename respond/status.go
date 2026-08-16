package respond

import (
	"net/http"

	"github.com/Donk3ys/kit/apperr"
)

// StatusFor maps a Kind to its HTTP status. This mapping deliberately lives
// here rather than on apperr.Kind: the taxonomy is transport-neutral, and
// this is one transport's opinion about it.
//
// An unrecognised Kind maps to 500 on purpose. That is almost certainly a bug
// in the caller, and 500 is the one status that guarantees the boundary logs
// it rather than passing it off as an expected client outcome.
func StatusFor(k apperr.Kind) int {
	switch k {
	case apperr.KindValidation:
		return http.StatusBadRequest
	case apperr.KindUnprocessable:
		return http.StatusUnprocessableEntity
	case apperr.KindUnauthorized:
		return http.StatusUnauthorized
	case apperr.KindForbidden:
		return http.StatusForbidden
	case apperr.KindNotFound:
		return http.StatusNotFound
	case apperr.KindConflict:
		return http.StatusConflict
	case apperr.KindRateLimited:
		return http.StatusTooManyRequests
	case apperr.KindPayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	case apperr.KindUnsupportedMedia:
		return http.StatusUnsupportedMediaType
	case apperr.KindTimeout:
		return http.StatusGatewayTimeout
	case apperr.KindExternal:
		return http.StatusServiceUnavailable
	case apperr.KindInternal:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

// statusTitle returns the human-readable title for a status, per RFC 9457's
// guidance that title is a short summary of the status, not of the instance.
func statusTitle(status int) string {
	if t := http.StatusText(status); t != "" {
		return t
	}
	return "Error"
}
