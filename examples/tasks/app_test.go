package main

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/the-maldridge/webapp"
	"github.com/the-maldridge/webapp/db"
	"github.com/the-maldridge/webapp/webapptest"
)

// These tests drive the example's real handler chain: the module is loaded
// through the registry the way an application loads it, requests go through the
// session codec and the CSRF check, and the database is real.
//
// That shape is the point. A unit test of list() with a mocked database cannot
// catch the ordering bug this application exists to exercise: the module
// registers its routes during construction, and authentication is installed
// after construction, because a service needs a database handle that does not
// exist until the application has opened one.

// app builds the example for a test: authentication installed, and then the
// application's own routes registered, which is the sequence run() follows.
//
// Registering the front door matters more than it looks. The module arrives on
// its own through the registry, but sign-in belongs to the application, so a
// test application without this line has a task list and no way to reach it.
func app(t *testing.T) *webapptest.App {
	t.Helper()

	a := webapptest.NewApp(t)
	a.InstallAuth()
	routes(a.App)
	return a
}

// TestModuleIsLoadedThroughTheRegistry is the smallest statement that the module
// is actually wired: the route it registered answers, and the framework knows
// what it declared.
func TestModuleIsLoadedThroughTheRegistry(t *testing.T) {
	a := app(t)

	if got := a.LoadedModules(); len(got) != 1 || got[0] != "tasks" {
		t.Fatalf("LoadedModules = %v, want [tasks]", got)
	}

	want := []string{"TASKS:ADMIN", "TASKS:DELETE", "TASKS:EDIT", "TASKS:VIEW"}
	perms := a.Permissions()
	if len(perms) != len(want) {
		t.Fatalf("Permissions = %v, want %v", perms, want)
	}
	for i, name := range want {
		if perms[i].String() != name {
			t.Errorf("permission #%d = %q, want %q", i, perms[i].String(), name)
		}
	}
	if _, ok := a.ModuleOwner(perms[0]); !ok {
		t.Error("a declared permission has no owner")
	}
}

// TestAnonymousIsRefusedTheList is the whole reason the guard exists. A 401 here
// rather than a redirect to a login page is what an API client needs, and the
// framework negotiates the difference; the test pins that a module's route did
// not opt out of it.
func TestAnonymousIsRefusedTheList(t *testing.T) {
	a := app(t)

	resp := a.Get("/ui/task/")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous list = %d, want 401; body: %s", resp.StatusCode, webapptest.Body(t, resp))
	}
}

func TestFrontDoorAndRootRedirect(t *testing.T) {
	a := app(t)

	resp := a.Get("/login")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /login = %d, want 200", resp.StatusCode)
	}
	body := webapptest.Body(t, resp)
	if !strings.Contains(body, `name="password"`) {
		t.Error("the login page has no password field")
	}
	if a.CSRFToken() == "" {
		t.Error("the login page did not mint a CSRF token; the form could not submit")
	}

	if resp := a.Get("/"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous GET / = %d, want 401", resp.StatusCode)
	}

	a.SignIn("ada", "TASKS:VIEW")
	resp = a.Get("/")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET / = %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/ui/task/" {
		t.Errorf("Location = %q, want /ui/task/", got)
	}
}

// TestSignInThroughTheForm exercises the path a person takes rather than the one
// a test mints: an account exists, credentials are posted, and the session that
// comes back is accepted by the module's own guard.
func TestSignInThroughTheForm(t *testing.T) {
	a := app(t)
	a.Account("ada", "a-password-long-enough", "TASKS:VIEW")

	if resp := a.Get("/login"); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /login = %d", resp.StatusCode)
	}

	resp := a.PostForm("/login", "username=ada&password=a-password-long-enough&next=/ui/task/")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign in = %d, want 303; body: %s", resp.StatusCode, webapptest.Body(t, resp))
	}
	if got := resp.Header.Get("Location"); got != "/ui/task/" {
		t.Errorf("Location = %q, want the destination that was asked for", got)
	}

	// The cookie the redirect arrived with has to satisfy a guarded route, which
	// is the assertion that the sign-in actually worked rather than merely
	// redirected.
	if resp := a.Get("/ui/task/"); resp.StatusCode != http.StatusOK {
		t.Fatalf("list after sign in = %d, want 200", resp.StatusCode)
	}
}

