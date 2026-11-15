// Examples for the application surface. The request-parsing helpers are pure and
// run against synthetic requests; construction, routing, and rendering examples
// carry no Output comment, which makes "go test" compile them without running
// them. That is the whole value: the call shapes in the documentation are checked
// by the compiler on every build.
package webapp_test

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"

	"github.com/the-maldridge/webapp"
	"github.com/the-maldridge/webapp/auth"
	"github.com/the-maldridge/webapp/db"
)

// task is the example application's model.
type task struct {
	ID    uint
	Title string
}

// Example shows the shortest application that does anything: construct, route,
// run. It is compiled, not run.
func Example() {
	a, err := webapp.New(
		// Configuration comes from TASKS_ADDR, TASKS_DB, and friends, with these
		// options as the defaults an operator can override.
		webapp.WithEnvPrefix("TASKS"),
		webapp.WithDBOptions(db.WithAutoMigrate(task{})),
	)
	if err != nil {
		// Nothing in the framework calls os.Exit, so main is the only place that
		// decides what a bad configuration costs.
		log.Fatal(err)
	}
	defer a.Close()

	a.Route("/ui/task", func(r webapp.Router) {
		r.GetErr("/", func(w http.ResponseWriter, r *http.Request) error {
			tasks, err := db.G[task](a.DB).Order("id ASC").Find(r.Context())
			if err != nil {
				return err
			}
			a.View(w, r, "@tasks/list.p2", webapp.Context{"Tasks": tasks})
			return nil
		})
	})

	log.Fatal(a.Run())
}

