package webapp

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/the-maldridge/webapp/db"
)

// Environment variable suffixes read when a value is not set by an
// option. The prefix comes from WithEnvPrefix and defaults to "WEBAPP".
const (
	suffixAddr         = "_ADDR"
	suffixDebug        = "_DEBUG"
	suffixProduction   = "_PRODUCTION"
	suffixCookieSecure = "_COOKIE_SECURE"
	suffixModules      = "_MODULES"
	suffixReadTimeout  = "_READ_TIMEOUT"
	suffixWriteTimeout = "_WRITE_TIMEOUT"
	suffixIdleTimeout  = "_IDLE_TIMEOUT"

	// suffixClientIPHeader, suffixTrustedProxyCIDRs, and
	// suffixTrustedProxyCount configure how the real client address is
	// determined. They are environment-only, and deliberately strict:
	// trusting the wrong header lets any client claim another address.
	suffixClientIPHeader    = "_CLIENT_IP_HEADER"
	suffixTrustedProxyCIDRs = "_TRUSTED_PROXY_CIDRS"
	suffixTrustedProxyCount = "_TRUSTED_PROXY_COUNT"
	suffixMaxBodyBytes      = "_MAX_BODY_BYTES"

	// DefaultEnvPrefix namespaces environment variables when the
	// application does not choose its own.
	DefaultEnvPrefix = "WEBAPP"

	// DefaultAddr is the listen address used when nothing else is
	// configured.
	DefaultAddr = ":8080"
)

// Conservative non-zero defaults. An unset read timeout is a slow-loris
// primitive, and an unset write timeout means one stuck client holds a
// goroutine and an open connection forever. The numbers are sized for a
// server-rendered application on a normal connection: generous enough
// for a slow mobile upload of a form POST, short enough that a wedged
// client cannot hold a worker indefinitely.
const (
	DefaultReadHeaderTimeout = 10 * time.Second
	DefaultReadTimeout       = 30 * time.Second
	DefaultWriteTimeout      = 45 * time.Second
	DefaultIdleTimeout       = 120 * time.Second
	// DefaultShutdownTimeout bounds how long Run waits for in-flight
	// requests after a signal. Beyond it the process exits anyway; a
	// request that cannot finish in that time is not going to.
	DefaultShutdownTimeout = 15 * time.Second
)

// Timeouts holds the http.Server deadlines. Zero fields are filled from
// the defaults above; WithTimeouts rejects explicitly negative values.
type Timeouts struct {
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
}

// withDefaults returns a copy with every unset field replaced.
func (t Timeouts) withDefaults() Timeouts {
	if t.ReadHeaderTimeout == 0 {
		t.ReadHeaderTimeout = DefaultReadHeaderTimeout
	}
	if t.ReadTimeout == 0 {
		t.ReadTimeout = DefaultReadTimeout
	}
	if t.WriteTimeout == 0 {
		t.WriteTimeout = DefaultWriteTimeout
	}
	if t.IdleTimeout == 0 {
		t.IdleTimeout = DefaultIdleTimeout
	}
	if t.ShutdownTimeout == 0 {
		t.ShutdownTimeout = DefaultShutdownTimeout
	}
	return t
}