func TestSignInFailureDoesNotSayWhichHalfWasWrong(t *testing.T) {
	a := app(t)
	a.Account("ada", "a-password-long-enough", "TASKS:VIEW")

	a.Get("/login")
	resp := a.PostForm("/login", "username=ada&password=wrong-password-here")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a refused sign in should re-render the form, got %d", resp.StatusCode)
	}
	body := webapptest.Body(t, resp)
	for _, phrase := range []string{"username", "password", "did not match"} {
		if !strings.Contains(strings.ToLower(body), phrase) {
			t.Errorf("the refusal should mention %q; body:\n%s", phrase, body)
		}
	}
	// One sentence for every reason. The distinction is what an enumeration
	// attack queries, so the page may not hold it even by accident.
	for _, leak := range []string{"no such", "unknown account", "not found"} {
		if strings.Contains(strings.ToLower(body), leak) {
			t.Errorf("the refusal leaks %q", leak)
		}
	}
}

// TestViewHolderSeesNoWriteControls is the visible half of the permission model:
// the links a viewer cannot use are not rendered, and the sentence explaining the
// empty list says who can make the first one.
func TestViewHolderSeesNoWriteControls(t *testing.T) {
	a := app(t)
	signInFor(t, a, "ada", "TASKS:VIEW")

	resp := a.Get("/ui/task/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d, want 200; body: %s", resp.StatusCode, webapptest.Body(t, resp))
	}
	body := webapptest.Body(t, resp)
	if strings.Contains(body, "/ui/task/new") {
		t.Error("a viewer who cannot edit was shown the new-task link")
	}
	if strings.Contains(body, "Task totals") {
		t.Error("a viewer without TASKS:ADMIN was shown the totals link")
	}

	// And the refusal behind the absent link is a 403, not a 404 and not a
	// success: the row would exist, and hiding it here is not a secret.
	resp = a.PostForm("/ui/task/new", "title=anyway")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("create as a viewer = %d, want 403", resp.StatusCode)
	}
}

func TestEditorCreatesReadsAndCompletes(t *testing.T) {
	a := app(t)
	signInFor(t, a, "ada", "TASKS:VIEW", "TASKS:EDIT")

	resp := a.PostForm("/ui/task/new", "title=File+the+report&notes=Before+Friday")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create = %d, want 303; body: %s", resp.StatusCode, webapptest.Body(t, resp))
	}
	page := a.Follow(resp)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("list after create = %d", page.StatusCode)
	}
	body := webapptest.Body(t, page)
	if !strings.Contains(body, "File the report") {
		t.Fatalf("the new task is not on the list:\n%s", body)
	}
	if !strings.Contains(body, "Before Friday") {
		t.Error("the notes are not on the list")
	}

	id := taskID(t, a, "ada")
	resp = a.PostForm(id+"/complete", "")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("complete = %d, want 303", resp.StatusCode)
	}

	// The two filters are read directly rather than followed: a list answers the
	// page, and a test that followed a 200 would be asserting nothing about the
	// pager it is standing on.
	done := webapptest.Body(t, a.Get("/ui/task/?state=done"))
	if !strings.Contains(done, "File the report") {
		t.Error("the completed task is not in the done filter")
	}
	open := webapptest.Body(t, a.Get("/ui/task/?state=open"))
	if strings.Contains(open, "File the report") {
		t.Error("the completed task is still in the open filter")
	}
}

