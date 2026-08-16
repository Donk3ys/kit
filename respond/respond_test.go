package respond_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Donk3ys/kit/apperr"
	"github.com/Donk3ys/kit/respond"
	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgconn"
)

// capturingHandler records every slog.Record so tests can assert on level and
// attributes rather than on formatted text.
type capturingHandler struct{ records []slog.Record }

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

func newTestBoundary() (*respond.Boundary, *capturingHandler) {
	h := &capturingHandler{}
	return respond.New(slog.New(h)), h
}

func attrValue(r slog.Record, key string) (slog.Value, bool) {
	var (
		found slog.Value
		ok    bool
	)
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			found, ok = a.Value, true
			return false
		}
		return true
	})
	return found, ok
}

func requestWithID(id string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/profiles/42", nil)
	if id == "" {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), chimw.RequestIDKey, id))
}

func TestStatusFor(t *testing.T) {
	tests := []struct {
		kind apperr.Kind
		want int
	}{
		{apperr.KindValidation, http.StatusBadRequest},
		{apperr.KindUnprocessable, http.StatusUnprocessableEntity},
		{apperr.KindUnauthorized, http.StatusUnauthorized},
		{apperr.KindForbidden, http.StatusForbidden},
		{apperr.KindNotFound, http.StatusNotFound},
		{apperr.KindConflict, http.StatusConflict},
		{apperr.KindRateLimited, http.StatusTooManyRequests},
		{apperr.KindPayloadTooLarge, http.StatusRequestEntityTooLarge},
		{apperr.KindUnsupportedMedia, http.StatusUnsupportedMediaType},
		{apperr.KindTimeout, http.StatusGatewayTimeout},
		{apperr.KindExternal, http.StatusServiceUnavailable},
		{apperr.KindInternal, http.StatusInternalServerError},
		// An unrecognised Kind must fail closed to 500 so the boundary logs
		// it rather than passing it off as an expected client outcome.
		{apperr.Kind("unrecognised"), http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			if got := respond.StatusFor(tt.kind); got != tt.want {
				t.Errorf("StatusFor(%q) = %d, want %d", tt.kind, got, tt.want)
			}
		})
	}
}

// The problem body is API contract surface. A change to any member name here
// breaks every client, so it is pinned byte-for-byte.
func TestProblemBodyWireShape(t *testing.T) {
	b, _ := newTestBoundary()
	w := httptest.NewRecorder()

	b.Error(w, requestWithID("req-123"),
		apperr.NewNotFound("PROFILE_NOT_FOUND", "No profile for that year.", nil))

	if got := w.Code; got != http.StatusNotFound {
		t.Errorf("status = %d, want %d", got, http.StatusNotFound)
	}
	if got := w.Header().Get("Content-Type"); got != respond.ProblemContentType {
		t.Errorf("Content-Type = %q, want %q", got, respond.ProblemContentType)
	}

	want := `{"code":"PROFILE_NOT_FOUND","detail":"No profile for that year.",` +
		`"instance":"req-123","status":404,"title":"Not Found","type":"about:blank"}`
	if got := strings.TrimSpace(w.Body.String()); got != want {
		t.Errorf("body =\n  %s\nwant\n  %s", got, want)
	}
}

func TestInternalCauseNeverReachesTheClient(t *testing.T) {
	b, _ := newTestBoundary()
	w := httptest.NewRecorder()

	secret := errors.New("pq: password authentication failed for user \"admin\"")
	b.Error(w, requestWithID(""), secret)

	if strings.Contains(w.Body.String(), "password authentication") {
		t.Fatalf("internal cause leaked into the response body: %s", w.Body.String())
	}
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestExtensionsAreMergedAsTopLevelMembers(t *testing.T) {
	b, _ := newTestBoundary()
	w := httptest.NewRecorder()

	b.Error(w, requestWithID(""), apperr.NewInvalidParams(
		"VALIDATION_ERROR", "Some fields need attention.",
		apperr.InvalidParam{Name: "age", Reason: "must be a positive integer"},
	))

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshalling body: %v", err)
	}

	params, ok := body["invalid-params"].([]any)
	if !ok {
		t.Fatalf("invalid-params = %T, want a top-level array", body["invalid-params"])
	}
	first, ok := params[0].(map[string]any)
	if !ok || first["name"] != "age" {
		t.Errorf("invalid-params[0] = %v, want name=age", params[0])
	}
	if body["status"] != float64(http.StatusUnprocessableEntity) {
		t.Errorf("status = %v, want 422", body["status"])
	}
}

