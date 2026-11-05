package webapp

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/the-maldridge/webapp/auth"
	"github.com/the-maldridge/webapp/db"
)

// coreTestSessionKey is a fixed key so a session issued in one statement is
// readable in the next.
const coreTestSessionKey = "0000111122223333444455556666777788889999aaaabbbbccccddddeeeeffff"

// newCoreAuthService opens a database carrying the auth schema and builds a
// Service over it.
//
// The service keeps its own handle rather than sharing the application's
// because the two are constructed in opposite orders: the Service needs a
// database before the application has one, and InstallAuth deliberately does
// not care which handle an application uses.
func newCoreAuthService(t *testing.T, opts ...auth.Option) *auth.Service {
	t.Helper()

	handle, err := db.New(
		db.WithDriver(db.DriverSQLite),
		db.WithPath(filepath.Join(t.TempDir(), "auth.db")),
		db.WithAutoMigrate(auth.Account{}, auth.Group{}, auth.APIToken{}, auth.Profile{}),
	)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() {
		if err := handle.Close(); err != nil {
			t.Errorf("close auth db: %v", err)
		}
	})

	base := []auth.Option{
		auth.WithSessionKey(coreTestSessionKey),
		auth.WithLogger(discardLogger()),
	}
	svc, err := auth.New(handle, append(base, opts...)...)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	return svc
}

// installAuth wires a service into a test application, failing the test when
// installation itself fails.
func installAuth(t *testing.T, a *App, svc *auth.Service) {
	t.Helper()

	if err := a.InstallAuth(svc); err != nil {
		t.Fatalf("InstallAuth: %v", err)
	}
}

// sessionCookieFor issues a session cookie for a user through the service's
// own code path, so the tests exercise the real attributes rather than a
// hand-built approximation.
func sessionCookieFor(t *testing.T, svc *auth.Service, identity string, groups ...string) *http.Cookie {
	t.Helper()

	member := map[string]struct{}{}
	for _, g := range groups {
		member[g] = struct{}{}
	}

	rec := httptest.NewRecorder()
	if err := svc.Issue(rec, auth.User{Identity: identity, Groups: member, AuthedBy: "database"}); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == svc.CookieName() {
			return c
		}
	}
	t.Fatalf("no %q cookie issued; Set-Cookie: %v", svc.CookieName(), rec.Result().Header["Set-Cookie"])
	return nil
}

func TestInstallAuthRejectsNil(t *testing.T) {
	a := newTestApp(t)

	if err := a.InstallAuth(nil); err == nil {
		t.Fatal("InstallAuth(nil) must fail: an application that believes it has auth and does not is the worst outcome")
	}
	if a.AuthService() != nil {
		t.Error("a failed install must not leave a service behind")
	}
}

func TestInstallAuthRejectsDoubleInstall(t *testing.T) {
	a := newTestApp(t)
	svc := newCoreAuthService(t)

	installAuth(t, a, svc)

	if err := a.InstallAuth(newCoreAuthService(t)); err == nil {
		t.Fatal("installing a second identity must fail")
	}
	if a.AuthService() != svc {
		t.Error("the first service should still be installed")
	}
}

func TestInstallAuthRefusesGeneratedKeyInProduction(t *testing.T) {
	// t.Setenv keeps the test from racing with others, and the variable must
	// be absent rather than empty: an empty value is not a configured key.
	t.Setenv(auth.EnvSessionKey, "")

	handle, err := db.New(
		db.WithDriver(db.DriverSQLite),
		db.WithPath(filepath.Join(t.TempDir(), "prod.db")),
		db.WithAutoMigrate(auth.Account{}, auth.Group{}, auth.APIToken{}, auth.Profile{}),
	)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	defer handle.Close()

	// A service with no configured key still constructs outside production,
	// generating one for the process.
	svc, err := auth.New(handle, auth.WithLogger(discardLogger()))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	if svc.SessionKeyConfigured() {
		t.Fatal("setup: this service is supposed to have a generated key")
	}

	a := newTestApp(t, WithProduction())
	err = a.InstallAuth(svc)
	if err == nil {
		t.Fatal("a production boot with a per-process session key must fail")
	}
	if !strings.Contains(err.Error(), auth.EnvSessionKey) {
		t.Errorf("err = %v, want it to name the variable to set", err)
	}
}

