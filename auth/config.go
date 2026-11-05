package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// Environment variables this package reads. They keep the names authware
// uses, because the mechanism chain and the session codec are shared with it
// and one key in the environment is much better than two that have to be kept
// equal.
const (
	// EnvSessionKey names the key that encrypts and authenticates session
	// cookies. It is the root authority of the application: anyone holding
	// it can mint a session for any identity with any groups.
	EnvSessionKey = "AUTHWARE_SESSION_KEY"

	// EnvSessionLifetime is how long a session stays valid, parsed by
	// time.ParseDuration.
	EnvSessionLifetime = "AUTHWARE_SESSION_LIFETIME"
)

// Defaults for settings an application does not state.
const (
	// DefaultSessionLifetime is how long a session remains valid. One hour
	// is long enough that a person can finish reading something and short
	// enough that an unattended browser is not an open door all day.
	DefaultSessionLifetime = time.Hour

	// DefaultCookieName is the session cookie name. The web core overrides
	// it with one derived from the application's environment prefix, which
	// is what lets two applications sharing a host name without sharing a
	// purpose avoid reading each other's sessions.
	DefaultCookieName = "session"

	// CSRF_COOKIE is defined in csrf.go.
	csrfCookieName = "csrf_token"
)

// CookiePolicy describes the cookies this package sets.
//
// It is one struct rather than a handful of options because the attributes
// interact: SameSite=None is delivered as a session cookie unless Secure is
// set too, which browsers silently drop rather than accept. Setting them
// together and validating the combination is the only way for the
// combination to be checked at all.
type CookiePolicy struct {
	// Name is the session cookie name. Empty means DefaultCookieName.
	Name string

	// Domain scopes the cookie to a domain and its subdomains. Empty, which
	// is the default, means the exact host that set it: the narrower
	// choice, and the one that cannot hand a session to a sibling subdomain
	// somebody else administers.
	Domain string

	// Secure sets the Secure attribute. The web core enables it unless the
	// application is running in debug mode.
	Secure bool

	// SameSite is the SameSite attribute. Zero means Lax. The default is
	// also what browsers default to today, but a security property that
	// depends on a browser default is a property that changes whenever a
	// browser does.
	SameSite http.SameSite

	// Lifetime is how long a session stays valid. Zero means
	// DefaultSessionLifetime.
	Lifetime time.Duration
}

// withDefaults returns p with empty fields replaced by package defaults.
func (p CookiePolicy) withDefaults() CookiePolicy {
	if p.Name == "" {
		p.Name = DefaultCookieName
	}
	if p.Lifetime <= 0 {
		p.Lifetime = DefaultSessionLifetime
	}
	if p.SameSite == 0 {
		p.SameSite = http.SameSiteLaxMode
	}
	return p
}

// validate reports attribute combinations that browsers will not honor, or
// will honor in the opposite of the intended way.
func (p CookiePolicy) validate() error {
	var errs []error

	if p.Name == "" || strings.ContainsAny(p.Name, " \t\";,\\") {
		errs = append(errs, fmt.Errorf("cookie name %q is not usable", p.Name))
	}
	if p.SameSite == http.SameSiteNoneMode && !p.Secure {
		// Rejected rather than quietly downgraded: the only reason to ask
		// for None is cross-site embedding, and a cookie the browser drops
		// turns that feature into "signing in does not work here".
		errs = append(errs, errors.New("SameSite=None requires Secure"))
	}
	switch p.SameSite {
	case http.SameSiteDefaultMode, http.SameSiteLaxMode,
		http.SameSiteStrictMode, http.SameSiteNoneMode:
	default:
		errs = append(errs, fmt.Errorf("unknown SameSite mode %d", p.SameSite))
	}
	if p.Lifetime < 0 {
		errs = append(errs, errors.New("session lifetime may not be negative"))
	}
	return errors.Join(errs...)
}

// envLookup reads one environment variable. It matches the core's seam so
// tests can drive configuration without mutating the process environment.
type envLookup func(string) (string, bool)