// An extension named after a reserved member would desynchronise the body
// from the response status line.
func TestReservedMembersCannotBeOverwrittenByExtensions(t *testing.T) {
	b, logs := newTestBoundary()
	w := httptest.NewRecorder()

	err := apperr.NewForbidden("FORBIDDEN", "Not permitted.", nil).
		WithExtensions(map[string]any{"status": 200, "type": "evil", "requiredTier": "trusted"})
	b.Error(w, requestWithID(""), err)

	var body map[string]any
	if jsonErr := json.Unmarshal(w.Body.Bytes(), &body); jsonErr != nil {
		t.Fatalf("unmarshalling body: %v", jsonErr)
	}

	if body["status"] != float64(http.StatusForbidden) {
		t.Errorf("status = %v, want 403 — an extension overwrote it", body["status"])
	}
	if body["type"] != "about:blank" {
		t.Errorf("type = %v, want about:blank — an extension overwrote it", body["type"])
	}
	if body["requiredTier"] != "trusted" {
		t.Errorf("requiredTier = %v, want the non-reserved extension to survive", body["requiredTier"])
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("response status = %d, want 403", w.Code)
	}

	if len(logs.records) != 1 {
		t.Fatalf("got %d log records, want 1", len(logs.records))
	}
	if _, ok := attrValue(logs.records[0], "skipped_extensions"); !ok {
		t.Error("dropped extensions were not reported in the log line")
	}
}

func TestTypeBaseURIProducesADocumentationLink(t *testing.T) {
	b, _ := newTestBoundary()
	b.TypeBaseURI = "https://api.example.com/problems/"
	w := httptest.NewRecorder()

	b.Error(w, requestWithID(""),
		apperr.NewNotFound("PROFILE_NOT_FOUND", "Not found.", nil))

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshalling body: %v", err)
	}
	want := "https://api.example.com/problems/profile-not-found"
	if body["type"] != want {
		t.Errorf("type = %v, want %q", body["type"], want)
	}
}

func TestLogLevelFollowsSeverity(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantLines int
		wantLevel slog.Level
	}{
		{
			name:      "expected client outcomes are not logged",
			err:       apperr.NewNotFound("NOT_FOUND", "Not found.", nil),
			wantLines: 0,
		},
		{
			name:      "forbidden warns",
			err:       apperr.NewForbidden("FORBIDDEN", "Not permitted.", nil),
			wantLines: 1,
			wantLevel: slog.LevelWarn,
		},
		{
			name:      "external errors",
			err:       apperr.NewExternal("DEP_DOWN", "Try again.", "postgres", nil),
			wantLines: 1,
			wantLevel: slog.LevelError,
		},
		{
			name:      "unclassified errors are internal and critical",
			err:       errors.New("boom"),
			wantLines: 1,
			wantLevel: slog.LevelError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, logs := newTestBoundary()
			b.Error(httptest.NewRecorder(), requestWithID(""), tt.err)

			if len(logs.records) != tt.wantLines {
				t.Fatalf("got %d log records, want %d", len(logs.records), tt.wantLines)
			}
			if tt.wantLines == 0 {
				return
			}
			if logs.records[0].Level != tt.wantLevel {
				t.Errorf("level = %v, want %v", logs.records[0].Level, tt.wantLevel)
			}
		})
	}
}

