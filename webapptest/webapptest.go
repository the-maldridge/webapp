/*
Package webapptest builds applications for tests.

A test application is annoying to construct by hand for reasons that have
nothing to do with the code under test: the database defaults to a relative
file, so a suite that forgets to override it leaves webapp.db in the source tree
and the next run shares it; sessions need a signing key; the auth tables have to
exist before an authenticated request works; and a real listener means ports,
timeouts, and the flaky subset of the suite. This package decides all of that
once, in ways a test should not have to repeat or get wrong.

	a	func TestListShowsOnlyMyTasks(t *testing.T) {
		a := webapptest.NewApp(t)
		a.InstallAuth()
		a.Account("alice", "correct horse battery staple", "tasks:view")

		a.Route("/ui/task", func(r webapp.Router) {
			r.Use(a.RequireLogin())
			r.Get("/", a.listTasks)
		})

		a.SignIn("alice")
		resp := a.Get("/ui/task/")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d, body: %s", resp.StatusCode, webapptest.Body(t, resp))
		}
	}

# What NewApp does

NewApp builds an application with a database in a temporary directory, migrated
before the test starts and removed when it finishes; an address nobody binds;
and logging discarded so failures show up in the test report rather than in
scrolling text. It is closed by t.Cleanup, so a test that fails halfway does not
hold a database open.

Authentication is opt-in because plenty of applications have no auth and a test
helper should not require it. InstallAuth constructs a service over the
application's own database, adds its tables, and installs it, so an
authenticated request in a test goes through the real middleware chain, the real
session codec, and the real CSRF check rather than a stub that agrees with the
implementation by accident.

# Driving requests

Client returns an *http.Client whose transport calls the application's handler
directly: no listener, no port, no sleep, and a suite that runs as fast as the
code inside it. Cookies are kept between requests, so a sign-in followed by five
page loads behaves the way a browser would.

The client does not follow redirects. The framework's answer to a successful
write is a 303, and a helper that silently followed it would turn "CreateTask
redirects to the list" into a test of the list page. Assert the Location, or
call Follow to land on the target.

# Identity in tests

SignIn gives the client a real session cookie for an identity, which is what a
route guard checks. The cheaper testas package stamps an identity into a request
context instead, which is enough to test a handler or a navigation fragment in
isolation but does not satisfy a guard; see that package's documentation for
why. When a test is about authorization on a running application, use SignIn.
*/
package webapptest

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/the-maldridge/webapp"
	"github.com/the-maldridge/webapp/auth"
	"github.com/the-maldridge/webapp/db"
)

// EnvTestPostgresDSN names the environment variable that switches a test
// application from SQLite to PostgreSQL.
//
// It exists because two behaviours are impossible to catch against one engine:
// foreign-key ordering, which SQLite defers and PostgreSQL enforces when a
// table is created, and identifier rules, where PostgreSQL reserves words
// SQLite accepts. A suite that only ever runs on SQLite passes right up until
// somebody deploys the other one.
//
// Pointing it at a database the suite may destroy is the intended use. The
// schema is not cleaned between runs, so a test asserting an exact row count
// should scope its own rows rather than the table.
const EnvTestPostgresDSN = "WEBAPPTEST_PG_DSN"

// testPrefix namespaces the environment of a test application so that a suite
// cannot accidentally pick up configuration from the application it is testing,
// and so that two suites running in one process cannot share a session key by
// sharing a variable.
const testPrefix = "WEBAPPTEST"

// testSessionKey is a fixed signing key. Sessions minted by one statement in a
// test are readable in the next, which is the property a test of a
// cookie-authenticated flow actually needs; the key's entropy is irrelevant
// because nothing here is deployed and nothing here outlives the process.
const testSessionKey = "0000111122223333444455556666777788889999aaaabbbbccccddddeeeeffff"

// App is a test application: a real webapp.App with a disposable database, a
// client that reaches it without a network, and the few operations a test needs
// in order to arrive at an interesting state.
//
// The embedded *webapp.App is the application itself, so routes, renders,
// configuration, and the database handle are reached through it directly.
type App struct {
	*webapp.App

	// T is the testing.T the application was built for.
	T *testing.T

	// Auth is the installed authentication service, or nil when the test
	// never asked for one.
	Auth *auth.Service

	// client is the cookie-keeping, handler-driven HTTP client lazily
	// wrapped around the handler.
	client *http.Client

	// cookies is the jar holding that browser state: the session and CSRF
	// cookies a test's requests carry.
	cookies *cookiejar.Jar
}

// NewApp builds an application for a test.
//
// Options are ordinary webapp options and are applied after the test defaults,
// so a test overrides what it cares about and inherits the rest. The ones worth
// knowing about are webapp.WithTemplates to mount a template namespace and
// webapp.WithModule or webapp.WithComponent to exercise the module registry.
//
// The database is a file in t.TempDir unless EnvTestPostgresDSN is set.
// Migrations run, including the ones contributed by components, because a test
// that skips the step that creates its tables is testing a state the application
// can never be in.
func NewApp(t *testing.T, opts ...webapp.Option) *App {
	t.Helper()

	base := []webapp.Option{
		webapp.WithEnvPrefix(testPrefix),
		webapp.WithAddr("127.0.0.1:0"),
		webapp.WithLogger(discardLogger()),
	}

	dbOpts := []db.Option{}
	if dsn := os.Getenv(EnvTestPostgresDSN); dsn != "" {
		dbOpts = append(dbOpts, db.WithDriver(db.DriverPostgres), db.WithDSN(dsn))
	} else {
		dbOpts = append(dbOpts, db.WithPath(filepath.Join(t.TempDir(), "test.db")))
	}
	base = append(base, webapp.WithDBOptions(dbOpts...))

	app, err := webapp.New(append(base, opts...)...)
	if err != nil {
		t.Fatalf("webapp.New: %v", err)
	}
	if app.Handler == nil {
		t.Fatalf("webapp.New returned an application with no handler")
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Errorf("close test application: %v", err)
		}
	})

	return &App{App: app, T: t}
}