// Config is the resolved application configuration. New builds one; it
// is exported so that applications can inspect what was decided for
// them, and so that a Config assembled by hand can be checked with
// Validate.
type Config struct {
	// EnvPrefix namespaces every environment variable the framework
	// reads, and is forwarded to the database layer.
	EnvPrefix string

	// Addr is the TCP address to listen on.
	Addr string

	// Debug enables verbose error pages, template debug mode, and
	// anything else that is useful in development and unsafe in
	// public.
	Debug bool

	// Production turns on hardening: Secure cookies, no generated
	// secrets, generic error pages. It is the switch that separates
	// "safe in a demo" from "safe on the internet".
	Production bool

	// CookieSecure controls the Secure attribute on cookies. When
	// unset it defaults to true unless Debug is on, so that a local
	// application over plain HTTP still works.
	CookieSecure bool

	// EnabledModules restricts the optional modules that are loaded.
	// Empty means every module the application registered.
	EnabledModules []string

	// Timeouts are the http.Server deadlines.
	Timeouts Timeouts

	// MaxBodyBytes bounds a request body on state-changing methods.
	// Zero selects DefaultMaxBodyBytes.
	MaxBodyBytes int64

	// ClientIPHeader names a single-IP header set by a reverse proxy,
	// for example "X-Real-IP" or "CF-Connecting-IP". It is trusted
	// because the proxy overwrites it on every request; the header name
	// has to be named explicitly for that reason, since the similar-looking
	// True-Client-IP and X-Azure-ClientIP pass through from the client by
	// default in the products that set them.
	ClientIPHeader string

	// TrustedProxyCIDRs enumerates the networks of the proxies in front
	// of this server. When set, the client address is read from
	// X-Forwarded-For by walking right to left and skipping those
	// networks. CDNs publish their ranges for this purpose.
	TrustedProxyCIDRs []string

	// TrustedProxyCount is the number of proxy hops in front of this
	// server, used when the IPs are not enumerable. The rightmost that
	// many X-Forwarded-For entries are skipped.
	TrustedProxyCount int

	// Logger receives request logs, failures, and panics. Nil means
	// log/slog's default logger, which is the right answer for most
	// applications because it inherits whatever handler main installed.
	Logger *slog.Logger

	// Components contribute database models and migrations. Plain model
	// values may be used instead of implementing Component.
	Components []any

	// DBOptions are forwarded verbatim to db.New. The application
	// prefix is prepended so that a prefix set on the application also
	// names the database variables.
	DBOptions []db.Option

	// env is injected so tests can control lookups without mutating
	// process state.
	env envLookup

	// addrSet, debugSet, productionSet and cookieSecureSet record
	// which values came from an option, so that the environment does
	// not override an explicit choice.
	addrSet         bool
	debugSet        bool
	productionSet   bool
	cookieSecureSet bool
	timeoutsSet     bool
}

// envLookup indirections environment access for testing.
type envLookup func(string) (string, bool)

// osLookupEnv is the production accessor.
func osLookupEnv(key string) (string, bool) { return os.LookupEnv(key) }

// newConfig applies options, then environment fallbacks, then defaults.
// Parsing errors are collected rather than returned one at a time.
func newConfig(env envLookup, opts ...Option) (*Config, error) {
	if env == nil {
		env = osLookupEnv
	}

	c := &Config{
		EnvPrefix: DefaultEnvPrefix,
		Addr:      DefaultAddr,
		env:       env,
	}
	c.Timeouts = c.Timeouts.withDefaults()

	var errs []error
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt(c); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	if err := c.applyEnv(); err != nil {
		return nil, err
	}
	return c, nil
}

// applyEnv fills fields that no option set. An unparsable value is an
// error naming the variable, never a silent zero.
func (c *Config) applyEnv() error {
	var errs []error

	// An explicit bool cannot be distinguished from an unset false by
	// reading the value alone, so the environment only contributes when
	// no option claimed the field.
	if !c.addrSet {
		if v, ok := c.lookup(suffixAddr); ok && v != "" {
			c.Addr = v
		}
	}
	if !c.debugSet {
		v, present, err := c.lookupBool(suffixDebug)
		switch {
		case err != nil:
			errs = append(errs, err)
		case present:
			c.Debug = v
		}
	}
	if !c.productionSet {
		v, present, err := c.lookupBool(suffixProduction)
		switch {
		case err != nil:
			errs = append(errs, err)
		case present:
			c.Production = v
		}
	}
	if !c.cookieSecureSet {
		v, present, err := c.lookupBool(suffixCookieSecure)
		switch {
		case err != nil:
			errs = append(errs, err)
		case present:
			c.CookieSecure = v
			c.cookieSecureSet = true
		}
	}
	if v, ok := c.lookup(suffixModules); ok && len(c.EnabledModules) == 0 {
		c.EnabledModules = splitList(v)
	}
	if v, ok := c.lookup(suffixClientIPHeader); ok && c.ClientIPHeader == "" {
		c.ClientIPHeader = strings.TrimSpace(v)
	}
	if v, ok := c.lookup(suffixTrustedProxyCIDRs); ok && len(c.TrustedProxyCIDRs) == 0 {
		c.TrustedProxyCIDRs = splitList(v)
	}
	if v, ok := c.lookup(suffixTrustedProxyCount); ok && c.TrustedProxyCount == 0 {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s%s: %q is not a whole number of proxies", c.EnvPrefix, suffixTrustedProxyCount, v))
		} else {
			c.TrustedProxyCount = n
		}
	}
	if v, ok := c.lookup(suffixMaxBodyBytes); ok && c.MaxBodyBytes == 0 {
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s%s: %q is not a byte count", c.EnvPrefix, suffixMaxBodyBytes, v))
		} else {
			c.MaxBodyBytes = n
		}
	}
	if !c.timeoutsSet {
		for suffix, target := range map[string]*time.Duration{
			suffixReadTimeout:  &c.Timeouts.ReadTimeout,
			suffixWriteTimeout: &c.Timeouts.WriteTimeout,
			suffixIdleTimeout:  &c.Timeouts.IdleTimeout,
		} {
			if v, ok := c.lookup(suffix); ok {
				d, err := time.ParseDuration(v)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s%s: %w", c.EnvPrefix, suffix, err))
					continue
				}
				*target = d
			}
		}
	}

	c.Timeouts = c.Timeouts.withDefaults()

	// Cookies default to Secure, because the dangerous direction is the
	// one where an application deployed on TLS silently sends session
	// cookies in the clear. Local development is the only case that
	// wants this off, and debug mode is the signal for it.
	if !c.cookieSecureSet {
		c.CookieSecure = !c.Debug
	}

	return errors.Join(errs...)
}

