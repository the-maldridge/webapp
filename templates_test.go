package webapp

import (
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/the-maldridge/webapp/auth"
)

// mapFS builds an in-memory template root.
func mapFS(files map[string]string) fs.FS {
	m := fstest.MapFS{}
	for name, body := range files {
		m[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return m
}

// requestFor builds a request carrying an authenticated identity with the named
// groups, for tests that ask what a particular viewer sees. An empty identity
// means anonymous, which is a real case rather than a skipped one: hiding a
// link and refusing the route behind it have to agree for it too.
func requestFor(identity string, groups ...string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if identity == "" {
		return req
	}
	member := map[string]struct{}{}
	for _, g := range groups {
		member[g] = struct{}{}
	}
	ctx := auth.ContextWithUser(req.Context(), auth.User{
		Identity: identity,
		Groups:   member,
		AuthedBy: "test",
	})
	return req.WithContext(ctx)
}

func TestNewInstallsCoreTemplates(t *testing.T) {
	a := newTestApp(t)
	if a.Renderer == nil {
		t.Fatal("New installed no renderer; every page would be a 500")
	}
	if !a.registry.Mounted(templateNamespace) {
		t.Errorf("registry is missing the %q namespace: %v",
			templateNamespace, a.registry.Namespaces())
	}
}

func TestViewRendersFrameworkTemplate(t *testing.T) {
	a := newTestApp(t)
	a.Route("/login-page", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.View(w, r, "@core/login.p2", Context{})
		})
	})

	rec := a.get(t, "/login-page")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `name="username"`) {
		t.Errorf("login form is missing its username field:\n%s", body)
	}
	if !strings.Contains(body, `type="password"`) {
		t.Error("login form is missing its password field")
	}
	// The base layout must come along with it, which is the extends chain
	// working through namespaced names.
	if !strings.Contains(body, "<!doctype html>") {
		t.Error("the login page did not extend the base layout")
	}
}

func TestMarkupInContextIsNotEscapedAgain(t *testing.T) {
	a := newTestApp(t)
	a.Route("/csrf", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.View(w, r, "@core/login.p2", Context{
				"csrfInput": template.HTML(`<input type="hidden" name="csrf_token" value="tok">`),
			})
		})
	})

	body := a.get(t, "/csrf").Body.String()
	if !strings.Contains(body, `<input type="hidden" name="csrf_token" value="tok">`) {
		t.Errorf("the CSRF field was escaped and is visible markup:\n%s", body)
	}
	if strings.Contains(body, "&lt;input") {
		t.Error("markup in the render context was double-escaped")
	}
}

func TestTextInContextIsEscaped(t *testing.T) {
	a := newTestApp(t)
	a.Route("/escape", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.View(w, r, "@core/errors/internal.p2", Context{
				"status":     http.StatusInternalServerError,
				"statusText": "Internal Server Error",
				"requestId":  "<script>steal()</script>",
			})
		})
	})

	body := a.get(t, "/escape").Body.String()
	if strings.Contains(body, "<script>steal()") {
		t.Error("a reference ID reached the page unescaped")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Error("the reference ID is missing, escaped or not")
	}
}

// The three pages a viewer can act on differently: something is missing,
// something is refused, something broke. Every other status falls through to
// the built-in page, so the table stops at those three plus a status that has
// no template of its own.
func TestErrorPagesRender(t *testing.T) {
	tests := []struct {
		status int
		want   string
	}{
		{http.StatusNotFound, "Nothing is here"},
		{http.StatusGone, "Nothing is here"},
		{http.StatusUnauthorized, "needs an account"},
		{http.StatusForbidden, "needs an account"},
		{http.StatusInternalServerError, "Something failed on the server"},
		{http.StatusBadGateway, "Something failed on the server"},
	}

	for _, tc := range tests {
		status, want := tc.status, tc.want
		a := newTestApp(t)
		a.Route("/fail", func(r Router) {
			r.Get("/", func(w http.ResponseWriter, r *http.Request) {
				a.Fail(w, r, status, errors.New("the underlying cause"))
			})
		})

		rec := a.get(t, "/fail")
		if rec.Code != status {
			t.Fatalf("status = %d, want %d", rec.Code, status)
		}
		body := rec.Body.String()
		if !strings.Contains(body, want) {
			t.Errorf("status %d page is missing %q:\n%s", status, want, body)
		}
		// The status must be in the document as well as the status line, or a
		// screenshot of a failure is ambiguous.
		if !strings.Contains(body, strconv.Itoa(status)) {
			t.Errorf("status %d does not appear in its own page", status)
		}
		if strings.Contains(body, "the underlying cause") {
			t.Errorf("status %d disclosed the error text without debug mode", status)
		}
	}
}

