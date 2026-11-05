package auth

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/the-maldridge/webapp/db"
)

// newTestDB opens a temporary SQLite database carrying the auth schema.
//
// The models are migrated through the real path rather than by hand, so a
// change to a TableName or an index is caught by every test in this package
// rather than by the one that looks at the schema.
func newTestDB(t *testing.T) *db.DB {
	t.Helper()

	handle, err := db.New(
		db.WithDriver(db.DriverSQLite),
		db.WithPath(filepath.Join(t.TempDir(), "auth.db")),
		db.WithAutoMigrate(Account{}, Group{}, APIToken{}, Profile{}),
	)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() {
		if err := handle.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return handle
}

// newTestService builds a Service over a fresh database with an explicit key,
// so sessions are stable across a test's requests.
func newTestService(t *testing.T, opts ...Option) *Service {
	t.Helper()
	return newTestServiceOn(t, newTestDB(t), opts...)
}

// newTestServiceOn builds a Service over a caller-supplied database, for tests
// that share a handle between two services.
func newTestServiceOn(t *testing.T, handle *db.DB, opts ...Option) *Service {
	t.Helper()

	base := []Option{
		WithSessionKey(testSessionKey),
		WithLogger(discardLogger()),
		withEnv(stubEnv),
	}
	base = append(base, opts...)

	svc, err := New(handle, base...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc
}

// testSessionKey is a fixed 64-hex-character key. Tests use a constant so a
// session sealed in one statement opens in the next.
const testSessionKey = "0000111122223333444455556666777788889999aaaabbbbccccddddeeeeffff"

// testPassword satisfies minPasswordLength.
const testPassword = "correct horse battery staple"

// stubEnv reads nothing, so a test running where AUTHWARE_SESSION_KEY happens
// to be set behaves like every other test.
func stubEnv(string) (string, bool) { return "", false }

// envReturning serves a fixed table.
func envReturning(values map[string]string) envLookup {
	return func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
}

// discardLogger keeps the package's logging out of test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// serve runs one request through a handler and returns the recorder.
func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// okHandler is the downstream handler: it reports that it was reached.
func okHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}
}

// responseCookie returns a cookie from a response by name.
func responseCookie(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()

	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no cookie %q in response; Set-Cookie: %v", name, rec.Result().Header["Set-Cookie"])
	return nil
}

// setCookieHeader renders cookies as a request header.
func setCookieHeader(cookies ...*http.Cookie) string {
	parts := make([]string, 0, len(cookies))
	for _, c := range cookies {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

// newCSRFFor mints a token the middleware accepts for an identity.
func newCSRFFor(t *testing.T, svc *Service, identity string) string {
	t.Helper()

	token, err := newCSRFToken(svc.csrfKey, identity, svc.now().Add(time.Hour))
	if err != nil {
		t.Fatalf("newCSRFToken: %v", err)
	}
	return token
}

// codecFor builds a codec over a key without going through New.
func codecFor(t *testing.T, key string) *sessionCodec {
	t.Helper()

	codec, err := newSessionCodec(key)
	if err != nil {
		t.Fatalf("newSessionCodec: %v", err)
	}
	return codec
}

// sealSession produces a cookie value for a user, the way Issue does.
func sealSession(t *testing.T, key string, u User, expires time.Time) string {
	t.Helper()

	value, err := codecFor(t, key).seal(Session{Expires: expires, User: u})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	return value
}

// mustCreateAccount creates an account, failing the test on any error.
func mustCreateAccount(t *testing.T, svc *Service, identity, password string) *Account {
	t.Helper()

	acct, err := svc.CreateAccount(context.Background(), identity, password)
	if err != nil {
		t.Fatalf("CreateAccount(%q): %v", identity, err)
	}
	return acct
}

// mustGrant adds a group membership, failing the test on any error.
func mustGrant(t *testing.T, svc *Service, identity, group string) {
	t.Helper()

	if err := svc.Grant(context.Background(), identity, group); err != nil {
		t.Fatalf("Grant(%q, %q): %v", identity, group, err)
	}
}

// base64Identity is how a CSRF payload stores an identity, defined once so the
// tests do not each repeat the encoding.
func base64Identity(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}