func TestInstallAuthAcceptsConfiguredKeyInProduction(t *testing.T) {
	t.Setenv(auth.EnvSessionKey, coreTestSessionKey)

	a := newTestApp(t, WithProduction())
	svc := newCoreAuthService(t)
	installAuth(t, a, svc)
}

func TestCookieNameCarriesEnvPrefix(t *testing.T) {
	svc := newCoreAuthService(t)
	if got := svc.CookieName(); got != "session" {
		t.Fatalf("before install, cookie name = %q, want the package default", got)
	}

	a := newTestApp(t)
	installAuth(t, a, svc)

	if got := svc.CookieName(); got != "testapp_session" {
		t.Errorf("cookie name = %q, want it prefixed so two applications on one host stay separate", got)
	}
}

func TestCookieNameUnprefixedAtDefaultPrefix(t *testing.T) {
	svc := newCoreAuthService(t)
	installAuth(t, newTestApp(t, WithEnvPrefix("WEBAPP")), svc)

	if got := svc.CookieName(); got != "session" {
		t.Errorf("cookie name = %q, want the plain default at the default prefix", got)
	}
}

func TestInstallAuthMarksCookiesSecure(t *testing.T) {
	svc := newCoreAuthService(t)
	a := newTestApp(t)
	installAuth(t, a, svc)

	rec := httptest.NewRecorder()
	if err := svc.Issue(rec, auth.User{Identity: "ada"}); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	cookie := sessionCookieFor(t, svc, "ada")
	if !cookie.Secure {
		t.Error("cookies should default to Secure; debug is what turns it off")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax stated explicitly rather than inherited", cookie.SameSite)
	}
	if !cookie.HttpOnly {
		t.Error("the session cookie must not be readable by scripts")
	}
}

func TestInstallAuthDebugSendsInsecureCookies(t *testing.T) {
	svc := newCoreAuthService(t)
	installAuth(t, newTestApp(t, WithDebug()), svc)

	cookie := sessionCookieFor(t, svc, "ada")
	if cookie.Secure {
		t.Error("debug mode is the explicit signal for plain-HTTP local development")
	}
}

func TestGuardedWithoutServiceIsInternalServerError(t *testing.T) {
	a := newTestApp(t)

	a.Handle("/secret", a.Guarded(auth.P("tasks", "edit"), okHandler))

	rec := a.get(t, "/secret")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: a gate with no service is a configuration bug, not an open route", rec.Code)
	}
}

