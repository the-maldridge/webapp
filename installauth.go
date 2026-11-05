package webapp

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/the-maldridge/webapp/auth"
)

// Auth wiring.
//
// The auth package produces identities and denials and renders nothing: it
// does not know whether the client wants a page or JSON, it does not own the
// error templates, and it must not import this package to find out. The
// dependency points inward, so everything that requires knowledge of the
// framework — content negotiation, the render funnel, the shared template
// context — lives in this file.

// InstallAuth connects an authentication service to the application.
//
// It splices the authentication, CSRF, and profile middleware into the
// already-installed chain immediately after the security headers, installs the
// denial renderer, and shares the current user and the CSRF input with
// templates.
//
// It is a method rather than a construction option because a Service needs a
// database handle, and that handle does not exist until New has opened it. The
// ordering is not incidental: the auth tables have to be part of the schema
// decided before the first request, which is what RegisterComponent and
// Service.Models exist for.
//
// Call it before registering routes. Authentication runs before routing either
// way, but a route registered afterwards cannot be reasoned about by reading
// the route table, which is the property the guards exist to provide.
func (a *App) InstallAuth(svc *auth.Service) error {
	if svc == nil {
		return errors.New("webapp: InstallAuth requires a service")
	}
	if a.auth != nil {
		return errors.New("webapp: InstallAuth called twice; an application has one identity")
	}

	// A generated session key means nobody configured one. In development
	// that is convenient; in production it means every restart signs everyone
	// out and every replica accepts only the cookies it issued itself, both of
	// which present as a network problem to whoever is debugging it.
	if a.Config.Production && !svc.SessionKeyConfigured() {
		return fmt.Errorf(
			"webapp: refusing to start in production without %s: a per-process session key "+
				"log everyone out on restart and makes each replica its own authority",
			auth.EnvSessionKey)
	}

	// The cookie name carries the environment prefix so that two applications
	// on one host cannot read each other's sessions, and Secure follows the
	// configuration, which is what makes a plain-HTTP deployment behind a
	// TLS-terminating proxy an explicit choice rather than an accident.
	policy := auth.CookiePolicy{
		Name:     prefixCookieName(a.Config.EnvPrefix, auth.DefaultCookieName),
		Secure:   a.Config.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		Lifetime: svc.SessionLifetime(),
	}
	if err := svc.SetCookiePolicy(policy); err != nil {
		return fmt.Errorf("webapp: %w", err)
	}
	if err := svc.SetDenialHandler(a.renderDenial); err != nil {
		return fmt.Errorf("webapp: %w", err)
	}

	a.auth = svc

	// Order within the spliced group matters. Authentication first, so that
	// the CSRF check can bind a token to the identity this request was
	// authenticated as rather than resolving the credential a second time;
	// profile last, because provisioning a row needs the identity the first
	// one established.
	a.Use(
		svc.Middleware(),
		svc.CSRFMiddleware(),
		svc.ProfileMiddleware(),
	)

	// Templates read the current user and the CSRF field. Both are injected
	// rather than passed by handlers, because a page that forgot to pass the
	// user renders as anonymous, and a page that forgot the CSRF token fails
	// on submit — two failures that only surface on the pages nobody clicked
	// during review.
	a.inject(func(r *http.Request, ctx Context) {
		user, ok := auth.UserFrom(r)
		if !ok {
			ctx["user"] = nil
			ctx["isAuthenticated"] = false
		} else {
			ctx["user"] = user
			ctx["isAuthenticated"] = true
		}

		// The markup is escaped inside auth and wrapped here, once. The renderer
		// recognises template.HTML at the context boundary and marks the value
		// safe, so templates write {{ csrfInput }} with no |safe filter and no
		// possibility of a token carrying markup.
		ctx["csrfInput"] = template.HTML(auth.CSRFInput(r))
		ctx["csrfToken"] = auth.CSRF(r)
	})

	return nil
}

// AuthService returns the installed authentication service, or nil when the
// application has none.
func (a *App) AuthService() *auth.Service { return a.auth }

// Use appends middleware to the application's chain.
//
// Appended middleware runs after the security headers and before the router,
// which is the only position that can establish an identity a route guard can
// read. It cannot be expressed with chi's Use: that panics once a route exists,
// and a middleware installed on the router does not run for the request
// logging, correlation ID, or security headers this chain owns.
//
// Order among the appended entries is the order given.
func (a *App) Use(middleware ...func(http.Handler) http.Handler) {
	for _, m := range middleware {
		if m == nil {
			continue
		}
		a.extensions = append(a.extensions, m)
	}
	a.rebuildChain()
}

