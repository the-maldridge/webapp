package webapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
)

// HandlerErr is a handler that reports failure by returning an error.
//
// It exists because the alternative is the per-branch "log it, set a
// status, render the internal error template, remember to return" block
// that accumulates in this style of application and, more often than
// not, gets one of the four steps wrong. With a returned error the
// framework owns the status code, the log line, and the disclosure
// policy.
//
// Register one with the Err-suffixed router methods, or wrap it where an
// http.Handler is required:
//
//	r.Get("/{id}", a.WrapErr(func(w http.ResponseWriter, r *http.Request) error {
//		task, err := a.store.Get(r.Context(), webapp.ParamUint(r, "id"))
//		if err != nil {
//			return err
//		}
//		a.View(w, r, "task.p2", webapp.Context{"task": task})
//		return nil
//	}))
type HandlerErr func(http.ResponseWriter, *http.Request) error

// ErrNotFound reports that a record a handler needed does not exist.
// Returning it from a HandlerErr produces a 404 rather than a 500, which
// matters because "no such task" and "database unreachable" must not
// look the same to a client or to whoever is reading logs.
var ErrNotFound = errors.New("webapp: not found")

// Rejected is a refusal of the user's input, not a server failure. It
// carries what to re-render so that a form keeps what was typed and
// shows why it was refused.
//
// Rejected is deliberately a distinct type rather than a status code on
// a generic error: the log level, the status, and whether the response
// is a render or an error page all differ, and getting that from
// errors.As keeps handlers free of the decision.
type Rejected struct {
	// Template is the template to re-render, usually the form that was
	// submitted. Empty means there is nothing to re-render and the
	// reasons are reported as a plain error.
	Template string

	// Values are the submitted values, made available to the re-rendered
	// template so the user is not asked to type everything again.
	Values Context

	// Reasons are human-readable, per-field or whole-form explanations.
	Reasons []string

	// Status is the response code. Zero means 422.
	Status int
}

func (r *Rejected) Error() string {
	if len(r.Reasons) == 0 {
		return "submission rejected"
	}
	return strings.Join(r.Reasons, "; ")
}

// AsRejected returns the rejection carried by err, if any.
func AsRejected(err error) (*Rejected, bool) {
	var rej *Rejected
	if errors.As(err, &rej) {
		return rej, true
	}
	return nil, false
}

// dispatchError classifies an error from a HandlerErr and turns it into
// a response. A rejection is a re-render; everything else is a failure at
// the given status.
func (a *App) dispatchError(w http.ResponseWriter, r *http.Request, err error) {
	if rej, ok := AsRejected(err); ok {
		status := rej.Status
		if status == 0 {
			status = http.StatusUnprocessableEntity
		}
		if rej.Template == "" {
			a.Fail(w, r, status, err)
			return
		}

		// The re-render must not succeed with a 200: a client that
		// posted invalid input has to see the 4xx even though it gets
		// a page. That requires the status to be set before the body,
		// which is why the render funnel takes an explicit status.
		ctx := Context{}
		for k, v := range rej.Values {
			ctx[k] = v
		}
		ctx["rejected"] = rej.Reasons
		a.renderWithStatus(w, r, status, rej.Template, ctx)
		return
	}

	switch {
	case errors.Is(err, ErrNotFound):
		a.Fail(w, r, http.StatusNotFound, err)
	case isBodyTooLarge(err):
		// Checked before the bad-input case on purpose: parsing a form
		// that was too large produces an input error wrapping a body-size
		// error, and the cause is the more accurate answer. "Too large" is
		// actionable in a way that "a field was missing" is not.
		a.Fail(w, r, http.StatusRequestEntityTooLarge, err)
	case ErrBadInput(err):
		// The client sent something unusable. That is a 400 and a warn at
		// most; recording it as a server failure buries the lines that
		// describe the application actually being broken.
		a.Fail(w, r, http.StatusBadRequest, err)
	default:
		a.Fail(w, r, http.StatusInternalServerError, err)
	}
}