func TestRequireLoginWithoutServiceDenies(t *testing.T) {
	a := newTestApp(t)

	a.Handle("/secret", a.RequireLogin()(http.HandlerFunc(okHandler)).ServeHTTP)

	if rec := a.get(t, "/secret"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestGuardedRouteRefusesAnonymous(t *testing.T) {
	a := newTestApp(t)
	svc := newCoreAuthService(t)
	installAuth(t, a, svc)

	a.Route("/admin", func(r Router) {
		r.Get("/", r.Guard(auth.P("tasks", "edit"), okHandler))
	})

	rec := a.get(t, "/admin")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got == "" {
		t.Error("a 401 must say how to authenticate")
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Error("the protected handler's body leaked")
	}
}

func TestGuardedRouteAnswersJSONToAPIClients(t *testing.T) {
	a := newTestApp(t)
	svc := newCoreAuthService(t)
	installAuth(t, a, svc)

	a.Route("/admin", func(r Router) {
		r.Get("/", r.Guard(auth.P("tasks", "edit"), okHandler))
	})

	rec := a.send(t, withAccept(httptest.NewRequest(http.MethodGet, "/admin", nil), "application/json"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want JSON for a client that asked for JSON", got)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %q", rec.Body.String())
	}
	if body["error"] == "" {
		t.Errorf("body = %+v, want an error field", body)
	}
}

func TestGuardRouteRefusesWrongGroup(t *testing.T) {
	a := newTestApp(t)
	svc := newCoreAuthService(t)
	installAuth(t, a, svc)

	a.Route("/admin", func(r Router) {
		r.Get("/", r.RequireLogin(okHandler))
	})
	a.Route("/edit", func(r Router) {
		r.Get("/", r.Guard(auth.P("tasks", "edit"), okHandler))
	})

	viewer := withCookie(httptest.NewRequest(http.MethodGet, "/edit", nil),
		sessionCookieFor(t, svc, "ada", "TASKS:VIEW"))
	rec := a.send(t, viewer)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	// The required grant is named outward on purpose: the person asking
	// already knows who they are, and "forbidden" is a support ticket.
	if got := rec.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("WWW-Authenticate = %q, want none on a 403", got)
	}
}

func TestGuardRouteAdmitsHolder(t *testing.T) {
	a := newTestApp(t)
	svc := newCoreAuthService(t)
	installAuth(t, a, svc)

	a.Route("/edit", func(r Router) {
		r.Get("/", r.Guard(auth.P("tasks", "edit"), okHandler))
	})

	holder := withCookie(httptest.NewRequest(http.MethodGet, "/edit", nil),
		sessionCookieFor(t, svc, "ada", "TASKS:EDIT"))
	rec := a.send(t, holder)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestRequireLoginRouteAdmitsAnyIdentity(t *testing.T) {
	a := newTestApp(t)
	svc := newCoreAuthService(t)
	installAuth(t, a, svc)

	a.Route("/me", func(r Router) {
		r.Get("/", r.RequireLogin(okHandler))
	})

	rec := a.send(t, withCookie(httptest.NewRequest(http.MethodGet, "/me", nil),
		sessionCookieFor(t, svc, "ada")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestPublicRouteNeedsNoIdentity(t *testing.T) {
	a := newTestApp(t)
	svc := newCoreAuthService(t)
	installAuth(t, a, svc)

	a.Route("/open", func(r Router) {
		r.Get("/", r.Public(okHandler))
	})

	if rec := a.get(t, "/open"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestUseAppendsToChainAroundRouter(t *testing.T) {
	a := newTestApp(t)

	// Appended on purpose: the chain has to be recomposable after
	// construction, which is the whole reason App.Use exists instead of chi's
	// Use. The route below is registered first to prove that.
	a.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Appended", "yes")
			next.ServeHTTP(w, r)
		})
	})

	// A route that does not exist still has to run the chain: a 404 emitted by
	// the not-found handler with no middleware applied would mean appended
	// middleware silently does nothing for half the traffic.
	rec := a.get(t, "/nothing-here")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("X-Appended"); got != "yes" {
		t.Fatal("the appended middleware did not run: the chain was not rebuilt")
	}
}

func TestUseRunsAfterSecurityHeaders(t *testing.T) {
	a := newTestApp(t)

	// Order matters: authentication middleware installed by InstallAuth must
	// sit inside the security headers and logging, not outside them, or a
	// denial escapes with an unhardened response.
	var sawSecurityHeader bool
	a.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sawSecurityHeader = w.Header().Get("X-Content-Type-Options") != ""
			next.ServeHTTP(w, r)
		})
	})
	a.get(t, "/")

	if !sawSecurityHeader {
		t.Error("appended middleware should run inside the security-headers middleware")
	}
}