// TestRejectedSubmissionKeepsWhatWasTyped is the reason Rejected is a type
// rather than a status code on an error: the page has to come back with the
// words in it, at a status that says the write did not happen.
func TestRejectedSubmissionKeepsWhatWasTyped(t *testing.T) {
	a := app(t)
	signInFor(t, a, "ada", "TASKS:EDIT")

	resp := a.PostForm("/ui/task/new", "title=&notes=Typed+at+length")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("empty title = %d, want 422; body: %s", resp.StatusCode, webapptest.Body(t, resp))
	}
	body := webapptest.Body(t, resp)
	if !strings.Contains(body, "Typed at length") {
		t.Error("the notes the person typed did not come back")
	}
	if !strings.Contains(body, "A task needs a title.") {
		t.Error("the page does not say what was wrong")
	}
}

func TestTitleLongerThanTheColumnIsRefused(t *testing.T) {
	a := app(t)
	signInFor(t, a, "ada", "TASKS:EDIT")

	long := strings.Repeat("long ", maxTitleLength)
	resp := a.PostForm("/ui/task/new", "title="+urlEscape(long))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("oversized title = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(webapptest.Body(t, resp), "the most a task can hold") {
		t.Error("the refusal does not name the limit")
	}
}

// TestCrossAccountReachIsANotFoundRatherThanAForbidden is the ownership rule.
//
// A 403 confirms that the row exists, which is information the caller was not
// given and the one thing an enumeration loop wants. The task is not that
// caller's, and the honest answer to "what is at this address" is the same one
// the address would give if it were empty.
func TestCrossAccountReachIsANotFoundRatherThanAForbidden(t *testing.T) {
	a := app(t)
	signInFor(t, a, "ada", "TASKS:EDIT")

	resp := a.PostForm("/ui/task/new", "title=Ada%27s+own")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	id := taskID(t, a, "ada")

	a.SignOut()
	signInFor(t, a, "bob", "TASKS:EDIT", "TASKS:DELETE")

	if resp := a.Get(id + "/edit"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("another account's edit page = %d, want 404", resp.StatusCode)
	}
	if resp := a.PostForm(id+"/delete", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("another account's delete = %d, want 404", resp.StatusCode)
	}
	// And the row is genuinely still there.
	if got := taskOwner(t, a, id); got == "" {
		t.Error("the refusal also deleted the row")
	}
}

// TestModuleAdminReachesAnotherAccountRow pins the one deliberate override, and
// that it lives in claim() rather than being spread across the handlers.
func TestModuleAdminReachesAnotherAccountRow(t *testing.T) {
	a := app(t)
	signInFor(t, a, "ada", "TASKS:EDIT")

	resp := a.PostForm("/ui/task/new", "title=Owned+by+ada")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	id := taskID(t, a, "ada")

	a.SignOut()
	signInFor(t, a, "root", "TASKS:ADMIN")

	if got := a.Get(id + "/edit").StatusCode; got != http.StatusOK {
		t.Fatalf("module admin on another account's row = %d, want 200", got)
	}
	if got := a.PostForm(id+"/delete", "").StatusCode; got != http.StatusSeeOther {
		t.Fatalf("module admin delete = %d, want 303", got)
	}
	if got := taskOwner(t, a, id); got != "" {
		t.Error("the row survived the admin delete")
	}
}

// TestAdminTotalsAreGated is the administrative page: the grant that reaches
// another person's row is also the only thing that reaches this page, and the
// counts are counts of everything.
func TestAdminTotalsAreGated(t *testing.T) {
	a := app(t)
	signInFor(t, a, "ada", "TASKS:EDIT")
	a.PostForm("/ui/task/new", "title=One")
	signInFor(t, a, "root", "TASKS:ADMIN")
	a.PostForm("/ui/task/new", "title=Two")

	signInFor(t, a, "ada", "TASKS:VIEW")
	if got := a.Get("/ui/task/stats").StatusCode; got != http.StatusForbidden {
		t.Fatalf("totals as a viewer = %d, want 403", got)
	}

	signInFor(t, a, "root", "TASKS:ADMIN")
	resp := a.Get("/ui/task/stats")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("totals as an admin = %d; body: %s", resp.StatusCode, webapptest.Body(t, resp))
	}
	body := webapptest.Body(t, resp)
	if !strings.Contains(body, "<dd>2</dd>") {
		t.Errorf("the totals should count both accounts' tasks:\n%s", body)
	}
	if !strings.Contains(body, "not only yours") {
		t.Error("the page does not say it is counting everything")
	}
}