// lookup returns a prefixed environment variable.
func (c *Config) lookup(suffix string) (string, bool) {
	return c.env(c.EnvPrefix + suffix)
}

// lookupBool reads a prefixed boolean variable. Absent or empty is
// reported as not present rather than as false, because "the operator did
// not set this" and "the operator set this to false" lead to different
// defaults. Values like "ture" are rejected: a debug flag that quietly
// evaluates to false in production is indistinguishable from one that was
// never set at all.
func (c *Config) lookupBool(suffix string) (value bool, present bool, err error) {
	raw, ok := c.lookup(suffix)
	if !ok || strings.TrimSpace(raw) == "" {
		return false, false, nil
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, true, fmt.Errorf("%s%s: %q is not a boolean (use 1, t, true, or their opposites)",
			c.EnvPrefix, suffix, raw)
	}
	return v, true, nil
}

// splitList parses a comma-separated module allowlist. Empty elements
// are dropped so that "a,,b" and " a, b " behave sensibly.
func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// Validate reports every configuration problem at once. It is called by
// New; applications that build a Config themselves should call it too.
func (c *Config) Validate() error {
	var errs []error

	if c.EnvPrefix == "" {
		errs = append(errs, errors.New("environment prefix is empty"))
	}
	if strings.ContainsAny(c.EnvPrefix, " \t=") {
		errs = append(errs, fmt.Errorf("environment prefix %q may not contain spaces or '='", c.EnvPrefix))
	}
	if c.Addr != "" {
		if _, _, err := net.SplitHostPort(c.Addr); err != nil {
			errs = append(errs, fmt.Errorf("listen address %q is not host:port: %w", c.Addr, err))
		}
	}
	if c.MaxBodyBytes < 0 {
		errs = append(errs, fmt.Errorf("max body size may not be negative, got %d", c.MaxBodyBytes))
	}
	if c.TrustedProxyCount < 0 {
		errs = append(errs, fmt.Errorf("trusted proxy count may not be negative, got %d", c.TrustedProxyCount))
	}
	if c.TrustedProxyCount > 0 && len(c.TrustedProxyCIDRs) > 0 {
		errs = append(errs, errors.New("trusted proxies are configured both by count and by CIDR; choose one"))
	}
	for _, prefix := range c.TrustedProxyCIDRs {
		if _, _, err := net.ParseCIDR(prefix); err != nil {
			errs = append(errs, fmt.Errorf("trusted proxy %q is not a CIDR prefix: %w", prefix, err))
		}
	}

	if err := c.Timeouts.validate(); err != nil {
		errs = append(errs, err)
	}

	// Debug and production contradict each other. Rather than silently
	// letting one win, refuse to start.
	if c.Debug && c.Production {
		errs = append(errs, errors.New("debug and production are both enabled; error detail and hardening cannot both apply"))
	}

	return errors.Join(errs...)
}

// validate checks the resolved deadlines.
func (t Timeouts) validate() error {
	var errs []error
	for name, d := range map[string]time.Duration{
		"read header timeout": t.ReadHeaderTimeout,
		"read timeout":        t.ReadTimeout,
		"write timeout":       t.WriteTimeout,
		"idle timeout":        t.IdleTimeout,
		"shutdown timeout":    t.ShutdownTimeout,
	} {
		if d < 0 {
			errs = append(errs, fmt.Errorf("%s may not be negative, got %s", name, d))
		}
	}
	// A read deadline shorter than the header deadline can never fire
	// usefully, and usually means one of the two was set by accident.
	if t.ReadTimeout > 0 && t.ReadHeaderTimeout > 0 && t.ReadTimeout < t.ReadHeaderTimeout {
		errs = append(errs, fmt.Errorf("read timeout (%s) is shorter than read header timeout (%s)",
			t.ReadTimeout, t.ReadHeaderTimeout))
	}
	return errors.Join(errs...)
}
