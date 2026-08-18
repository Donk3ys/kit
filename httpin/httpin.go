// Package httpin decodes and validates request input, turning every failure
// into a classified *apperr.Error so handlers can return it unchanged.
//
// Decoding is strict on purpose. A request that sends a field the server does
// not know about is rejected rather than silently ignored: a client that
// misspells "amountCents" should learn about it immediately, not discover
// months later that the value was never applied.
package httpin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"reflect"
	"strings"

	"github.com/Donk3ys/kit/apperr"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
)

// DefaultMaxBodyBytes is the per-route body limit applied when a caller passes
// zero. It is a backstop below chi's RequestSize middleware, not a substitute
// for it: this one only guards routes that decode a body.
const DefaultMaxBodyBytes = 1 << 20 // 1 MiB

var validate = newValidator()

// Validator exposes the shared validator so an app can register its own tag
// validations and struct-level rules at startup. It is not safe to register
// against it once requests are being served.
func Validator() *validator.Validate { return validate }

func newValidator() *validator.Validate {
	v := validator.New(validator.WithRequiredStructEnabled())
	// Report the JSON field name rather than the Go field name. The client
	// sent "amountCents" and the contract documents "amountCents"; telling it
	// about "AmountCents" leaks an implementation detail it cannot act on.
	v.RegisterTagNameFunc(func(fld reflect.StructField) string {
		name := strings.SplitN(fld.Tag.Get("json"), ",", 2)[0]
		if name == "" || name == "-" {
			return fld.Name
		}
		return name
	})
	return v
}

// DecodeAndValidate reads a JSON request body into T and validates it.
//
// It enforces a body-size limit, requires a JSON content type, rejects unknown
// fields, rejects trailing content after the object, and separates malformed
// syntax (400) from semantic validation failure (422, with per-field
// invalid-params). Pass zero for maxBytes to use DefaultMaxBodyBytes.
//
// w is required because http.MaxBytesReader needs it to stop reading from a
// client that keeps sending after the limit.
func DecodeAndValidate[T any](w http.ResponseWriter, r *http.Request, maxBytes int64) (T, error) {
	var zero, target T

	if err := requireJSONContentType(r); err != nil {
		return zero, err
	}

	if maxBytes <= 0 {
		maxBytes = DefaultMaxBodyBytes
	}

	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBytes))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&target); err != nil {
		return zero, decodeError(err, maxBytes)
	}

	// {"a":1}{"b":2} must not quietly pass with only the first object applied.
	//
	// This decodes a second value rather than asking dec.More(), which was the
	// earlier check and is the wrong tool: More is defined over the elements of
	// the array or object being parsed, so it answers false for a trailing "]"
	// or "}" exactly as it does for a clean end of input, and
	// `{"taxYear":2026}]` was accepted. It also reports a read error as false,
	// so maxBytes tripping while it scanned past the object read as end of
	// input as well.
	switch err := dec.Decode(new(json.RawMessage)); {
	case errors.Is(err, io.EOF):
		// Nothing follows the object, which is the only acceptable outcome.
	case err != nil:
		// Classified rather than lumped in below, so a body that overruns
		// maxBytes only after a complete object is still a 413.
		return zero, decodeError(err, maxBytes)
	default:
		return zero, apperr.NewValidation("MALFORMED_JSON",
			"The request body must contain exactly one JSON object.", nil)
	}

	if err := validate.Struct(target); err != nil {
		return zero, validationError(err)
	}
	return target, nil
}

// requireJSONContentType rejects anything but application/json. Beyond
// correctness this is a CSRF control: a cross-origin form post cannot set this
// content type without a preflight, so an endpoint that insists on it cannot
// be driven by a hidden form on someone else's page.
func requireJSONContentType(r *http.Request) error {
	raw := r.Header.Get("Content-Type")
	if raw == "" {
		return apperr.NewUnsupportedMedia("UNSUPPORTED_MEDIA_TYPE",
			"A Content-Type of application/json is required.", nil)
	}
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil || mediaType != "application/json" {
		return apperr.NewUnsupportedMedia("UNSUPPORTED_MEDIA_TYPE",
			"A Content-Type of application/json is required.", err).
			WithAttrs(slog.String("content_type", raw))
	}
	return nil
}

