package httpin_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/Donk3ys/kit/apperr"
	"github.com/Donk3ys/kit/httpin"
)

type CreateProfile struct {
	Email   string `json:"email"   validate:"required,email"`
	TaxYear int    `json:"taxYear" validate:"required,oneof=2026 2027"`
}

func jsonRequest(body string) (*httptest.ResponseRecorder, *http.Request) {
	r := httptest.NewRequest(http.MethodPost, "/profiles", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return httptest.NewRecorder(), r
}

func ExampleDecodeAndValidate() {
	w, r := jsonRequest(`{"email":"a@b.com","taxYear":2026}`)

	in, err := httpin.DecodeAndValidate[CreateProfile](w, r, 0)
	if err != nil {
		fmt.Println("unexpected:", err)
		return
	}
	fmt.Println(in.Email, in.TaxYear)

	// Output: a@b.com 2026
}

// Validation failures are reported per field, using the names the client sent
// rather than the Go identifiers.
func ExampleDecodeAndValidate_validation() {
	w, r := jsonRequest(`{"email":"not-an-email","taxYear":1999}`)

	_, err := httpin.DecodeAndValidate[CreateProfile](w, r, 0)

	appErr, _ := apperr.From(err)
	fmt.Println(appErr.Kind)
	for _, p := range appErr.Extensions[apperr.ExtensionKeyInvalidParams].([]apperr.InvalidParam) {
		fmt.Printf("%s: %s\n", p.Name, p.Reason)
	}

	// Output:
	// unprocessable
	// email: must be a valid email address
	// taxYear: must be one of: 2026, 2027
}

// Decoding is strict: a misspelled field is rejected rather than silently
// ignored, so the client learns about it immediately.
func ExampleDecodeAndValidate_unknownField() {
	w, r := jsonRequest(`{"email":"a@b.com","taxYear":2026,"taxYr":2027}`)

	_, err := httpin.DecodeAndValidate[CreateProfile](w, r, 0)

	appErr, _ := apperr.From(err)
	params := appErr.Extensions[apperr.ExtensionKeyInvalidParams].([]apperr.InvalidParam)
	fmt.Printf("%s: %s\n", params[0].Name, params[0].Reason)

	// Output: taxYr: is not a recognised field
}

// Requiring application/json is also a CSRF control: a cross-origin form
// cannot set that content type without a preflight.
func ExampleDecodeAndValidate_contentType() {
	r := httptest.NewRequest(http.MethodPost, "/profiles",
		strings.NewReader("email=a@b.com"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	_, err := httpin.DecodeAndValidate[CreateProfile](httptest.NewRecorder(), r, 0)

	appErr, _ := apperr.From(err)
	fmt.Println(appErr.Kind, appErr.Code)

	// Output: unsupported_media UNSUPPORTED_MEDIA_TYPE
}

func ExampleUUIDParam() {
	id, err := httpin.UUIDParam("0f8c9a1e-2b3d-4c5e-8f90-1a2b3c4d5e6f", "profileId")
	fmt.Println(id, err)

	_, err = httpin.UUIDParam("banana", "profileId")
	appErr, _ := apperr.From(err)
	fmt.Println(appErr.Kind)

	// Output:
	// 0f8c9a1e-2b3d-4c5e-8f90-1a2b3c4d5e6f <nil>
	// unprocessable
}
