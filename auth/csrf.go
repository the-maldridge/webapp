package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CSRF protection: signed double-submit.
//
// The scheme is two facts that must agree. A random value, signed and
// expiring, is stored in a cookie and echoed back by the page in a header or
// a form field. An attacker's cross-site request can cause the cookie to be
// sent — that is what browsers do — but cannot read it or produce a matching
// second value, so the pair fails to verify. No server-side state is
// involved, which matters because a server-side map would either be per
// process, breaking a horizontal fleet, or shared, requiring the storage
// layer this design does not otherwise need.
//
// The signature uses a key derived from the session key with a
// domain-separating label, never the session key itself; see deriveCSRFKey.

// csrfTokenLength is the entropy of a CSRF token's random half.
const csrfTokenLength = 32

// csrfSignatureSeparator divides a token's payload from its signature. It is
// not a character that appears in base64url output, so it cannot be mistaken
// for part of either half.
const csrfSignatureSeparator = "."

// csrfField is the form field a token is submitted in; csrfHeader is the
// header form. The cookie name lives with the other cookie names, in
// CookiePolicy.
const (
	csrfField       = "csrf_token"
	csrfHeader      = "X-CSRF-Token"
	csrfTokenPrefix = "v1"

	// csrfPayloadSep divides a token payload's fields. It must be a
	// character that is legal inside a cookie value and absent from
	// base64url output: the token travels in a cookie, and net/http silently
	// deletes bytes that are not, which turns a minted token into one that
	// can never verify again. A pipe is a valid HTTP token character, so the
	// value survives the cookie, the header, and a form field unchanged.
	csrfPayloadSep = "|"
)

// defaultMultipartMemory bounds how much of a multipart body is buffered in
// memory while a CSRF token is read out of it. Anything past it goes to a
// temporary file, which net/http removes when the request finishes. The value
// matches net/http's own default; the request body cap bounds the total.
const defaultMultipartMemory = 32 << 20

// newCSRFToken returns a signed token bound to an identity, valid for one
// session lifetime.
//
// The identity is inside the signed payload so that a token minted for one
// user cannot be replayed as another's. That is the residual risk of a plain
// double-submit: a value proving "this browser was served a page" proves
// nothing about which account is acting, and a token that travels between
// accounts turns a session-fixation attempt into a successful one.
func newCSRFToken(key []byte, identity string, expiresAt time.Time) (string, error) {
	buf := make([]byte, csrfTokenLength)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: could not read random bytes: %w", err)
	}
	nonce := base64.RawURLEncoding.EncodeToString(buf)
	payload := strings.Join([]string{
		csrfTokenPrefix,
		base64.RawURLEncoding.EncodeToString([]byte(identity)),
		nonce,
		strconv.FormatInt(expiresAt.Unix(), 10),
	}, csrfPayloadSep)

	return signCSRF(key, payload), nil
}

// signCSRF returns "payload.signature".
func signCSRF(key []byte, payload string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return payload + csrfSignatureSeparator + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// csrfTokenValid reports whether a token is well formed, correctly signed,
// bound to the given identity, and unexpired.
//
// Every failure is the same false. The difference between "tampered" and
// "expired" is a log detail; telling a client which one it hit helps nobody
// who is not already in a position to read logs.
func csrfTokenValid(key []byte, token, identity string, now time.Time) bool {
	payload, sig, found := strings.Cut(token, csrfSignatureSeparator)
	if !found || payload == "" || sig == "" {
		return false
	}

	want := signCSRF(key, payload)
	_, wantSig, _ := strings.Cut(want, csrfSignatureSeparator)
	if subtle.ConstantTimeCompare([]byte(sig), []byte(wantSig)) != 1 {
		return false
	}

	fields := strings.Split(payload, csrfPayloadSep)
	if len(fields) != 4 || fields[0] != csrfTokenPrefix {
		return false
	}
	bound, err := base64.RawURLEncoding.DecodeString(fields[1])
	if err != nil {
		return false
	}
	if subtle.ConstantTimeCompare(bound, []byte(identity)) != 1 {
		return false
	}

	unix, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil {
		return false
	}
	// One minute of grace, so a token issued moments before this request's
	// deadline crossing does not reject a form the user submitted in time.
	return now.Before(time.Unix(unix, 0).Add(time.Minute))
}

// csrfContextKey is the private key the token minted for this request travels
// under, so that a page being rendered can put it in a form.
type csrfContextKey struct{}

// withRenderedCSRF records the token a response being built should use.
func withRenderedCSRF(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, csrfContextKey{}, token)
}

