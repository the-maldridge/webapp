package auth

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Field names the login form is read with. They are constants rather than
// parameters because the framework renders the form, so there is exactly one
// producer and one consumer, and a mismatch between them is a login form that
// does nothing.
const (
	UsernameField = "username"
	PasswordField = "password"
	NextField     = "next"
)

// ErrLoginRejected reports that a login attempt failed. It carries no detail:
// the distinction between an unknown identity and a wrong password exists
// internally, in the sentinels Authenticate aggregates, and never reaches a
// response.
var ErrLoginRejected = errors.New("auth: sign-in failed")

// Issue writes a session cookie for a user.
//
// It is exported for the one flow that needs it besides the login handler:
// an application that authenticates someone some other way — a single sign-on
// redirect, a device-code exchange — still has to end up with the same cookie,
// and a second implementation of that always drifts from the first on an
// attribute that matters.
func (s *Service) Issue(w http.ResponseWriter, u User) error {
	if u.Identity == "" {
		return errors.New("auth: cannot issue a session without an identity")
	}

	u.Identity = normalizeIdentity(u.Identity)
	if u.Groups == nil {
		u.Groups = map[string]struct{}{}
	}

	expires := s.now().Add(s.policy.Lifetime)
	value, err := s.codec.seal(Session{Expires: expires, User: u})
	if err != nil {
		return err
	}
	writeSession(w, s.cookieOptions(), value, expires)
	return nil
}

// ClearSession expires the session and CSRF cookies.
//
// Both are cleared. A stale CSRF token is harmless on its own, but leaving it
// means the next anonymous form on the same browser carries a value bound to
// the identity that just signed out, and "harmless given how the check works"
// is a claim that has to stay true forever to remain true.
func (s *Service) ClearSession(w http.ResponseWriter) {
	clearSession(w, s.cookieOptions())
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    "",
		Path:     "/",
		Domain:   s.policy.Domain,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: false,
		Secure:   s.policy.Secure,
		SameSite: s.policy.SameSite,
	})
}

// LoginOptions configure the login submit handler. Every field is optional;
// the defaults implement the ordinary case of "verify, set a cookie, redirect
// to where the user was going".
type LoginOptions struct {
	// UsernameField, PasswordField, and NextField override the form field
	// names. Empty uses the package names.
	UsernameField string
	PasswordField string
	NextField     string

	// Fallback is where to send a successful login that did not name a
	// destination. Empty means "/".
	Fallback string

	// OnSuccess replaces the default behaviour of issuing a session and
	// redirecting. Set it to render something instead, or to record the
	// login. When it is set the handler does not issue a cookie, so an
	// application that wants both calls Issue from here.
	OnSuccess func(w http.ResponseWriter, r *http.Request, u User, next string)

	// OnFailure replaces the default 401-with-no-body. Set it to re-render
	// the login form; the error it receives is ErrLoginRejected, always that
	// one value, for every reason including "no such account".
	OnFailure func(w http.ResponseWriter, r *http.Request, next string, err error)
}

// LoginHandler returns a handler that verifies submitted credentials.
//
// It only handles POST. A login form that accepts GET puts the credential in
// a URL, and URLs end up in access logs, browser history, and the Referer
// header of whatever the page links to next, so the shape of the endpoint is
// part of what it protects.
//
// Every failure produces the same response, whether the identity does not
// exist, the password was wrong, the account is disabled, or the fields were
// empty. A login form that explains which of those happened is an account
// enumeration service that costs nothing to query.
//
// Rate limiting is deliberately not here: the right policy depends on what
// the application is willing to lock — an address, an identity, a tenant —
// and a framework default for it is a guess that someone has to debug at 3am
// while locking out their own operators.
func (s *Service) LoginHandler(opts LoginOptions) http.HandlerFunc {
	userField := orDefault(opts.UsernameField, UsernameField)
	passField := orDefault(opts.PasswordField, PasswordField)
	nextField := orDefault(opts.NextField, NextField)
	fallback := orDefault(opts.Fallback, "/")

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			s.deny(w, r, Denial{Kind: DenialForbidden})
			return
		}

		if err := parseFormOrFail(s, r); err != nil {
			// The body was unreadable or oversized. Nothing was submitted, so
			// the generic refusal is accurate and reveals nothing.
			s.deny(w, r, Denial{Kind: DenialForbidden})
			return
		}

		identity := r.Form.Get(userField)
		password := r.Form.Get(passField)
		next := SafeNext(r.Form.Get(nextField), fallback)

		u, err := s.Authenticate(r.Context(), identity, password)
		if err != nil {
			s.logger.InfoContext(r.Context(), "auth: sign-in failed",
				"identity", normalizeIdentity(identity),
				"client", s.clientAddressForLog(r),
				"error", err)
			if opts.OnFailure != nil {
				opts.OnFailure(w, r, next, ErrLoginRejected)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		if opts.OnSuccess != nil {
			opts.OnSuccess(w, r, u, next)
			return
		}

		if err := s.Issue(w, u); err != nil {
			s.logger.ErrorContext(r.Context(), "auth: could not issue a session", "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		s.logger.InfoContext(r.Context(), "auth: signed in",
			"identity", u.Identity, "via", u.AuthedBy)
		redirectSeeOther(w, next)
	}
}

// LogoutHandler returns a handler that clears the session and redirects.
//
// Only POST reaches the application state. A logout reachable by GET can be
// triggered by an <img> tag, which is a nuisance attack rather than a serious
// one, but it is also the attack that gets mistaken for a session management
// bug while the real problem is that the application treats "signed out" as a
// state worth forcing on someone.
func (s *Service) LogoutHandler(redirectTo string) http.HandlerFunc {
	target := SafeNext(redirectTo, "/")
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			s.deny(w, r, Denial{Kind: DenialForbidden})
			return
		}
		s.ClearSession(w)
		redirectSeeOther(w, target)
	}
}

