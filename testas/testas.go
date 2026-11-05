/*
Package testas puts an identity on a request without a login.

As is the cheapest way to test something that reads "who is asking": a handler
that filters rows by owner, a navigation fragment that hides a link, a helper
that refuses a request. It builds no application, opens no database, and mints
no cookie, so a test of one handler does not pay for a whole application.

	viewer := testas.As(httptest.NewRequest(http.MethodGet, "/ui/task/", nil),
		"alice", "tasks:view")
	if got := visibleTasks(viewer); len(got) != 2 {
		t.Errorf("alice sees %d tasks, want 2", len(got))
	}

# What it is not

The identity is asserted, not earned. Nothing has verified a password, so a
request carrying a testas identity has not been authenticated, and a route guard
will refuse it. That is deliberate: a helper that satisfied a guard by importing
the wrong package would make every authorization test look like it passed.

The consequence is a simple rule. Testing a handler or a template that reads the
current user, use As. Testing that a route is protected, that permissions are
enforced by the router, or that a denial renders correctly, use
webapptest.App.SignIn, which obtains a real session from a real service and
therefore exercises the middleware that actually decides.

The same asymmetry exists in production: an identity is whatever the
authentication middleware says it is, and everything downstream trusts it. As is
the same agreement, reached early, for the narrow case where the middleware is
not what is under test.
*/
package testas

import (
	"net/http"
	"strings"

	"github.com/the-maldridge/webapp/auth"
)

// As returns a request carrying an identity with the given groups.
//
// Group names are upper-cased, because permission matching is: a test writing
// "tasks:view" gets the same membership the real session would carry, rather
// than one that never matches and sends the author looking for a bug in the
// permission logic.
//
// An empty identity means anonymous, which is a case worth testing rather than
// skipping: hiding a link and refusing the route behind it have to agree for it
// too. Passing "" returns the request with no identity stamped, which is what
// anonymous means here.
//
// The original request is left untouched; the identity travels on the returned
// one, so a table-driven test can build a fixture once and derive a viewer per
// case.
func As(req *http.Request, identity string, groups ...string) *http.Request {
	if req == nil {
		return nil
	}
	if identity == "" {
		return req
	}

	member := make(map[string]struct{}, len(groups))
	for _, g := range groups {
		name := strings.ToUpper(strings.TrimSpace(g))
		if name == "" {
			continue
		}
		member[name] = struct{}{}
	}

	ctx := auth.ContextWithUser(req.Context(), auth.User{
		Identity: identity,
		Groups:   member,
		AuthedBy: "testas",
	})
	return req.WithContext(ctx)
}