func TestInstallAuthInjectsTemplateValues(t *testing.T) {
	a := newTestApp(t)
	svc := newCoreAuthService(t)
	installAuth(t, a, svc)

	a.Route("/probe", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			ctx := a.baseContext(r)
			for _, key := range []string{"user", "isAuthenticated", "csrfInput", "csrfToken"} {
				if _, ok := ctx[key]; !ok {
					t.Errorf("render context is missing %q", key)
				}
			}
			if ctx["isAuthenticated"] != false {
				t.Error("an anonymous request should not be reported as authenticated")
			}
			if ctx["csrfToken"] == "" {
				t.Error("the CSRF token should be available for forms")
			}
			okHandler(w, r)
		})
	})

	if rec := a.get(t, "/probe"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestInstallAuthInjectsAuthenticatedUser(t *testing.T) {
	a := newTestApp(t)
	svc := newCoreAuthService(t)
	installAuth(t, a, svc)

	a.Route("/probe", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			ctx := a.baseContext(r)
			if ctx["isAuthenticated"] != true {
				t.Error("a session-carrying request should be reported as authenticated")
			}
			user, ok := ctx["user"].(auth.User)
			if !ok {
				t.Fatalf("user = %T, want auth.User", ctx["user"])
			}
			if user.Identity != "ada" {
				t.Errorf("identity = %q", user.Identity)
			}
			// The CSRF input is markup already escaped inside auth, wrapped
			// once here so templates need no |safe filter.
			input, ok := ctx["csrfInput"].(template.HTML)
			if !ok {
				t.Fatalf("csrfInput = %T, want template.HTML", ctx["csrfInput"])
			}
			if !strings.Contains(string(input), `name="csrf_token"`) {
				t.Errorf("csrfInput = %q", input)
			}
			okHandler(w, r)
		})
	})

	rec := a.send(t, withCookie(httptest.NewRequest(http.MethodGet, "/probe", nil),
		sessionCookieFor(t, svc, "ada", "TASKS:EDIT")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
}

func TestInstalledMiddlewareMakesIdentityReachableByHandlers(t *testing.T) {
	a := newTestApp(t)
	svc := newCoreAuthService(t)
	installAuth(t, a, svc)

	a.Route("/whoami", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			user, ok := auth.UserFrom(r)
			if !ok {
				t.Error("the installed middleware should have stamped the user")
			}
			if user.Identity != "ada" {
				t.Errorf("identity = %q", user.Identity)
			}
			okHandler(w, r)
		})
	})

	rec := a.send(t, withCookie(httptest.NewRequest(http.MethodGet, "/whoami", nil),
		sessionCookieFor(t, svc, "ada")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestInstalledCSRFProtectsUnsafeRoutes(t *testing.T) {
	a := newTestApp(t)
	svc := newCoreAuthService(t)
	installAuth(t, a, svc)

	a.Route("/things", func(r Router) {
		r.Post("/", r.RequireLogin(okHandler))
	})

	// A cookie-authenticated POST with no CSRF token: exactly what a
	// cross-site form produces, and it must not reach the handler.
	rec := a.send(t, withCookie(httptest.NewRequest(http.MethodPost, "/things", nil),
		sessionCookieFor(t, svc, "ada")))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestInstalledLoginFlowEndToEnd(t *testing.T) {
	a := newTestApp(t)
	svc := newCoreAuthService(t)
	installAuth(t, a, svc)

	ctx := t.Context()
	if _, err := svc.CreateAccount(ctx, "ada", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	a.Route("/login", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			// The form carries the CSRF token the middleware just minted.
			_, _ = w.Write([]byte(auth.CSRFInput(r)))
		})
		r.Post("/", svc.LoginHandler(auth.LoginOptions{Fallback: "/"}))
	})
	a.Route("/whoami", func(r Router) {
		r.Get("/", r.RequireLogin(okHandler))
	})

	// 1. Load the form and keep both cookies the response sets.
	form := a.get(t, "/login")
	if form.Code != http.StatusOK {
		t.Fatalf("form status = %d", form.Code)
	}
	var csrfValue string
	for _, c := range form.Result().Cookies() {
		if c.Name == svc.CSRFName() {
			csrfValue = c.Value
		}
	}
	if csrfValue == "" {
		t.Fatal("the safe request should have been given a CSRF token")
	}
	if !strings.Contains(form.Body.String(), csrfValue) {
		t.Error("the rendered form should carry the token")
	}

	// 2. A wrong password, with the CSRF token a browser would send, is
	// refused without issuing a session.
	wrong := loginFormRequest(svc, csrfValue, "ada", "correct horse battery stakeout")
	refused := a.send(t, wrong)
	if refused.Code != http.StatusUnauthorized {
		t.Fatalf("a wrong password should be refused, got %d", refused.Code)
	}
	for _, c := range refused.Result().Cookies() {
		if c.Name == svc.CookieName() {
			t.Fatal("a refused login must not issue a session")
		}
	}

	// 3. The right password reaches the redirect and issues a session.
	good := a.send(t, loginFormRequest(svc, csrfValue, "ada", "correct horse battery staple"))
	if good.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303; body: %s", good.Code, good.Body.String())
	}

	var session *http.Cookie
	for _, c := range good.Result().Cookies() {
		if c.Name == svc.CookieName() {
			session = c
		}
	}
	if session == nil {
		t.Fatal("no session cookie after a successful login")
	}

	// 4. The issued session opens a protected route.
	verify := a.send(t, withCookie(httptest.NewRequest(http.MethodGet, "/whoami", nil), session))
	if verify.Code != http.StatusOK {
		t.Fatalf("protected route with the issued session = %d, want 200", verify.Code)
	}
}

