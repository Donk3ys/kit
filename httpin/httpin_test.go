package httpin_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Donk3ys/kit/apperr"
	"github.com/Donk3ys/kit/httpin"
	"github.com/google/uuid"
)

type employment struct {
	Employer string `json:"employer" validate:"required"`
	Months   int    `json:"months"   validate:"gte=1,lte=12"`
}

type profileRequest struct {
	Email       string       `json:"email"       validate:"required,email"`
	AmountCents int64        `json:"amountCents" validate:"gte=0"`
	TaxYear     int          `json:"taxYear"     validate:"required,oneof=2026 2027"`
	Employment  []employment `json:"employment"  validate:"dive"`
}

func postJSON(body string) (*httptest.ResponseRecorder, *http.Request) {
	r := httptest.NewRequest(http.MethodPost, "/profiles", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return httptest.NewRecorder(), r
}

func paramsFrom(t *testing.T, err error) []apperr.InvalidParam {
	t.Helper()
	appErr, ok := apperr.From(err)
	if !ok {
		t.Fatalf("error was not an *apperr.Error: %v", err)
	}
	params, ok := appErr.Extensions[apperr.ExtensionKeyInvalidParams].([]apperr.InvalidParam)
	if !ok {
		t.Fatalf("no invalid-params on the error: %+v", appErr.Extensions)
	}
	return params
}

func TestDecodeAndValidateAcceptsAValidBody(t *testing.T) {
	w, r := postJSON(`{"email":"a@b.com","amountCents":100,"taxYear":2026,"employment":[]}`)

	got, err := httpin.DecodeAndValidate[profileRequest](w, r, 0)
	if err != nil {
		t.Fatalf("DecodeAndValidate returned %v", err)
	}
	if got.Email != "a@b.com" || got.AmountCents != 100 || got.TaxYear != 2026 {
		t.Errorf("decoded = %+v, want the posted values", got)
	}
}

func TestContentTypeIsRequiredToBeJSON(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
	}{
		{"missing", ""},
		// The CSRF-relevant case: a cross-origin form post cannot set
		// application/json without a preflight, but it can set this.
		{"form encoded", "application/x-www-form-urlencoded"},
		{"multipart", "multipart/form-data; boundary=x"},
		{"text", "text/plain"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/profiles",
				strings.NewReader(`{"email":"a@b.com","taxYear":2026}`))
			if tt.contentType != "" {
				r.Header.Set("Content-Type", tt.contentType)
			}

			_, err := httpin.DecodeAndValidate[profileRequest](httptest.NewRecorder(), r, 0)
			if !apperr.IsKind(err, apperr.KindUnsupportedMedia) {
				t.Errorf("error = %v, want an unsupported-media error", err)
			}
		})
	}
}

func TestContentTypeParametersAreAccepted(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/profiles",
		strings.NewReader(`{"email":"a@b.com","amountCents":0,"taxYear":2026}`))
	r.Header.Set("Content-Type", "application/json; charset=utf-8")

	if _, err := httpin.DecodeAndValidate[profileRequest](httptest.NewRecorder(), r, 0); err != nil {
		t.Errorf("charset parameter was rejected: %v", err)
	}
}

// A misspelled field that is silently ignored is discovered months later, when
// the value turns out never to have been applied.
func TestUnknownFieldsAreRejectedAndNamed(t *testing.T) {
	w, r := postJSON(`{"email":"a@b.com","taxYear":2026,"amuntCents":100}`)

	_, err := httpin.DecodeAndValidate[profileRequest](w, r, 0)
	if !apperr.IsKind(err, apperr.KindUnprocessable) {
		t.Fatalf("error = %v, want unprocessable", err)
	}
	params := paramsFrom(t, err)
	if len(params) != 1 || params[0].Name != "amuntCents" {
		t.Errorf("params = %+v, want the unknown field named", params)
	}
}

func TestMalformedAndEmptyBodies(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantKind apperr.Kind
		wantCode string
	}{
		{"empty", "", apperr.KindValidation, "EMPTY_BODY"},
		{"broken syntax", `{"email":`, apperr.KindValidation, "MALFORMED_JSON"},
		{"not an object", `[1,2,3]`, apperr.KindUnprocessable, "VALIDATION_ERROR"},
		{"trailing object", `{"email":"a@b.com","taxYear":2026}{"x":1}`,
			apperr.KindValidation, "MALFORMED_JSON"},
		// Regression: the check used to be dec.More(), which reports false for
		// a trailing "]" or "}" because it is asking about elements of a
		// container, not about end of input. These three bodies were accepted.
		{"trailing bracket", `{"email":"a@b.com","taxYear":2026}]`,
			apperr.KindValidation, "MALFORMED_JSON"},
		{"trailing brace", `{"email":"a@b.com","taxYear":2026}}`,
			apperr.KindValidation, "MALFORMED_JSON"},
		{"trailing comma", `{"email":"a@b.com","taxYear":2026},`,
			apperr.KindValidation, "MALFORMED_JSON"},
		{"trailing garbage", `{"email":"a@b.com","taxYear":2026} nope`,
			apperr.KindValidation, "MALFORMED_JSON"},
		{"trailing scalar", `{"email":"a@b.com","taxYear":2026} null`,
			apperr.KindValidation, "MALFORMED_JSON"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, r := postJSON(tt.body)
			_, err := httpin.DecodeAndValidate[profileRequest](w, r, 0)

			appErr, ok := apperr.From(err)
			if !ok {
				t.Fatalf("error = %v, want an *apperr.Error", err)
			}
			if appErr.Kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", appErr.Kind, tt.wantKind)
			}
			if appErr.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", appErr.Code, tt.wantCode)
			}
		})
	}
}

