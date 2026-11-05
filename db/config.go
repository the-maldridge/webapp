package db

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// osLookupEnv is the environment accessor used in production. Tests
// substitute their own map.
func osLookupEnv(key string) (string, bool) {
	return os.LookupEnv(key)
}

// Driver identifies a supported database backend.
type Driver string

// The supported database backends.
const (
	DriverSQLite   Driver = "sqlite"
	DriverPostgres Driver = "postgres"
)

// Environment variable suffixes consulted when a value is not set
// explicitly. The prefix comes from WithEnvPrefix and defaults to
// "WEBAPP".
const (
	suffixDriver = "_DB_DRIVER"
	suffixPath   = "_DB"
	suffixDSN    = "_DB_DSN"
)

// DefaultPoolSize is the maximum number of open connections used when
// no pool configuration is supplied. SQLite serializes writers, so a
// small pool is correct there; the same number is a sane conservative
// starting point for PostgreSQL.
const DefaultPoolSize = 8

// DefaultSlowQuery is the duration past which a query is logged as
// slow.
const DefaultSlowQuery = 250 * time.Millisecond

// DefaultConnMaxLifetime is how long a pooled connection may be reused.
// A finite lifetime lets an application survive database-side
// connection recycling without a restart.
const DefaultConnMaxLifetime = time.Hour

// Config holds the resolved database configuration. Call Validate
// before using a Config that was assembled by hand.
type Config struct {
	// EnvPrefix namespaces the environment variables that supply
	// defaults, for example "TASKS" reads TASKS_DB_DRIVER.
	EnvPrefix string

	// Driver selects the backend. Empty means sqlite.
	Driver Driver

	// Path is the SQLite database file. Ignored by other drivers.
	Path string

	// DSN is the PostgreSQL connection string. Ignored by other
	// drivers.
	DSN string

	// MaxOpenConns, MaxIdleConns, and ConnMaxLifetime configure the
	// underlying connection pool. Zero means "use the default".
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration

	// SlowQuery is the threshold past which a query is logged. Zero
	// means "use the default", a negative value disables the check.
	SlowQuery time.Duration

	// driverSet records that WithDriver ran, so that an environment
	// variable does not override an explicit choice.
	driverSet bool

	// Models are migrated with AutoMigrate at construction, in
	// registration order.
	Models []any

	// Migrations are applied after AutoMigrate, in lexicographic
	// order by Version.
	Migrations []Migration

	// SkipMigrations suppresses both migration layers. Tests that
	// only exercise queries use this to boot faster.
	SkipMigrations bool

	// env is injected so tests can control lookups.
	env envLookup
}

// envLookup indirections environment access so that configuration
// resolution can be tested without mutating process state.
type envLookup func(string) (string, bool)

// Option configures a Config. Options are applied in order, so a later
// option overrides an earlier one, and every option beats the
// environment.
type Option func(*Config) error

// WithEnvPrefix sets the namespace for environment lookups. Passing an
// empty string leaves the existing prefix alone.
func WithEnvPrefix(prefix string) Option {
	return func(c *Config) error {
		if prefix != "" {
			c.EnvPrefix = strings.ToUpper(prefix)
		}
		return nil
	}
}

// WithDriver selects the database backend explicitly.
func WithDriver(d Driver) Option {
	return func(c *Config) error {
		c.Driver = d
		c.driverSet = true
		return nil
	}
}

// WithPath sets the SQLite database file.
func WithPath(path string) Option {
	return func(c *Config) error {
		c.Path = path
		return nil
	}
}

// WithDSN sets the PostgreSQL connection string.
func WithDSN(dsn string) Option {
	return func(c *Config) error {
		c.DSN = dsn
		return nil
	}
}

// WithPool configures the connection pool. Zero arguments keep the
// defaults, and maxIdle may not exceed maxOpen.
func WithPool(maxOpen, maxIdle int, lifetime time.Duration) Option {
	return func(c *Config) error {
		if maxOpen < 0 {
			return fmt.Errorf("db: max open connections must not be negative, got %d", maxOpen)
		}
		if maxIdle < 0 {
			return fmt.Errorf("db: max idle connections must not be negative, got %d", maxIdle)
		}
		if maxOpen > 0 && maxIdle > maxOpen {
			return fmt.Errorf("db: max idle connections (%d) may not exceed max open (%d)", maxIdle, maxOpen)
		}
		c.MaxOpenConns = maxOpen
		c.MaxIdleConns = maxIdle
		c.ConnMaxLifetime = lifetime
		return nil
	}
}