func TestErrorPageShowsDetailOnlyInDebug(t *testing.T) {
	a := newTestApp(t, WithDebug())
	a.Route("/fail", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.Fail(w, r, http.StatusInternalServerError, errors.New("the postgres user lacks a password"))
		})
	})

	body := a.get(t, "/fail").Body.String()
	if !strings.Contains(body, "the postgres user lacks a password") {
		t.Error("debug mode did not disclose the cause")
	}
}

func TestErrorPageNamesRequiredPermission(t *testing.T) {
	a := newTestApp(t)
	a.Route("/fail", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.Fail(w, r, http.StatusForbidden, &permDenial{
				msg:  "you may not do that",
				perm: auth.P("tasks", "edit"),
			})
		})
	})

	body := a.get(t, "/fail").Body.String()
	if !strings.Contains(body, "TASKS:EDIT") {
		t.Errorf("the page did not name the required grant:\n%s", body)
	}
}

// A refusal at a guarded route is the path that matters, because it is the one
// an application cannot get wrong by forgetting to mention the permission: the
// auth layer builds the denial and the page has to explain it.
func TestGuardedRefusalPageExplainsItself(t *testing.T) {
	a := newTestApp(t)
	svc := newCoreAuthService(t)
	installAuth(t, a, svc)

	a.Route("/edit", func(r Router) {
		r.Get("/", r.Guard(auth.P("tasks", "edit"), okHandler))
	})

	rec := a.send(t, withCookie(
		httptest.NewRequest(http.MethodGet, "/edit", nil),
		sessionCookieFor(t, svc, "ada", "TASKS:VIEW")))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "not enough for this page") {
		t.Errorf("a signed-in refusal did not say what it meant:\n%s", body)
	}
	if !strings.Contains(body, "ada") {
		t.Error("the page did not say who was refused")
	}
	if !strings.Contains(body, "TASKS:EDIT") {
		t.Error("the page did not name the grant that was missing")
	}
}

func TestErrorPageHasNoReferenceWithoutOne(t *testing.T) {
	// Without the request-ID middleware a bare renderer call has an empty ID,
	// and "Reference:" followed by nothing reads like a broken page rather
	// than an application that does not assign IDs.
	a := newTestApp(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	a.renderErrorPage(rec, req, http.StatusNotFound, nil, "")
	if strings.Contains(rec.Body.String(), "Reference:") {
		t.Errorf("an empty reference was printed:\n%s", rec.Body.String())
	}
}

func TestOtherStatusesUseTheFallbackPage(t *testing.T) {
	a := newTestApp(t)
	a.Route("/teapot", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.Fail(w, r, http.StatusTeapot, errors.New("nope"))
		})
	})

	rec := a.get(t, "/teapot")
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "418") {
		t.Errorf("the fallback page should still state the status:\n%s", rec.Body.String())
	}
}

func TestBrokenErrorTemplateFallsBack(t *testing.T) {
	// An error page that fails to render must not become a second failure. The
	// renderer is replaced with one that fails, standing in for a template that
	// was compiled and then broke at execution time.
	a := newTestApp(t)
	a.Renderer = &stubRenderer{err: errors.New("template exploded"), prefix: "<html>partial"}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec.Body.Reset()

	a.renderErrorPage(rec, req, http.StatusInternalServerError, errors.New("original cause"), "abc123")

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want the requested 500 preserved", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "template exploded") {
		t.Error("the render failure leaked into the response")
	}
}

