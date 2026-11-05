package webapp

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// HeartbeatPath is the liveness endpoint. It is answered by middleware,
// before routing and before the rest of the stack, so that a load
// balancer can still get an answer when the application's handlers are
// wedged or the database is down and a route would block.
const HeartbeatPath = "/-/alive"

// requestIDHeader is the response header carrying the correlation ID.
// It is echoed to the client so that a user reporting a failure can
// quote something that maps to a log line.
const requestIDHeader = "X-Request-Id"

// requestLogger logs one line per request with method, path, status,
// bytes, duration, and the correlation ID.
//
// Status and byte count require the response to be wrapped; chi's
// wrapper is used so that a Hijack for a websocket or a Flush for
// streaming keeps working. A handler that never calls WriteHeader
// reports 200, matching net/http.
func (a *App) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(rec, r)

		status := rec.Status()
		if status == 0 {
			status = http.StatusOK
		}

		// A 5xx is the application's fault and belongs at error level
		// so it reaches whoever watches logs; a 4xx is a client or
		// scanner condition and would drown the useful lines at that
		// level.
		lvl := slog.LevelInfo
		switch {
		case status >= 500:
			lvl = slog.LevelError
		case status >= 400:
			lvl = slog.LevelWarn
		}

		a.log().Log(r.Context(), lvl, "http request",
			"request_id", middleware.GetReqID(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"bytes", rec.BytesWritten(),
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", clientAddr(r),
		)
	})
}

// recoverer turns a panic into a 500 with a logged stack.
//
// It is deliberately not chi's Recoverer: that one writes to the
// standard library's log package, which bypasses every handler, level,
// and field the application configured. Here the panic is logged through
// slog at error level with the correlation ID, and the response goes
// through Fail so that the status code and the disclosure policy are the
// same as for any other failure.
//
// http.ErrAbortHandler is re-panicked untouched. That sentinel is how a
// handler asks net/http to drop a connection, notably when a client
// disconnects mid-stream, and swallowing it would turn a correct
// abandonment into a bogus half-written 500.
func (a *App) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler {
				panic(rec)
			}

			a.log().ErrorContext(r.Context(), "panic handled by webapp",
				"request_id", middleware.GetReqID(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"panic", rec,
				"stack", string(debug.Stack()),
			)

			// A panic after the status line went out cannot be
			// reported as a 500. Failing anyway would write a second
			// header and emit a superfluous-WriteHeader log line from
			// net/http, so the response is simply left truncated, which
			// is what the client can act on.
			if started(w) {
				return
			}
			a.Fail(w, r, http.StatusInternalServerError, errPanic{})
		}()

		next.ServeHTTP(w, r)
	})
}

// errPanic is the error reported for a recovered panic. It carries no
// text about the panic value, which may contain request data or
// internal identifiers and is logged rather than rendered.
type errPanic struct{}

func (errPanic) Error() string { return "internal server error" }

// secureHeaders sets the response headers that are correct for every
// server-rendered application and cheap to set globally.
//
// A Content-Security-Policy is not included. A useful CSP is specific to
// an application's origins and its use of inline script, and a wrong one
// breaks the UI in ways that look like a templating bug. Applications
// that have one set it in a handler or their own middleware, which runs
// after this and wins.
func (a *App) secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		next.ServeHTTP(w, r)
	})
}

// requestID assigns the correlation ID and echoes it in a response
// header, so a user reporting a failure can quote something that maps to
// a log line.
//
// The header is set from inside the chain rather than before entering it:
// the ID only exists in the context once the assigning middleware has
// run, and reading it beforehand yields the empty string, which is a very
// quiet way to end up with every log line and every response header
// saying nothing.
func requestID(next http.Handler) http.Handler {
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(requestIDHeader, middleware.GetReqID(r.Context()))
		next.ServeHTTP(w, r)
	})
	return middleware.RequestID(echo)
}