// isBodyTooLarge reports whether an error came from a request body that
// exceeded the configured limit.
//
// The check is an errors.As on the concrete type rather than errors.Is on
// a sentinel, because net/http has no exported sentinel for it: reading
// past an http.MaxBytesReader's limit yields a *http.MaxBytesError, and a
// form whose Content-Length is already too large yields the same type from
// ParseForm. Both are the client oversized, and both are a 413.
func isBodyTooLarge(err error) bool {
	var maxBytes *http.MaxBytesError
	return errors.As(err, &maxBytes)
}

// View renders a template with status 200. It is the only render
// entrypoint, which is what lets ambient context reach every page.
func (a *App) View(w http.ResponseWriter, r *http.Request, name string, ctx Context) {
	a.renderWithStatus(w, r, http.StatusOK, name, ctx)
}

// renderWithStatus is the single funnel for successful renders and for
// re-rendered rejections.
func (a *App) renderWithStatus(w http.ResponseWriter, r *http.Request, status int, name string, ctx Context) {
	data := a.baseContext(r)
	for k, v := range ctx {
		data[k] = v
	}
	data = sanitize(data, 0)

	if a.Renderer == nil {
		// A rejected submission still has a status worth reporting even
		// with no templating installed, so the render failure inherits it
		// rather than downgrading everything to 500.
		if status < 400 {
			status = http.StatusInternalServerError
		}
		a.Fail(w, r, status,
			fmt.Errorf("cannot render %q: no template renderer is installed", name))
		return
	}

	body := w.Header()
	body.Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)

	if err := a.Renderer.Render(r.Context(), w, name, data); err != nil {
		// Headers are already out, so the status cannot change. Log the
		// real cause and stop; whatever partial bytes reached the client
		// are the client's problem, and a template error is never
		// rendered into the page.
		a.log().ErrorContext(r.Context(), "template execution failed",
			"request_id", a.requestID(r),
			"template", name,
			"error", err,
		)
	}
}

// Fail is the single error exit. It always sets an accurate status code,
// which is the part such applications get wrong most often: an error page
// served with 200 tells a monitoring system, a browser extension, and an
// API client that the request succeeded.
//
// Detail policy: the error text reaches the client only in debug mode.
// Otherwise the client sees a generic page and the correlation ID, which
// matches a log line that does have the detail. Error text routinely
// contains table names, file paths, and connection strings, and it is
// frequently untrusted input echoed back.
func (a *App) Fail(w http.ResponseWriter, r *http.Request, status int, err error) {
	if status == 0 {
		status = http.StatusInternalServerError
	}
	id := a.requestID(r)

	lvl := levelForStatus(status)
	a.log().Log(r.Context(), lvl, "request failed",
		"request_id", id,
		"method", r.Method,
		"path", r.URL.Path,
		"status", status,
		"error", err,
	)

	if started(w) {
		// The handler wrote a status or a body before returning an
		// error. Nothing can be reported honestly at this point other
		// than the log line above, and writing again would corrupt the
		// response.
		return
	}

	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		enc := json.NewEncoder(w)
		payload := map[string]any{"status": status, "requestId": id}
		if a.Config.Debug {
			payload["error"] = errString(err)
		}
		if encErr := enc.Encode(payload); encErr != nil {
			a.log().ErrorContext(r.Context(), "could not write error response",
				"request_id", id, "error", encErr)
		}
		return
	}

	if a.errorPage != nil {
		a.errorPage(w, r, status, err, id)
		return
	}
	a.defaultErrorPage(w, r, status, err, id)
}

// SetErrorPage replaces the function that renders error pages. It exists
// so that the templating layer, and an application with a custom error
// design, can take over without the core importing either. Passing nil
// restores the built-in page.
func (a *App) SetErrorPage(fn func(w http.ResponseWriter, r *http.Request, status int, err error, id string)) {
	a.errorPage = fn
}

