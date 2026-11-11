package auth

import (
	"errors"
	"net/http"
	"strings"
)

// DenialKind classifies a refusal well enough for the response layer to pick
// a status code and a message without knowing anything about how the request
// was authenticated.
type DenialKind int

const (
	// DenialNoUser means no identity could be established. Outward: 401.
	DenialNoUser DenialKind = iota

	// DenialForbidden means an identity was established and may not do this.
	// Outward: 403.
	DenialForbidden

	// DenialCSRF means a state-changing request did not carry a token that
	// matched its session. Outward: 403.
	DenialCSRF
)

// String names the kind for logs and JSON bodies.
func (k DenialKind) String() string {
	switch k {
	case DenialNoUser:
		return "no_identity"
	case DenialForbidden:
		return "forbidden"
	case DenialCSRF:
		return "csrf"
	default:
		return "denied"
	}
}

// Status returns the HTTP status the kind maps to.
//
// CSRF is a 403 rather than a 400. A forged request is not malformed input:
// every field in it is well formed, and the reason it is refused is
// authorization. A 400 also invites re-rendering the form back at whoever
// submitted it, which is a cheap way to learn what the server expected.
func (k DenialKind) Status() int {
	switch k {
	case DenialNoUser:
		return http.StatusUnauthorized
	case DenialForbidden, DenialCSRF:
		return http.StatusForbidden
	default:
		return http.StatusForbidden
	}
}

// Denial is a refusal to serve a request.
//
// The auth package produces denials and never renders them. It has no idea
// whether the client wants a page or JSON, does not own the error templates,
// and must not import the web core to find out; the core installs a
// DenialHandler that knows all three. This is also why the guards here return
// plain middleware functions rather than framework types: the dependency
// points inward, and the seam is this one struct.
type Denial struct {
	// Kind is what sort of refusal this is.
	Kind DenialKind

	// Perm is the permission that was required, when the refusal was about a
	// permission. It is reported to the client on purpose: "you need
	// TASKS:ADMIN" is far more useful than "forbidden", and the identity that
	// would need it is already known to whoever is asking.
	Perm Perm

	// HasPerm records what the caller held, for logs. It is never rendered.
	HasPerm []string
}

// Error lets a Denial be returned from a handler that reports failures as
// errors, so a guard check does not need a second error channel.
func (d Denial) Error() string {
	switch d.Kind {
	case DenialNoUser:
		return "authentication required"
	case DenialCSRF:
		return "cross-site request forgery check failed"
	default:
		if d.Perm.Module == "" {
			return "permission denied"
		}
		return "permission denied: " + d.Perm.String()
	}
}

// DenialHandler renders a denial. The web core supplies one that negotiates
// between a page and a JSON body; the default here writes JSON, which is
// never pretty and always correct about the status code.
type DenialHandler func(http.ResponseWriter, *http.Request, Denial)

// defaultDenialHandler answers with a minimal JSON body.
//
// A 401 gets WWW-Authenticate so a client that can authenticate by header
// knows it may try, which is the difference between a client that loops
// correctly and one that gives up. A client whose first stated preference is
// HTML does not get one, because the browser that receives the challenge
// paints its own dialog over the response body instead of showing it; a
// browser gets a page. The condition is the client's Accept header rather
// than any notion of which credential mechanisms are installed, because what
// a client can do with a challenge is decided by what it is, and the only
// thing a server gets to know about that is what it asked for.
func defaultDenialHandler(w http.ResponseWriter, r *http.Request, d Denial) {
	body := `{"error":"` + jsonEscape(d.Error()) + `"`
	if d.Kind == DenialForbidden && d.Perm.Module != "" {
		body += `,"required":"` + jsonEscape(d.Perm.String()) + `"`
	}
	body += "}\n"

	header := w.Header()
	header.Set("Content-Type", "application/json; charset=utf-8")
	header.Set("Cache-Control", "no-store")
	if d.Kind == DenialNoUser && !wantsHTML(r) {
		header.Set("WWW-Authenticate", `Basic realm="restricted", charset="UTF-8"`)
	}
	w.WriteHeader(d.Kind.Status())
	_, _ = w.Write([]byte(body))
}