func TestLogCarriesCorrelationAndErrorAttributes(t *testing.T) {
	b, logs := newTestBoundary()

	b.Error(httptest.NewRecorder(), requestWithID("req-abc"),
		apperr.NewInternal("INTERNAL_ERROR", "An unexpected error occurred.", "assemble", errors.New("root cause")))

	if len(logs.records) != 1 {
		t.Fatalf("got %d log records, want 1", len(logs.records))
	}
	rec := logs.records[0]

	for key, want := range map[string]string{
		"code":       "INTERNAL_ERROR",
		"kind":       "internal",
		"severity":   "critical",
		"request_id": "req-abc",
		"method":     http.MethodGet,
		"operation":  "assemble", // carried through from the constructor's attrs
	} {
		got, ok := attrValue(rec, key)
		if !ok {
			t.Errorf("log line is missing attribute %q", key)
			continue
		}
		if got.String() != want {
			t.Errorf("attr %q = %q, want %q", key, got.String(), want)
		}
	}

	// The cause belongs in the log even though it never reaches the client.
	errAttr, ok := attrValue(rec, "error")
	if !ok || !strings.Contains(errAttr.String(), "root cause") {
		t.Errorf("error attr = %q, want it to contain the cause", errAttr.String())
	}
}

func TestWrapWritesNothingExtraOnSuccess(t *testing.T) {
	b, logs := newTestBoundary()
	w := httptest.NewRecorder()

	b.Wrap(func(w http.ResponseWriter, _ *http.Request) error {
		return respond.JSON(w, http.StatusCreated, map[string]string{"id": "42"})
	})(w, requestWithID(""))

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"id":"42"}` {
		t.Errorf("body = %s, want {\"id\":\"42\"}", got)
	}
	if len(logs.records) != 0 {
		t.Errorf("a successful request logged %d lines, want 0", len(logs.records))
	}
}

func TestWrapRoutesReturnedErrorsThroughTheBoundary(t *testing.T) {
	b, logs := newTestBoundary()
	w := httptest.NewRecorder()

	b.Wrap(func(http.ResponseWriter, *http.Request) error {
		return apperr.NewConflict("EMAIL_TAKEN", "That email is already registered.", nil)
	})(w, requestWithID(""))

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != respond.ProblemContentType {
		t.Errorf("Content-Type = %q, want %q", got, respond.ProblemContentType)
	}
	if len(logs.records) != 0 {
		t.Errorf("an expected client outcome logged %d lines, want 0", len(logs.records))
	}
}

// A handler that writes and then fails leaves headers on the wire. The
// boundary must log and stop, not corrupt the response with a second body.
func TestAlreadyCommittedResponseIsLoggedButNotRewritten(t *testing.T) {
	b, logs := newTestBoundary()
	rec := httptest.NewRecorder()
	w := chimw.NewWrapResponseWriter(rec, 1)

	b.Wrap(func(w http.ResponseWriter, _ *http.Request) error {
		if err := respond.JSON(w, http.StatusOK, map[string]string{"partial": "yes"}); err != nil {
			return err
		}
		return errors.New("failed after the response was sent")
	})(w, requestWithID(""))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want the original 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"partial":"yes"}` {
		t.Errorf("body = %s, want the original body untouched", got)
	}
	if len(logs.records) != 1 {
		t.Fatalf("got %d log records, want the failure logged exactly once", len(logs.records))
	}
}