// loginFormRequest builds a browser-shaped login POST: the CSRF cookie from
// the form render plus the token echoed in the form body.
func loginFormRequest(svc *auth.Service, csrfValue, username, password string) *http.Request {
	body := "username=" + url.QueryEscape(username) + "&password=" + url.QueryEscape(password) +
		"&csrf_token=" + url.QueryEscape(csrfValue)
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: svc.CSRFName(), Value: csrfValue})
	return req
}

// withAccept sets an Accept header and returns the request.
func withAccept(r *http.Request, accept string) *http.Request {
	r.Header.Set("Accept", accept)
	return r
}

// withCookie attaches a cookie to a request and returns it.
func withCookie(r *http.Request, cookie *http.Cookie) *http.Request {
	r.AddCookie(cookie)
	return r
}

// TestPrefixCookieName pins the derivation, including the two cases where a
// prefix is deliberately not applied.
func TestPrefixCookieName(t *testing.T) {
	cases := map[string]string{
		"TESTAPP":     "testapp_session",
		"testapp":     "testapp_session",
		"MY_APP":      "my_app_session",
		"WEBAPP":      "session",
		"":            "session",
		"  TESTAPP  ": "testapp_session",
	}
	for prefix, want := range cases {
		if got := prefixCookieName(prefix, "session"); got != want {
			t.Errorf("prefixCookieName(%q) = %q, want %q", prefix, got, want)
		}
	}
}

// TestAuthServiceReflectsInstall documents that the getter is the only
// exported route to the service, so an application cannot end up with two.
func TestAuthServiceReflectsInstall(t *testing.T) {
	a := newTestApp(t)
	if a.AuthService() != nil {
		t.Fatal("an application without InstallAuth has no service")
	}

	svc := newCoreAuthService(t)
	installAuth(t, a, svc)
	if a.AuthService() != svc {
		t.Error("AuthService should return the installed service itself")
	}
}

func TestSessionLifetimeFlowsThroughInstall(t *testing.T) {
	svc := newCoreAuthService(t, auth.WithCookiePolicy(auth.CookiePolicy{Lifetime: 3 * time.Hour}))
	if got := svc.SessionLifetime(); got != 3*time.Hour {
		t.Fatalf("lifetime = %s, want it kept as configured", got)
	}

	a := newTestApp(t)
	installAuth(t, a, svc)

	if got := svc.SessionLifetime(); got != 3*time.Hour {
		t.Errorf("lifetime after install = %s, want the application not to reset it", got)
	}
}