func TestApplicationTemplatesResolveBeforeCore(t *testing.T) {
	a := newTestApp(t, WithTemplates("app", mapFS(map[string]string{
		"base.p2":  `app base`,
		"page.p2":  `{% extends "base.p2" %}`,
		"sub.p2":   `sub content`,
		"wrap.p2":  `{% include "sub.p2" %}`,
		"login.p2": `app login`,
	})))

	a.Route("/page", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.View(w, r, "page.p2", Context{})
		})
	})
	a.Route("/wrap", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.View(w, r, "wrap.p2", Context{})
		})
	})
	// A bare name that the application defines shadows the framework's file of
	// the same name. This is the theme mechanism and the hazard at once, which
	// is why the framework's own error pages are named @core/... and cannot be.
	a.Route("/login", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.View(w, r, "login.p2", Context{})
		})
	})

	if body := a.get(t, "/page").Body.String(); strings.TrimSpace(body) != "app base" {
		t.Errorf("bare extends resolved outside the application root: %q", body)
	}
	if body := a.get(t, "/wrap").Body.String(); strings.TrimSpace(body) != "sub content" {
		t.Errorf("bare include = %q", body)
	}
	if body := a.get(t, "/login").Body.String(); strings.TrimSpace(body) != "app login" {
		t.Errorf("an application template did not shadow the framework's: %q", body)
	}
}

func TestCoreErrorPagesCannotBeShadowed(t *testing.T) {
	a := newTestApp(t, WithTemplates("app", mapFS(map[string]string{
		"errors/notfound.p2": `application 404`,
	})))

	a.Route("/missing", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.Fail(w, r, http.StatusNotFound, errors.New("gone"))
		})
	})

	body := a.get(t, "/missing").Body.String()
	if strings.Contains(body, "application 404") {
		t.Error("an application replaced the framework's 404 page")
	}
	if !strings.Contains(body, "Nothing is here") {
		t.Errorf("the framework's own 404 did not render:\n%s", body)
	}
}

func TestWithTemplatesRejectsReservedAndInvalid(t *testing.T) {
	if _, err := newTestAppNoBuild(t, WithTemplates(templateNamespace, mapFS(nil))); err == nil {
		t.Error("an application was allowed to mount the framework's namespace")
	}
	if _, err := newTestAppNoBuild(t, WithTemplates("a/b", mapFS(nil))); err == nil {
		t.Error("a namespace containing a slash was accepted")
	}
	if _, err := newTestAppNoBuild(t, WithTemplates("app", nil)); err == nil {
		t.Error("a nil filesystem was accepted")
	}
}

func TestWithTemplateDebugOverlaysAndReloads(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "page.p2", "from disk v1")

	a := newTestApp(t, WithTemplateDebug(dir))

	a.Route("/page", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.View(w, r, "page.p2", Context{})
		})
	})
	if body := a.get(t, "/page").Body.String(); !strings.Contains(body, "from disk v1") {
		t.Fatalf("the debug directory was not used: %q", body)
	}

	// The point of the option: an edit is visible on the next request, without
	// a rebuild. A one-file overlay that requires a compile is a decoy.
	writeTemplate(t, dir, "page.p2", "from disk v2")
	if body := a.get(t, "/page").Body.String(); !strings.Contains(body, "from disk v2") {
		t.Errorf("an edited template was not reloaded: %q", body)
	}
}

func TestWithTemplateDebugFallsBackToEmbedded(t *testing.T) {
	// A directory containing one file is the normal state of someone debugging a
	// single page. A fallback to the embedded copy is what makes that possible
	// at all — without it, mounting an overlay means losing every template that
	// is not in it.
	dir := t.TempDir()
	writeTemplate(t, dir, "only.p2", "overlay")

	a := newTestApp(t, WithTemplateDebug(dir))
	a.Route("/login-page", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.View(w, r, "@core/login.p2", Context{})
		})
	})
	a.Route("/only", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.View(w, r, "only.p2", Context{})
		})
	})

	if body := a.get(t, "/login-page").Body.String(); !strings.Contains(body, `name="username"`) {
		t.Errorf("the embedded login page stopped rendering: %q", body)
	}
	if body := a.get(t, "/only").Body.String(); !strings.Contains(body, "overlay") {
		t.Errorf("the overlay file did not render: %q", body)
	}
}