// InstallAuth builds an authentication service over the application's database
// and installs it, returning the service.
//
// The tables are added through RegisterComponent, so this works after
// construction, which is the only order available, since a service needs a
// database handle and the application owns it. Any auth.Option is applied last,
// after the test session key, so a test can substitute a mechanism or a clock
// without fighting the defaults.
func (a *App) InstallAuth(opts ...auth.Option) *auth.Service {
	a.T.Helper()

	if a.Auth != nil {
		a.T.Fatal("InstallAuth called twice; an application has one identity")
	}

	svc, err := auth.New(a.DB, append([]auth.Option{
		auth.WithSessionKey(testSessionKey),
		auth.WithLogger(discardLogger()),
	}, opts...)...)
	if err != nil {
		a.T.Fatalf("auth.New: %v", err)
	}

	// The service is a Component as well as a service: its models go through
	// the same path an application's would, which is what puts the accounts,
	// groups, tokens, and profiles tables in the same database the handlers
	// query.
	if err := a.RegisterComponent(svc); err != nil {
		a.T.Fatalf("register auth models: %v", err)
	}
	if err := a.App.InstallAuth(svc); err != nil {
		a.T.Fatalf("InstallAuth: %v", err)
	}

	a.Auth = svc
	return svc
}

// Account creates a local account with a password and group memberships.
//
// It is the fast way to reach an authorised state: no signup flow has to exist,
// because the framework has no registration endpoint to reach. A test that is
// about the signup flow should build it and use it.
//
// A failing password hash makes the test stop here rather than produce an
// account that cannot authenticate, because the resulting confusion, "my
// credentials are correct and I am still refused", costs more than this check.
func (a *App) Account(identity, password string, groups ...string) *auth.Account {
	a.T.Helper()
	a.mustAuth()

	acct, err := a.Auth.CreateAccount(a.T.Context(), identity, password)
	if err != nil {
		a.T.Fatalf("create account %q: %v", identity, err)
	}
	for _, g := range groups {
		if err := a.Auth.Grant(a.T.Context(), identity, g); err != nil {
			a.T.Fatalf("grant %q to %q: %v", g, identity, err)
		}
	}
	return acct
}

// SignIn gives the client a valid session for an identity with the given
// groups.
//
// The cookie is produced by the service itself rather than assembled here, so
// its name, attributes, and format are the ones the middleware actually accepts.
// No account has to exist: a session is an assertion about an identity, and
// inventing one for a test is exactly what the signing key is for.
//
// This is what satisfies a route guard. For a handler tested on its own,
// testas.As is cheaper.
func (a *App) SignIn(identity string, groups ...string) {
	a.T.Helper()
	a.mustAuth()

	session := auth.User{Identity: identity, Groups: groupSet(groups), AuthedBy: "webapptest"}
	res := a.issue(session)

	cookies := res.Cookies()
	if len(cookies) == 0 {
		a.T.Fatalf("issuing a session for %q set no cookie", identity)
	}
	a.jar().SetCookies(baseURL(""), cookies)
}

// SignOut discards the client's cookies, which is what signing out looks like
// to the client that has to do it again afterwards.
func (a *App) SignOut() {
	a.T.Helper()

	a.cookies = newJar(a.T)
	if a.client != nil {
		a.client.Jar = a.cookies
	}
}

// mustAuth stops a test that asked for an account or a session without
// installing authentication, naming the call that fixes it. The alternative is
// a nil-pointer failure inside this package, which reads like a bug in the
// helper rather than a missing line in the test.
func (a *App) mustAuth() {
	a.T.Helper()
	if a.Auth == nil {
		a.T.Fatal("this test needs authentication: call InstallAuth first")
	}
}

// issue mints a session cookie through the service and returns the response it
// was written into, so the caller can read the Set-Cookie headers a browser
// would have received.
func (a *App) issue(user auth.User) *http.Response {
	a.T.Helper()
	a.mustAuth()

	rec := httptest.NewRecorder()
	if err := a.Auth.Issue(rec, user); err != nil {
		a.T.Fatalf("issue session for %q: %v", user.Identity, err)
	}
	return rec.Result()
}

// groupSet turns group names into the set the session carries. Names are
// upper-cased because permission matching is, and a test that wrote
// "tasks:view" and got a membership that never matched would be debugging the
// helper instead of the application.
func groupSet(names []string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[upperASCII(name)] = struct{}{}
	}
	return set
}

// upperASCII upper-cases ASCII letters without pulling in the locale-aware
// mapping, so a group name behaves identically here and in the permission
// parser.
func upperASCII(s string) string {
	const delta = 'a' - 'A'
	out := []byte(s)
	for i, c := range out {
		if c >= 'a' && c <= 'z' {
			out[i] = c - delta
		}
	}
	return string(out)
}

// discardLogger sends log output nowhere. Test output is where a failure
// explanation belongs, and a framework logger writing to stderr buries it.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
