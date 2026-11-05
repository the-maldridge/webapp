package webapp

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/the-maldridge/webapp/db"
)

// baseTestOptions is the configuration every test application shares: a
// temporary database, a discard logger, an address nobody binds. It is a
// function rather than a literal in each helper so that a test constructing an
// application that is expected to fail still cannot leave a database file
// behind it in the source tree.
func baseTestOptions(t *testing.T, opts ...Option) []Option {
	t.Helper()

	base := []Option{
		WithEnvPrefix("TESTAPP"),
		WithAddr("127.0.0.1:0"),
		WithLogger(discardLogger()),
		WithDBOptions(
			db.WithPath(filepath.Join(t.TempDir(), "test.db")),
			db.WithoutMigrations(),
		),
	}
	return append(base, opts...)
}

// newTestApp builds an application backed by a temporary SQLite database
// with no routes registered. Tests add routes and drive a.Handler through
// httptest, so no port, no listener, and no sleeps are involved.
func newTestApp(t *testing.T, opts ...Option) *App {
	t.Helper()

	a, err := New(baseTestOptions(t, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.Handler == nil {
		t.Fatal("New returned an application with no handler")
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	return a
}

// send drives one request through the whole handler chain.
func (a *App) send(t *testing.T, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	a.Handler.ServeHTTP(rec, req)
	return rec
}

// get is the shorthand nearly every test wants.
func (a *App) get(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()

	return a.send(t, httptest.NewRequest(http.MethodGet, target, nil))
}

// postForm sends an urlencoded body.
func (a *App) postForm(t *testing.T, target, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return a.send(t, req)
}

// discardLogger sends log output nowhere, so test output stays readable.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// recorded is one log line captured by recorder.
type recorded struct {
	level   slog.Level
	message string
	attrs   map[string]any
}

// recorder is a slog handler that keeps every record, for tests about what
// was reported at what level.
type recorder struct {
	mu      sync.Mutex
	records []recorded
}

func (r *recorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *recorder) Handle(ctx context.Context, rec slog.Record) error {
	attrs := map[string]any{}
	rec.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})

	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, recorded{level: rec.Level, message: rec.Message, attrs: attrs})
	return nil
}

func (r *recorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *recorder) WithGroup(string) slog.Handler      { return r }

// atLevels returns the levels of every captured record with the given
// message.
func (r *recorder) atLevels(message string) []slog.Level {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []slog.Level
	for _, rec := range r.records {
		if rec.message == message {
			out = append(out, rec.level)
		}
	}
	return out
}

// find returns the first captured record with the given message.
func (r *recorder) find(message string) (recorded, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, rec := range r.records {
		if rec.message == message {
			return rec, true
		}
	}
	return recorded{}, false
}

// slogWith builds a logger that writes into a recorder.
func slogWith(r *recorder) *slog.Logger { return slog.New(r) }

// acceptHeader builds a request carrying only an Accept header, for tests
// about content negotiation.
func acceptHeader(accept string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	return req
}

// timeoutAfterTest is a channel that fires if a test taking a channel
// timeout gets stuck, so a hang fails with a useful message instead of the
// whole package timing out.
func timeoutAfterTest() <-chan time.Time { return time.After(5 * time.Second) }

// stubRenderer records what was rendered and can be made to fail.
type stubRenderer struct {
	// name and data capture the last render.
	name string
	data Context

	// err is returned instead of writing anything, simulating a template
	// execution failure. prefix is written first, to simulate partial
	// output already on its way to the client.
	err    error
	prefix string
}

func (s *stubRenderer) Render(_ context.Context, out io.Writer, name string, data Context) error {
	s.name = name
	s.data = data
	if s.err != nil {
		if s.prefix != "" {
			_, _ = io.WriteString(out, s.prefix)
		}
		return s.err
	}
	_, err := io.WriteString(out, "rendered:"+name)
	return err
}
