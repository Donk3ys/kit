package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// memStore is an in-memory widgetStore, so the whole stack can be exercised
// without a database. It is the only thing swapped out — the router, the
// middleware chain, the boundary and the service are the production ones.
type memStore struct {
	byName map[string]uuid.UUID
	byUUID map[uuid.UUID]Widget
}

func newMemStore() *memStore {
	return &memStore{byName: map[string]uuid.UUID{}, byUUID: map[uuid.UUID]Widget{}}
}

func (m *memStore) ByID(_ context.Context, id uuid.UUID) (Widget, error) {
	w, ok := m.byUUID[id]
	if !ok {
		return Widget{}, errWidgetNotFound
	}
	return w, nil
}

func (m *memStore) Create(_ context.Context, w Widget) error {
	if _, taken := m.byName[w.Name]; taken {
		return errNameTaken
	}
	m.byName[w.Name] = w.ID
	m.byUUID[w.ID] = w
	return nil
}

func (m *memStore) Delete(_ context.Context, id uuid.UUID) error {
	w, ok := m.byUUID[id]
	if !ok {
		return errWidgetNotFound
	}
	delete(m.byUUID, id)
	delete(m.byName, w.Name)
	return nil
}

// instanceRE redacts the request ID so example output is deterministic. The
// real value is a fresh ULID per request — that is the point of it.
var instanceRE = regexp.MustCompile(`"instance":"[^"]*"`)

// fixedID makes created widgets deterministic for the same reason.
const fixedID = "0f8c9a1e-2b3d-4c5e-8f90-1a2b3c4d5e6f"

func newTestRouter() http.Handler {
	svc := &widgetService{
		store:  newMemStore(),
		nextID: func() uuid.UUID { return uuid.MustParse(fixedID) },
	}
	// Logs go nowhere so the example output is just the wire traffic. In
	// production this is where the one-line-per-failure log would appear.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newRouter(logger, nil, svc)
}

// call performs a request and prints what a client would actually see.
func call(h http.Handler, method, path, body string) {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	// TrimRight because a 204 carries no Content-Type, and a dangling space
	// would make this example's output whitespace-sensitive.
	fmt.Println(strings.TrimRight(
		fmt.Sprintf("%s %s -> %d %s", method, path, w.Code, w.Header().Get("Content-Type")), " "))
	if w.Body.Len() > 0 {
		fmt.Println(" ", instanceRE.ReplaceAllString(strings.TrimSpace(w.Body.String()),
			`"instance":"<request-id>"`))
	}
}

