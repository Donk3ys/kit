package httpmw_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Donk3ys/kit/httpmw"
	"github.com/Donk3ys/kit/respond"
	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
)

type capturingHandler struct{ records []slog.Record }

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

func newCapturingLogger() (*slog.Logger, *capturingHandler) {
	h := &capturingHandler{}
	return slog.New(h), h
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

func TestRecovererTurnsAPanicIntoAProblemResponse(t *testing.T) {
	logger, logs := newCapturingLogger()
	b := respond.New(logger)

	h := httpmw.Recoverer(b)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("something went very wrong")
	}))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != respond.ProblemContentType {
		t.Errorf("Content-Type = %q, want %q", got, respond.ProblemContentType)
	}
	// The panic value is diagnostic, not client-facing.
	if strings.Contains(w.Body.String(), "something went very wrong") {
		t.Errorf("panic value leaked into the response: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"code":"PANIC"`) {
		t.Errorf("body = %s, want a PANIC code", w.Body.String())
	}

	if len(logs.records) != 1 {
		t.Fatalf("got %d log records, want 1", len(logs.records))
	}
	stack, ok := attrValue(logs.records[0], "stack")
	if !ok {
		t.Fatal("no stack attribute on the panic log line")
	}
	// Captured inside the deferred recover, so the panicking frame is present.
	if !strings.Contains(stack.String(), "httpmw_test") {
		t.Errorf("stack does not reach the panicking frame:\n%s", stack.String())
	}
	if errAttr, _ := attrValue(logs.records[0], "error"); !strings.Contains(errAttr.String(), "something went very wrong") {
		t.Errorf("panic value missing from the log: %q", errAttr.String())
	}
}

// net/http expects to see ErrAbortHandler itself; swallowing it would turn a
// deliberate abort into a spurious 500.
func TestRecovererRepanicsErrAbortHandler(t *testing.T) {
	b := respond.New(slog.New(&capturingHandler{}))

	h := httpmw.Recoverer(b)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	defer func() {
		rvr := recover()
		if rvr == nil {
			t.Fatal("ErrAbortHandler was swallowed instead of re-panicked")
		}
		if rvr != http.ErrAbortHandler {
			t.Errorf("re-panicked with %v, want http.ErrAbortHandler", rvr)
		}
	}()

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/abort", nil))
}

