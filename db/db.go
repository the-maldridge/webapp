package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// DB is a database handle. It embeds *gorm.DB, so every GORM method is
// available directly and an application that needs to hand a handle to
// a library expecting GORM can pass d.DB or d.Raw().
type DB struct {
	*gorm.DB

	config *Config
}

// New opens the database described by opts and brings it up to date.
//
// The steps, in order: resolve and validate configuration, open the
// connection pool, apply driver pragmas and pool limits, run
// AutoMigrate for the registered models, then run pending versioned
// migrations. Any failure is returned; New never exits the process and
// never logs a secret.
func New(opts ...Option) (*DB, error) {
	return newWithEnv(nil, opts...)
}

// newWithEnv is New with an injected environment, used by tests.
func newWithEnv(env envLookup, opts ...Option) (*DB, error) {
	config, err := newConfig(env, opts...)
	if err != nil {
		return nil, err
	}

	gormConfig := &gorm.Config{
		// Prepared statements are cached per connection. The access
		// pattern these applications have is the same handful of
		// queries repeated, so the cache almost always pays.
		PrepareStmt: true,
		// Single-row writes are wrapped in an implicit transaction by
		// default. PostgreSQL is markedly slower for that and SQLite
		// gains nothing, so the wrapper is opt-in per call site via
		// Transaction.
		SkipDefaultTransaction: true,
		// Context cancellation must actually cancel the query, which
		// requires propagating values into the statement.
		TranslateError: false,
	}
	if q := config.SlowQuery; q >= 0 {
		gormConfig.Logger = newSlogLogger(q)
	}

	var (
		handle  *gorm.DB
		openErr error
	)
	switch config.Driver {
	case DriverSQLite:
		handle, openErr = gorm.Open(sqlite.Open(sqliteDSN(config.Path)), gormConfig)
	case DriverPostgres:
		handle, openErr = gorm.Open(postgres.Open(config.DSN), gormConfig)
	default:
		return nil, fmt.Errorf("db: unsupported database driver %q", string(config.Driver))
	}
	if openErr != nil {
		return nil, fmt.Errorf("db: could not open %s database: %w", config.Driver, openErr)
	}

	d := &DB{DB: handle, config: config}
	if err := d.configurePool(); err != nil {
		return nil, err
	}
	if err := d.applyMigrations(context.Background()); err != nil {
		return nil, err
	}

	slog.Debug("db: database ready", "driver", string(config.Driver), "pool_max_open", d.maxOpen())
	return d, nil
}

// sqliteDSN returns path with the pragmas every SQLite deployment
// needs appended.
//
// The parameter names belong to the mattn/go-sqlite3 driver used by
// gorm.io/driver/sqlite, which recognizes a fixed set of DSN keys
// rather than a generic pragma mechanism.
//
// Without these three, the defaults are actively harmful for a web
// application: journal mode DELETE means a read and a write cannot
// interleave, the default busy timeout of zero means concurrent writers
// fail immediately with SQLITE_BUSY instead of waiting, and foreign key
// enforcement is off per connection, so every declared relation in the
// schema is silently unenforced.
func sqliteDSN(path string) string {
	params := url.Values{}
	params.Set("_journal_mode", "WAL")
	params.Set("_busy_timeout", "5000")
	params.Set("_foreign_keys", "ON")
	params.Set("_txlock", "immediate")

	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	return path + separator + params.Encode()
}

// configurePool applies connection pool limits. Unbounded pools let a
// traffic spike exhaust database-side connection limits, and an
// unlimited lifetime prevents the application from recovering from
// database-side connection recycling.
func (d *DB) configurePool() error {
	sqlDB, err := d.DB.DB()
	if err != nil {
		return fmt.Errorf("db: could not reach the connection pool: %w", err)
	}

	maxOpen := d.config.MaxOpenConns
	if maxOpen <= 0 {
		maxOpen = DefaultPoolSize
	}
	maxIdle := d.config.MaxIdleConns
	if maxIdle <= 0 {
		maxIdle = maxOpen / 2
		if maxIdle < 1 {
			maxIdle = 1
		}
	}
	lifetime := d.config.ConnMaxLifetime
	if lifetime == 0 {
		lifetime = DefaultConnMaxLifetime
	}

	sqlDB.SetMaxOpenConns(maxOpen)
	sqlDB.SetMaxIdleConns(maxIdle)
	sqlDB.SetConnMaxLifetime(lifetime)
	return nil
}

func (d *DB) maxOpen() int {
	if d.config.MaxOpenConns > 0 {
		return d.config.MaxOpenConns
	}
	return DefaultPoolSize
}

// Raw returns the underlying *gorm.DB. The embedded field usually makes
// this unnecessary; Raw exists so the intent is explicit at call sites
// that hand the handle to code expecting plain GORM.
func (d *DB) Raw() *gorm.DB {
	if d == nil {
		return nil
	}
	return d.DB
}

// Driver reports which backend this handle talks to.
func (d *DB) Driver() Driver {
	if d == nil {
		return ""
	}
	return d.config.Driver
}

// SQL returns the pooled database handle for callers that need
// database/sql directly, for example to register a metrics collector.
func (d *DB) SQL() (*sql.DB, error) {
	if d == nil {
		return nil, ErrNoDriver
	}
	return d.DB.DB()
}

// Close releases the connection pool.
func (d *DB) Close() error {
	if d == nil {
		return nil
	}
	sqlDB, err := d.DB.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// ErrNoDriver reports a use of an uninitialized or closed handle.
var ErrNoDriver = errors.New("db: database handle is not initialized")