// WithSessionKey sets the key that encrypts session cookies. It is the
// explicit form of AUTHWARE_SESSION_KEY.
//
// The key is not used raw as an AES key — it is hashed — so any length works,
// which is precisely why a short one must be refused out loud rather than
// silently becoming a weak key that looks configured.
func WithSessionKey(key string) Option {
	return func(s *Service) error {
		if err := checkSessionKey(key); err != nil {
			return err
		}
		s.sessionKey = key
		s.keySetByOption = true
		return nil
	}
}

// checkSessionKey enforces the entropy floor on a configured key.
func checkSessionKey(key string) error {
	if len(key) < sessionKeySize {
		return fmt.Errorf(
			"auth: session key is %d bytes, minimum %d; generate one with %q",
			len(key), sessionKeySize, "openssl rand -hex 32")
	}
	return nil
}

// WithCookiePolicy sets the cookie attributes for sessions and CSRF tokens.
// Empty fields take the package defaults; the rest are used verbatim,
// including a deliberate Secure false.
func WithCookiePolicy(p CookiePolicy) Option {
	return func(s *Service) error {
		p = p.withDefaults()
		if err := p.validate(); err != nil {
			return fmt.Errorf("auth: invalid cookie policy: %w", err)
		}
		s.policy = p
		if p.Lifetime > 0 {
			s.lifetimeSet = true
		}
		return nil
	}
}

// WithProduction marks the service as running in production. The only effect
// is that a missing session key becomes a startup failure instead of a
// randomly generated one: sessions that die on every restart and replicas
// that each accept only their own cookies are both consequences of a key that
// was never set, and neither looks like a configuration problem from outside.
//
// The web core sets this from its own configuration, so an application built
// on it does not need to call it.
func WithProduction(enabled bool) Option {
	return func(s *Service) error {
		s.production = enabled
		return nil
	}
}

// WithDenialHandler replaces how the guards in this package respond to a
// refusal. The default writes a plain JSON body with the right status code,
// which is correct for an API and useless for a server-rendered page; the web
// core installs a handler that renders the application's error pages and
// negotiates between the two.
//
// It is a property of the Service rather than a package global so that two
// services in one process — an application and a test — cannot reach each
// other's renderer.
func WithDenialHandler(h DenialHandler) Option {
	return func(s *Service) error {
		if h == nil {
			return errors.New("auth: WithDenialHandler called with nil")
		}
		s.deny = h
		return nil
	}
}

// withEnv injects the environment reader. Unexported: it exists so this
// package's tests can drive configuration without touching the process
// environment, and an application has no reason to want it.
func withEnv(lookup envLookup) Option {
	return func(s *Service) error {
		if lookup == nil {
			return errors.New("auth: withEnv called with nil")
		}
		s.env = lookup
		return nil
	}
}

// applySettings resolves everything the options left unset from the
// environment and the documented defaults, then builds the codec and the
// derived CSRF key.
//
// It runs after options so precedence is explicit: option, then environment,
// then default. The lifetime needs a "was it set" flag rather than a
// zero-check because the default is non-zero: a zero field would mean "one
// hour" and could not also mean "nobody chose this".
func (s *Service) applySettings() error {
	if s.env == nil {
		s.env = os.LookupEnv
	}

	if s.sessionKey == "" {
		if v, ok := s.env(EnvSessionKey); ok && strings.TrimSpace(v) != "" {
			s.sessionKey = v
		}
	}

	if !s.lifetimeSet {
		if v, ok := s.env(EnvSessionLifetime); ok && strings.TrimSpace(v) != "" {
			d, err := time.ParseDuration(strings.TrimSpace(v))
			if err != nil {
				return fmt.Errorf("auth: %s=%q is not a duration: %w", EnvSessionLifetime, v, err)
			}
			s.policy.Lifetime = d
		}
	}

	if s.sessionKey == "" {
		if s.production {
			return fmt.Errorf(
				"auth: production requires %s to be set; without it every restart logs everyone "+
					"out and every replica is its own authority", EnvSessionKey)
		}
		key, err := newRandomKey()
		if err != nil {
			return err
		}
		s.sessionKey = key
	}

	if err := checkSessionKey(s.sessionKey); err != nil {
		return err
	}

	s.policy = s.policy.withDefaults()
	if err := s.policy.validate(); err != nil {
		return fmt.Errorf("auth: invalid cookie policy: %w", err)
	}

	codec, err := newSessionCodec(s.sessionKey)
	if err != nil {
		return err
	}
	s.codec = codec

	csrfKey, err := deriveCSRFKey(s.sessionKey)
	if err != nil {
		return err
	}
	s.csrfKey = csrfKey

	if s.deny == nil {
		s.deny = defaultDenialHandler
	}
	return nil
}