// The regression this guards: recovering against the unwrapped ResponseWriter
// makes the committed check always read false, and a second body gets appended
// to a response that already went out.
func TestRecovererDoesNotRewriteAnAlreadyCommittedResponse(t *testing.T) {
	logger, logs := newCapturingLogger()
	b := respond.New(logger)

	h := httpmw.Recoverer(b)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = respond.JSON(w, http.StatusOK, map[string]string{"partial": "yes"})
		panic("failed after responding")
	}))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/late-panic", nil))

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want the original 200", w.Code)
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"partial":"yes"}` {
		t.Errorf("body = %s, want the original body untouched", got)
	}
	if len(logs.records) != 1 {
		t.Errorf("got %d log records, want the panic logged exactly once", len(logs.records))
	}
}

func TestRecovererIsTransparentWhenNothingPanics(t *testing.T) {
	logger, logs := newCapturingLogger()
	b := respond.New(logger)

	h := httpmw.Recoverer(b)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = respond.JSON(w, http.StatusOK, map[string]string{"ok": "yes"})
	}))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/fine", nil))

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if len(logs.records) != 0 {
		t.Errorf("a successful request logged %d lines, want 0", len(logs.records))
	}
}

func TestAccessLogRecordsOneLinePerRequest(t *testing.T) {
	logger, logs := newCapturingLogger()

	r := chi.NewRouter()
	r.Use(chimw.RequestID)
	r.Use(httpmw.AccessLog(logger))
	r.Get("/profiles/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_ = respond.JSON(w, http.StatusCreated, map[string]string{"id": "42"})
	})

	req := httptest.NewRequest(http.MethodGet, "/profiles/0f8c9a1e?token=secret", nil)
	req.Header.Set("User-Agent", "kit-test/1.0")
	r.ServeHTTP(httptest.NewRecorder(), req)

	if len(logs.records) != 1 {
		t.Fatalf("got %d log records, want 1", len(logs.records))
	}
	rec := logs.records[0]

	if rec.Level != slog.LevelInfo {
		t.Errorf("level = %v, want Info", rec.Level)
	}
	for key, want := range map[string]string{
		"method":     http.MethodGet,
		"route":      "/profiles/{id}", // bounded, not the resolved path
		"path":       "/profiles/0f8c9a1e",
		"user_agent": "kit-test/1.0",
	} {
		got, ok := attrValue(rec, key)
		if !ok {
			t.Errorf("missing attribute %q", key)
			continue
		}
		if got.String() != want {
			t.Errorf("attr %q = %q, want %q", key, got.String(), want)
		}
	}

	if status, _ := attrValue(rec, "status"); status.Int64() != http.StatusCreated {
		t.Errorf("status = %d, want 201", status.Int64())
	}
	if bytes, _ := attrValue(rec, "bytes"); bytes.Int64() == 0 {
		t.Error("bytes = 0, want the response size")
	}
	if id, ok := attrValue(rec, "request_id"); !ok || id.String() == "" {
		t.Error("request_id was not picked up from chi's RequestID middleware")
	}
	if _, ok := attrValue(rec, "duration"); !ok {
		t.Error("missing duration attribute")
	}
}

// TestAccessLogReportsAnEmptySuccessAs200 is a regression test. The line used
// to carry chi's raw status, which stays 0 until something writes — so a
// handler that returned without writing was logged as status 0 while net/http
// sent the client a 200. A dashboard counting non-2xx saw failures that never
// happened.
func TestAccessLogReportsAnEmptySuccessAs200(t *testing.T) {
	logger, logs := newCapturingLogger()

	h := httpmw.AccessLog(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ping", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("net/http sent %d; this test assumes an empty response is a 200", rec.Code)
	}
	if len(logs.records) != 1 {
		t.Fatalf("got %d log records, want 1", len(logs.records))
	}
	if status, _ := attrValue(logs.records[0], "status"); status.Int64() != http.StatusOK {
		t.Errorf("status = %d, want 200 — what the client actually received", status.Int64())
	}
}

// The normalisation above must not reinterpret a status the handler chose.
func TestAccessLogReportsAnExplicitStatusUnchanged(t *testing.T) {
	logger, logs := newCapturingLogger()

	h := httpmw.AccessLog(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodDelete, "/widgets/1", nil))

	if status, _ := attrValue(logs.records[0], "status"); status.Int64() != http.StatusNoContent {
		t.Errorf("status = %d, want 204", status.Int64())
	}
}

// A query string is a routine place for credentials — an EventSource that
// cannot set headers and falls back to ?token= is the standard case.
func TestAccessLogNeverRecordsTheQueryString(t *testing.T) {
	logger, logs := newCapturingLogger()

	h := httpmw.AccessLog(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/events?token=super-secret-jwt", nil))

	if len(logs.records) != 1 {
		t.Fatalf("got %d log records, want 1", len(logs.records))
	}
	logs.records[0].Attrs(func(a slog.Attr) bool {
		if strings.Contains(a.Value.String(), "super-secret-jwt") {
			t.Errorf("attribute %q leaked the query string: %q", a.Key, a.Value.String())
		}
		return true
	})
}

func TestAccessLogSkipsConfiguredPaths(t *testing.T) {
	logger, logs := newCapturingLogger()

	h := httpmw.AccessLog(logger, "/healthz", "/readyz")(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	for _, path := range []string{"/healthz", "/readyz"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	if len(logs.records) != 0 {
		t.Fatalf("probe paths logged %d lines, want 0", len(logs.records))
	}

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/profiles", nil))
	if len(logs.records) != 1 {
		t.Errorf("got %d log records for a normal path, want 1", len(logs.records))
	}
}

// AccessLog sits outside Recoverer so a panicking request still produces an
// access line, carrying the status the recoverer settled on.
func TestAccessLogRecordsPanickedRequestsWithTheRecoveredStatus(t *testing.T) {
	logger, logs := newCapturingLogger()
	b := respond.New(logger)

	r := chi.NewRouter()
	r.Use(chimw.RequestID)
	r.Use(httpmw.AccessLog(logger))
	r.Use(httpmw.Recoverer(b))
	r.Get("/boom", func(http.ResponseWriter, *http.Request) { panic("kaboom") })

	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/boom", nil))

	var access *slog.Record
	for i := range logs.records {
		if logs.records[i].Message == "request" {
			access = &logs.records[i]
		}
	}
	if access == nil {
		t.Fatal("a panicking request produced no access log line")
	}
	if status, _ := attrValue(*access, "status"); status.Int64() != http.StatusInternalServerError {
		t.Errorf("access log status = %d, want the recovered 500", status.Int64())
	}
}

func TestSecurityHeaders(t *testing.T) {
	tests := []struct {
		name    string
		cfg     httpmw.SecurityHeadersConfig
		want    map[string]string
		notSet  []string
		comment string
	}{
		{
			name: "zero value is sound for a JSON API behind a TLS proxy",
			want: map[string]string{
				"X-Content-Type-Options":     "nosniff",
				"X-Frame-Options":            "DENY",
				"Referrer-Policy":            "no-referrer",
				"Content-Security-Policy":    httpmw.DefaultContentSecurityPolicy,
				"Cross-Origin-Opener-Policy": "same-origin",
			},
			// HSTS and CORP are deployment decisions this package cannot make.
			// X-XSS-Protection is deprecated and must never be set.
			notSet: []string{
				"Strict-Transport-Security",
				"Cross-Origin-Resource-Policy",
				"X-XSS-Protection",
			},
		},
		{
			name: "HSTS when configured",
			cfg:  httpmw.SecurityHeadersConfig{HSTSMaxAgeSeconds: 31536000},
			want: map[string]string{"Strict-Transport-Security": "max-age=31536000"},
		},
		{
			name: "HSTS with subdomains",
			cfg: httpmw.SecurityHeadersConfig{
				HSTSMaxAgeSeconds:     31536000,
				HSTSIncludeSubdomains: true,
			},
			want: map[string]string{
				"Strict-Transport-Security": "max-age=31536000; includeSubDomains",
			},
		},
		{
			name: "overrides",
			cfg: httpmw.SecurityHeadersConfig{
				ContentSecurityPolicy:     "default-src 'self'",
				CrossOriginResourcePolicy: "same-site",
			},
			want: map[string]string{
				"Content-Security-Policy":      "default-src 'self'",
				"Cross-Origin-Resource-Policy": "same-site",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := httpmw.SecurityHeaders(tt.cfg)(
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusOK)
				}))

			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

			for header, want := range tt.want {
				if got := w.Header().Get(header); got != want {
					t.Errorf("%s = %q, want %q", header, got, want)
				}
			}
			for _, header := range tt.notSet {
				if got := w.Header().Get(header); got != "" {
					t.Errorf("%s = %q, want it unset", header, got)
				}
			}
		})
	}
}

// Double-wrapping hides the inner recorder from the outer one, so status and
// byte counts silently read as zero.
func TestWritersAreNotDoubleWrapped(t *testing.T) {
	logger, logs := newCapturingLogger()
	b := respond.New(logger)

	r := chi.NewRouter()
	r.Use(httpmw.AccessLog(logger))
	r.Use(httpmw.Recoverer(b))
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		_ = respond.JSON(w, http.StatusTeapot, map[string]string{"a": "b"})
	})

	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

	if len(logs.records) != 1 {
		t.Fatalf("got %d log records, want 1", len(logs.records))
	}
	status, _ := attrValue(logs.records[0], "status")
	if status.Int64() != http.StatusTeapot {
		t.Errorf("access log status = %d, want 418 — the outer wrapper lost sight "+
			"of the inner one", status.Int64())
	}
	if bytes, _ := attrValue(logs.records[0], "bytes"); bytes.Int64() == 0 {
		t.Error("bytes = 0 — the outer wrapper lost sight of the inner one")
	}
}
