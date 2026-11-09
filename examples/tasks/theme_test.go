package main

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/the-maldridge/webapp"
	"github.com/the-maldridge/webapp/webapptest"
)

// These tests run the example against a theme other than the default one, and
// they are the end-to-end statement that the mechanism works.
//
// Nothing in the example's templates mentions a theme: each one extends
// "@core/base.p2", which is the framework's own name for its base layout. A theme
// is mounted as that name rather than in front of it, so an application's pages
// change appearance without any of them being edited, and the strongest evidence
// that substitution was the right mechanism is this suite passing unchanged under
// a second theme.

// bulmaBundle is the theme's vendored stylesheet relative to the repository,
// which is generated rather than committed.
const bulmaBundle = "../../theme/bulma/static/css/bulma.min.css"

// bulmaApp builds the example rendering with the Bulma theme.
func bulmaApp(t *testing.T) *webapptest.App {
	t.Helper()

	requireBundle(t)

	a := webapptest.NewApp(t, webapp.WithTheme("bulma"))
	a.InstallAuth()
	routes(a.App)
	return a
}

// TestExampleRendersUnderAnotherTheme runs the three pages a person lands on and
// asserts each came from the theme rather than the default.
func TestExampleRendersUnderAnotherTheme(t *testing.T) {
	a := bulmaApp(t)
	signInFor(t, a, "ada", "TASKS:VIEW", "TASKS:EDIT")

	resp := a.Get("/ui/task/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/task/ under the theme = %d; body: %s", resp.StatusCode, webapptest.Body(t, resp))
	}
	body := webapptest.Body(t, resp)

	// The navbar is the theme's structure, and the example's own markup is inside
	// it: the toolbar comes from the module and must survive the change.
	for _, want := range []string{`class="navbar`, "task-toolbar", "task-empty"} {
		if !strings.Contains(body, want) {
			t.Errorf("the themed task list does not contain %q", want)
		}
	}

	// The theme's stylesheet is at the framework's URL, which is the only reason a
	// template never has to know which theme is active.
	if got := a.Get("/static/core/css/bulma.min.css").StatusCode; got != http.StatusOK {
		t.Errorf("the theme's stylesheet = %d, want 200", got)
	}
	// And the default theme's is not reachable, because only one theme is mounted.
	if got := a.Get("/static/core/css/app.css").StatusCode; got == http.StatusOK {
		t.Error("the default theme's stylesheet is still served under another theme")
	}
}

// TestLoginUnderAnotherThemeKeepsTheSecurityInvariants names the parts of the
// sign-in page that are not styling.
//
// A theme is free to redraw the form, and the framework cannot stop it, so what
// the framework checks instead is that the things a theme must not lose are still
// true here: the CSRF field, the absent password, and one failure message
// regardless of which half of the credentials was wrong.
func TestLoginUnderAnotherThemeKeepsTheSecurityInvariants(t *testing.T) {
	a := bulmaApp(t)
	a.Account("ada", "a-password-long-enough", "TASKS:VIEW")

	body := webapptest.Body(t, a.Get("/login"))
	if !strings.Contains(body, `name="password"`) {
		t.Error("the themed login page has no password field")
	}
	if strings.Contains(body, `name="password" value=`) {
		t.Error("the themed login page repopulates the password")
	}
	if a.CSRFToken() == "" {
		t.Error("the themed login page did not mint a CSRF token")
	}

	// Both halves wrong, and the answer is the same message either way.
	const failure = "That username and password did not match."

	resp := a.PostForm("/login", "username=nobody&password=wrong-and-long")
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatal("sign in with an unknown account succeeded")
	}
	if got := webapptest.Body(t, resp); !strings.Contains(got, failure) {
		t.Errorf("unknown-account failure page does not carry the shared message %q", failure)
	}

	resp = a.PostForm("/login", "username=ada&password=wrong-and-long")
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatal("sign in with a wrong password succeeded")
	}
	if got := webapptest.Body(t, resp); !strings.Contains(got, failure) {
		t.Errorf("wrong-password failure page does not carry the shared message %q; the themed page "+
			"may have dropped the loginError value", failure)
	}
}

// TestThemeDoesNotLeakCustomPropertiesToTheExample asserts the example's own
// stylesheet is self-sufficient under a theme that defines none of the variables
// the default theme used to publish.
func TestThemeDoesNotLeakCustomPropertiesToTheExample(t *testing.T) {
	a := bulmaApp(t)
	signInFor(t, a, "ada", "TASKS:VIEW")

	page := webapptest.Body(t, a.Get("/ui/task/"))
	if strings.Contains(page, "--wa-") {
		t.Error("the themed task list still references the default theme's custom properties")
	}

	sheet := webapptest.Body(t, a.Get("/static/tasks/css/tasks.css"))
	if strings.Contains(sheet, "--wa-") {
		t.Error("tasks.css consumes custom properties a theme defines; it must declare its own")
	}
}

// TestUnknownThemeIsRefused confirms the example cannot be switched into a theme
// it did not import, which is what stops a deployment variable from reaching for a
// stylesheet that is not in the binary.
func TestUnknownThemeIsRefused(t *testing.T) {
	if _, err := webapp.New(webapp.WithEnvPrefix("TASKSTHEME"), webapp.WithTheme("nope")); err == nil {
		t.Fatal("constructing with an unregistered theme succeeded")
	} else if !strings.Contains(err.Error(), "builtin") {
		t.Errorf("the error does not name what is available: %v", err)
	}
}

// requireBundle skips unless the generated bundle is present.
func requireBundle(t *testing.T) {
	t.Helper()

	if _, err := os.Stat(bulmaBundle); err != nil {
		t.Skipf("the alternative theme's stylesheet is not generated (%v); run go generate ./...", err)
	}
}
