// Package httpmw holds the small number of chi middlewares that have to know
// about kit's error boundary. Everything else in this layer already exists and
// should be taken from upstream rather than rebuilt here:
//
//	chi/middleware.RequestID      request correlation, read by respond
//	chi/middleware.RealIP         client IP behind a trusted proxy — see the caution below
//	chi/middleware.RequestSize    request body limit
//	chi/middleware.Timeout        per-request deadline
//	chi/middleware.Compress       response compression
//	go-chi/cors                   CORS
//	otelhttp.NewHandler           tracing and HTTP metrics, under OTel semantic conventions
//
// A recommended chain, outermost first:
//
//	r.Use(chimw.RequestID)                        // must precede AccessLog and the boundary
//	r.Use(chimw.RealIP)                           // ONLY behind a proxy you control
//	r.Use(httpmw.AccessLog(logger, "/healthz"))   // outside Recoverer, so panics still get a line
//	r.Use(httpmw.Recoverer(boundary))
//	r.Use(httpmw.SecurityHeaders(httpmw.SecurityHeadersConfig{}))
//	r.Use(chimw.RequestSize(1 << 20))
//	r.Use(chimw.Timeout(30 * time.Second))
//
// Caution on chi's RealIP: it trusts X-Forwarded-For unconditionally. Behind a
// proxy that overwrites the header this is correct; exposed directly to the
// internet it lets any caller forge its own address, which silently defeats
// rate limiting and misattributes audit logs. Install it only when a proxy you
// control is guaranteed to be in front.
package httpmw

import (
	"net/http"

	chimw "github.com/go-chi/chi/v5/middleware"
)

// wrapWriter returns w as a chi WrapResponseWriter, wrapping it only if some
// outer middleware has not already done so. Double-wrapping would make the
// inner recorder invisible to the outer one, so status and byte counts would
// silently read as zero.
func wrapWriter(w http.ResponseWriter, r *http.Request) chimw.WrapResponseWriter {
	if ww, ok := w.(chimw.WrapResponseWriter); ok {
		return ww
	}
	return chimw.NewWrapResponseWriter(w, r.ProtoMajor)
}