// TestWriteWithoutCSRFFails is the framework's CSRF check reached through the
// example's own routes, which is the only version of the test worth having: a
// route that mounted its handler outside the middleware chain would pass a test
// of the middleware alone.
func TestWriteWithoutCSRFFails(t *testing.T) {
	a := app(t)
	signInFor(t, a, "ada", "TASKS:VIEW", "TASKS:EDIT")

	// A page load first, because the client has to hold a CSRF cookie for the
	// refusal to be about the missing field rather than about there being no
	// token to check. A cross-site form would have arrived with the cookie
	// without asking.
	if resp := a.Get("/ui/task/"); resp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d", resp.StatusCode)
	}
	if a.CSRFToken() == "" {
		t.Fatal("no CSRF cookie after a page load")
	}

	resp := a.PostFormNoCSRF("/ui/task/new", "title=Forged")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("write without a token = %d, want 403", resp.StatusCode)
	}
	if got := taskCount(t, a, "ada"); got != 0 {
		t.Errorf("the forged write created %d rows", got)
	}
}

// TestNavigationIsFilteredPerRequest is the difference between hiding a link and
// guarding a route being two decisions: here they are one declaration, read twice.
func TestNavigationIsFilteredPerRequest(t *testing.T) {
	a := app(t)

	signInFor(t, a, "ada", "TASKS:VIEW")
	body := webapptest.Body(t, a.Get("/ui/task/"))
	nav := primaryNav(t, body)
	if !strings.Contains(nav, "Tasks") {
		t.Errorf("the viewer's navigation is missing the task list:\n%s", nav)
	}
	if strings.Contains(nav, "Task totals") {
		t.Errorf("the viewer's navigation offers the administrative page:\n%s", nav)
	}

	signInFor(t, a, "root", "TASKS:ADMIN")
	body = webapptest.Body(t, a.Get("/ui/task/"))
	nav = primaryNav(t, body)
	if !strings.Contains(nav, "Task totals") {
		t.Errorf("the admin's navigation is missing the totals page:\n%s", nav)
	}
	if !strings.Contains(nav, `href="/ui/task/stats"`) {
		t.Errorf("the admin's totals item does not link to the totals page:\n%s", nav)
	}
}

// TestPaginationWalksPages uses one more task than a page holds, which is the
// smallest input that distinguishes a pager from a list.
func TestPaginationWalksPages(t *testing.T) {
	a := app(t)
	signInFor(t, a, "ada", "TASKS:VIEW", "TASKS:EDIT")

	for i := 0; i <= db.DefaultPageSize; i++ {
		insertTask(t, a, "ada", "Task "+itoa(i))
	}

	first := webapptest.Body(t, a.Get("/ui/task/"))
	if !strings.Contains(first, "Page 1 of 2") {
		t.Fatalf("page one does not know there are two:\n%s", tail(first))
	}
	second := webapptest.Body(t, a.Get("/ui/task/?page=2"))
	if !strings.Contains(second, "Page 2 of 2") {
		t.Fatalf("page two is wrong:\n%s", tail(second))
	}

	// A page beyond the end is empty rather than an error, and a filter that
	// survives the pager is the filter the person asked for.
	if got := a.Get("/ui/task/?page=99").StatusCode; got != http.StatusOK {
		t.Fatalf("page beyond the end = %d, want 200 with an empty list", got)
	}
	if got := a.Get("/ui/task/?state=nonsense").StatusCode; got != http.StatusOK {
		t.Fatalf("an unrecognised filter = %d, want 200", got)
	}
}