// defaultErrorPage renders a minimal, dependency-free error page. It is
// what runs before the templating layer installs its own renderer, and
// what the templating layer falls back to if rendering itself fails.
//
// Every dynamic value is escaped. An error string can contain the
// contents of a request body, and a page that echoes it unescaped is a
// reflected XSS reachable by any attacker who can make a handler fail.
func (a *App) defaultErrorPage(w http.ResponseWriter, r *http.Request, status int, err error, id string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)

	fmt.Fprintf(w, "<!doctype html><html><head><title>%d %s</title></head><body>",
		status, html.EscapeString(http.StatusText(status)))
	fmt.Fprintf(w, "<h1>%d %s</h1>", status, html.EscapeString(http.StatusText(status)))

	if a.Config.Debug && err != nil {
		fmt.Fprintf(w, "<pre>%s</pre>", html.EscapeString(err.Error()))
	}
	if id != "" {
		fmt.Fprintf(w, "<p>Reference: %s</p>", html.EscapeString(id))
	}
	fmt.Fprint(w, "</body></html>")
}

// SeeOther answers a successful mutation with 303, which is the correct
// way to turn a POST into a redirect: 303 tells the client to follow with
// GET, so reloading the page cannot repeat the mutation.
//
// The target is expected to be a path the application generated. Echoing
// user-supplied values into it makes an open redirect, so callers who
// take a destination from a form must validate it against a allowlist
// first.
func (a *App) SeeOther(w http.ResponseWriter, r *http.Request, url string) {
	w.Header().Set("Location", url)
	w.WriteHeader(http.StatusSeeOther)
}

// levelForStatus maps a status code to the log level that code deserves.
func levelForStatus(status int) slog.Level {
	switch {
	case status >= 500:
		return slog.LevelError
	case status >= 400:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// wantsJSON reports whether the client's stated preference is JSON.
//
// The first recognized media type in the Accept header wins, which is
// preference order rather than a substring test, and it is what makes the
// browser case work without special-casing browsers: a browser lists
// text/html first, so it gets a page even though its Accept also mentions
// json, and an API client that lists json first asked for what it got.
func wantsJSON(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if accept == "" {
		return false
	}
	for _, part := range strings.Split(accept, ",") {
		media := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		switch media {
		case "text/html":
			return false
		case "application/json", "+json":
			return true
		default:
			if strings.HasSuffix(media, "+json") {
				return true
			}
		}
	}
	return false
}

// errString returns a non-nil error's text, or the empty string.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// maxSanitizeDepth bounds the walk that converts errors to strings. Deep
// structures in a render context are a design smell, and an unbounded
// walk over a cyclic one is a hang.
const maxSanitizeDepth = 8

// sanitize converts error values found in a render context into strings.
//
// Templating engines autoescape strings but call String methods on other
// values and insert the result as-is, so an error that quotes a request
// body can smuggle markup into a page. Errors reach contexts routinely,
// because reporting why something failed is exactly what a page does.
func sanitize(ctx Context, depth int) Context {
	if depth > maxSanitizeDepth {
		return ctx
	}
	for k, v := range ctx {
		if s, ok := sanitizeValue(v, depth); ok {
			ctx[k] = s
		}
	}
	return ctx
}

// sanitizeValue returns v replaced by an escaped-safe equivalent, and
// whether a replacement happened.
func sanitizeValue(v any, depth int) (any, bool) {
	switch t := v.(type) {
	case error:
		return errString(t), true
	case Context:
		return sanitize(t, depth+1), true
	case map[string]any:
		return sanitize(Context(t), depth+1), true
	case []any:
		for i, elem := range t {
			if s, ok := sanitizeValue(elem, depth+1); ok {
				t[i] = s
			}
		}
		return t, true
	}

	// A nil pointer or interface in a context is common enough that it
	// should not be reported as an error value.
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice:
		if rv.IsNil() {
			return nil, true
		}
	}
	return nil, false
}