// RequireLogin returns middleware that refuses anonymous requests. Without an
// installed service it refuses everything: an application that asked for a
// login gate it does not have has a configuration bug, and serving the route
// anyway is the wrong direction for that to fail in.
func (a *App) RequireLogin() func(http.Handler) http.Handler {
	return a.Guard(auth.Perm{})
}

// Guard returns middleware that enforces a permission.
//
// The service is resolved when a request arrives, not when the guard is built.
// Authentication is installed after construction, because a service needs a
// database handle and the handle does not exist until New has opened one, so a
// guard that captured the service at registration would capture its absence for
// every route registered during construction, which is every route a module
// registers, and answer 500 forever on an application that has one.
func (a *App) Guard(p auth.Perm) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc := a.AuthService()
			if svc == nil {
				denyConfiguration(w, r)
				return
			}
			svc.Require(p)(next).ServeHTTP(w, r)
		})
	}
}

// Guarded applies the application's permission check to a single handler and
// returns a handler, which is what a route registration wants. Like Guard, it
// denies rather than passing through when no service is installed.
func (a *App) Guarded(p auth.Perm, h http.HandlerFunc) http.HandlerFunc {
	return a.Guard(p)(h).ServeHTTP
}

// Public returns the handler unchanged, under a name that records the decision.
// Paired with an application that gates by default, it makes every route state
// whether it needs an identity.
func (a *App) Public(h http.HandlerFunc) http.HandlerFunc { return h }

// denyConfiguration is the gate when no service is installed: a 500 rather than
// a pass. The status is a server error because that is what it is — the
// application is misconfigured — and the alternative is protected routes that
// are quietly open depending on which call site forgot what.
func denyConfiguration(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// permDenial is a refusal that names the permission it needed.
//
// It is a type rather than a sentence in an error because the error page has to
// state the grant, and reading one back out of formatted text means the page
// breaks the moment the wording does. Wrapping costs nothing and makes the
// permission available to whoever renders it.
type permDenial struct {
	msg  string
	perm auth.Perm
}

func (e *permDenial) Error() string { return e.msg }

// renderDenial answers a refusal produced by the auth package.
//
// Content negotiation is the core's job: an API caller that receives an HTML
// login page, or anything at all that receives status 200, believes the write
// succeeded. So the status is correct first, JSON goes to clients that asked
// for JSON, and everyone else gets the error page with the required permission
// named.
func (a *App) renderDenial(w http.ResponseWriter, r *http.Request, d auth.Denial) {
	status := d.Kind.Status()

	// 401 carries a challenge so a client that can authenticate by header
	// knows it may retry. Without it, a correct credential in the wrong shape
	// produces a response no client can act on.
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="restricted", charset="UTF-8"`)
	}

	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		body := `{"error":"` + jsonEscapeString(d.Error()) + `"`
		if d.Kind == auth.DenialForbidden && d.Perm.Module != "" {
			body += `,"required":"` + jsonEscapeString(d.Perm.String()) + `"`
		}
		if id := a.requestID(r); id != "" {
			body += `,"requestId":"` + jsonEscapeString(id) + `"`
		}
		body += "}\n"
		if _, err := w.Write([]byte(body)); err != nil {
			a.log().ErrorContext(r.Context(), "could not write denial response",
				"request_id", a.requestID(r), "error", err)
		}
		return
	}

	// The page names the required permission deliberately: telling a signed-in
	// user which grant they need is support-free, and the identity asking
	// already knows it is asking.
	err := errors.New(d.Error())
	if d.Kind == auth.DenialForbidden && d.Perm.Module != "" {
		err = &permDenial{msg: fmt.Sprintf("%s: requires %s", d.Error(), d.Perm), perm: d.Perm}
	}
	a.Fail(w, r, status, err)
}

// prefixCookieName derives a cookie name from the environment prefix so that
// two applications sharing a host do not share a session.
func prefixCookieName(prefix, name string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" || prefix == DefaultEnvPrefix {
		return name
	}
	return strings.ToLower(prefix) + "_" + name
}

// jsonEscapeString escapes a string for a JSON string literal, including the
// HTML-significant characters as escapes so a body cannot be smuggled into an
// HTML page by a client that asked for JSON and got its input back.
func jsonEscapeString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '<':
			b.WriteString(`\u003c`)
		case '>':
			b.WriteString(`\u003e`)
		case '&':
			b.WriteString(`\u0026`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}
