package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Via identifies how a request was authenticated.
//
// It is recorded rather than discarded because applications legitimately need
// it: credential creation should require a fresh password rather than a
// long-lived cookie, an audit line reading "alice deleted the project" means
// something different when the request came from a token that has not been
// rotated in a year, and a login form should not be offered to a request that
// already presented a session.
type Via int

const (
	// ViaNone means the request is anonymous.
	ViaNone Via = iota

	// ViaSession means a session cookie was accepted.
	ViaSession

	// ViaBasic means an Authorization: Basic header was verified.
	ViaBasic

	// ViaToken means an Authorization: Bearer token was verified.
	ViaToken
)

// String names the mechanism for logs.
func (v Via) String() string {
	switch v {
	case ViaSession:
		return "session"
	case ViaBasic:
		return "basic"
	case ViaToken:
		return "token"
	default:
		return "anonymous"
	}
}

// AuthInfo is the outcome of authenticating one request.
type AuthInfo struct {
	// User is the established identity. Valid only when Via is not ViaNone.
	User User

	// Via is how it was established.
	Via Via

	// Err records why authentication failed when Via is ViaNone and a
	// credential was presented. It is for logs: the response a client gets is
	// one indistinguishable answer, and a mechanism error must not become a
	// hint about which half of a credential was wrong.
	Err error

	// Expires is when a session-derived identity stops being valid. Zero for
	// header-authenticated requests, which are re-verified every time.
	Expires time.Time
}

// infoContextKey is the private key AuthInfo travels under. Private so that a
// colliding key from another package cannot forge an identity: the value is an
// authorization decision, not a convenience.
type infoContextKey struct{}

// contextWithInfo returns ctx carrying info.
func contextWithInfo(ctx context.Context, info AuthInfo) context.Context {
	return context.WithValue(ctx, infoContextKey{}, info)
}

// infoFromContext returns the AuthInfo a middleware stamped, and false when
// none was stamped.
func infoFromContext(ctx context.Context) (AuthInfo, bool) {
	info, ok := ctx.Value(infoContextKey{}).(AuthInfo)
	return info, ok
}

// resolve establishes who is making a request, trying each credential the
// request carries in a fixed order.
//
// The order is session, bearer, then basic, and it is not arbitrary. A
// browser sends a session cookie on every request without being told to, so
// anything that also carries a token must let the token win when both are
// present, or an API client that kept an expired cookie from before signing in
// would be identified as the wrong principal. Session first, then the explicit
// header credentials, is the order that matches what each client intends: the
// cookie is ambient, the header is deliberate. The two header credentials share
// the one Authorization header, so a request never carries both; bearer is
// simply tried first.
//
// A credential that fails does not stop the search. An application that
// rotated its session key leaves every browser holding a cookie that cannot
// open, and those same browsers may be running an API client with a valid
// token; failing the request because the ambient credential was stale would
// make the rotation impossible to survive.
func (s *Service) resolve(r *http.Request) AuthInfo {
	var failures []error

	if user, expires, ok, err := s.fromSession(r); ok {
		return AuthInfo{User: user, Via: ViaSession, Expires: expires}
	} else if err != nil {
		failures = append(failures, err)
	}

	if user, ok, err := s.fromBearer(r); ok {
		return AuthInfo{User: user, Via: ViaToken}
	} else if err != nil {
		failures = append(failures, err)
	}

	if user, ok, err := s.fromBasic(r); ok {
		return AuthInfo{User: user, Via: ViaBasic}
	} else if err != nil {
		failures = append(failures, err)
	}

	info := AuthInfo{}
	if len(failures) > 0 {
		info.Err = errors.Join(failures...)
	}
	return info
}

// fromSession opens a session cookie. It reports a distinct triple: accepted,
// and whether a failure is worth logging. A cookie that simply does not
// decrypt is the ordinary condition of a browser whose session expired, and
// logging it would bury the interesting lines in noise.
func (s *Service) fromSession(r *http.Request) (User, time.Time, bool, error) {
	value, ok := readSessionValue(r, s.policy.Name)
	if !ok {
		return User{}, time.Time{}, false, nil
	}
	session, opened := s.codec.open(value)
	if !opened {
		return User{}, time.Time{}, false, nil
	}
	if session.Expires.IsZero() || !s.now().Before(session.Expires) {
		return User{}, time.Time{}, false, nil
	}

	// Normalized before the emptiness check, not after: a payload naming
	// "   " is a whitespace-only principal, and one that survives to satisfy
	// RequireLogin is the worst possible outcome of a bug somewhere else.
	user := session.User
	user.Identity = normalizeIdentity(user.Identity)
	if user.Identity == "" {
		return User{}, time.Time{}, false, nil
	}
	if user.Groups == nil {
		user.Groups = map[string]struct{}{}
	}
	return user, session.Expires, true, nil
}

// fromBearer resolves an Authorization: Bearer token.
func (s *Service) fromBearer(r *http.Request) (User, bool, error) {
	token, ok := bearerToken(r)
	if !ok {
		return User{}, false, nil
	}
	user, err := s.AuthenticateToken(r.Context(), token)
	if err != nil {
		return User{}, false, err
	}
	if user.Identity == "" {
		return User{}, false, nil
	}
	return user, true, nil
}

// fromBasic verifies an Authorization: Basic credential.
//
// Credentials are compared by hashing, so the decoding here is only to satisfy
// the header's format, and a request that fails it is treated as carrying no
// Basic credential at all rather than as an error. The specification's own
// advice is to ignore a malformed header, and it is correct: answering it is a
// way to learn something.
func (s *Service) fromBasic(r *http.Request) (User, bool, error) {
	identity, password, ok := r.BasicAuth()
	if !ok || identity == "" || password == "" {
		return User{}, false, nil
	}
	user, err := s.Authenticate(r.Context(), identity, password)
	if err != nil {
		return User{}, false, err
	}
	return user, true, nil
}

// bearerToken returns a bearer token from the Authorization header.
func bearerToken(r *http.Request) (string, bool) {
	value := r.Header.Get("Authorization")
	if value == "" {
		return "", false
	}
	scheme, rest, found := strings.Cut(value, " ")
	if !found {
		return "", false
	}
	if !strings.EqualFold(scheme, "bearer") {
		return "", false
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", false
	}
	return rest, true
}
