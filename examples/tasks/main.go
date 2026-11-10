// Package main is the example application: a task list built on the framework.
//
// It is the integration test the design names. Everything the framework does is
// used the way an application would use it: a module with routes, templates,
// assets, schema, and navigation; authentication installed after construction;
// permissions that gate both a route and a link; ownership that decides which of
// a permitted person's rows are theirs; pagination; form validation that
// re-renders what was typed; and either database driver from configuration alone.
//
// # Running it
//
//	cd examples
//	go run ./tasks
//
// The first boot with no accounts has to be told who the first account is:
//
//	TASKS_BOOTSTRAP_IDENTITY=ada \
//	TASKS_BOOTSTRAP_PASSWORD=choose-something-long \
//	  go run ./tasks
//
// Then open http://localhost:8080 and sign in. See README.md for the rest of the
// configuration, including how to run it against PostgreSQL.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/the-maldridge/webapp"
	"github.com/the-maldridge/webapp/auth"
	"github.com/the-maldridge/webapp/db"

	// The alternative theme is imported for its registration only. Without this
	// line TASKS_THEME=bulma is a startup error listing what exists, which is the
	// behaviour an application wants: a deployment variable cannot reach for a
	// stylesheet that was never compiled into the binary. Importing it is the whole
	// of what installing a theme is, so switching the example's appearance is a
	// build decision and not something that can be asked for by accident.
	//
	// The theme is a separate repository, which is the point this example makes by
	// existing: nothing in the example knows or cares that the layout rendering it
	// came from somewhere other than the framework's own module.
	_ "github.com/the-maldridge/webapp-bulma"
)

// EnvBootstrapIdentity and EnvBootstrapPassword name the account the first boot
// makes sure exists.
//
// They are configuration rather than a built-in default because a framework that
// ships a default administrator password has shipped a way in, and the
// applications that read that default are the ones that never remove it. Setting
// one without the other is a mistake worth reporting at startup: the alternative
// is an application that runs with no way in at all, which is the kind of failure
// attributed to the database.
const (
	EnvBootstrapIdentity = "TASKS_BOOTSTRAP_IDENTITY"
	EnvBootstrapPassword = "TASKS_BOOTSTRAP_PASSWORD"
)

// EnvTemplateDebug names a directory of templates to read ahead of the embedded
// ones. It is the example's own variable because the option it maps to is a
// directory, which is not something an environment default would ever get right.
const EnvTemplateDebug = "TASKS_TEMPLATE_DEBUG"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "tasks:", err)
		os.Exit(1)
	}
}

func run() error {
	// The application is a shell: everything the example demonstrates lives in
	// the module, registered by that module's own package in init().
	a, err := webapp.New(options()...)
	if err != nil {
		return err
	}
	defer a.Close()

	if err := enableAuth(a); err != nil {
		return err
	}
	if err := bootstrap(a); err != nil {
		return err
	}

	routes(a)
	return a.Run()
}

// options assembles the construction options.
//
// The tasks module is not listed here. It registers itself in init(), the way a
// module's own package does, and an application with no whitelist loads
// everything it linked. The other form is webapp.WithModule(ModuleInfo{...}),
// which supplies a module to this main() without making it visible to anything
// else in the process; both are the same description, and both are validated the
// same way, so a module that is valid one way is not secretly invalid the other.
//
// TASKS_MODULES narrows the loaded set to a list, which is the operator's half of
// the registry: the same binary, one feature off. TASKS_SCOPE does the same for a
// deployment role.
//
// The database default is the other thing worth naming. Left unset, the framework
// opens a file named for itself rather than for the application, and an example
// run from a checkout should not leave a file that looks like a test fixture in
// the repository root.
func options() []webapp.Option {
	opts := []webapp.Option{
		webapp.WithEnvPrefix("TASKS"),
		webapp.WithSiteTitle("Tasks"),
	}

	if debug := strings.TrimSpace(os.Getenv(EnvTemplateDebug)); debug != "" {
		opts = append(opts, webapp.WithTemplateDebug(debug))
	}

	dbOpts := []db.Option{}
	if !dbConfigured() {
		dbOpts = append(dbOpts, db.WithPath("tasks.db"))
	}
	return append(opts, webapp.WithDBOptions(dbOpts...))
}

// dbConfigured reports whether the environment already chooses a database. If it
// does, the example stays out of the way: the point of showing both drivers is
// that the application's own configuration wins.
func dbConfigured() bool {
	for _, key := range []string{"TASKS_DB", "TASKS_DB_DSN", "TASKS_DB_DRIVER"} {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			return true
		}
	}
	return false
}

