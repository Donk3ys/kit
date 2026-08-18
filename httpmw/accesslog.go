package httpmw

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/Donk3ys/kit/respond"
	chimw "github.com/go-chi/chi/v5/middleware"
)

// AccessLog emits one structured line per request. It exists rather than
// chi's own Logger because that one writes coloured text to an io.Writer,
// which is unusable as structured data, and because this shares
// respond.RouteTemplate so a request and its failure carry identical labels.
//
// Requests whose path exactly matches one of skipPaths are not logged. Health
// and readiness probes hit every few seconds and drown everything else.
//
// It logs at Info regardless of status. Failures already get their own line
// from the error boundary, at a level derived from severity; duplicating that
// judgement here would mean two levels for one event that disagree.
func AccessLog(logger *slog.Logger, skipPaths ...string) func(http.Handler) http.Handler {
	skip := make(map[string]struct{}, len(skipPaths))
	for _, p := range skipPaths {
		skip[p] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, skipped := skip[r.URL.Path]; skipped {
				next.ServeHTTP(w, r)
				return
			}

			ww := wrapWriter(w, r)
			start := time.Now()

			// Deferred so a panic recovered further in still produces an
			// access line, with the status the recoverer ended up writing.
			defer func() {
				attrs := []slog.Attr{
					slog.String("method", r.Method),
					slog.String("route", respond.RouteTemplate(r)),
					// Path without RawQuery, deliberately. Query strings carry
					// credentials often enough — an EventSource that cannot set
					// headers and falls back to ?token= is the standard case —
					// that logging them turns the access log into a secret store.
					slog.String("path", r.URL.Path),
					// Normalised, because chi reports 0 until something
					// writes, while net/http sends 200 for a handler that
					// writes nothing at all. Logging the raw value made a
					// successful empty response look like a request that never
					// got a reply.
					slog.Int("status", loggedStatus(ww.Status())),
					slog.Int("bytes", ww.BytesWritten()),
					slog.Duration("duration", time.Since(start)),
				}
				if id := chimw.GetReqID(r.Context()); id != "" {
					attrs = append(attrs, slog.String("request_id", id))
				}
				if r.RemoteAddr != "" {
					attrs = append(attrs, slog.String("remote_addr", r.RemoteAddr))
				}
				if ua := r.UserAgent(); ua != "" {
					attrs = append(attrs, slog.String("user_agent", ua))
				}
				logger.LogAttrs(r.Context(), slog.LevelInfo, "request", attrs...)
			}()

			next.ServeHTTP(ww, r)
		})
	}
}

// loggedStatus resolves a not-yet-written status to what the client actually
// received. Only 0 is translated: any status the handler set is reported as-is,
// including ones it set and then failed to write.
func loggedStatus(status int) int {
	if status == 0 {
		return http.StatusOK
	}
	return status
}