func TestWithTemplateDebugRefusedInProduction(t *testing.T) {
	// A writable directory ahead of the binary's own templates is a
	// code-execution surface, so the combination is refused rather than warned
	// about — nobody reads a startup warning about template precedence at 3am.
	_, err := newTestAppNoBuild(t, WithProduction(), WithTemplateDebug(t.TempDir()))
	if err == nil {
		t.Fatal("template debugging was allowed in production")
	}
	if !strings.Contains(err.Error(), "production") {
		t.Errorf("error %q does not say why", err)
	}
}

func TestWithTemplateDebugRejectsEmptyDir(t *testing.T) {
	if _, err := newTestAppNoBuild(t, WithTemplateDebug("   ")); err == nil {
		t.Error("an empty debug directory was accepted")
	}
}

func TestWithNoTemplates(t *testing.T) {
	a := newTestApp(t, WithNoTemplates(), WithDebug())
	if a.Renderer != nil {
		t.Fatal("WithNoTemplates still installed a renderer")
	}
	if a.registry != nil {
		t.Error("WithNoTemplates still mounted a registry")
	}

	a.Route("/page", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.View(w, r, "page.p2", Context{})
		})
	})

	rec := a.get(t, "/page")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	// Even with no templating, a failure is a failure: the status has to be
	// right and the reason has to be readable, because "nothing happened" is
	// how a missing configuration option stays hidden for a day.
	if !strings.Contains(rec.Body.String(), "no template renderer") {
		t.Errorf("the fallback page did not explain itself:\n%s", rec.Body.String())
	}
}

func TestMissingTemplateIsAFiveHundred(t *testing.T) {
	a := newTestApp(t)
	a.Route("/nope", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.View(w, r, "does-not-exist.p2", Context{})
		})
	})

	rec := a.get(t, "/nope")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestStaticAssetsAreServed(t *testing.T) {
	a := newTestApp(t)

	rec := a.get(t, "/static/core/css/app.css")
	if rec.Code != http.StatusOK {
		t.Fatalf("the framework stylesheet did not serve: status %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/css") {
		t.Errorf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "public") {
		t.Errorf("Cache-Control = %q, want a public policy", got)
	}
	// Traversal has to miss rather than escape. The file server refuses it, but
	// the assertion belongs here because this is the mount that decides it.
	if up := a.get(t, "/static/core/../../go.mod"); up.Code == http.StatusOK {
		t.Error("a path traversal through the static mount returned a file")
	}
}

func TestBaseLayoutLinksTheFrameworkStylesheet(t *testing.T) {
	// The layout and the mount are written in different files and have to agree
	// on a URL. If they do not, every page starts with a broken link and nobody
	// notices until the design looks wrong in a screenshot.
	a := newTestApp(t)
	a.Route("/page", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.View(w, r, "@core/login.p2", Context{})
		})
	})

	body := a.get(t, "/page").Body.String()
	if !strings.Contains(body, `href="/static/core/css/app.css"`) {
		t.Fatalf("the base layout does not link the stylesheet the core serves:\n%s", body)
	}
	if rec := a.get(t, "/static/core/css/app.css"); rec.Code != http.StatusOK {
		t.Error("the linked stylesheet is not served")
	}
}

func TestAssetCarriesBuildVersion(t *testing.T) {
	a := newTestApp(t)
	url := a.Asset("css/app.css")
	if !strings.HasPrefix(url, "/static/core/css/app.css") {
		t.Errorf("Asset = %q", url)
	}
	if a.buildVersion != "" && !strings.Contains(url, "v=") {
		t.Errorf("Asset = %q, want a version stamp", url)
	}
}