// decodeError classifies a json decode failure. The distinction that matters
// is malformed versus too large: one is the client's bug, the other is a limit
// it may not have known about, and they need different statuses.
func decodeError(err error, maxBytes int64) error {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		return apperr.NewPayloadTooLarge("PAYLOAD_TOO_LARGE",
			fmt.Sprintf("The request body must not exceed %d bytes.", maxBytes), err)
	}

	if errors.Is(err, io.EOF) {
		return apperr.NewValidation("EMPTY_BODY", "A JSON request body is required.", err)
	}

	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return apperr.NewValidation("MALFORMED_JSON",
			fmt.Sprintf("The request body is not valid JSON (at byte %d).", syntaxErr.Offset), err)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return apperr.NewValidation("MALFORMED_JSON",
			"The request body ended before the JSON object was complete.", err)
	}

	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return apperr.NewInvalidParams("VALIDATION_ERROR", "Some fields could not be read.",
			apperr.InvalidParam{
				Name:   typeErr.Field,
				Reason: fmt.Sprintf("must be of type %s", typeErr.Type.String()),
			})
	}

	// encoding/json reports this as a bare string with no typed equivalent,
	// so matching the prefix is the only way to name the offending field.
	const unknownField = "json: unknown field "
	if msg := err.Error(); strings.HasPrefix(msg, unknownField) {
		name := strings.Trim(strings.TrimPrefix(msg, unknownField), `"`)
		return apperr.NewInvalidParams("VALIDATION_ERROR", "The request contains an unknown field.",
			apperr.InvalidParam{Name: name, Reason: "is not a recognised field"})
	}

	return apperr.NewValidation("MALFORMED_JSON", "The request body could not be read.", err)
}

// validationError converts validator's errors into invalid-params.
func validationError(err error) error {
	var fieldErrs validator.ValidationErrors
	if !errors.As(err, &fieldErrs) {
		// A misconfigured validator (a bad tag, a non-struct target) is our
		// bug, not the client's.
		return apperr.NewInternal("VALIDATION_FAILED",
			"An unexpected error occurred.", "validate_request", err)
	}

	params := make([]apperr.InvalidParam, 0, len(fieldErrs))
	for _, fe := range fieldErrs {
		params = append(params, apperr.InvalidParam{Name: paramName(fe), Reason: reasonFor(fe)})
	}
	return apperr.NewInvalidParams("VALIDATION_ERROR", "Some fields need attention.", params...)
}

// paramName renders the dotted JSON path to a field: "employment[0].employer"
// for a nested slice element. This settles the convention apperr.InvalidParam
// leaves open — a dotted path rather than a bare name or a JSON Pointer,
// because it survives nesting and still reads as the client's own field names.
func paramName(fe validator.FieldError) string {
	ns := fe.Namespace()
	// Namespace is prefixed with the top-level struct name, which is a Go
	// type the client has never heard of.
	if i := strings.IndexByte(ns, '.'); i >= 0 {
		return ns[i+1:]
	}
	return fe.Field()
}

// reasonFor renders a human-readable reason for the common validator tags.
// The text reaches the client, so it names the rule rather than the tag.
func reasonFor(fe validator.FieldError) string {
	switch fe.Tag() {
	case "required":
		return "is required"
	case "email":
		return "must be a valid email address"
	case "uuid", "uuid4":
		return "must be a valid UUID"
	case "url":
		return "must be a valid URL"
	case "min":
		return fmt.Sprintf("must be at least %s", fe.Param())
	case "max":
		return fmt.Sprintf("must be at most %s", fe.Param())
	case "len":
		return fmt.Sprintf("must be exactly %s in length", fe.Param())
	case "gt":
		return fmt.Sprintf("must be greater than %s", fe.Param())
	case "gte":
		return fmt.Sprintf("must be %s or greater", fe.Param())
	case "lt":
		return fmt.Sprintf("must be less than %s", fe.Param())
	case "lte":
		return fmt.Sprintf("must be %s or less", fe.Param())
	case "oneof":
		return fmt.Sprintf("must be one of: %s", strings.ReplaceAll(fe.Param(), " ", ", "))
	case "eqfield":
		return fmt.Sprintf("must match %s", fe.Param())
	default:
		if fe.Param() != "" {
			return fmt.Sprintf("failed the %s rule (%s)", fe.Tag(), fe.Param())
		}
		return fmt.Sprintf("failed the %s rule", fe.Tag())
	}
}

// UUIDParam parses a URL path parameter as a UUID.
//
// A malformed ID is a 400 rather than a 404: the client sent something that is
// not an identifier at all, which is a different mistake from asking for one
// that does not exist.
func UUIDParam(value, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(value)
	if err != nil {
		return uuid.Nil, apperr.NewInvalidParams("VALIDATION_ERROR",
			"A path parameter is not a valid identifier.",
			apperr.InvalidParam{Name: name, Reason: "must be a valid UUID"})
	}
	// The nil UUID parses cleanly but is never a real identifier, and letting
	// it through means a zero value reaching a query as though it were real.
	if id == uuid.Nil {
		return uuid.Nil, apperr.NewInvalidParams("VALIDATION_ERROR",
			"A path parameter is not a valid identifier.",
			apperr.InvalidParam{Name: name, Reason: "must not be the nil UUID"})
	}
	return id, nil
}
