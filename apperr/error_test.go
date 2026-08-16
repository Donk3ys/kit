package apperr_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/Donk3ys/kit/apperr"
)

func TestErrorString(t *testing.T) {
	cause := errors.New("pq: connection refused")

	tests := []struct {
		name string
		err  *apperr.Error
		want string
	}{
		{
			name: "detail and cause are joined",
			err:  apperr.NewNotFound("PROFILE_NOT_FOUND", "Profile not found.", cause),
			want: "Profile not found.: pq: connection refused",
		},
		{
			name: "detail alone when there is no cause",
			err:  apperr.NewNotFound("PROFILE_NOT_FOUND", "Profile not found.", nil),
			want: "Profile not found.",
		},
		{
			name: "cause alone when there is no detail",
			err:  apperr.NewNotFound("PROFILE_NOT_FOUND", "", cause),
			want: "pq: connection refused",
		},
		{
			name: "nil receiver does not panic",
			err:  nil,
			want: "<nil>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUnwrapSupportsErrorsIsThroughWrapping(t *testing.T) {
	sentinel := errors.New("no rows in result set")
	err := fmt.Errorf("loading profile: %w",
		apperr.NewNotFound("PROFILE_NOT_FOUND", "Profile not found.", sentinel))

	if !errors.Is(err, sentinel) {
		t.Error("errors.Is did not reach the cause through the *Error and an fmt wrap")
	}
}

func TestFromAndIsKindTraverseTheChain(t *testing.T) {
	err := fmt.Errorf("service: %w",
		fmt.Errorf("repository: %w",
			apperr.NewConflict("EMAIL_TAKEN", "That email is already registered.", nil)))

	got, ok := apperr.From(err)
	if !ok {
		t.Fatal("From did not find the *Error in a doubly wrapped chain")
	}
	if got.Code != "EMAIL_TAKEN" {
		t.Errorf("From returned Code %q, want %q", got.Code, "EMAIL_TAKEN")
	}

	if !apperr.IsKind(err, apperr.KindConflict) {
		t.Error("IsKind(KindConflict) = false, want true")
	}
	if apperr.IsKind(err, apperr.KindNotFound) {
		t.Error("IsKind(KindNotFound) = true, want false")
	}
	if apperr.IsKind(errors.New("plain"), apperr.KindInternal) {
		t.Error("IsKind matched an error with no *Error in its chain")
	}
}

func TestLogSeverity(t *testing.T) {
	tests := []struct {
		name string
		err  *apperr.Error
		want apperr.Severity
	}{
		{
			name: "derived from Kind by the constructor",
			err:  apperr.NewInternal("INTERNAL_ERROR", "An unexpected error occurred.", "calculate", nil),
			want: apperr.SeverityCritical,
		},
		{
			name: "expected client outcomes stay low",
			err:  apperr.NewNotFound("NOT_FOUND", "Not found.", nil),
			want: apperr.SeverityLow,
		},
		{
			name: "forbidden is worth seeing",
			err:  apperr.NewForbidden("FORBIDDEN", "Not permitted.", nil),
			want: apperr.SeverityMedium,
		},
		{
			name: "explicit override wins",
			err: apperr.NewExternal("EMAIL_SEND_FAILED", "Could not send email.", "smtp", nil).
				WithSeverity(apperr.SeverityLow),
			want: apperr.SeverityLow,
		},
		{
			name: "zero value on a struct literal derives from Kind",
			err:  &apperr.Error{Kind: apperr.KindTimeout, Code: "TIMEOUT"},
			want: apperr.SeverityHigh,
		},
		{
			name: "nil receiver does not panic",
			err:  nil,
			want: apperr.SeverityLow,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.LogSeverity(); got != tt.want {
				t.Errorf("LogSeverity() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The builders must copy. A package-level sentinel decorated at one call site
// would otherwise be permanently altered for every other caller.
func TestBuildersDoNotMutateTheReceiver(t *testing.T) {
	sentinel := apperr.NewForbidden("INSUFFICIENT_TIER", "Not permitted.", nil)

	decorated := sentinel.
		WithAttrs(slog.String("required_tier", "trusted")).
		WithExtensions(map[string]any{"requiredTier": "trusted"}).
		WithSeverity(apperr.SeverityHigh).
		WithCause(errors.New("tier check failed"))

	if len(sentinel.Attrs) != 0 {
		t.Errorf("WithAttrs mutated the receiver: Attrs = %v", sentinel.Attrs)
	}
	if sentinel.Extensions != nil {
		t.Errorf("WithExtensions mutated the receiver: Extensions = %v", sentinel.Extensions)
	}
	if sentinel.Severity != apperr.SeverityMedium {
		t.Errorf("WithSeverity mutated the receiver: Severity = %q", sentinel.Severity)
	}
	if sentinel.Cause != nil {
		t.Errorf("WithCause mutated the receiver: Cause = %v", sentinel.Cause)
	}

	if len(decorated.Attrs) != 1 {
		t.Errorf("decorated copy lost its attrs: %v", decorated.Attrs)
	}
	if decorated.Extensions["requiredTier"] != "trusted" {
		t.Errorf("decorated copy lost its extensions: %v", decorated.Extensions)
	}
	if decorated.Code != sentinel.Code {
		t.Errorf("decorated copy changed Code: %q", decorated.Code)
	}
}

// Two independent decorations of the same sentinel must not see each other.
func TestBuildersIsolateSiblingCopies(t *testing.T) {
	sentinel := apperr.NewValidation("BAD_REQUEST", "Malformed request.", nil)

	a := sentinel.WithAttrs(slog.String("field", "email"))
	b := sentinel.WithAttrs(slog.String("field", "amount"))

	if len(a.Attrs) != 1 || a.Attrs[0].Value.String() != "email" {
		t.Errorf("copy a was contaminated: %v", a.Attrs)
	}
	if len(b.Attrs) != 1 || b.Attrs[0].Value.String() != "amount" {
		t.Errorf("copy b was contaminated: %v", b.Attrs)
	}
}

func TestWithExtensionsMergesAndOverwrites(t *testing.T) {
	base := apperr.NewConflict("CONFLICT", "Conflict.", nil).
		WithExtensions(map[string]any{"a": 1, "b": 2})

	merged := base.WithExtensions(map[string]any{"b": 3, "c": 4})

	want := map[string]any{"a": 1, "b": 3, "c": 4}
	for k, v := range want {
		if merged.Extensions[k] != v {
			t.Errorf("merged[%q] = %v, want %v", k, merged.Extensions[k], v)
		}
	}
	if base.Extensions["b"] != 2 {
		t.Errorf("merge mutated the receiver: base[b] = %v, want 2", base.Extensions["b"])
	}
}

func TestNilReceiverBuildersReturnNil(t *testing.T) {
	var e *apperr.Error
	if e.WithAttrs(slog.String("k", "v")) != nil {
		t.Error("WithAttrs on nil returned non-nil")
	}
	if e.WithExtensions(map[string]any{"k": "v"}) != nil {
		t.Error("WithExtensions on nil returned non-nil")
	}
	if e.WithSeverity(apperr.SeverityHigh) != nil {
		t.Error("WithSeverity on nil returned non-nil")
	}
	if e.WithCause(errors.New("x")) != nil {
		t.Error("WithCause on nil returned non-nil")
	}
	if e.Unwrap() != nil {
		t.Error("Unwrap on nil returned non-nil")
	}
}

// service and operation are diagnostic context, not client-facing text.
func TestExternalAndInternalAttachContextAsAttributes(t *testing.T) {
	ext := apperr.NewExternal("DEPENDENCY_FAILED", "Try again shortly.", "postgres", nil)
	if len(ext.Attrs) != 1 || ext.Attrs[0].Key != "external_service" {
		t.Fatalf("NewExternal attrs = %v, want one external_service attr", ext.Attrs)
	}
	if ext.Attrs[0].Value.String() != "postgres" {
		t.Errorf("external_service = %q, want %q", ext.Attrs[0].Value.String(), "postgres")
	}
	if ext.SafeDetail != "Try again shortly." {
		t.Errorf("service name leaked into SafeDetail: %q", ext.SafeDetail)
	}

	in := apperr.NewInternal("INTERNAL_ERROR", "An unexpected error occurred.", "assemble", nil)
	if len(in.Attrs) != 1 || in.Attrs[0].Key != "operation" {
		t.Fatalf("NewInternal attrs = %v, want one operation attr", in.Attrs)
	}
	if in.Attrs[0].Value.String() != "assemble" {
		t.Errorf("operation = %q, want %q", in.Attrs[0].Value.String(), "assemble")
	}
}

func TestNewInvalidParams(t *testing.T) {
	err := apperr.NewInvalidParams("VALIDATION_ERROR", "Some fields need attention.",
		apperr.InvalidParam{Name: "email", Reason: "must be a valid address"},
		apperr.InvalidParam{Name: "amountCents", Reason: "must be positive"},
	)

	if err.Kind != apperr.KindUnprocessable {
		t.Errorf("Kind = %q, want %q", err.Kind, apperr.KindUnprocessable)
	}

	params, ok := err.Extensions[apperr.ExtensionKeyInvalidParams].([]apperr.InvalidParam)
	if !ok {
		t.Fatalf("extension %q = %T, want []InvalidParam",
			apperr.ExtensionKeyInvalidParams, err.Extensions[apperr.ExtensionKeyInvalidParams])
	}
	if len(params) != 2 || params[0].Name != "email" || params[1].Name != "amountCents" {
		t.Errorf("params = %+v, want email and amountCents", params)
	}
}

// The member name and element shape are contract surface that RFC 9457's
// example fixes for us — a rename here silently breaks every client.
func TestInvalidParamsSerialiseToTheRFCShape(t *testing.T) {
	err := apperr.NewInvalidParams("VALIDATION_ERROR", "Some fields need attention.",
		apperr.InvalidParam{Name: "age", Reason: "must be a positive integer"},
	)

	got, marshalErr := json.Marshal(err.Extensions)
	if marshalErr != nil {
		t.Fatalf("marshalling extensions: %v", marshalErr)
	}

	want := `{"invalid-params":[{"name":"age","reason":"must be a positive integer"}]}`
	if string(got) != want {
		t.Errorf("extensions JSON =\n  %s\nwant\n  %s", got, want)
	}
}

// Every constructor is a near-identical one-liner, which is precisely where a
// copy-paste slip survives review — NewTimeout passing KindExternal would be
// invisible in a diff and wrong in production.
func TestConstructorsSetTheirOwnKind(t *testing.T) {
	tests := []struct {
		name string
		got  *apperr.Error
		want apperr.Kind
	}{
		{"NewValidation", apperr.NewValidation("C", "d", nil), apperr.KindValidation},
		{"NewUnprocessable", apperr.NewUnprocessable("C", "d", nil), apperr.KindUnprocessable},
		{"NewUnauthorized", apperr.NewUnauthorized("C", "d", nil), apperr.KindUnauthorized},
		{"NewForbidden", apperr.NewForbidden("C", "d", nil), apperr.KindForbidden},
		{"NewNotFound", apperr.NewNotFound("C", "d", nil), apperr.KindNotFound},
		{"NewConflict", apperr.NewConflict("C", "d", nil), apperr.KindConflict},
		{"NewRateLimited", apperr.NewRateLimited("C", "d", nil), apperr.KindRateLimited},
		{"NewPayloadTooLarge", apperr.NewPayloadTooLarge("C", "d", nil), apperr.KindPayloadTooLarge},
		{"NewUnsupportedMedia", apperr.NewUnsupportedMedia("C", "d", nil), apperr.KindUnsupportedMedia},
		{"NewTimeout", apperr.NewTimeout("C", "d", nil), apperr.KindTimeout},
		{"NewExternal", apperr.NewExternal("C", "d", "svc", nil), apperr.KindExternal},
		{"NewInternal", apperr.NewInternal("C", "d", "op", nil), apperr.KindInternal},
		{"NewInvalidParams", apperr.NewInvalidParams("C", "d"), apperr.KindUnprocessable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got.Kind != tt.want {
				t.Errorf("Kind = %q, want %q", tt.got.Kind, tt.want)
			}
			if tt.got.Code != "C" {
				t.Errorf("Code = %q, want %q", tt.got.Code, "C")
			}
			if tt.got.LogSeverity() != apperr.DefaultSeverity(tt.want) {
				t.Errorf("LogSeverity() = %q, want %q",
					tt.got.LogSeverity(), apperr.DefaultSeverity(tt.want))
			}
		})
	}
}

func TestDefaultSeverity(t *testing.T) {
	tests := []struct {
		kind apperr.Kind
		want apperr.Severity
	}{
		{apperr.KindValidation, apperr.SeverityLow},
		{apperr.KindUnprocessable, apperr.SeverityLow},
		{apperr.KindUnauthorized, apperr.SeverityLow},
		{apperr.KindForbidden, apperr.SeverityMedium},
		{apperr.KindNotFound, apperr.SeverityLow},
		{apperr.KindConflict, apperr.SeverityLow},
		{apperr.KindRateLimited, apperr.SeverityLow},
		{apperr.KindPayloadTooLarge, apperr.SeverityLow},
		{apperr.KindUnsupportedMedia, apperr.SeverityLow},
		{apperr.KindTimeout, apperr.SeverityHigh},
		{apperr.KindExternal, apperr.SeverityHigh},
		{apperr.KindInternal, apperr.SeverityCritical},
		{apperr.Kind("unrecognised"), apperr.SeverityLow},
	}

	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			if got := apperr.DefaultSeverity(tt.kind); got != tt.want {
				t.Errorf("DefaultSeverity(%q) = %q, want %q", tt.kind, got, tt.want)
			}
		})
	}
}