func TestNavIsFilteredAndOrdered(t *testing.T) {
	a := newTestApp(t)
	a.Nav(
		NavItem{Weight: 50, Label: "Tasks", Href: "/tasks/"},
		NavItem{Weight: 90, Label: "Admin", Href: "/admin/", Perm: auth.CoreAdmin()},
		NavItem{Weight: 10, Label: "Docs", Href: "/docs/", Perm: auth.P("docs", "view")},
	)

	viewer := navFor(requestFor("ada", "TASKS:VIEW"), a.navItems)
	if len(viewer) != 1 {
		t.Fatalf("entries = %+v, want only the ungated item", viewer)
	}
	if viewer[0].Label != "Tasks" {
		t.Errorf("entry = %+v", viewer[0])
	}

	admin := navFor(requestFor("root", "CORE:ADMIN"), a.navItems)
	if len(admin) != 3 {
		t.Fatalf("a superuser sees %d items, want 3", len(admin))
	}

	anonymous := navFor(requestFor(""), a.navItems)
	if len(anonymous) != 1 {
		t.Fatalf("an anonymous viewer sees %d items, want 1", len(anonymous))
	}

	sortNavItems(a.navItems)
	if a.navItems[0].Label != "Admin" {
		t.Errorf("weight did not sort first: %+v", a.navItems)
	}
}

func TestNavTiesBreakAlphabetically(t *testing.T) {
	items := []NavItem{
		{Weight: 50, Label: "Zebra"},
		{Weight: 50, Label: "Apple"},
		{Weight: 90, Label: "Admin"},
	}
	sortNavItems(items)
	got := []string{items[0].Label, items[1].Label, items[2].Label}
	want := []string{"Admin", "Apple", "Zebra"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("order = %v, want %v", got, want)
			break
		}
	}
}

// The active item is decided by a path segment, so the derivation is a table
// rather than a review comment: the interesting cases are the ones where a link
// is not a path at all.
func TestNavBaseDerivesFirstSegment(t *testing.T) {
	for href, want := range map[string]string{
		"/tasks/":       "tasks",
		"/tasks/42":     "tasks",
		"/":             "",
		"":              "",
		"admin/users":   "admin",
		"https://x/y/z": "https:",
		"///leading///": "leading",
	} {
		if got := navBase(href); got != want {
			t.Errorf("navBase(%q) = %q, want %q", href, got, want)
		}
	}
}

// The navigation in a rendered page is the part an application cannot get
// subtly wrong per page, so it is checked end to end: registration, filtering,
// ordering, and the active item all arrive through one render.
func TestNavAppearsInRenderedPage(t *testing.T) {
	a := newTestApp(t)
	a.Nav(
		NavItem{Weight: 50, Label: "Tasks", Href: "/tasks/"},
		NavItem{Weight: 90, Label: "Admin", Href: "/admin/", Perm: auth.CoreAdmin()},
	)
	svc := newCoreAuthService(t)
	installAuth(t, a, svc)

	a.Route("/tasks", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.View(w, r, "@core/login.p2", Context{})
		})
	})

	body := a.get(t, "/tasks").Body.String()
	if !strings.Contains(body, "Tasks") {
		t.Errorf("the navigation is missing from a rendered page:\n%s", body)
	}
	if strings.Contains(body, "/admin/") {
		t.Error("an anonymous viewer was shown the administration link")
	}

	holder := a.send(t, withCookie(
		httptest.NewRequest(http.MethodGet, "/tasks", nil),
		sessionCookieFor(t, svc, "root", "CORE:ADMIN")))
	adminBody := holder.Body.String()
	if !strings.Contains(adminBody, "/admin/") {
		t.Errorf("a superuser was not shown the administration link:\n%s", adminBody)
	}
	// The page is under /tasks, so that is the item marked current; a template
	// guessing at this would highlight nothing, or the wrong thing, on the
	// pages nobody looked at.
	if !strings.Contains(adminBody, `href="/tasks/" aria-current="page"`) {
		t.Errorf("the current section was not marked:\n%s", adminBody)
	}
}