// ExampleNew shows the options that change how an application behaves rather than
// what it serves.
func ExampleNew() {
	a, err := webapp.New(
		// Production refuses the conveniences that would be vulnerabilities in
		// public: no generated session key, no error detail in a response body.
		webapp.WithProduction(),
		// Debug is a developer's setting and is not implied by production being
		// off. Parsing it is strict: TASKS_DEBUG=ture is a startup error rather
		// than a flag that quietly turned itself off.
		webapp.WithDebug(),
		// Body size is capped before anything reads it, so an oversized upload is
		// a 413 rather than a memory problem.
		webapp.WithMaxBodyBytes(4<<20),
		// Only trust a client-IP header if something in front of this process
		// actually sets it, and say how many hops to skip when reading it.
		webapp.WithTrustedProxies("10.0.0.0/8"),
		webapp.WithClientIPHeader("X-Forwarded-For"),
		// The theme is chosen by name and has to be one that is registered: an
		// unknown name is a construction error listing what is available.
		webapp.WithTheme("bulma"),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer a.Close()
}

// ExampleApp_InstallAuth wires authentication in after construction, which is the
// only order that works: a service needs a database handle, and the handle does
// not exist until New has opened one.
func ExampleApp_InstallAuth() {
	a, err := webapp.New(webapp.WithEnvPrefix("TASKS"))
	if err != nil {
		log.Fatal(err)
	}
	defer a.Close()

	svc, err := auth.New(a.DB, auth.WithSessionKey("set-me-from-a-secret-manager"))
	if err != nil {
		log.Fatal(err)
	}
	if err := a.InstallAuth(svc); err != nil {
		log.Fatal(err)
	}

	// Guards resolve the service per request, so a route registered before this
	// call is still protected and a module never has to know when the install
	// happened.
	a.Route("/ui/task", func(r webapp.Router) {
		// A grant, expressed as the application-wide middleware: 401 with no
		// identity, 403 with one that lacks the grant.
		r.Get("/dash", a.Guarded(auth.P("tasks", "view"), dashboard))

		// The same gate over a handler that returns errors. Guard is middleware and
		// WrapErr turns an error-returning handler into an http.HandlerFunc, so the
		// two compose.
		r.Handle("/", a.Guard(auth.P("tasks", "view"))(a.WrapErr(listTasks)))

		// Any signed-in identity, whatever it may do.
		r.Get("/comment", r.RequireLogin(commentPage))

		// Something reachable while not signed in, which the login page has to be.
		r.Get("/form", r.Public(loginForm))
	})
}

// The example application's handlers.
func dashboard(w http.ResponseWriter, r *http.Request)   {}
func commentPage(w http.ResponseWriter, r *http.Request) {}
func loginForm(w http.ResponseWriter, r *http.Request)   {}

func listTasks(w http.ResponseWriter, r *http.Request) error {
	id, err := webapp.QueryUint(r, "assignee")
	if err != nil {
		// A returned error reaches the framework's error responder, which sets a
		// real status code and logs a correlation ID. This is the block that
		// replaces http.Error(w, err.Error(), 500).
		return err
	}
	_ = id
	return nil
}

// ExampleRouter_Guard shows the gate helpers as they compose on a subtree.
func ExampleRouter_Guard() {
	a, err := webapp.New(webapp.WithEnvPrefix("TASKS"))
	if err != nil {
		log.Fatal(err)
	}
	defer a.Close()

	a.Route("/ui/report", func(r webapp.Router) {
		// Every route in the group is gated by the same grant, so the check is
		// stated once beside the prefix it applies to rather than repeated in each
		// handler and forgotten in one of them.
		r.Group(func(r webapp.Router) {
			r.Get("/quarterly", r.Guard(auth.P("reports", "view"), reportPage))
			r.Get("/raw", r.Guard(auth.P("reports", "admin"), reportPage))
		})
	})
}

func reportPage(w http.ResponseWriter, r *http.Request) {}

// ExampleApp_Fail shows the single error exit and the different kind of failure
// that is the user's rather than the server's.
func ExampleApp_Fail() {
	a, err := webapp.New(webapp.WithEnvPrefix("TASKS"))
	if err != nil {
		log.Fatal(err)
	}
	defer a.Close()

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/ui/task/1", nil)

	// Fail sets an accurate status, logs at error level with the request's
	// correlation ID, and shows detail only in debug. A production visitor gets a
	// generic page and an ID that matches a log line.
	a.Fail(w, r, http.StatusNotFound, errors.New("no such task"))
}

// ExampleAsRejected shows a rejection, which re-renders a form with what the person
// typed still in it.
func ExampleAsRejected() {
	var err error = &webapp.Rejected{
		Template: "@tasks/new.p2",
		Values:   webapp.Context{"Title": ""},
		Reasons:  []string{"a title is required"},
		Status:   http.StatusUnprocessableEntity,
	}

	rejected, ok := webapp.AsRejected(err)
	if !ok {
		log.Fatal("not a rejection")
	}

	// A rejection is logged at info level, not error: the server worked, the input
	// did not qualify.
	fmt.Println(rejected.Status, rejected.Reasons[0])

	// Output:
	// 422 a title is required
}

// ExampleParamUint shows the call whose error must not be discarded.
func ExampleParamUint() {
	// In a handler the route parameters come from the router:
	//
	//	id, err := webapp.ParamUint(r, "id")
	//	if err != nil {
	//		return err // a 400, not a lookup on key zero
	//	}
	//
	// The failure this prevents is the idiomatic version with the error thrown
	// away, strconv.ParseUint(chi.URLParam(r, "id"), 10, 64), where "abc" becomes
	// zero and a malformed URL turns into a mysterious 404 or, worse, a row
	// attached to whatever owns key zero.
	var h webapp.HandlerErr = func(w http.ResponseWriter, r *http.Request) error {
		_, err := webapp.ParamUint(r, "id")
		return err
	}
	fmt.Println(h != nil)

	// Output:
	// true
}

// ExampleBadInput shows what the parsing helpers report.
func ExampleBadInput() {
	r := httptest.NewRequest(http.MethodGet, "/ui/task/?assignee=abc", nil)

	_, err := webapp.QueryUint(r, "assignee")

	var bad *webapp.BadInput
	if errors.As(err, &bad) {
		fmt.Println(bad.Name, "|", bad.Value, "|", bad.Reason)
	}

	// ErrBadInput is how the error responder knows this is a 4xx: a malformed URL
	// is the client's mistake, and logging it as a server failure hides real ones.
	fmt.Println(webapp.ErrBadInput(err))

	// Output:
	// assignee | abc | must be a non-negative integer
	// true
}

func ExampleQueryUintDefault() {
	r := httptest.NewRequest(http.MethodGet, "/ui/task/?assignee=7", nil)

	// An absent filter defaults. A malformed one is an error rather than a silently
	// dropped filter, which would show more rows than were asked for.
	assignee, err := webapp.QueryUintDefault(r, "assignee", 0)
	fmt.Println(assignee, err)

	project, err := webapp.QueryUintDefault(r, "project", 3)
	fmt.Println(project, err)

	// Output:
	// 7 <nil>
	// 3 <nil>
}

func ExamplePage() {
	// Paging is clamped on the way in, so a request cannot ask for an offset past
	// the end of a table or a result set sized to exhaust memory.
	r := httptest.NewRequest(http.MethodGet, "/ui/task/?page=3&size=50", nil)
	page, size, err := webapp.Page(r)
	fmt.Println(page, size, err)

	r = httptest.NewRequest(http.MethodGet, "/ui/task/?page=0&size=100000", nil)
	page, size, err = webapp.Page(r)
	fmt.Println(page, size, err)

	// Output:
	// 3 50 <nil>
	// 1 250 <nil>
}

func ExampleQueryBool() {
	// "false" is false, which a non-empty check gets wrong and which is how a
	// hidden input carrying the string "false" ends up meaning yes.
	for _, q := range []string{"", "?archive=true", "?archive=false", "?archive=1"} {
		r := httptest.NewRequest(http.MethodGet, "/ui/task/"+q, nil)
		v, err := webapp.QueryBool(r, "archive")
		fmt.Println(v, err)
	}

	// Output:
	// false <nil>
	// true <nil>
	// false <nil>
	// true <nil>
}

// ExampleWithModule registers a feature area that configuration can switch off
// without editing the binary that contains it.
func ExampleWithModule() {
	a, err := webapp.New(
		webapp.WithEnvPrefix("TASKS"),
		// The whitelist of what loads. Naming a module that is not registered is a
		// startup error, because ignoring one is a deployment quietly missing a
		// feature.
		webapp.WithModules("tasks"),
		webapp.WithModule(webapp.ModuleInfo{
			Name:        "tasks",
			Scopes:      []string{"web"},
			Required:    true,
			Permissions: []auth.Perm{auth.P("tasks", "view"), auth.P("tasks", "edit")},
			Factory: func(s *webapp.Scope) (any, error) {
				// The scope is the way in: the application, the database, the
				// configuration, and whatever a dependency registered.
				return &taskRepo{d: s.DB()}, nil
			},
		}),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer a.Close()

	fmt.Println(a.LoadedModules())
}

// taskRepo is the value the example module's factory returns. Which contributions a
// module makes is decided by the interfaces its value implements from a fixed list:
// RouteContributor, Templater, Staticer, Navigator, Component, Migrator. There is
// no hook set beyond that list, which is what keeps a module's behavior readable
// from its type.
type taskRepo struct{ d *db.DB }

// Routes makes this a webapp.RouteContributor.
func (repo *taskRepo) Routes(rt webapp.Router) {
	rt.GetErr("/", func(w http.ResponseWriter, r *http.Request) error {
		tasks, err := db.G[task](repo.d).Order("id ASC").Find(r.Context())
		if err != nil {
			return err
		}
		_ = tasks
		return nil
	})
}
