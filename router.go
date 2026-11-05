package webapp

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// Router is the routing surface an application sees. It mirrors chi,
// with patterns and parameter names using chi's syntax, so that
// documentation for either applies, and it wraps a chi router rather
// than embedding one: chi's own Route takes a callback over
// chi.Router, and an application should never receive that type.
//
// Raw chi is never far away. Mount attaches any http.Handler, including
// a *chi.Mux built by hand, for the case where the wrapper genuinely
// does not cover something.
type Router struct {
	inner chi.Router
	app   *App
}

// Use appends middleware that applies to routes registered after this
// call on this router, and to sub-routers created from it.
//
// This is how authentication is expressed structurally: a subtree
// registered after a gate middleware cannot be reached without passing
// it, which is a stronger guarantee than a check inside each handler
// that one handler may simply not have.
func (r Router) Use(middleware ...func(http.Handler) http.Handler) {
	r.inner.Use(middleware...)
}

// With returns a router that adds middleware for a single registration.
func (r Router) With(middleware ...func(http.Handler) http.Handler) Router {
	return Router{inner: r.inner.With(middleware...), app: r.app}
}

// Group creates an inline router with a fresh middleware stack sharing
// the parent's routing pattern.
func (r Router) Group(fn func(r Router)) Router {
	return Router{inner: r.inner.Group(func(cr chi.Router) { fn(Router{inner: cr, app: r.app}) }), app: r.app}
}

// Route mounts a sub-router at pattern.
func (r Router) Route(pattern string, fn func(r Router)) {
	r.inner.Route(pattern, func(cr chi.Router) { fn(Router{inner: cr, app: r.app}) })
}

// Mount attaches an arbitrary handler below pattern. Trailing-path
// semantics are chi's: the mounted handler sees the remainder of the
// path, available as RoutePattern's wildcard.
func (r Router) Mount(pattern string, h http.Handler) {
	r.inner.Mount(pattern, h)
}

// Handle registers a handler for every method at pattern.
func (r Router) Handle(pattern string, h http.Handler) {
	r.inner.Handle(pattern, h)
}

// HandleErr registers an error-returning handler for every method.
func (r Router) HandleErr(pattern string, h HandlerErr) {
	r.inner.Handle(pattern, r.app.WrapErr(h))
}

// Method registers a handler for one method at pattern.
func (r Router) Method(method, pattern string, h http.Handler) {
	r.inner.Method(method, pattern, h)
}

// MethodErr registers an error-returning handler for one method.
func (r Router) MethodErr(method, pattern string, h HandlerErr) {
	r.inner.Method(method, pattern, r.app.WrapErr(h))
}

// Get registers a handler for GET.
func (r Router) Get(pattern string, h http.HandlerFunc) { r.inner.Get(pattern, h) }

// GetErr registers an error-returning handler for GET.
func (r Router) GetErr(pattern string, h HandlerErr) { r.inner.Get(pattern, r.app.WrapErr(h)) }

// Post registers a handler for POST.
func (r Router) Post(pattern string, h http.HandlerFunc) { r.inner.Post(pattern, h) }

// PostErr registers an error-returning handler for POST.
func (r Router) PostErr(pattern string, h HandlerErr) { r.inner.Post(pattern, r.app.WrapErr(h)) }

// Put registers a handler for PUT.
func (r Router) Put(pattern string, h http.HandlerFunc) { r.inner.Put(pattern, h) }

// Patch registers a handler for PATCH.
func (r Router) Patch(pattern string, h http.HandlerFunc) { r.inner.Patch(pattern, h) }

// Delete registers a handler for DELETE.
func (r Router) Delete(pattern string, h http.HandlerFunc) { r.inner.Delete(pattern, h) }

// Head registers a handler for HEAD.
func (r Router) Head(pattern string, h http.HandlerFunc) { r.inner.Head(pattern, h) }

// Options registers a handler for OPTIONS.
func (r Router) Options(pattern string, h http.HandlerFunc) { r.inner.Options(pattern, h) }

// NotFound replaces the application-wide not-found handler.
func (r Router) NotFound(h http.HandlerFunc) { r.inner.NotFound(h) }

// MethodNotAllowed replaces the application-wide method-not-allowed
// handler.
func (r Router) MethodNotAllowed(h http.HandlerFunc) { r.inner.MethodNotAllowed(h) }

// RoutePattern returns the registered pattern that matched the request,
// for example "/task/{id}". This is what lets a nav fragment highlight
// a section, or a form post to its own URL, without comparing paths by
// hand. It is empty outside a matched route, notably in middleware that
// runs before routing.
func RoutePattern(r *http.Request) string {
	if x := chi.RouteContext(r.Context()); x != nil {
		return x.RoutePattern()
	}
	return ""
}

// URLParam returns a named path parameter, or the empty string when the
// name is not part of the matched pattern. Handlers that need to tell a
// missing parameter apart from an empty one should use the typed helpers
// in params.go, which report parse errors instead.
func URLParam(r *http.Request, name string) string {
	return chi.URLParam(r, name)
}
