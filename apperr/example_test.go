package apperr_test

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/Donk3ys/kit/apperr"
)

// Service code names the failure and returns it. It does not log, and it does
// not decide a status code — the HTTP boundary does both.
func ExampleNewNotFound() {
	err := apperr.NewNotFound("PROFILE_NOT_FOUND", "No profile for that year.",
		errors.New("no rows in result set"))

	fmt.Println(err.Kind)
	fmt.Println(err.Code)
	fmt.Println(err.SafeDetail)
	fmt.Println(err.LogSeverity())

	// Output:
	// not_found
	// PROFILE_NOT_FOUND
	// No profile for that year.
	// low
}

// The cause is preserved for errors.Is and for the log line, but is never
// serialised to a client.
func ExampleError_Unwrap() {
	sentinel := errors.New("no rows in result set")
	err := fmt.Errorf("loading profile: %w",
		apperr.NewNotFound("PROFILE_NOT_FOUND", "Not found.", sentinel))

	fmt.Println(errors.Is(err, sentinel))
	fmt.Println(apperr.IsKind(err, apperr.KindNotFound))

	// Output:
	// true
	// true
}

// Branch on classification rather than on error strings.
func ExampleIsKind() {
	err := findProfile()

	switch {
	case apperr.IsKind(err, apperr.KindNotFound):
		fmt.Println("render an empty state")
	case apperr.IsKind(err, apperr.KindForbidden):
		fmt.Println("prompt to upgrade")
	default:
		fmt.Println("let it propagate")
	}

	// Output: render an empty state
}

func findProfile() error {
	return apperr.NewNotFound("PROFILE_NOT_FOUND", "Not found.", nil)
}

// Builders return copies, so a package-level sentinel can be decorated at a
// call site without being altered for every other caller.
func ExampleError_WithAttrs() {
	var errTierTooLow = apperr.NewForbidden("INSUFFICIENT_TIER", "Not permitted.", nil)

	decorated := errTierTooLow.
		WithAttrs(slog.String("required_tier", "trusted")).
		WithExtensions(map[string]any{"requiredTier": "trusted"})

	fmt.Println(len(errTierTooLow.Attrs), errTierTooLow.Extensions == nil)
	fmt.Println(len(decorated.Attrs), decorated.Extensions["requiredTier"])

	// Output:
	// 0 true
	// 1 trusted
}

// Field-level failures reach the client as RFC 9457 invalid-params, so a form
// can mark individual controls instead of showing one flattened message.
func ExampleNewInvalidParams() {
	err := apperr.NewInvalidParams("VALIDATION_ERROR", "Some fields need attention.",
		apperr.InvalidParam{Name: "email", Reason: "must be a valid email address"},
		apperr.InvalidParam{Name: "employment[1].months", Reason: "must be at most 12"},
	)

	for _, p := range err.Extensions[apperr.ExtensionKeyInvalidParams].([]apperr.InvalidParam) {
		fmt.Printf("%s: %s\n", p.Name, p.Reason)
	}

	// Output:
	// email: must be a valid email address
	// employment[1].months: must be at most 12
}

// Severity defaults from Kind. Override it when a specific failure deserves
// more or less attention than its Kind implies.
func ExampleError_WithSeverity() {
	critical := apperr.NewExternal("DB_DOWN", "Try again shortly.", "postgres", nil)
	tolerable := apperr.NewExternal("EMAIL_SEND_FAILED", "Try again shortly.", "smtp", nil).
		WithSeverity(apperr.SeverityLow)

	fmt.Println(critical.LogSeverity())
	fmt.Println(tolerable.LogSeverity())

	// Output:
	// high
	// low
}
