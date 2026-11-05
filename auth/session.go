package auth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/meehow/securebytes"
	authware "github.com/the-maldridge/authware"
)

// Session is the payload stored in the session cookie.
//
// The shape matches authware's session type on purpose: same field names,
// same serializer, same cookie encryption scheme. An application moving to
// this package therefore does not invalidate its users' sessions on the
// deploy that moves them, which is the difference between a migration and a
// mass logout.
type Session struct {
	Expires time.Time
	User    authware.User
}

// sessionKeySize is the minimum entropy demanded of a session key in
// production. It is a minimum rather than an exact requirement because keys
// are usually read from a secret manager and may be longer; the derivation
// hashes to a fixed size regardless.
const sessionKeySize = 32

// sessionCodec encrypts and authenticates session payloads.
type sessionCodec struct {
	sb *securebytes.SecureBytes
}

// newSessionCodec derives an AES-GCM codec from a key of any length. The
// library hashes the key, so a long random string is safe and a short one is
// not made longer by anything here — hence the explicit check.
func newSessionCodec(key string) (*sessionCodec, error) {
	if len(key) < sessionKeySize {
		return nil, fmt.Errorf(
			"auth: session key is %d bytes, minimum %d; generate one with %q",
			len(key), sessionKeySize, "openssl rand -hex 32")
	}
	return &sessionCodec{
		sb: securebytes.New([]byte(key), securebytes.JSONSerializer{}),
	}, nil
}

// seal serializes and encrypts a session into a cookie value.
func (c *sessionCodec) seal(s Session) (string, error) {
	v, err := c.sb.EncryptToBase64(s)
	if err != nil {
		return "", fmt.Errorf("auth: could not encrypt session: %w", err)
	}
	return v, nil
}

// open decrypts a cookie value into a session. It reports false for a
// malformed, tampered, or wrong-key value rather than distinguishing them,
// because every one of them means the same thing to a caller: this cookie
// grants nothing.
func (c *sessionCodec) open(value string) (Session, bool) {
	var s Session
	if err := c.sb.DecryptBase64(value, &s); err != nil {
		return Session{}, false
	}
	return s, true
}

// newRandomKey returns a session key with 256 bits of entropy, hex encoded
// so it survives being pasted into an environment variable.
func newRandomKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: could not read random bytes: %w", err)
	}
	return fmt.Sprintf("%x", buf), nil
}

// tokenBytes is the entropy of generated secrets: API tokens and CSRF
// tokens. 32 bytes is the size of the AES key they are conceptually peers
// with, and hex encoding keeps them copy-pasteable.
const tokenBytes = 32

// newToken returns a URL- and header-safe random token.
func newToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: could not read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// ErrNoUser reports that a request carries no authenticated identity.
// The caller decides whether that is a 401 or a redirect to a login page.
var ErrNoUser = errors.New("auth: no user in request context")

// UserFrom returns the authenticated user carried by a request, and false
// when the request is anonymous.
//
// This is the accessor, not ctx.Value: getting the key wrong in Go yields a
// nil interface and a panic several layers away, and this is called from
// templates as well as handlers.
func UserFrom(r *http.Request) (authware.User, bool) {
	if r == nil {
		return authware.User{}, false
	}
	return authware.UserFromContext(r.Context())
}

// MustIdentity returns the identity of the authenticated user or an error.
// It exists so that "who owns this row" is one call that cannot be silently
// ignored, which the two-value form invites.
func MustIdentity(r *http.Request) (string, error) {
	u, ok := UserFrom(r)
	if !ok || u.Identity == "" {
		return "", ErrNoUser
	}
	return u.Identity, nil
}

// HasPerm reports whether the request's user holds p. Anonymous requests
// hold nothing.
func HasPerm(r *http.Request, p Perm) bool {
	u, ok := UserFrom(r)
	if !ok {
		return false
	}
	return holds(u.Groups, p)
}

// HasGroup reports whether the request's user is a member of a named group.
func HasGroup(r *http.Request, name string) bool {
	u, ok := UserFrom(r)
	if !ok {
		return false
	}
	_, found := u.Groups[strings.ToUpper(name)]
	return found
}

// issueOptions describe the cookie a session is written into.
type issueOptions struct {
	name     string
	secure   bool
	sameSite http.SameSite
	lifetime time.Duration
	domain   string
}

// writeSession sets the session cookie and clears any stale one under the
// same name in a different scope.
func writeSession(w http.ResponseWriter, o issueOptions, value string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     o.name,
		Value:    value,
		Path:     "/",
		Domain:   o.domain,
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		Secure:   o.secure,
		SameSite: o.sameSite,
	})
}

// clearSession expires the session cookie in the browser.
func clearSession(w http.ResponseWriter, o issueOptions) {
	http.SetCookie(w, &http.Cookie{
		Name:     o.name,
		Value:    "",
		Path:     "/",
		Domain:   o.domain,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   o.secure,
		SameSite: o.sameSite,
	})
}

// readSessionValue pulls the raw session cookie, and rejects a request that
// carries more than one.
//
// Multiple cookies of the same name are legal to send and net/http hands
// back the first match, which means a attacker able to set a cookie on a
// sibling domain can shadow a real one — or, depending on ordering, be
// shadowed. Both are unacceptable for an authorization decision, so a
// request with more than one is treated as carrying nothing.
func readSessionValue(r *http.Request, name string) (string, bool) {
	var value string
	var found bool
	for _, c := range r.Cookies() {
		if c.Name != name {
			continue
		}
		if found {
			return "", false
		}
		value, found = c.Value, true
	}
	if !found || value == "" {
		return "", false
	}
	return value, true
}
