// Package webapp is the application surface for a server-rendered Go
// web application: configuration, a database handle, a router with a
// fixed middleware stack, and a single place where responses and
// failures are produced.
//
// The shape is deliberately small. An application constructs an App,
// registers routes, and calls Run:
//
//	func main() {
//		a, err := webapp.New(
//			webapp.WithEnvPrefix("TASKS"),
//			webapp.WithDBOptions(db.WithAutoMigrate(Task{})),
//		)
//		if err != nil {
//			return err
//		}
//
//		a.Route("/ui/task", func(r webapp.Router) {
//			r.Get("/", a.handleList)
//			r.Post("/new", a.handleCreate)
//		})
//
//		return a.Run()
//	}
//
// Handlers are ordinary http.HandlerFunc values, usually methods on a
// struct that captured its own dependencies. webapp.HandlerErr, which
// returns an error, is preferred for new code: a returned error reaches
// the framework's error responder, which sets a real status code and
// logs a correlation ID.
//
// # Configuration
//
// Precedence is always explicit option, then environment variable, then
// documented default. With a prefix of "TASKS" the framework reads:
//
//	TASKS_ADDR          listen address                    ":8080"
//	TASKS_DEBUG         verbose errors, template debug    unset
//	TASKS_PRODUCTION    hardening, no generated secrets   unset
//	TASKS_COOKIE_SECURE Secure attribute on cookies       true unless TASKS_DEBUG
//	TASKS_MODULES       comma-separated module allowlist  all
//
// The database adds its own set, documented in package
// github.com/the-maldridge/webapp/db, under the same prefix.
//
// Values are parsed strictly: TASKS_DEBUG=ture is a startup error rather
// than a silently false flag, because a debug flag that quietly turns
// itself off in production is indistinguishable from one that was never
// set.
//
// # Construction
//
// New runs a fixed sequence and returns on the first failure:
//
//  1. apply options
//  2. fill in defaults from the environment
//  3. validate the whole configuration, aggregating every problem
//  4. open the database and run migrations
//  5. take on components' models and run their migrations
//  6. register the renderer and build the middleware stack
//  7. build the router and install the error handlers
//
// Nothing in that list calls os.Exit or panics. An application that
// cannot be configured returns an error to main, which decides what to
// report and what to exit with.
//
// # Failure handling
//
// App.Fail is the single error exit. It always sets an accurate status
// code, logs at error level with the request's correlation ID, and
// renders error text only when debug mode is on; production users see a
// generic page and an ID that matches a log line.
//
// A returned error that unwraps to *webapp.Rejected is different in
// kind: it is the user's input being refused, not the server failing. It
// re-renders a form with the submitted values and the reasons, and is
// logged at info level.
package webapp
