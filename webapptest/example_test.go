// Examples for the test harness. None of these carry an Output comment, which
// means "go test" compiles them and never runs them. That is deliberate: a test
// helper's documentation is exactly as good as its ability to be called, and a
// signature that moved turns these into build failures rather than into tests that
// pass for the wrong reason.
package webapptest_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/the-maldridge/webapp"
	"github.com/the-maldridge/webapp/auth"
	"github.com/the-maldridge/webapp/webapptest"
)

// testT stands in for the *testing.T a test would pass. It is never called with
// one: an example without an Output comment is compiled and not run, so nothing
// here reports a failure or fails to clean up.
func testT() *testing.T { return &testing.T{} }

func ExampleNewApp() {
	a := webapptest.NewApp(testT())
	defer a.Close()

	// The database is a file in a temporary directory, migrated before the test
	// starts and deleted by t.Cleanup, so a suite never leaves a database in the
	// source tree or shares one between two tests. Nothing here binds a port:
	// requests go straight to the handler.
	a.Route("/ui/task", func(r webapp.Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintln(w, "hello")
		})
	})

	resp := a.Get("/ui/task/")
	if resp.StatusCode != http.StatusOK {
		// Body reads the response for a failure message, which is the difference
		// between "status 500" and a stack trace in the test report.
		panic(webapptest.Body(testT(), resp))
	}
}

func ExampleApp_InstallAuth() {
	a := webapptest.NewApp(testT())
	defer a.Close()

	// InstallAuth builds a real service over the application's own database and
	// runs it through the real middleware, session codec, and CSRF check. A test
	// that passes against a stub has proven the stub agrees with the
	// implementation by accident.
	svc := a.InstallAuth(auth.WithSessionKey("a-test-key-long-enough-to-be-accepted"))

	a.Account("alice", "correct horse battery staple", "tasks:view")

	// Account returns the row, so a test can assert on what it made as well as on
	// what the application does with it.
	_ = svc
}

func ExampleApp_SignIn() {
	a := webapptest.NewApp(testT())
	defer a.Close()

	a.InstallAuth()
	a.Account("alice", "correct horse battery staple", "tasks:view")

	a.Route("/ui/task", func(r webapp.Router) {
		gate := a.RequireLogin()
		r.Handle("/", gate(http.NotFoundHandler()))
	})

	// SignIn gives the client a real session cookie, which is what a route guard
	// checks. The cheaper testas package stamps an identity into a context, which
	// does not satisfy a guard, by design.
	a.SignIn("alice")
	resp := a.Get("/ui/task/")
	fmt.Println(resp.StatusCode)

	// SignOut ends the session, which is how a test shows that a logout actually
	// cleared something rather than the route having been open all along.
	a.SignOut()
}

func ExampleApp_PostForm() {
	a := webapptest.NewApp(testT())
	defer a.Close()

	a.InstallAuth()
	a.Account("alice", "correct horse battery staple", "tasks:edit")
	a.SignIn("alice")

	a.Route("/ui/task", func(r webapp.Router) {
		r.PostErr("/new", func(w http.ResponseWriter, r *http.Request) error {
			title, err := webapp.FormString(r, "title")
			if err != nil {
				return err
			}
			fmt.Println(title)
			// The framework's own redirect helper, so the status and the header
			// are set the way every other write in the application sets them.
			a.SeeOther(w, r, "/ui/task/")
			return nil
		})
	})

	// PostForm carries the CSRF token for the signed-in identity, which is what a
	// form post needs. PostFormNoCSRF is the deliberate exception, for the test
	// that asserts the check fires.
	resp := a.PostFormValues("/ui/task/new", url.Values{"title": []string{"write it down"}})

	// The client does not follow redirects, because the framework's answer to a
	// successful write is a 303 and a helper that followed it silently would turn
	// "create redirects to the list" into a test of the list page.
	fmt.Println(resp.StatusCode, resp.Header.Get("Location"))

	followed := a.Follow(resp)
	fmt.Println(followed.Request.URL.Path)
}

func ExampleApp_CSRFToken() {
	a := webapptest.NewApp(testT())
	defer a.Close()
	a.InstallAuth()

	// A request built by hand needs the token the middleware expects. Everything
	// else about a hand-built request is the point of building it that way.
	token := a.CSRFToken()
	_ = token
}