func TestWrongFieldTypeNamesTheField(t *testing.T) {
	w, r := postJSON(`{"email":"a@b.com","amountCents":"lots","taxYear":2026}`)

	_, err := httpin.DecodeAndValidate[profileRequest](w, r, 0)
	params := paramsFrom(t, err)
	if len(params) != 1 || params[0].Name != "amountCents" {
		t.Fatalf("params = %+v, want amountCents named", params)
	}
	if !strings.Contains(params[0].Reason, "int64") {
		t.Errorf("reason = %q, want it to name the expected type", params[0].Reason)
	}
}

func TestBodySizeLimit(t *testing.T) {
	big := `{"email":"` + strings.Repeat("a", 500) + `@b.com","taxYear":2026}`
	w, r := postJSON(big)

	_, err := httpin.DecodeAndValidate[profileRequest](w, r, 64)
	if !apperr.IsKind(err, apperr.KindPayloadTooLarge) {
		t.Fatalf("error = %v, want payload-too-large", err)
	}
	appErr, _ := apperr.From(err)
	if !strings.Contains(appErr.SafeDetail, "64") {
		t.Errorf("detail = %q, want it to state the limit", appErr.SafeDetail)
	}
}

// Field names must be the ones the client sent, not the Go identifiers.
func TestValidationReportsJSONFieldNames(t *testing.T) {
	w, r := postJSON(`{"email":"not-an-email","amountCents":-1,"taxYear":1999}`)

	_, err := httpin.DecodeAndValidate[profileRequest](w, r, 0)
	if !apperr.IsKind(err, apperr.KindUnprocessable) {
		t.Fatalf("error = %v, want unprocessable", err)
	}

	got := map[string]string{}
	for _, p := range paramsFrom(t, err) {
		got[p.Name] = p.Reason
	}

	for name, wantReason := range map[string]string{
		"email":       "must be a valid email address",
		"amountCents": "must be 0 or greater",
		"taxYear":     "must be one of: 2026, 2027",
	} {
		if got[name] != wantReason {
			t.Errorf("param %q reason = %q, want %q", name, got[name], wantReason)
		}
	}
	if _, leaked := got["Email"]; leaked {
		t.Error("a Go field name reached the client")
	}
}

// The convention apperr.InvalidParam left open, settled here: a dotted path
// that survives nesting and still reads as the client's own field names.
func TestNestedFieldsUseADottedPath(t *testing.T) {
	w, r := postJSON(`{"email":"a@b.com","taxYear":2026,
		"employment":[{"employer":"Acme","months":6},{"employer":"","months":99}]}`)

	_, err := httpin.DecodeAndValidate[profileRequest](w, r, 0)
	if err == nil {
		t.Fatal("the invalid nested element was accepted")
	}

	names := map[string]bool{}
	for _, p := range paramsFrom(t, err) {
		names[p.Name] = true
	}
	for _, want := range []string{"employment[1].employer", "employment[1].months"} {
		if !names[want] {
			t.Errorf("missing param %q; got %v", want, names)
		}
	}
	if names["employment[0].employer"] {
		t.Error("the valid element was reported as invalid")
	}
}

func TestValidatorIsSharedAndExtensible(t *testing.T) {
	if httpin.Validator() == nil {
		t.Fatal("Validator() returned nil")
	}
	if httpin.Validator() != httpin.Validator() {
		t.Error("Validator() returned different instances; registrations would be lost")
	}
}

func TestUUIDParam(t *testing.T) {
	valid := uuid.New()

	tests := []struct {
		name    string
		value   string
		wantErr bool
		reason  string
	}{
		{name: "valid", value: valid.String()},
		{name: "not a uuid", value: "banana", wantErr: true, reason: "must be a valid UUID"},
		{name: "empty", value: "", wantErr: true, reason: "must be a valid UUID"},
		// Parses cleanly but is never a real identifier; letting it through
		// means a zero value reaching a query as though it were real.
		{
			name:    "nil uuid",
			value:   "00000000-0000-0000-0000-000000000000",
			wantErr: true,
			reason:  "must not be the nil UUID",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := httpin.UUIDParam(tt.value, "profileId")

			if !tt.wantErr {
				if err != nil {
					t.Fatalf("UUIDParam returned %v", err)
				}
				if got != valid {
					t.Errorf("got %v, want %v", got, valid)
				}
				return
			}

			if err == nil {
				t.Fatal("UUIDParam accepted an invalid value")
			}
			params := paramsFrom(t, err)
			if len(params) != 1 || params[0].Name != "profileId" {
				t.Fatalf("params = %+v, want profileId named", params)
			}
			if params[0].Reason != tt.reason {
				t.Errorf("reason = %q, want %q", params[0].Reason, tt.reason)
			}
			if got != uuid.Nil {
				t.Errorf("got %v on failure, want uuid.Nil", got)
			}
		})
	}
}

