package webapp

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/the-maldridge/webapp/db"
)

// Option configures an application. Options are applied in order, so a
// later option overrides an earlier one, and every option beats the
// environment.
//
// An Option that returns an error aborts construction; errors are never
// deferred to the first request.
type Option func(*Config) error

// WithEnvPrefix sets the namespace for environment lookups, for example
// "TASKS" so that the listen address comes from TASKS_ADDR. It also
// names the database variables, so an application needs to state its
// prefix once.
func WithEnvPrefix(prefix string) Option {
	return func(c *Config) error {
		if prefix == "" {
			return errors.New("WithEnvPrefix: prefix may not be empty")
		}
		c.EnvPrefix = strings.ToUpper(prefix)
		return nil
	}
}

// WithAddr sets the listen address.
func WithAddr(addr string) Option {
	return func(c *Config) error {
		if addr == "" {
			return errors.New("WithAddr: address may not be empty; use :8080 for the default port")
		}
		c.Addr = addr
		c.addrSet = true
		return nil
	}
}

// WithDebug enables verbose error pages and template debugging. It
// cannot be combined with WithProduction.
func WithDebug() Option {
	return func(c *Config) error {
		c.Debug = true
		c.debugSet = true
		return nil
	}
}

// WithProduction enables deployment hardening. Session signing material
// must be provided rather than generated, error pages stay generic, and
// cookies are Secure unless WithCookieSecure(false) overrides it.
func WithProduction() Option {
	return func(c *Config) error {
		c.Production = true
		c.productionSet = true
		return nil
	}
}

// WithCookieSecure overrides the Secure attribute on cookies. The
// default is Secure unless debug mode is on. Turning it off is
// legitimate only behind a TLS-terminating proxy that does not preserve
// the scheme, so the choice is explicit rather than automatic.
func WithCookieSecure(secure bool) Option {
	return func(c *Config) error {
		c.CookieSecure = secure
		c.cookieSecureSet = true
		return nil
	}
}

// WithTimeouts sets the http.Server deadlines. Zero-valued fields keep
// the framework default; negative values are rejected, since "negative"
// has no meaning here and is almost certainly a sign flip.
func WithTimeouts(t Timeouts) Option {
	return func(c *Config) error {
		if err := t.validate(); err != nil {
			return fmt.Errorf("WithTimeouts: %w", err)
		}
		c.Timeouts = t.withDefaults()
		c.timeoutsSet = true
		return nil
	}
}

// WithShutdownTimeout bounds how long Run waits for in-flight requests
// after a termination signal.
func WithShutdownTimeout(d time.Duration) Option {
	return func(c *Config) error {
		if d < 0 {
			return fmt.Errorf("WithShutdownTimeout: %s may not be negative", d)
		}
		c.Timeouts.ShutdownTimeout = d
		c.timeoutsSet = true
		return nil
	}
}

// WithDBOptions forwards options to the database layer. The
// application's environment prefix is prepended, so the last option
// below still wins over an earlier one.
func WithDBOptions(opts ...db.Option) Option {
	return func(c *Config) error {
		c.DBOptions = append(c.DBOptions, opts...)
		return nil
	}
}

// WithLogger sends request logs, failures, and panics to a specific
// logger. By default the process-wide log/slog logger is used, which is
// correct for an application whose main installed the handler it wants.
func WithLogger(l *slog.Logger) Option {
	return func(c *Config) error {
		if l == nil {
			return errors.New("WithLogger: logger may not be nil")
		}
		c.Logger = l
		return nil
	}
}

// WithMaxBodyBytes bounds a state-changing request body. Applications
// that accept uploads set this; the default is deliberately small enough
// that a hostile client cannot make the server buffer a large body per
// concurrent request.
func WithMaxBodyBytes(n int64) Option {
	return func(c *Config) error {
		if n < 0 {
			return errors.New("WithMaxBodyBytes: limit may not be negative")
		}
		c.MaxBodyBytes = n
		return nil
	}
}

