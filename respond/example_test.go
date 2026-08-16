package respond_test

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"

	"github.com/Donk3ys/kit/apperr"
	"github.com/Donk3ys/kit/respond"
)

// quietBoundary keeps example output to the response, not the log line.
func quietBoundary() *respond.Boundary {
	return respond.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// Handlers return errors instead of writing them. Wrap is the only place a
// failed request is written, which is what makes "log exactly once" a property
// of the structure rather than a rule every handler has to remember.
func ExampleBoundary_Wrap() {
	b := quietBoundary()

	handler := b.Wrap(func(w http.ResponseWriter, r *http.Request) error {
		return apperr.NewNotFound("PROFILE_NOT_FOUND", "No profile for that year.", nil)
	})

	w := httptest.NewRecorder()
	handler(w, httptest.NewRequest(http.MethodGet, "/profiles/42", nil))

	fmt.Println(w.Code)
	fmt.Println(w.Header().Get("Content-Type"))
	fmt.Println(w.Body.String())

	// Output:
	// 404
	// application/problem+json
	// {"code":"PROFILE_NOT_FOUND","detail":"No profile for that year.","status":404,"title":"Not Found","type":"about:blank"}
}

// A successful handler returns the result of JSON, so the happy and unhappy
// paths are the same shape.
func ExampleJSON() {
	b := quietBoundary()

	handler := b.Wrap(func(w http.ResponseWriter, r *http.Request) error {
		return respond.JSON(w, http.StatusOK, map[string]any{"id": "42", "taxYear": 2026})
	})

	w := httptest.NewRecorder()
	handler(w, httptest.NewRequest(http.MethodGet, "/profiles/42", nil))

	fmt.Println(w.Code)
	fmt.Println(w.Header().Get("Content-Type"))
	fmt.Println(w.Body.String())

	// Output:
	// 200
	// application/json
	// {"id":"42","taxYear":2026}
}

// An unclassified error fails closed: the client gets a generic 500 and the
// cause reaches the log only. Nothing from the driver is echoed back.
func ExampleBoundary_Error() {
	b := quietBoundary()

	handler := b.Wrap(func(w http.ResponseWriter, r *http.Request) error {
		return fmt.Errorf("pq: password authentication failed for user %q", "admin")
	})

	w := httptest.NewRecorder()
	handler(w, httptest.NewRequest(http.MethodGet, "/profiles/42", nil))

	fmt.Println(w.Code)
	fmt.Println(w.Body.String())

	// Output:
	// 500
	// {"code":"INTERNAL_ERROR","detail":"An unexpected error occurred.","status":500,"title":"Internal Server Error","type":"about:blank"}
}

// Extension members are top-level siblings of the standard ones, so a client
// reads requiredTier alongside status.
func ExampleBoundary_Error_extensions() {
	b := quietBoundary()

	err := apperr.NewForbidden("INSUFFICIENT_TIER", "Trusted tier is required.", nil).
		WithExtensions(map[string]any{"requiredTier": "trusted"})

	w := httptest.NewRecorder()
	b.Error(w, httptest.NewRequest(http.MethodPost, "/deltas", nil), err)

	fmt.Println(w.Body.String())

	// Output:
	// {"code":"INSUFFICIENT_TIER","detail":"Trusted tier is required.","requiredTier":"trusted","status":403,"title":"Forbidden","type":"about:blank"}
}

// TypeBaseURI turns the type member into a documentation link per code.
func ExampleBoundary_typeBaseURI() {
	b := quietBoundary()
	b.TypeBaseURI = "https://api.example.com/problems"

	w := httptest.NewRecorder()
	b.Error(w, httptest.NewRequest(http.MethodGet, "/profiles/42", nil),
		apperr.NewNotFound("PROFILE_NOT_FOUND", "Not found.", nil))

	fmt.Println(w.Body.String())

	// Output:
	// {"code":"PROFILE_NOT_FOUND","detail":"Not found.","status":404,"title":"Not Found","type":"https://api.example.com/problems/profile-not-found"}
}