// Reasons are client-facing copy. Each one is rendered from a validator tag,
// so a wrong or missing case ships a message a user actually reads.
type tagCoverage struct {
	Req      string `json:"req"      validate:"required"`
	Email    string `json:"email"    validate:"omitempty,email"`
	UUID     string `json:"uuid"     validate:"omitempty,uuid"`
	URL      string `json:"url"      validate:"omitempty,url"`
	Min      string `json:"min"      validate:"omitempty,min=5"`
	Max      string `json:"max"      validate:"omitempty,max=3"`
	Len      string `json:"len"      validate:"omitempty,len=4"`
	Gt       int    `json:"gt"       validate:"omitempty,gt=10"`
	Gte      int    `json:"gte"      validate:"omitempty,gte=10"`
	Lt       int    `json:"lt"       validate:"omitempty,lt=2"`
	Lte      int    `json:"lte"      validate:"omitempty,lte=2"`
	OneOf    string `json:"oneof"    validate:"omitempty,oneof=a b c"`
	Password string `json:"password"`
	Confirm  string `json:"confirm"  validate:"omitempty,eqfield=Password"`
	Alpha    string `json:"alpha"    validate:"omitempty,alpha"`
	Contains string `json:"contains" validate:"omitempty,contains=xyz"`
	// No json tag: the Go name is the only name available to report.
	Untagged string `validate:"omitempty,numeric"`
	Hidden   string `json:"-"        validate:"omitempty,numeric"`
}

func TestReasonForEveryHandledTag(t *testing.T) {
	// Hidden is deliberately absent: a json:"-" field is not part of the wire
	// contract, so sending it is an unknown field and decoding rejects the
	// request before validation runs at all.
	w, r := postJSON(`{
		"email":"nope","uuid":"nope","url":"nope","min":"ab","max":"abcd","len":"ab",
		"gt":1,"gte":1,"lt":9,"lte":9,"oneof":"z","password":"a","confirm":"b",
		"alpha":"123","contains":"abc","Untagged":"abc"
	}`)

	_, err := httpin.DecodeAndValidate[tagCoverage](w, r, 0)
	if err == nil {
		t.Fatal("an invalid body was accepted")
	}

	got := map[string]string{}
	for _, p := range paramsFrom(t, err) {
		got[p.Name] = p.Reason
	}

	want := map[string]string{
		"req":      "is required",
		"email":    "must be a valid email address",
		"uuid":     "must be a valid UUID",
		"url":      "must be a valid URL",
		"min":      "must be at least 5",
		"max":      "must be at most 3",
		"len":      "must be exactly 4 in length",
		"gt":       "must be greater than 10",
		"gte":      "must be 10 or greater",
		"lt":       "must be less than 2",
		"lte":      "must be 2 or less",
		"oneof":    "must be one of: a, b, c",
		"confirm":  "must match Password",
		"alpha":    "failed the alpha rule",
		"contains": "failed the contains rule (xyz)",
		"Untagged": "failed the numeric rule",
	}
	for name, wantReason := range want {
		if got[name] != wantReason {
			t.Errorf("param %q reason = %q, want %q", name, got[name], wantReason)
		}
	}
}

func TestTruncatedJSONIsMalformedNotAServerError(t *testing.T) {
	w, r := postJSON(`{"email": "abc`)

	_, err := httpin.DecodeAndValidate[profileRequest](w, r, 0)
	appErr, ok := apperr.From(err)
	if !ok {
		t.Fatalf("error = %v, want an *apperr.Error", err)
	}
	if appErr.Kind != apperr.KindValidation || appErr.Code != "MALFORMED_JSON" {
		t.Errorf("kind/code = %q/%q, want validation/MALFORMED_JSON", appErr.Kind, appErr.Code)
	}
}

// Validating a non-struct is our configuration bug, not the client's, and
// must not be reported as a 4xx.
func TestNonStructTargetIsAnInternalError(t *testing.T) {
	w, r := postJSON(`5`)

	_, err := httpin.DecodeAndValidate[int](w, r, 0)
	if !apperr.IsKind(err, apperr.KindInternal) {
		t.Errorf("error = %v, want an internal error", err)
	}
}

// A json:"-" field is not part of the wire contract at all, so a client that
// sends it is naming a field that does not exist.
func TestJSONDashFieldsAreUnknownToTheDecoder(t *testing.T) {
	w, r := postJSON(`{"req":"x","Hidden":"123"}`)

	_, err := httpin.DecodeAndValidate[tagCoverage](w, r, 0)
	params := paramsFrom(t, err)
	if len(params) != 1 || params[0].Name != "Hidden" {
		t.Fatalf("params = %+v, want Hidden reported as unknown", params)
	}
	if params[0].Reason != "is not a recognised field" {
		t.Errorf("reason = %q, want it to say the field is unrecognised", params[0].Reason)
	}
}