// WithSlowQuery sets the duration past which a query is logged as
// slow. A negative value disables slow-query logging.
func WithSlowQuery(d time.Duration) Option {
	return func(c *Config) error {
		c.SlowQuery = d
		return nil
	}
}

// WithAutoMigrate appends models to be migrated on every boot. The
// order is significant: a model referencing another model must be
// registered after it, because PostgreSQL validates foreign keys at
// table creation.
func WithAutoMigrate(models ...any) Option {
	return func(c *Config) error {
		c.Models = append(c.Models, models...)
		return nil
	}
}

// WithMigrations appends versioned migrations.
func WithMigrations(migrations ...Migration) Option {
	return func(c *Config) error {
		c.Migrations = append(c.Migrations, migrations...)
		return nil
	}
}

// WithoutMigrations suppresses both migration layers.
func WithoutMigrations() Option {
	return func(c *Config) error {
		c.SkipMigrations = true
		return nil
	}
}

// newConfig applies opts to a Config pre-populated with defaults,
// resolves environment fallbacks, and validates the result.
func newConfig(env envLookup, opts ...Option) (*Config, error) {
	if env == nil {
		env = osLookupEnv
	}
	c := &Config{
		EnvPrefix:       "WEBAPP",
		Driver:          DriverSQLite,
		ConnMaxLifetime: DefaultConnMaxLifetime,
		SlowQuery:       DefaultSlowQuery,
		env:             env,
	}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt(c); err != nil {
			return nil, err
		}
	}
	if c.env == nil {
		c.env = env
	}
	c.applyEnv()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// applyEnv fills any field the caller left unset from the environment.
// Explicit values always win, so an option that set a field causes the
// corresponding lookup to be skipped.
func (c *Config) applyEnv() {
	if !c.driverSet {
		if v, ok := c.env(c.EnvPrefix + suffixDriver); ok && v != "" {
			c.Driver = Driver(strings.ToLower(strings.TrimSpace(v)))
		}
	}
	if c.Path == "" {
		if v, ok := c.env(c.EnvPrefix + suffixPath); ok {
			c.Path = v
		}
	}
	if c.DSN == "" {
		if v, ok := c.env(c.EnvPrefix + suffixDSN); ok {
			c.DSN = v
		}
	}
	if c.Driver == DriverSQLite && c.Path == "" {
		c.Path = "webapp.db"
	}
}

// Validate reports every configuration problem at once rather than
// only the first, so an operator with three mistakes in their
// environment fixes them in one restart.
func (c *Config) Validate() error {
	var errs []error

	switch c.Driver {
	case DriverSQLite:
		if c.Path == "" {
			errs = append(errs, fmt.Errorf("no database path: set WithPath or %s%s", c.EnvPrefix, suffixPath))
		}
	case DriverPostgres:
		if c.DSN == "" {
			errs = append(errs, fmt.Errorf("postgres requires a DSN: set WithDSN or %s%s", c.EnvPrefix, suffixDSN))
		}
	case "":
		errs = append(errs, errors.New("no database driver selected"))
	default:
		errs = append(errs, fmt.Errorf("unsupported database driver %q (supported: sqlite, postgres)", string(c.Driver)))
	}

	if c.MaxIdleConns > c.MaxOpenConns && c.MaxOpenConns > 0 {
		errs = append(errs, fmt.Errorf("max idle connections (%d) may not exceed max open (%d)", c.MaxIdleConns, c.MaxOpenConns))
	}

	seen := make(map[string]struct{}, len(c.Migrations))
	for _, m := range c.Migrations {
		if strings.TrimSpace(m.Version) == "" {
			errs = append(errs, errors.New("migration has an empty Version; versions sort lexicographically and identify the migration"))
			continue
		}
		if m.Up == nil {
			errs = append(errs, fmt.Errorf("migration %q has a nil Up function", m.Version))
		}
		if _, dup := seen[m.Version]; dup {
			errs = append(errs, fmt.Errorf("migration version %q is registered more than once", m.Version))
		}
		seen[m.Version] = struct{}{}
	}

	return errors.Join(errs...)
}
