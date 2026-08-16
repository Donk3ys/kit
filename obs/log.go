package obs

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

// LogConfig configures NewLogger. The zero value gives JSON at Info to stdout,
// which is what a container wants.
type LogConfig struct {
	Level slog.Level
	// Format is "json" or "text". Anything else, including empty, is JSON:
	// structured output is the safe default for anything that gets shipped,
	// and a typo should not silently downgrade production logs to text.
	Format string
	// AddSource records the calling file and line. Useful in development,
	// measurably costly under load.
	AddSource bool
	// Output defaults to os.Stdout. Logs belong on stdout, not stderr: stderr
	// is for the process failing to start, and mixing the two makes ordinary
	// operation look like a fault to most collectors.
	Output io.Writer
}

// NewLogger builds a *slog.Logger whose records carry the current trace and
// span IDs. It returns the standard library type, not a wrapper — anything
// that takes a *slog.Logger takes this.
func NewLogger(cfg LogConfig) *slog.Logger {
	out := cfg.Output
	if out == nil {
		out = os.Stdout
	}

	opts := &slog.HandlerOptions{Level: cfg.Level, AddSource: cfg.AddSource}

	var base slog.Handler
	if strings.EqualFold(cfg.Format, "text") {
		base = slog.NewTextHandler(out, opts)
	} else {
		base = slog.NewJSONHandler(out, opts)
	}

	return slog.New(WithTraceContext(base))
}

// WithTraceContext wraps a slog.Handler so every record logged with a context
// inside a recording span gains trace_id and span_id. That correlation is what
// turns "this request failed" into "here is the trace of the request that
// failed"; without it, logs and traces are two haystacks.
//
// Exported separately so an app with its own handler — a test capturer, a
// third-party sink — gets the same behaviour without adopting NewLogger.
func WithTraceContext(h slog.Handler) slog.Handler {
	if h == nil {
		return nil
	}
	return traceHandler{Handler: h}
}

type traceHandler struct{ slog.Handler }

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs and WithGroup must re-wrap, or correlation is silently lost the
// first time anyone calls logger.With(...) — which every service does, usually
// in the composition root, so the loss would be total rather than partial.

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{Handler: h.Handler.WithAttrs(attrs)}
}

// WithGroup carries one caveat: because the trace attributes are added to the
// record, they land inside any group opened here rather than at the top level.
// Open groups on a derived logger rather than on the root if that matters.
func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{Handler: h.Handler.WithGroup(name)}
}
