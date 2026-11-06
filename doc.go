// Package webapp is the application surface for a server-rendered Go
// web application: configuration, a database handle, a router with a
// fixed middleware stack, and a single place where responses and
// failures are produced.
//
// The shape is deliberately small. An application constructs an App,
// registers routes, and calls Run:
//
//	func main() {
//		if err := run(); err != nil {
//			fmt.Fprintln(os.Stderr, err)
//			os.Exit(1)
//		}
//	}
//
//	func run() error {
//		a, err := webapp.New(
//			webapp.WithEnvPrefix("TASKS"),
//			webapp.WithDBOptions(db.WithAutoMigrate(Task{})),
//		)
//		if err != nil {
//			return err
//		}
//		defer a.Close()
//
//		h := &taskHandlers{db: a.DB}
//		a.Route("/ui/task", func(r webapp.Router) {
//			r.GetErr("/", h.handleList)
//			r.PostErr("/new", h.handleCreate)
//		})
//
//		return a.Run()
//	}
//
// Handlers are ordinary http.HandlerFunc values, usually methods on a
// struct that captured its own dependencies. The Err forms, which return
// an error, are preferred for new code: a returned error reaches the
// framework's error responder, which sets a real status code and logs a
// correlation ID.
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
//	TASKS_MODULES       comma-separated module whitelist   all
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
//  6. build the middleware stack and the router
//  7. load modules, which register routes, templates, and assets
//  8. mount static assets and install the renderer
//
// Nothing in that list calls os.Exit or panics. An application that
// cannot be configured returns an error to main, which decides what to
// report and what to exit with.
//
// The order is load-bearing in one place worth naming: authentication can
// only be installed after New returns, because a service needs a database
// handle and the handle does not exist until the application has opened one,
// while a module registers its guarded routes during New. Guards therefore
// resolve the service per request rather than at registration, and a module's
// r.Guard and r.RequireLogin work without the module knowing when
// InstallAuth happened.
//
// # Modules
//
// A module is a feature area that configuration can turn off without editing
// the binary that contains it. A module's package calls webapp.Register in
// init, which is what "the binary contains the feature" means, and the
// application decides which of what it linked actually loads. An application
// that would rather assemble modules itself passes them to webapp.WithModule;
// both forms take the same ModuleInfo and are validated the same way.
//
// What a module may contribute is a fixed list rather than an open-ended hook
// set, and the list is which interfaces its value implements:
//
//	RouteContributor  routes, indistinguishable from the application's own
//	Templater         a namespace of templates
//	Staticer          a namespace of assets, served at /static/<ns>/
//	Navigator         navigation entries, gated per request
//	Component         database models to sync
//	Migrator          a schema step that runs after models sync
//
// The last two are the application-wide component interfaces rather than
// module-specific ones, so there is one rule about what migrates instead of a
// parallel pair whose signatures nobody can keep straight.
//
// A module declares the permissions it owns, which is what lets an
// administration screen offer exactly the grants that do something in the
// deployment it is running in, rather than a list that includes grants whose
// feature was switched off.
//
// Two variables narrow the loaded set. <PREFIX>_MODULES is a whitelist of
// names, and <PREFIX>_SCOPE names the deployment role this process serves. An
// empty whitelist means "no whitelist", the same as leaving it unset; naming a
// module that is not registered is a startup error, because ignoring one is a
// deployment quietly missing a feature.
//
// Load order is deterministic: dependencies first, ties broken alphabetically.
// That matters because modules seed rows and contribute navigation, so an
// order that varied between boots would let two deployments of identical code
// differ, and "the seed is wrong" would depend on luck to reproduce.
//
// examples/tasks is a worked example of all of this, and its tests are the
// integration suite the framework does not otherwise have.
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