// enableAuth installs authentication.
//
// The order is the one the framework imposes and it is worth reading as a
// sequence rather than skimming: a service needs a database handle, that handle
// exists only after New returned, and the accounts table has to be part of the
// schema before the first request, which is what registering the service as a
// component does. Without that registration the login page renders perfectly and
// every sign-in fails on a missing table, on a machine where everything looks
// configured.
func enableAuth(a *webapp.App) error {
	svc, err := auth.New(a.DB, auth.WithLogger(slog.Default()))
	if err != nil {
		return fmt.Errorf("authentication: %w", err)
	}
	if err := a.RegisterComponent(svc); err != nil {
		return fmt.Errorf("authentication schema: %w", err)
	}
	if err := a.InstallAuth(svc); err != nil {
		return fmt.Errorf("authentication: %w", err)
	}
	return nil
}

// bootstrap makes sure the operator can get in.
//
// A fresh database with no accounts and a login-only front door is a sealed
// building, and the applications that get out of that state do it by adding a
// route that creates an account without asking who asked. So the example has an
// explicit, configuration-only path instead, and it grants the groups the
// permissions in this application are issued through rather than inventing a
// parallel notion of "admin".
func bootstrap(a *webapp.App) error {
	identity := strings.TrimSpace(os.Getenv(EnvBootstrapIdentity))
	password := os.Getenv(EnvBootstrapPassword)

	switch {
	case identity == "" && password == "":
		return nil
	case identity == "":
		return fmt.Errorf("%s is set without %s", EnvBootstrapPassword, EnvBootstrapIdentity)
	case password == "":
		return fmt.Errorf("%s is set without %s", EnvBootstrapIdentity, EnvBootstrapPassword)
	}

	svc := a.AuthService()
	if svc == nil {
		return errors.New("a bootstrap account needs authentication, which is not installed")
	}

	ctx := context.Background()
	if _, err := svc.Account(ctx, identity); err != nil {
		if _, err := svc.CreateAccount(ctx, identity, password); err != nil {
			return fmt.Errorf("bootstrap account %s: %w", identity, err)
		}
		slog.Info("bootstrap account created", "identity", identity)
	}

	// Idempotent on purpose: a restart with the variables still set must not
	// fail, and must not overwrite a password the operator changed meanwhile.
	// Grant adds a membership and is a no-op when it already exists, so the
	// worst case here is that it confirms what is already true.
	for _, group := range bootstrapGroups {
		if err := svc.Grant(ctx, identity, group); err != nil {
			return fmt.Errorf("grant %s to %s: %w", group, identity, err)
		}
	}
	return nil
}

// bootstrapGroups are the memberships the bootstrap account gets. Both are named
// exactly as the permissions are spelled, because a group called "tasks-admin"
// that nobody can also write as TASKS:ADMIN is a group that grants nothing.
var bootstrapGroups = []string{"TASKS:VIEW", "TASKS:EDIT", "TASKS:DELETE", "TASKS:ADMIN"}

// routes registers the application's own routes.
//
// Sign-in and sign-out belong to the application rather than to any feature: they
// are about who you are, not about what you may do, and a module that owned them
// would be a module the others depend on for being reachable at all. The root
// redirect exists so that the address in the README is the one address.
func routes(a *webapp.App) {
	svc := a.AuthService()

	a.Route("/", func(r webapp.Router) {
		r.Get("/", r.RequireLogin(toList(a)))
		r.Get("/login", r.Public(loginPage(a)))
		r.Post("/login", r.Public(signIn(a, svc)))
		r.Post("/logout", r.RequireLogin(svc.LogoutHandler("/login")))
	})
}

// toList sends a signed-in visitor to the task list.
//
// The view permission is not re-checked here: the list's own guard answers for
// it, and an account that reaches a redirect into a 403 has learned something
// true, namely that its groups allow it to be signed in and nothing else. A home
// page that decided the same question a second time is a second answer, and the
// two drift.
func toList(a *webapp.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.SeeOther(w, r, "/ui/task/")
	}
}

// loginPage renders the framework's sign-in form.
//
// The framework owns that template, which is why the name is namespace-qualified:
// an application may replace its own login design by mounting a file, and must
// not be able to replace the page the framework renders when the application is
// the thing that is broken.
func loginPage(a *webapp.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.View(w, r, "@core/login.p2", webapp.Context{
			"action": "/login",
			"next":   r.URL.Query().Get(auth.NextField),
		})
	}
}

// signIn is the service's login handler with the framework's failure page.
//
// The default failure response is a 401 with no body, which is correct for an API
// and useless to a person who mistyped a password: they need the field back. The
// message is one sentence for every reason the attempt failed, including "no such
// account", because the difference is what an enumeration attack reads, and it is
// the reason this handler is allowed to say "did not match" and nothing more.
func signIn(a *webapp.App, svc *auth.Service) http.HandlerFunc {
	return svc.LoginHandler(auth.LoginOptions{
		Fallback: "/ui/task/",
		OnFailure: func(w http.ResponseWriter, r *http.Request, next string, err error) {
			a.View(w, r, "@core/login.p2", webapp.Context{
				"action":     "/login",
				"next":       next,
				"username":   r.FormValue(auth.UsernameField),
				"loginError": "That username and password did not match.",
			})
		},
	})
}