func TestClassification(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "context deadline becomes a gateway timeout",
			err:        context.DeadlineExceeded,
			wantStatus: http.StatusGatewayTimeout,
			wantCode:   "REQUEST_TIMEOUT",
		},
		{
			name:       "client cancellation is reported but not alarming",
			err:        context.Canceled,
			wantStatus: http.StatusGatewayTimeout,
			wantCode:   "REQUEST_CANCELED",
		},
		{
			name:       "postgres statement timeout is an external failure",
			err:        &pgconn.PgError{Code: "57014"},
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "DATABASE_TIMEOUT",
		},
		{
			name:       "postgres lock timeout is an external failure",
			err:        &pgconn.PgError{Code: "55P03"},
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "DATABASE_TIMEOUT",
		},
		{
			name:       "other postgres errors stay internal",
			err:        &pgconn.PgError{Code: "23505"},
			wantStatus: http.StatusInternalServerError,
			wantCode:   "INTERNAL_ERROR",
		},
		{
			name:       "an unclassified error fails closed to internal",
			err:        errors.New("boom"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   "INTERNAL_ERROR",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := newTestBoundary()
			w := httptest.NewRecorder()
			b.Error(w, requestWithID(""), tt.err)

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshalling body: %v", err)
			}
			if body["code"] != tt.wantCode {
				t.Errorf("code = %v, want %q", body["code"], tt.wantCode)
			}
		})
	}
}

func TestClientCancellationIsNotLogged(t *testing.T) {
	b, logs := newTestBoundary()
	b.Error(httptest.NewRecorder(), requestWithID(""), context.Canceled)

	if len(logs.records) != 0 {
		t.Errorf("client cancellation logged %d lines, want 0 — this is the noisiest "+
			"failure in a busy service", len(logs.records))
	}
}

type rollbackFailure struct{ error }

func (rollbackFailure) UnexpectedCleanupFailure() bool { return true }

// A failed rollback must not be reported as the domain error that triggered
// it: the data may be in an unknown state, which is the more serious fact.
func TestCleanupFailureOutranksAWrappedAppError(t *testing.T) {
	b, _ := newTestBoundary()
	w := httptest.NewRecorder()

	domain := apperr.NewNotFound("PROFILE_NOT_FOUND", "Not found.", nil)
	b.Error(w, requestWithID(""), rollbackFailure{error: domain})

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshalling body: %v", err)
	}
	if body["code"] != "TRANSACTION_CLEANUP_FAILED" {
		t.Errorf("code = %v, want TRANSACTION_CLEANUP_FAILED", body["code"])
	}
}

func TestEmptySafeDetailFallsBackToStatusText(t *testing.T) {
	b, _ := newTestBoundary()
	w := httptest.NewRecorder()

	b.Error(w, requestWithID(""), apperr.NewConflict("EMAIL_TAKEN", "", nil))

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshalling body: %v", err)
	}
	if body["detail"] != "Conflict" {
		t.Errorf("detail = %v, want the status text as a fallback", body["detail"])
	}
}

func TestJSONReportsEncodeFailureWithoutWritingAPartialBody(t *testing.T) {
	w := httptest.NewRecorder()

	err := respond.JSON(w, http.StatusOK, map[string]any{"bad": make(chan int)})

	if err == nil {
		t.Fatal("JSON returned nil for an unserialisable value")
	}
	if !apperr.IsKind(err, apperr.KindInternal) {
		t.Errorf("error kind = %v, want internal", err)
	}
	if w.Body.Len() != 0 {
		t.Errorf("a partial body was written: %q", w.Body.String())
	}
}

func TestNoContent(t *testing.T) {
	w := httptest.NewRecorder()
	if err := respond.NoContent(w); err != nil {
		t.Fatalf("NoContent returned %v", err)
	}
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", w.Body.String())
	}
}

// Route labels must be bounded: the raw path would make every distinct ID its
// own log field and its own metric time series.
func TestRouteTemplateUsesTheChiPatternNotTheResolvedPath(t *testing.T) {
	b, logs := newTestBoundary()

	r := chi.NewRouter()
	r.Use(chimw.RequestID)
	r.Get("/profiles/{id}", b.Wrap(func(http.ResponseWriter, *http.Request) error {
		return apperr.NewInternal("INTERNAL_ERROR", "An unexpected error occurred.", "load", nil)
	}))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/profiles/0f8c9a1e", nil))

	if len(logs.records) != 1 {
		t.Fatalf("got %d log records, want 1", len(logs.records))
	}
	route, ok := attrValue(logs.records[0], "route")
	if !ok || route.String() != "/profiles/{id}" {
		t.Errorf("route = %q, want /profiles/{id}", route.String())
	}
	// chi's RequestID middleware is the source of the instance member.
	if id, ok := attrValue(logs.records[0], "request_id"); !ok || id.String() == "" {
		t.Error("request_id was not picked up from chi's RequestID middleware")
	}
}

