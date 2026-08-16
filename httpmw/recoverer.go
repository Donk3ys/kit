package httpmw

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/Donk3ys/kit/apperr"
	"github.com/Donk3ys/kit/respond"
)

// Recoverer catches panics from inner handlers and turns them into a logged
// RFC 9457 problem response, rather than chi's plain-text 500. It exists
// alongside chi's own Recoverer for exactly that reason: a client should get
// the same error shape whether the failure was returned or panicked.
//
// The stack is captured inside the deferred recover, while the panicking
// frames are still on the stack — capturing at any later point would record
// the middleware chain instead of the failure site.
func Recoverer(b *respond.Boundary) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Hoisted so the deferred func reports on the same writer the
			// handler used. Recovering against the unwrapped w would make the
			// boundary's already-committed check always read false, and a
			// panic after a partial write would corrupt the response.
			ww := wrapWriter(w, r)

			defer func() {
				rvr := recover()
				if rvr == nil {
					return
				}

				// http.ErrAbortHandler is the documented way for a handler to
				// abandon a request silently; net/http expects to see it and
				// suppresses it itself. Swallowing it here would turn an
				// intentional abort into a spurious 500.
				if rvr == http.ErrAbortHandler {
					panic(rvr)
				}

				err := apperr.NewInternal(
					"PANIC", respond.GenericInternalDetail, "panic",
					fmt.Errorf("panic: %v", rvr),
				).WithAttrs(slog.String("stack", string(debug.Stack())))

				// The boundary handles the already-committed case: if the
				// handler had written before panicking, this only logs.
				b.Error(ww, r, err)
			}()

			next.ServeHTTP(ww, r)
		})
	}
}