// CurrentProfileIdentity returns the identity to hang application data off.
// It is the authenticated identity, and only ever that: the request is the
// only source, which is what makes "who owns this row" unspoofable by a form
// field that says otherwise.
func CurrentProfileIdentity(r *http.Request) (string, error) {
	return MustIdentity(r)
}

// SafeNext returns a same-site redirect target, or fallback if raw is not one.
//
// An open redirect is the most dismissed vulnerability in this list and the
// one users actually get phished with: the link is genuinely on the real site,
// and the destination is whatever the attacker chose. So the accepted shape is
// narrow on purpose — a path beginning with exactly one slash — and everything
// else falls back rather than being negotiated with.
//
// The specific rejections each close something:
//
//   - "//host" and "\/\/host" are scheme-relative URLs; browsers complete them
//     with the current scheme and leave.
//   - a backslash anywhere is normalized to a slash by browsers, so "/\host"
//     is "//host" to a browser and to nothing else.
//   - "/%2F%2Fhost" decodes to a scheme-relative URL after the browser has
//     already decided the path was local, so the check runs on the decoded
//     form as well as the raw one.
//   - CR, LF, and NUL are header injection: a redirect target that survives
//     into a Location header must not be able to end the header line.
func SafeNext(raw, fallback string) string {
	if raw == "" {
		return fallback
	}
	if strings.ContainsAny(raw, "\r\n\x00\t") {
		return fallback
	}
	if strings.Contains(raw, "\\") {
		return fallback
	}
	if !strings.HasPrefix(raw, "/") {
		return fallback
	}
	if strings.HasPrefix(raw, "//") {
		return fallback
	}

	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return fallback
	}
	if strings.HasPrefix(decoded, "//") || strings.Contains(decoded, "://") {
		return fallback
	}
	if strings.ContainsAny(decoded, "\r\n\x00") {
		return fallback
	}
	return raw
}

// orDefault returns value or fallback when value is empty.
func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// redirectSeeOther issues a 303 to an already-sanitized target.
//
// 303 rather than 302 or 307: after a POST that created state, the client
// should fetch the destination with GET, and 303 is the status that says so
// without depending on client behaviour the older statuses leave ambiguous.
// The header is written directly rather than through http.Redirect because
// that helper resolves relative URLs against the request URL, and the target
// here was validated as a path a moment earlier; re-resolving it would put a
// browser-facing decision back in net/http's hands.
func redirectSeeOther(w http.ResponseWriter, target string) {
	h := w.Header()
	h.Set("Location", target)
	h.Set("Cache-Control", "no-store")
	h.Del("Content-Type")
	w.WriteHeader(http.StatusSeeOther)
}

// parseFormOrFail parses a request body the way the credential handlers need,
// distinguishing "no body" from "body could not be read".
func parseFormOrFail(s *Service, r *http.Request) error {
	if isMultipart(r) {
		return r.ParseMultipartForm(defaultMultipartMemory)
	}
	return r.ParseForm()
}

// clientAddressForLog returns the address to record with a failed sign-in.
//
// The framework's own client-IP resolution lives in the web core, which this
// package must not import. The connection address is the honest fallback: it
// cannot be forged, and an application that fronts itself with a proxy and
// wants the real address in these lines overrides it in its own denial or
// failure handler, where it already knows the proxy configuration.
func (s *Service) clientAddressForLog(r *http.Request) string {
	if r.RemoteAddr == "" {
		return "unknown"
	}
	return r.RemoteAddr
}
