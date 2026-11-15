// Examples for the identity stamped onto a request.
package testas_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/the-maldridge/webapp/auth"
	"github.com/the-maldridge/webapp/testas"
)

func ExampleAs() {
	// One fixture, one derived request per case: the original is left alone.
	fixture := httptest.NewRequest(http.MethodGet, "/ui/task/", nil)

	viewer := testas.As(fixture, "alice", "tasks:view")

	who, ok := auth.UserFrom(viewer)
	fmt.Println(who.Identity, who.AuthedBy, ok)

	// Group names arrive upper-cased because permission matching is, so writing
	// them in lower case is not a membership that never matches.
	fmt.Println(auth.HasPerm(viewer, auth.P("tasks", "view")))
	fmt.Println(auth.HasPerm(viewer, auth.P("tasks", "edit")))

	// Module administration satisfies everything in the module, and the core
	// grant satisfies everything at all.
	moduleAdmin := testas.As(fixture, "root", "tasks:admin")
	fmt.Println(auth.HasPerm(moduleAdmin, auth.P("tasks", "edit")))

	// An empty identity means anonymous, which is a case worth testing rather
	// than skipping: hiding a link and refusing the route behind it have to
	// agree for it too.
	anon := testas.As(fixture, "")
	_, ok = auth.UserFrom(anon)
	fmt.Println(ok)

	// Output:
	// alice testas true
	// true
	// false
	// true
	// false
}

// ExampleAs shows the limitation that decides when not to use this package. It is
// compiled, not run.
func ExampleAs_aGuardRefusesIt() {
	// The identity is asserted, not earned. A route guard decides from what the
	// authentication middleware resolved, and nothing resolved this, so the
	// guard answers 401:
	//
	//	r.Get("/ui/task/", r.Guard(auth.P("tasks", "view"), handler)) // 401
	//
	// Testing that a route is protected therefore means webapptest.App.SignIn,
	// which obtains a real session from a real service. As is for the handler
	// that only reads who is asking.
	r := testas.As(httptest.NewRequest(http.MethodGet, "/ui/task/", nil), "alice", "tasks:view")
	fmt.Println(auth.HasPerm(r, auth.P("tasks", "view")))

	// Output:
	// true
}