// WithClientIPHeader trusts a single-IP header that the reverse proxy in
// front of this server overwrites on every request, such as "X-Real-IP"
// or "CF-Connecting-IP".
//
// The header must be named explicitly rather than guessed at, because
// the familiar-looking True-Client-IP and X-Azure-ClientIP pass through
// from the client by default in the products that set them, and trusting
// one of those lets any request claim any source address.
func WithClientIPHeader(header string) Option {
	return func(c *Config) error {
		if header == "" {
			return errors.New("WithClientIPHeader: header name may not be empty")
		}
		c.ClientIPHeader = header
		return nil
	}
}

// WithTrustedProxies enumerates the networks of the proxies in front of
// this server, so that the client address can be read from
// X-Forwarded-For safely by skipping those hops. CDNs publish their
// ranges for exactly this purpose.
func WithTrustedProxies(cidrs ...string) Option {
	return func(c *Config) error {
		for _, prefix := range cidrs {
			if _, _, err := net.ParseCIDR(prefix); err != nil {
				return fmt.Errorf("WithTrustedProxies: %q is not a CIDR prefix: %w", prefix, err)
			}
		}
		c.TrustedProxyCIDRs = cidrs
		return nil
	}
}

// WithTrustedProxyCount declares the number of proxy hops between this
// server and the public internet, for deployments whose proxy addresses are
// not enumerable. That many rightmost X-Forwarded-For entries are skipped
// and the next one is the client.
//
// WithTrustedProxies is the better choice whenever the ranges are known: a
// count cannot be checked against anything, so one off by one and a client
// chooses its own recorded address. Verify from a known source address
// before trusting the number in production.
func WithTrustedProxyCount(hops int) Option {
	return func(c *Config) error {
		if hops < 1 {
			return fmt.Errorf("WithTrustedProxyCount: %d is not a whole number of proxies", hops)
		}
		c.TrustedProxyCount = hops
		return nil
	}
}

// WithComponent registers a component during construction so its models
// are migrated before the application can serve anything. Plain model
// values are accepted too.
func WithComponent(components ...any) Option {
	return func(c *Config) error {
		c.Components = append(c.Components, components...)
		return nil
	}
}

// WithModules restricts which optional modules are loaded, by name. An
// empty list means every registered module.
func WithModules(names ...string) Option {
	return func(c *Config) error {
		var clean []string
		for _, n := range names {
			if n = strings.TrimSpace(n); n == "" {
				return errors.New("WithModules: module names may not be empty")
			}
			clean = append(clean, n)
		}
		c.EnabledModules = clean
		return nil
	}
}

// WithDeploymentScope declares which role this process serves, such as "web"
// or "worker". A module whose Scopes list is non-empty and does not name it is
// not loaded.
//
// It exists for one binary running several ways, which is otherwise expressed
// by build tags nobody outside the release engineer can reproduce, or by
// loading everything and hoping a worker does not serve a page.
func WithDeploymentScope(scope string) Option {
	return func(c *Config) error {
		scope = strings.ToLower(strings.TrimSpace(scope))
		if scope == "" {
			return errors.New("WithDeploymentScope: scope may not be empty")
		}
		c.DeploymentScope = scope
		return nil
	}
}

// WithModule supplies a module to this application without adding it to the
// process-global registry that webapp.Register writes to.
//
// It is the form to prefer when a module's enablement is a decision this
// main() makes rather than a deployment making of a linked-in list: nothing is
// visible to any other application in the process, and a defective description
// is an error from New rather than a panic during package initialisation.
func WithModule(modules ...ModuleInfo) Option {
	return func(c *Config) error {
		for _, m := range modules {
			info, err := validateModuleInfo(m)
			if err != nil {
				return fmt.Errorf("WithModule: %w", err)
			}
			for _, existing := range c.Modules {
				if existing.Name == info.Name {
					return fmt.Errorf("WithModule: module %q is supplied twice", info.Name)
				}
			}
			c.Modules = append(c.Modules, info)
		}
		return nil
	}
}