// Example_endToEnd drives every kit package through one router: chi routing
// and request IDs, httpmw's access log, recoverer and security headers,
// httpin's decoding and validation, the service's classification, and
// respond's problem-details boundary.
//
// Read it top to bottom as the story of what a client sees.
func Example_endToEnd() {
	h := newTestRouter()

	// A path parameter that is not an identifier at all — a different mistake
	// from asking for one that does not exist, so 422 rather than 404.
	call(h, http.MethodGet, "/api/v1/widgets/banana", "")

	// Well-formed id, no such widget. The service maps its store's sentinel;
	// kit never guesses that a missing row means 404.
	call(h, http.MethodGet, "/api/v1/widgets/11111111-1111-1111-1111-111111111111", "")

	// Two field failures at once, named as the client sent them.
	call(h, http.MethodPost, "/api/v1/widgets", `{"name":"","quantity":-4}`)

	// Strict decoding: a misspelled field is rejected, not ignored.
	call(h, http.MethodPost, "/api/v1/widgets", `{"name":"bolt","quantity":1,"qty":2}`)

	// The happy path.
	call(h, http.MethodPost, "/api/v1/widgets", `{"name":"bolt","quantity":12}`)

	// The same name again: a conflict, carrying an extension member.
	call(h, http.MethodPost, "/api/v1/widgets", `{"name":"bolt","quantity":3}`)

	// Read it back, then remove it.
	call(h, http.MethodGet, "/api/v1/widgets/"+fixedID, "")
	call(h, http.MethodDelete, "/api/v1/widgets/"+fixedID, "")
	call(h, http.MethodGet, "/api/v1/widgets/"+fixedID, "")

	// Output:
	// GET /api/v1/widgets/banana -> 422 application/problem+json
	//   {"code":"VALIDATION_ERROR","detail":"A path parameter is not a valid identifier.","instance":"<request-id>","invalid-params":[{"name":"id","reason":"must be a valid UUID"}],"status":422,"title":"Unprocessable Entity","type":"https://api.example.com/problems/validation-error"}
	// GET /api/v1/widgets/11111111-1111-1111-1111-111111111111 -> 404 application/problem+json
	//   {"code":"WIDGET_NOT_FOUND","detail":"No widget with that id.","instance":"<request-id>","status":404,"title":"Not Found","type":"https://api.example.com/problems/widget-not-found"}
	// POST /api/v1/widgets -> 422 application/problem+json
	//   {"code":"VALIDATION_ERROR","detail":"Some fields need attention.","instance":"<request-id>","invalid-params":[{"name":"name","reason":"is required"},{"name":"quantity","reason":"must be 0 or greater"}],"status":422,"title":"Unprocessable Entity","type":"https://api.example.com/problems/validation-error"}
	// POST /api/v1/widgets -> 422 application/problem+json
	//   {"code":"VALIDATION_ERROR","detail":"The request contains an unknown field.","instance":"<request-id>","invalid-params":[{"name":"qty","reason":"is not a recognised field"}],"status":422,"title":"Unprocessable Entity","type":"https://api.example.com/problems/validation-error"}
	// POST /api/v1/widgets -> 201 application/json
	//   {"id":"0f8c9a1e-2b3d-4c5e-8f90-1a2b3c4d5e6f","name":"bolt","quantity":12}
	// POST /api/v1/widgets -> 409 application/problem+json
	//   {"code":"WIDGET_NAME_TAKEN","conflictingName":"bolt","detail":"A widget with that name already exists.","instance":"<request-id>","status":409,"title":"Conflict","type":"https://api.example.com/problems/widget-name-taken"}
	// GET /api/v1/widgets/0f8c9a1e-2b3d-4c5e-8f90-1a2b3c4d5e6f -> 200 application/json
	//   {"id":"0f8c9a1e-2b3d-4c5e-8f90-1a2b3c4d5e6f","name":"bolt","quantity":12}
	// DELETE /api/v1/widgets/0f8c9a1e-2b3d-4c5e-8f90-1a2b3c4d5e6f -> 204
	// GET /api/v1/widgets/0f8c9a1e-2b3d-4c5e-8f90-1a2b3c4d5e6f -> 404 application/problem+json
	//   {"code":"WIDGET_NOT_FOUND","detail":"No widget with that id.","instance":"<request-id>","status":404,"title":"Not Found","type":"https://api.example.com/problems/widget-not-found"}
}

// A panic produces the same problem+json shape a returned error does, rather
// than chi's plain-text 500 — and the panic value never reaches the client.
func Example_panicRecovery() {
	h := newTestRouter()
	call(h, http.MethodGet, "/api/v1/boom", "")

	// Output:
	// GET /api/v1/boom -> 500 application/problem+json
	//   {"code":"PANIC","detail":"An unexpected error occurred.","instance":"<request-id>","status":500,"title":"Internal Server Error","type":"https://api.example.com/problems/panic"}
}

// Security headers are applied to every response, including error responses.
func Example_securityHeaders() {
	h := newTestRouter()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	for _, name := range []string{
		"X-Content-Type-Options",
		"X-Frame-Options",
		"Referrer-Policy",
		"Content-Security-Policy",
	} {
		fmt.Printf("%s: %s\n", name, w.Header().Get(name))
	}

	// Output:
	// X-Content-Type-Options: nosniff
	// X-Frame-Options: DENY
	// Referrer-Policy: no-referrer
	// Content-Security-Policy: default-src 'none'; frame-ancestors 'none'; base-uri 'none'
}