// SessionKeyConfigured reports whether the session key came from the
// application rather than being generated for this process. The web core uses
// it to refuse a production boot: the decision belongs to the core because
// "production" is its configuration, and the answer belongs here because the
// key is this package's.
func (s *Service) SessionKeyConfigured() bool {
	return s.keySetByOption || envHasSessionKey(s.env)
}

// envHasSessionKey reports whether the environment supplies a session key.
func envHasSessionKey(lookup envLookup) bool {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	v, ok := lookup(EnvSessionKey)
	return ok && strings.TrimSpace(v) != ""
}

// CookieName returns the session cookie name in effect.
func (s *Service) CookieName() string { return s.policy.Name }

// CSRFName returns the CSRF cookie name in effect.
func (s *Service) CSRFName() string { return csrfCookieName }

// SessionLifetime returns how long a session stays valid.
func (s *Service) SessionLifetime() time.Duration { return s.policy.Lifetime }

// SetCookiePolicy replaces the cookie attributes after construction. It
// exists for the web core, which derives the session cookie name from the
// application's environment prefix and only has that answer after the service
// was built.
//
// It replaces the whole policy rather than one field because the attributes
// are validated as a set; a setter per field would re-open exactly the
// invalid combinations the struct exists to prevent.
func (s *Service) SetCookiePolicy(p CookiePolicy) error {
	p = p.withDefaults()
	if err := p.validate(); err != nil {
		return fmt.Errorf("auth: invalid cookie policy: %w", err)
	}
	s.policy = p
	return nil
}

// SetDenialHandler replaces the denial renderer after construction. See
// WithDenialHandler.
func (s *Service) SetDenialHandler(h DenialHandler) error {
	if h == nil {
		return errors.New("auth: SetDenialHandler called with nil")
	}
	s.deny = h
	return nil
}

// cookieOptions returns the cookie attributes for a session issued now, in
// the shape the session helpers take.
func (s *Service) cookieOptions() issueOptions {
	return issueOptions{
		name:     s.policy.Name,
		domain:   s.policy.Domain,
		secure:   s.policy.Secure,
		sameSite: s.policy.SameSite,
		lifetime: s.policy.Lifetime,
	}
}

// deriveCSRFKey derives the CSRF signing key from the session key.
//
// The two must never be the same bytes. A single key used both to seal
// sessions and to sign CSRF tokens lets anything that can obtain a signature
// for one purpose use it toward the other, and separating them costs nothing:
// an HMAC with a domain-separating label over the session key is a key that
// cannot be turned back into the session key.
func deriveCSRFKey(sessionKey string) ([]byte, error) {
	if sessionKey == "" {
		return nil, errors.New("auth: cannot derive a CSRF key from an empty session key")
	}
	mac := hmac.New(sha256.New, []byte(sessionKey))
	// The label is part of the input, not the key: two applications that
	// share a session key for some reason still derive different CSRF keys
	// if they label differently, and nothing about the label is secret.
	mac.Write([]byte("webapp/csrf-token/v1"))
	return mac.Sum(nil), nil
}