// wantsHTML reports whether the client's first stated preference is an HTML
// page. It is a deliberately small reader of Accept: the first media type
// with a quality, or none, decides, and anything that is not HTML or
// xhtml is "not a browser as far as this knows". An absent or empty header
// means no stated preference, which is not a request for a page.
//
// The web core answers the same question for itself; this package cannot
// import it, and duplicating nine lines is the cheaper price for an auth
// package with no dependency on a web framework.
func wantsHTML(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		media := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		switch media {
		case "":
			continue
		case "text/html", "application/xhtml+xml":
			return true
		default:
			return false
		}
	}
	return false
}

// jsonEscape escapes a string for embedding in a JSON string literal.
//
// Encoding/json would be the general answer; this is here because the denial
// body is assembled as text and the set of characters that can appear in a
// denial message is small and known. Anything outside ASCII is replaced rather
// than passed through.
func jsonEscape(s string) string {
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
		default:
			if r < 0x20 {
				b.WriteString(`\u001f`)
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ErrDenied reports that a guard refused a request. Callers rarely see it: a
// guard that denies has already produced a response. It exists so that
// middleware which wants to handle denials itself can detect one.
var ErrDenied = errors.New("auth: request denied")

// Middleware authenticates requests on a best-effort basis and continues
// either way.
//
// Anonymous is a valid outcome here: which routes require an identity is a
// decision made per route by RequireLogin and Require, not globally, because
// a global gate cannot express a login page, a password reset form, or a
// health endpoint. Because it runs before routing, the identity is available
// to the router, to the CSRF check, and to every handler.
func (s *Service) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			info := s.resolve(r)
			ctx := r.Context()
			if info.Via != ViaNone {
				ctx = ContextWithUser(ctx, info.User)
			}
			ctx = contextWithInfo(ctx, info)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Optional is Middleware under a name that says what it means at a call site
// where the alternative is a required gate.
func (s *Service) Optional() func(http.Handler) http.Handler { return s.Middleware() }

// RequireLogin returns middleware that refuses anonymous requests.
func (s *Service) RequireLogin() func(http.Handler) http.Handler {
	return s.Require(Perm{})
}

// Require returns middleware that refuses a request whose identity does not
// hold p.
//
// An empty Perm behaves like RequireLogin: "anybody, as long as we know who".
// Passing the zero value by accident therefore degrades to the less
// privileged outcome rather than the more privileged one, which is the
// direction an authorization bug should fail in.
//
// Anonymous requests get 401, not 403. The distinction is not pedantry: 401
// tells a client with a header credential that it may retry with it, which is
// why the default denial body carries WWW-Authenticate.
func (s *Service) Require(p Perm) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			info := s.Info(r)
			if info.Via == ViaNone || info.User.Identity == "" {
				s.deny(w, r, Denial{Kind: DenialNoUser, Perm: p})
				return
			}
			if p.Module == "" {
				next.ServeHTTP(w, r)
				return
			}
			// The session's groups are what is checked, deliberately: reading
			// them from the store on every request would make group revocation
			// take effect immediately, and would make every page load a
			// database round trip on a path where the answer is already in the
			// cookie. Service.HasPerm is the escape hatch for the decisions
			// that cannot wait for a session to expire.
			if !holds(info.User.Groups, p) {
				s.deny(w, r, Denial{Kind: DenialForbidden, Perm: p, HasPerm: groupNames(info.User.Groups)})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// groupNames returns a group set as a sorted-name slice for logging. Sorted so
// that two identical permission sets produce identical log lines.
func groupNames(groups map[string]struct{}) []string {
	if len(groups) == 0 {
		return nil
	}
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	// Insertion sort: group sets are tiny, and this keeps the helper free of
	// an import for one call.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}

// Info returns the authentication outcome for a request: who it was
// authenticated as, and by what.
//
// It is exported because "did this request authenticate by cookie or by
// header?" is a question applications legitimately ask — an application
// printing an API token should not be able to do it with a stolen session
// cookie's CSRF token attached, and the reverse — and because it degrades to a
// fresh resolution rather than failing when the authentication middleware was
// not installed.
func (s *Service) Info(r *http.Request) AuthInfo {
	if r == nil {
		return AuthInfo{}
	}
	if info, ok := infoFromContext(r.Context()); ok {
		return info
	}
	return s.resolve(r)
}

// User returns the authenticated user for a request, and false when the
// request is anonymous.
func (s *Service) User(r *http.Request) (User, bool) {
	info := s.Info(r)
	return info.User, info.Via != ViaNone
}
