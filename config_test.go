package webapp

import (
	"strings"
	"testing"
	"time"

	"github.com/the-maldridge/webapp/db"
)

func TestConfigDefaults(t *testing.T) {
	c, err := newConfig(emptyEnv())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults are not valid: %v", err)
	}

	if c.EnvPrefix != DefaultEnvPrefix {
		t.Errorf("EnvPrefix = %q, want %q", c.EnvPrefix, DefaultEnvPrefix)
	}
	if c.Addr != DefaultAddr {
		t.Errorf("Addr = %q, want %q", c.Addr, DefaultAddr)
	}
	if c.Timeouts.ReadTimeout <= 0 || c.Timeouts.WriteTimeout <= 0 ||
		c.Timeouts.ReadHeaderTimeout <= 0 || c.Timeouts.IdleTimeout <= 0 {
		t.Errorf("every server timeout must default to non-zero, got %+v", c.Timeouts)
	}
	// Cookies are Secure unless debug says otherwise: the dangerous
	// default is the one where a deployment on TLS silently sends
	// session cookies in the clear.
	if !c.CookieSecure {
		t.Error("cookies should default to Secure")
	}
}

func TestConfigEnvironment(t *testing.T) {
	c, err := newConfig(mapEnv{
		"TASKS_ADDR":           "127.0.0.1:9999",
		"TASKS_DEBUG":          "true",
		"TASKS_COOKIE_SECURE":  "false",
		"TASKS_MODULES":        "a, b,,c",
		"TASKS_READ_TIMEOUT":   "1m30s",
		"TASKS_MAX_BODY_BYTES": "2048",
	}.lookup, WithEnvPrefix("tasks"))
	if err != nil {
		t.Fatal(err)
	}
	if c.EnvPrefix != "TASKS" {
		t.Errorf("prefix = %q, want upper-cased TASKS", c.EnvPrefix)
	}
	if c.Addr != "127.0.0.1:9999" {
		t.Errorf("Addr = %q", c.Addr)
	}
	if !c.Debug {
		t.Error("Debug should be true")
	}
	if c.CookieSecure {
		t.Error("explicit COOKIE_SECURE=false should be honoured")
	}
	if got := strings.Join(c.EnabledModules, ","); got != "a,b,c" {
		t.Errorf("modules = %q, want a,b,c", got)
	}
	if c.Timeouts.ReadTimeout != 90*time.Second {
		t.Errorf("ReadTimeout = %s", c.Timeouts.ReadTimeout)
	}
	if c.MaxBodyBytes != 2048 {
		t.Errorf("MaxBodyBytes = %d", c.MaxBodyBytes)
	}
}

func TestExplicitOptionBeatsEnvironment(t *testing.T) {
	c, err := newConfig(mapEnv{"WEBAPP_ADDR": "127.0.0.1:1"}.lookup, WithAddr("127.0.0.1:2"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != "127.0.0.1:2" {
		t.Errorf("Addr = %q, the environment must not override an option", c.Addr)
	}
}

func TestDebugImpliesInsecureCookiesUnlessSet(t *testing.T) {
	debugOnly, err := newConfig(mapEnv{"WEBAPP_DEBUG": "1"}.lookup)
	if err != nil {
		t.Fatal(err)
	}
	if debugOnly.CookieSecure {
		t.Error("debug over plain HTTP should not force Secure cookies")
	}

	explicit, err := newConfig(mapEnv{"WEBAPP_DEBUG": "1"}.lookup, WithCookieSecure(true))
	if err != nil {
		t.Fatal(err)
	}
	if !explicit.CookieSecure {
		t.Error("an explicit WithCookieSecure must survive debug mode")
	}
}

func TestUnparsableBooleanIsAnError(t *testing.T) {
	// "ture" is the kind of typo that, silently parsed as false,
	// produces a production-looking server that nobody configured.
	_, err := newConfig(mapEnv{"WEBAPP_DEBUG": "ture"}.lookup)
	if err == nil {
		t.Fatal("expected an error for a non-boolean value")
	}
	if !strings.Contains(err.Error(), "WEBAPP_DEBUG") {
		t.Errorf("error should name the variable: %v", err)
	}
}

func TestValidateAggregatesEveryProblem(t *testing.T) {
	c := &Config{
		EnvPrefix:  "has space",
		Addr:       "not-a-port",
		Debug:      true,
		Production: true,
		Timeouts:   Timeouts{ReadTimeout: time.Second, ReadHeaderTimeout: time.Minute},
	}

	err := c.Validate()
	if err == nil {
		t.Fatal("expected validation to fail")
	}
	for _, want := range []string{"prefix", "listen address", "debug and production", "shorter than read header"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("aggregated error missing %q; got %v", want, err)
		}
	}
}

func TestValidateRejectsBadProxyConfig(t *testing.T) {
	c := &Config{
		EnvPrefix:         "T",
		TrustedProxyCIDRs: []string{"not-a-cidr"},
		TrustedProxyCount: 2,
		MaxBodyBytes:      -1,
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected validation to fail")
	}
	for _, want := range []string{"not a CIDR", "both by count and by CIDR", "body size"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("aggregated error missing %q; got %v", want, err)
		}
	}
}

func TestWithTimeoutsRejectsNegative(t *testing.T) {
	err := WithTimeouts(Timeouts{WriteTimeout: -time.Second})(&Config{})
	if err == nil {
		t.Fatal("negative timeouts must be rejected at construction")
	}
}

func TestWithTrustedProxiesValidatesCIDR(t *testing.T) {
	if err := WithTrustedProxies("10.0.0.0/8")(&Config{}); err != nil {
		t.Errorf("valid CIDR rejected: %v", err)
	}
	if err := WithTrustedProxies("10.0.0.0/33")(&Config{}); err == nil {
		t.Error("invalid CIDR accepted")
	}
}

// mapEnv and emptyEnv keep configuration tests from reading the real
// process environment.
type mapEnv map[string]string

func (m mapEnv) lookup(key string) (string, bool) {
	v, ok := m[key]
	return v, ok
}

func emptyEnv() envLookup {
	return mapEnv{}.lookup
}

func TestNewPropagatesConfigErrors(t *testing.T) {
	_, err := New(WithAddr(":::nope"))
	if err == nil {
		t.Fatal("New should fail on an unusable address")
	}
	// The failure must be an error rather than an exit: an application
	// decides how to report a bad configuration and with what status.
	if !strings.Contains(err.Error(), "listen address") {
		t.Errorf("error should name the problem: %v", err)
	}
}

func TestNewFailsWhenDatabaseCannotOpen(t *testing.T) {
	_, err := New(
		WithEnvPrefix("TESTAPP"),
		WithDBOptions(db.WithDriver(db.DriverPostgres)),
	)
	if err == nil {
		t.Fatal("postgres without a DSN should fail construction")
	}
}