// renderedCSRF returns the token the CSRF middleware minted for this request.
func renderedCSRF(r *http.Request) string {
	value, _ := r.Context().Value(csrfContextKey{}).(string)
	return value
}

// CSRF returns the token presented with a request: the header first, then the
// form field, then the token minted for this request, then the cookie the
// browser was given.
//
// The header wins because an XHR client sets one deliberately, while a form
// field may be a leftover from the page the XHR was made from. The minted
// value comes next and is what a first-time visitor's form carries: the
// middleware signs a token and puts it on the response, and there is nothing
// else on the request to read. The cookie is last and is only a source for
// rendering too, for the same reason. The verification path deliberately does
// not consult either — a value the browser sends on its own is precisely the
// one an attacker relies on being sent.
func CSRF(r *http.Request) string {
	if v := r.Header.Get(csrfHeader); v != "" {
		return strings.TrimSpace(v)
	}
	if r.Form != nil {
		if v := strings.TrimSpace(r.Form.Get(csrfField)); v != "" {
			return v
		}
	}
	if v := renderedCSRF(r); v != "" {
		return v
	}
	value, _ := cookieNamed(r, csrfCookieName)
	return value
}

// CSRFInput returns the token as a hidden input's markup, escaped.
//
// The token is base64url so it needs no escaping, but it is escaped anyway:
// the guarantee that a signature contains no angle brackets is a property of
// today's encoder, and escaping costs nothing. The return type is a string so
// that this package does not depend on a templating layer; the web core wraps
// the result in template.HTML once, where the escaping has already been done.
func CSRFInput(r *http.Request) string {
	token := CSRF(r)
	if token == "" {
		return ""
	}
	return `<input type="hidden" name="` + csrfField + `" value="` + html.EscapeString(token) + `">`
}

// CSRFMiddleware validates the token on requests that can change state.
//
// The rules, and why each is there:
//
//   - Safe methods are never rejected. A GET that changes something is
//     already a bug a CSRF token cannot fix.
//   - Safe methods ensure a token exists, including for anonymous requests,
//     so that the login form has one. A check that issues tokens only to
//     signed-in users cannot protect the login form, which is the endpoint
//     where a forged request is worth the most.
//   - Requests that authenticated by an Authorization header are skipped,
//     when that header actually verified. A cross-site form cannot set that
//     header, and a cross-origin fetch that could is blocked by CORS before
//     reaching the handler. Checking CSRF there would only break API clients
//     for no gain — and the skip is gated on the credential having succeeded,
//     because a request presenting one that did not verify has proven nothing
//     about who sent it.
//   - Cookie-authenticated unsafe requests must present a token bound to the
//     identity in the session.
//   - Anonymous unsafe requests must present one when the CSRF cookie came
//     along, which is the condition a browser satisfies and curl does not.
//     Without the rule, anonymous API posts break; with it, a cross-site
//     login or signup attempt is checked, because the browser that was served
//     the page has the cookie and the attacker cannot read it.
//
// Failure is 403. A forged request is not malformed input: every field in it
// is well formed, and the reason it is refused is authorization. A 400 that
// re-renders the form also hands whoever submitted it a description of what
// was expected.
func (s *Service) CSRFMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			info := s.Info(r)

			// A token is bound to whoever the request is authenticated as, or
			// to the empty identity when it is not.
			identity := ""
			if info.Via != ViaNone {
				identity = info.User.Identity
			}

			if !isUnsafeMethod(r.Method) {
				next.ServeHTTP(w, r.WithContext(s.renderedCSRFContext(w, r, identity)))
				return
			}

			// Authenticated by a header credential that verified. Not
			// forgeable cross-site, so there is nothing left to check. A
			// credential that failed to verify is not on this list: the
			// request has proved nothing, and treating it as exempt would be a
			// way to turn the exemption into a bypass.
			if info.Via == ViaBasic || info.Via == ViaToken {
				next.ServeHTTP(w, r)
				return
			}

			needsToken := info.Via == ViaSession || cookiePresent(r, csrfCookieName)
			if !needsToken {
				next.ServeHTTP(w, r)
				return
			}

			token := s.tokenFromRequest(r)
			if token == "" || !csrfTokenValid(s.csrfKey, token, identity, s.now()) {
				s.deny(w, r, Denial{Kind: DenialCSRF})
				return
			}

			next.ServeHTTP(w, r.WithContext(s.renderedCSRFContext(w, r, identity)))
		})
	}
}

