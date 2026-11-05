package db

import (
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

// mapEnv returns an environment lookup backed by m.
func mapEnv(m map[string]string) envLookup {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestPrecedenceOptionBeatsEnvBeatsDefault(t *testing.T) {
	env := mapEnv(map[string]string{
		"TASKS_DB_DRIVER": "postgres",
		"TASKS_DB":        "/env/path.db",
		"TASKS_DB_DSN":    "postgres://env/host",
	})

	t.Run("default", func(t *testing.T) {
		c, err := newConfig(mapEnv(nil))
		if err != nil {
			t.Fatal(err)
		}
		if c.Driver != DriverSQLite {
			t.Errorf("driver = %q, want sqlite", c.Driver)
		}
		if c.Path != "webapp.db" {
			t.Errorf("path = %q, want webapp.db", c.Path)
		}
	})

	t.Run("env", func(t *testing.T) {
		c, err := newConfig(env, WithEnvPrefix("TASKS"))
		if err != nil {
			t.Fatal(err)
		}
		if c.Driver != DriverPostgres {
			t.Errorf("driver = %q, want postgres", c.Driver)
		}
		if c.DSN != "postgres://env/host" {
			t.Errorf("dsn = %q, want the env value", c.DSN)
		}
	})

	t.Run("option", func(t *testing.T) {
		c, err := newConfig(env, WithEnvPrefix("TASKS"), WithPath("/opt/path.db"), WithDriver(DriverSQLite))
		if err != nil {
			t.Fatal(err)
		}
		if c.Driver != DriverSQLite {
			t.Errorf("driver = %q, want the sqlite option to win", c.Driver)
		}
		if c.Path != "/opt/path.db" {
			t.Errorf("path = %q, want the option value", c.Path)
		}
	})
}

func TestValidateReportsEveryProblem(t *testing.T) {
	c := &Config{
		EnvPrefix: "TASKS",
		Driver:    Driver("mysql"),
		Migrations: []Migration{
			{Version: "001", Up: nil},
			{Version: "001", Up: func(*gorm.DB) error { return nil }},
			{Version: "", Up: nil},
		},
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{"mysql", "001", "empty Version"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

func TestValidatePostgresRequiresDSN(t *testing.T) {
	c := &Config{EnvPrefix: "TASKS", Driver: DriverPostgres}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected an error for postgres without a DSN")
	}
	if !strings.Contains(err.Error(), "TASKS_DB_DSN") {
		t.Errorf("error %q should name the environment variable to set", err)
	}
}

func TestValidateRejectsInvalidPool(t *testing.T) {
	if err := newConfigMust(t, WithPool(2, 8, time.Hour)); err == nil {
		t.Fatal("expected an error when max idle exceeds max open")
	}
	if err := newConfigMust(t, WithPool(-1, 0, 0)); err == nil {
		t.Fatal("expected an error for a negative max open")
	}
	if err := newConfigMust(t, WithPool(4, 2, time.Minute)); err != nil {
		t.Errorf("unexpected error for a valid pool: %v", err)
	}
}

// newConfigMust applies opts and returns the validation error, or nil.
func newConfigMust(t *testing.T, opts ...Option) error {
	t.Helper()
	_, err := newConfig(mapEnv(nil), append([]Option{WithPath("x.db")}, opts...)...)
	return err
}

func TestSqliteDSNAddsPragmas(t *testing.T) {
	got := sqliteDSN("/data/app.db")
	for _, want := range []string{
		"_journal_mode=WAL",
		"_busy_timeout=5000",
		"_foreign_keys=ON",
		"_txlock=immediate",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dsn %q missing %q", got, want)
		}
	}
	if !strings.HasPrefix(got, "/data/app.db?") {
		t.Errorf("dsn %q should append parameters to the path", got)
	}
}

// A path that already carries a query string must be joined with &
// rather than a second ?, which would corrupt the first parameter list.
func TestSqliteDSNJoinsExistingQuery(t *testing.T) {
	got := sqliteDSN("/data/app.db?_txlock=deferred")
	if strings.Count(got, "?") != 1 {
		t.Errorf("dsn %q has more than one ?", got)
	}
	if !strings.Contains(got, "_txlock=deferred&_busy_timeout=5000") &&
		!strings.Contains(got, "&_busy_timeout=5000") {
		t.Errorf("dsn %q did not append with &", got)
	}
}

func TestNewRejectsUnsupportedDriver(t *testing.T) {
	_, err := newWithEnv(mapEnv(nil), WithDriver(Driver("oracle")))
	if err == nil {
		t.Fatal("expected an error for an unsupported driver")
	}
	if !strings.Contains(err.Error(), "oracle") {
		t.Errorf("error %q should name the driver", err)
	}
}

func TestConfigValidateNamesMissingPath(t *testing.T) {
	c := &Config{EnvPrefix: "APP", Driver: DriverSQLite}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "APP_DB") {
		t.Errorf("error %q should name APP_DB", err)
	}
}