// TestModuleAssetIsServedAndValidated covers the module's own URL namespace:
// /static/tasks/ is the module's, /static/core/ is the framework's, and an
// embedded file has to offer a validator or a browser downloads it forever.
func TestModuleAssetIsServedAndValidated(t *testing.T) {
	a := app(t)

	resp := a.Get("/static/tasks/css/tasks.css")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("module stylesheet = %d", resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag: an embedded file has no modification time, so a cached copy can never be confirmed")
	}
	if got := resp.Header.Get("Cache-Control"); got != "public, max-age=3600" {
		t.Errorf("Cache-Control = %q", got)
	}
	if body := webapptest.Body(t, resp); !strings.Contains(body, ".task-list") {
		t.Error("the stylesheet served is not the module's")
	}

	cond, err := http.NewRequest(http.MethodGet, "https://webapp.test/static/tasks/css/tasks.css", nil)
	if err != nil {
		t.Fatal(err)
	}
	cond.Header.Set("If-None-Match", etag)
	if got := a.Do(cond).StatusCode; got != http.StatusNotModified {
		t.Errorf("conditional request = %d, want 304", got)
	}
}

// TestModuleCanBeDisabledByConfiguration is the registry's promise: one
// variable, and the feature is gone without a different binary.
//
// The whitelist names a different module rather than nothing. An empty list
// means "no whitelist", not "load none", which is the right reading of a
// variable that defaults to unset, and it means the way to take one module out
// is to name the ones that stay.
func TestModuleCanBeDisabledByConfiguration(t *testing.T) {
	a := webapptest.NewApp(t,
		webapp.WithModule(spareInfo),
		webapp.WithModules("spare"),
	)
	a.InstallAuth()
	routes(a.App)

	if got := a.LoadedModules(); len(got) != 1 || got[0] != "spare" {
		t.Fatalf("LoadedModules = %v, want [spare]", got)
	}
	// Nothing a disabled module declared survives it, because the administration
	// screen offers what the loaded modules asked for and that grant would
	// otherwise look real while granting nothing.
	if got := a.Permissions(); len(got) != 0 {
		t.Errorf("a disabled module still contributes permissions: %v", got)
	}

	if got := a.Get("/ui/task/").StatusCode; got != http.StatusNotFound {
		t.Errorf("a disabled module still answers its route: %d", got)
	}
	// Both of its address spaces go with it. The assets are mounted by the
	// module, so nothing is left serving its files.
	if got := a.Get("/static/tasks/css/tasks.css").StatusCode; got != http.StatusNotFound {
		t.Errorf("a disabled module still serves its assets: %d", got)
	}
	// The framework's own do not.
	if got := a.Get("/static/core/css/app.css").StatusCode; got != http.StatusOK {
		t.Errorf("the framework's stylesheet with a module disabled = %d, want 200", got)
	}
	// And the application is still an application: the front door belongs to it,
	// not to the module that was just removed.
	if got := a.Get("/login").StatusCode; got != http.StatusOK {
		t.Errorf("front door with the module disabled = %d, want 200", got)
	}
	if got := a.Get("/").StatusCode; got != http.StatusUnauthorized {
		t.Errorf("anonymous root with the module disabled = %d, want 401", got)
	}
}

// TestWhitelistNamingAnUnregisteredModuleFails is the other half of that
// variable: it can leave a module out, but it cannot silently misspell one.
func TestWhitelistNamingAnUnregisteredModuleFails(t *testing.T) {
	_, err := webapp.New(
		webapp.WithEnvPrefix("WEBAPPTEST"),
		webapp.WithAddr("127.0.0.1:0"),
		webapp.WithModules("tasks", "not-a-module"),
		webapp.WithDBOptions(db.WithPath(filepath.Join(t.TempDir(), "whitelist.db"))),
	)
	if err == nil {
		t.Fatal("a whitelist naming nothing registered should stop construction")
	}
	if !strings.Contains(err.Error(), "not-a-module") {
		t.Errorf("the error should name the module it could not find:\n%v", err)
	}
	// The modules that were named correctly are not the problem, and the error
	// says which one is, so the fix is one edit rather than a bisect.
	if strings.Contains(err.Error(), `"tasks"`) {
		t.Errorf("the error blames a module that is registered:\n%v", err)
	}
}