func TestTemplateCanAskForAPermission(t *testing.T) {
	a, svc := newTestAppWithProbe(t)

	anon := a.get(t, "/probe").Body.String()
	if strings.Contains(anon, "EDIT YES") {
		t.Error("an anonymous request passed a permission check")
	}
	if !strings.Contains(anon, "EDIT NO") {
		t.Errorf("the probe template did not render:\n%s", anon)
	}

	granted := a.send(t, withCookie(
		httptest.NewRequest(http.MethodGet, "/probe", nil),
		sessionCookieFor(t, svc, "ada", "TASKS:EDIT")))
	if !strings.Contains(granted.Body.String(), "EDIT YES") {
		t.Errorf("a grant did not reach the template:\n%s", granted.Body.String())
	}
}

// A malformed permission name in a template denies and says so in the log. The
// alternative — an error — takes down every page that mentions the typo, and a
// silent denial with no log line hides it for as long as the template ships.
func TestCanDeniesMalformedPermissionNames(t *testing.T) {
	logs := &recorder{}
	a := newTestApp(t, WithTemplates("app", mapFS(map[string]string{
		"bad.p2": `{% if can("not a permission") %}YES{% else %}NO{% endif %}`,
	})))
	a.Config.Logger = slogWith(logs)

	a.Route("/bad", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.View(w, r, "bad.p2", Context{})
		})
	})

	rec := a.get(t, "/bad")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "NO") {
		t.Errorf("a malformed permission was not denied:\n%s", rec.Body.String())
	}
	if _, ok := logs.find("template asked about an invalid permission"); !ok {
		t.Error("the typo was swallowed without a log line")
	}
}

func TestWithSiteTitle(t *testing.T) {
	a := newTestApp(t, WithSiteTitle("Widget Co"))
	if got := a.siteTitle(); got != "Widget Co" {
		t.Errorf("siteTitle = %q", got)
	}
	if _, err := newTestAppNoBuild(t, WithSiteTitle("  ")); err == nil {
		t.Error("an empty site title was accepted")
	}
}

func TestSiteTitleDefaultsToPrefix(t *testing.T) {
	a := newTestApp(t)
	if got := a.siteTitle(); got != "TESTAPP" {
		t.Errorf("siteTitle = %q, want the environment prefix", got)
	}
}

func TestErrorPageReachesTemplatesWithContext(t *testing.T) {
	a := newTestApp(t, WithSiteTitle("Widgets"))
	a.Route("/fail", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.Fail(w, r, http.StatusNotFound, errors.New("no such widget"))
		})
	})

	body := a.get(t, "/fail").Body.String()
	// The title proves baseContext ran for an error page too, which is the only
	// reason an error page has navigation and a theme at all.
	if !strings.Contains(body, "Widgets") {
		t.Errorf("an error page did not receive shared context:\n%s", body)
	}
}

func TestJSONClientsBypassErrorTemplates(t *testing.T) {
	a := newTestApp(t)
	a.Route("/fail", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.Fail(w, r, http.StatusNotFound, errors.New("no such thing"))
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/fail", nil)
	req.Header.Set("Accept", "application/json")
	rec := a.send(t, req)
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("Content-Type = %q", ct)
	}
	if strings.Contains(rec.Body.String(), "Nothing is here") {
		t.Error("an API client received an HTML page")
	}
}

// newTestAppNoBuild is newTestApp without the cleanup registration, for tests
// that expect construction itself to fail. Templating stays enabled: refusing a
// template configuration is something New has to do while it is installing
// templates, and turning them off would test a code path no application takes.
func newTestAppNoBuild(t *testing.T, opts ...Option) (*App, error) {
	t.Helper()
	return New(baseTestOptions(t, opts...)...)
}

// newTestAppWithProbe builds an application with an installed auth service and
// a template that asks the permission callable. The service comes back because
// the caller needs a real session cookie to ask the second half of the
// question.
func newTestAppWithProbe(t *testing.T) (*App, *auth.Service) {
	t.Helper()

	a := newTestApp(t, WithTemplates("app", mapFS(map[string]string{
		"probe.p2": `{% if can("TASKS:EDIT") %}EDIT YES{% else %}EDIT NO{% endif %}`,
	})))
	svc := newCoreAuthService(t)
	installAuth(t, a, svc)

	a.Route("/probe", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.View(w, r, "probe.p2", Context{})
		})
	})
	return a, svc
}

func writeTemplate(t *testing.T, dir, name, body string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}