// renderedCSRFContext returns the context to continue a request with, carrying
// the token its forms should use: the request's still-valid cookie when it has
// one, otherwise the token just minted for it.
//
// Without this, the first page a visitor ever sees renders an empty CSRF field,
// because the token the middleware signed is only on the response, and every
// form on that page fails when submitted. Reading it back from the context is
// also how the cookie-less case stays honest: nothing here invents a token the
// response was not given.
func (s *Service) renderedCSRFContext(w http.ResponseWriter, r *http.Request, identity string) context.Context {
	return withRenderedCSRF(r.Context(), s.ensureCSRFCookie(w, r, identity))
}

// ensureCSRFCookie writes a CSRF cookie when the request's is missing,
// invalid, expired, or bound to a different identity than the request now
// has, and returns the token the response's forms should carry — the existing
// one when it was kept, the new one when it was minted, and the empty string
// only when minting failed.
//
// Re-issuing on identity change is what makes signing in work: the browser is
// given an anonymous token on the login page, and the request that submits it
// is authenticated. Without the refresh, the first form rendered after login
// would carry a token bound to nobody and every submission after it would
// fail.
func (s *Service) ensureCSRFCookie(w http.ResponseWriter, r *http.Request, identity string) string {
	if existing, ok := cookieNamed(r, csrfCookieName); ok {
		if csrfTokenValid(s.csrfKey, existing, identity, s.now()) {
			return existing
		}
	}

	expires := s.now().Add(s.policy.Lifetime)
	token, err := newCSRFToken(s.csrfKey, identity, expires)
	if err != nil {
		// No token means forms cannot be submitted. That is worth an error
		// line rather than a silent storm of 403s.
		s.logger.ErrorContext(r.Context(), "auth: could not mint CSRF token", "error", err)
		return ""
	}
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    token,
		Path:     "/",
		Domain:   s.policy.Domain,
		HttpOnly: false,
		Secure:   s.policy.Secure,
		SameSite: s.policy.SameSite,
		Expires:  expires,
	})
	return token
}

// isUnsafeMethod reports whether a method may change state.
//
// The list is explicit rather than "not GET and not HEAD" because OPTIONS and
// TRACE are both safe by definition, and rejecting them would break
// preflighted cross-origin clients for no security gain.
func isUnsafeMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// tokenFromRequest reads a CSRF token from the request without disturbing a
// handler's own parsing more than it has to.
//
// r.Form is reused when it is already populated. Otherwise the body is
// parsed, which populates it for the handler as a side effect; that is
// ParseForm's documented behaviour, not a surprise. A parse failure is not
// reported here: a request whose body cannot be parsed has no token in it,
// which is the answer the caller needs, and the handler reaches the same body
// with the same error and reports it as an input problem.
func (s *Service) tokenFromRequest(r *http.Request) string {
	if v := r.Header.Get(csrfHeader); v != "" {
		return strings.TrimSpace(v)
	}

	if r.Form == nil {
		var err error
		if isMultipart(r) {
			// Multipart bodies need their own parse; ParseForm alone does not
			// read them.
			err = r.ParseMultipartForm(defaultMultipartMemory)
		} else {
			err = r.ParseForm()
		}
		if err != nil {
			s.debugParseFailure(r, err)
			return ""
		}
	}
	return strings.TrimSpace(r.Form.Get(csrfField))
}

// isMultipart reports whether a request body is multipart/form-data, the one
// content type ParseForm does not handle.
func isMultipart(r *http.Request) bool {
	return strings.HasPrefix(
		strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))),
		"multipart/form-data")
}

// debugParseFailure records a body parse failure at debug level.
func (s *Service) debugParseFailure(r *http.Request, err error) {
	if errors.Is(err, http.ErrNotMultipart) {
		return
	}
	s.logger.DebugContext(r.Context(), "auth: body could not be parsed while checking CSRF",
		"error", err)
}

// cookieNamed returns the single value of a named request cookie.
//
// A request carrying more than one cookie of the same name yields nothing.
// Multiple cookies of that name are legal to send and net/http hands back the
// first match, so a caller able to set a cookie on a sibling domain could
// shadow the real one — or be shadowed, depending on ordering. Both are
// unacceptable for an authorization decision, so the pairing is treated as
// unverifiable, which is what "not found" means to every caller here.
func cookieNamed(r *http.Request, name string) (string, bool) {
	var (
		value string
		found int
	)
	for _, c := range r.Cookies() {
		if c.Name != name {
			continue
		}
		found++
		value = c.Value
	}
	if found != 1 || value == "" {
		return "", false
	}
	return value, true
}

// cookiePresent reports whether a request carries a non-empty cookie of that
// name, accepting an arbitrary number of them: this answers "is a browser in
// play?", not "what is the value?".
func cookiePresent(r *http.Request, name string) bool {
	for _, c := range r.Cookies() {
		if c.Name == name && c.Value != "" {
			return true
		}
	}
	return false
}
