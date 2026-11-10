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
//	TASKS_MODULES       comma-separated module whitelist  all
//	TASKS_THEME         theme the framework renders with  builtin
//
// A theme is selected by name, and the name has to match a theme that
// is registered: builtin, or one whose package the application imported.
// An unknown name is a construction error listing what is available.
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
// # Themes
//
// A theme is the set of pages the framework renders itself: the base layout,
// the login form, the navigation fragment, and the three error pages. The
// active theme is mounted as the framework's own namespace rather than in
// front of it, so an application's templates, which extend @core/base.p2 to
// reach the framework's layout, change appearance without any of them being
// edited.
//
// A theme may live anywhere. It is a ThemeInfo value registered from a
// package's init, and the application pulls it in with a blank import:
//
//	func init() {
//		webapp.RegisterTheme(webapp.ThemeInfo{
//			Name:     "kite",
//			FS:       func() fs.FS { return templates },
//			Static:   func() fs.FS { return assets },
//			Required: webapp.RequiredTemplates(),
//		})
//	}
//
// RequiredTemplates is the list the framework renders itself and therefore
// has to be able to compile, and package webapp/themetest checks a theme
// against every rule construction applies, from inside the theme's own
// package, with no application built. Both are usable from a module that has
// webapp as a dependency; nothing about writing a theme requires this
// repository.
//
// What construction checks, and why each one is there:
//
//	Templates. Every page the framework renders itself must exist and compile.
//	   An error template is rendered only when something has already gone
//	   wrong, which is the worst moment to discover one is broken and the
//	   least likely moment to investigate it.
//
//	Blocks. A layout must declare every block the framework's templates and an
//	   application's pages fill: title, head, bodyclass, header, content, and
//	   footer on the base layout, and detail on the error skeleton. Pongo2
//	   drops a filled block whose parent never declared it, with no error at
//	   parse time or render time, so a skeleton missing detail renders three
//	   error pages with the reason for the failure removed. Nothing else in
//	   the system notices, including a test that asserts the page renders.
//
//	Assets. A layout's stylesheet link resolves under /static/core, and the
//	   file has to be in the binary, so construction reads the layout and
//	   refuses a theme whose links do not resolve. A theme that vendors a
//	   generated asset has to ship it, because go generate runs only in the
//	   module that owns the package: an application importing the theme has no
//	   way to produce what the theme did not commit.
//
// Two things remain the theme's own responsibility, because the framework
// genuinely cannot check them:
//
//	Filters. A theme may register template filters from init, but pongo2's
//	   filter registry is one process-global map, so a filter name a theme
//	   invents can collide with one another package claimed first, and the
//	   loser finds out at startup. Prefixing the names is cheaper than an
//	   argument about who owns "truncate".
//
//	Behaviour. A theme owns its markup entirely, and the framework cannot
//	   stop it from redrawing the login form. What a redraw must not lose is
//	   the CSRF field, the password that is never repopulated, and the single
//	   failure message that does not reveal which half of the credentials was
//	   wrong.
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