func TestNewFallsBackToTheDefaultLogger(t *testing.T) {
	b := respond.New(nil)
	if b.Logger == nil {
		t.Error("New(nil) left a nil Logger, which would panic on the first failure")
	}
}

// The safety net: extensions are app-supplied, so an unserialisable value can
// reach the encoder. Answering with a valid problem beats answering with
// nothing.
func TestUnserialisableExtensionsFallBackToABodyWithoutThem(t *testing.T) {
	b, logs := newTestBoundary()
	w := httptest.NewRecorder()

	err := apperr.NewConflict("EMAIL_TAKEN", "That email is already registered.", nil).
		WithExtensions(map[string]any{"bad": make(chan int)})
	b.Error(w, requestWithID("req-9"), err)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != respond.ProblemContentType {
		t.Errorf("Content-Type = %q, want %q", got, respond.ProblemContentType)
	}

	var body map[string]any
	if jsonErr := json.Unmarshal(w.Body.Bytes(), &body); jsonErr != nil {
		t.Fatalf("fallback body was not valid JSON: %v (%s)", jsonErr, w.Body.String())
	}
	if body["code"] != "EMAIL_TAKEN" || body["instance"] != "req-9" {
		t.Errorf("fallback body lost its base members: %v", body)
	}
	if _, present := body["bad"]; present {
		t.Error("the unserialisable extension survived into the fallback body")
	}

	var reported bool
	for _, rec := range logs.records {
		if strings.Contains(rec.Message, "extensions could not be serialised") {
			reported = true
		}
	}
	if !reported {
		t.Error("the dropped extensions were not reported in the log")
	}
}

type failingWriter struct{ http.ResponseWriter }

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write: connection reset by peer")
}

func TestJSONReportsAWriteFailureAsMediumSeverity(t *testing.T) {
	w := failingWriter{ResponseWriter: httptest.NewRecorder()}

	err := respond.JSON(w, http.StatusOK, map[string]string{"id": "42"})
	if err == nil {
		t.Fatal("JSON returned nil when the write failed")
	}

	appErr, ok := apperr.From(err)
	if !ok {
		t.Fatalf("error was not an *apperr.Error: %T", err)
	}
	if appErr.Code != "RESPONSE_WRITE_FAILED" {
		t.Errorf("code = %q, want RESPONSE_WRITE_FAILED", appErr.Code)
	}
	// A client hanging up mid-response is worth seeing, not worth paging for.
	if got := appErr.LogSeverity(); got != apperr.SeverityMedium {
		t.Errorf("severity = %q, want %q", got, apperr.SeverityMedium)
	}
}

func TestRouteTemplateFallbacks(t *testing.T) {
	t.Run("net/http ServeMux pattern", func(t *testing.T) {
		var got string
		mux := http.NewServeMux()
		mux.HandleFunc("GET /profiles/{id}", func(_ http.ResponseWriter, r *http.Request) {
			got = respond.RouteTemplate(r)
		})
		mux.ServeHTTP(httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/profiles/0f8c9a1e", nil))

		if got != "GET /profiles/{id}" {
			t.Errorf("RouteTemplate = %q, want the ServeMux pattern", got)
		}
	})

	t.Run("raw path when nothing matched", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/unrouted/path", nil)
		if got := respond.RouteTemplate(r); got != "/unrouted/path" {
			t.Errorf("RouteTemplate = %q, want the raw path", got)
		}
	})
}
