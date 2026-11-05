package webapp

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/the-maldridge/webapp/auth"
	"github.com/the-maldridge/webapp/db"
)

// withCleanModuleRegistry restores the process-global registry after a test that
// registered into it. Without it, a test about registration would decide what
// every later application in the package loads, and the failure would surface as
// an unrelated test finding a route it never registered.
func withCleanModuleRegistry(t *testing.T) {
	t.Helper()

	moduleMu.Lock()
	saved := moduleRegistry
	moduleRegistry = nil
	moduleMu.Unlock()

	t.Cleanup(func() {
		moduleMu.Lock()
		moduleRegistry = saved
		moduleMu.Unlock()
	})
}

// registerModule registers for the duration of the test.
func registerModule(t *testing.T, info ModuleInfo) {
	t.Helper()
	Register(info)
}

// quietModule is the smallest thing the registry accepts: a value with no
// capabilities at all.
func quietModule(name string) ModuleInfo {
	return ModuleInfo{
		Name:    name,
		Factory: func(s *Scope) (any, error) { return name, nil },
	}
}

func TestDuplicateRegistrationPanics(t *testing.T) {
	withCleanModuleRegistry(t)

	Register(ModuleInfo{Name: "billing", Factory: func(s *Scope) (any, error) { return 1, nil }})

	defer func() {
		rec := recover()
		if rec == nil {
			t.Fatal("a second module with the same name registered without complaint")
		}
		if !strings.Contains(fmt.Sprint(rec), "billing") {
			t.Errorf("panic does not name the module: %v", rec)
		}
	}()
	Register(ModuleInfo{Name: "billing", Factory: func(s *Scope) (any, error) { return 2, nil }})
}

func TestRegistrationRejectsReservedAndMalformedNames(t *testing.T) {
	withCleanModuleRegistry(t)

	for _, name := range []string{"core", "dev", "", "@weird", "a/b", "with space"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Register(%q) was accepted", name)
				}
			}()
			Register(ModuleInfo{Name: name, Factory: func(s *Scope) (any, error) { return 1, nil }})
		}()
	}
}

func TestRegistrationRequiresFactory(t *testing.T) {
	withCleanModuleRegistry(t)

	defer func() {
		if recover() == nil {
			t.Fatal("a module with no factory was accepted; it would contribute nothing")
		}
	}()
	Register(ModuleInfo{Name: "hollow"})
}

func TestRequiredDependingOnOptionalPanics(t *testing.T) {
	t.Run("dependency registered first", func(t *testing.T) {
		withCleanModuleRegistry(t)
		Register(ModuleInfo{Name: "optional", Factory: func(s *Scope) (any, error) { return 1, nil }})

		defer func() {
			rec := recover()
			if rec == nil {
				t.Fatal("a required module may not depend on one configuration can disable")
			}
			if !strings.Contains(fmt.Sprint(rec), "optional") {
				t.Errorf("panic does not name the dependency: %v", rec)
			}
		}()
		Register(ModuleInfo{
			Name:         "essential",
			Required:     true,
			Dependencies: []string{"optional"},
			Factory:      func(s *Scope) (any, error) { return 2, nil },
		})
	})

	// Registration order must not decide whether the conflict is caught,
	// because package initialisation order across a program is not something a
	// module author can predict or control.
	t.Run("dependent registered first", func(t *testing.T) {
		withCleanModuleRegistry(t)
		Register(ModuleInfo{
			Name:         "essential",
			Required:     true,
			Dependencies: []string{"optional"},
			Factory:      func(s *Scope) (any, error) { return 2, nil },
		})

		defer func() {
			if recover() == nil {
				t.Fatal("registering the optional half of the conflict was accepted")
			}
		}()
		Register(ModuleInfo{Name: "optional", Factory: func(s *Scope) (any, error) { return 1, nil }})
	})
}

func TestRequiredModulesMayDependOnEachOther(t *testing.T) {
	withCleanModuleRegistry(t)

	Register(ModuleInfo{Name: "accounts", Required: true, Factory: func(s *Scope) (any, error) { return 1, nil }})
	Register(ModuleInfo{
		Name:         "audit",
		Required:     true,
		Dependencies: []string{"accounts"},
		Factory:      func(s *Scope) (any, error) { return 2, nil },
	})

	a := newTestApp(t)
	if got := strings.Join(a.LoadedModules(), ","); got != "accounts,audit" {
		t.Errorf("LoadedModules = %v, want accounts,audit", got)
	}
}

func TestSelfDependencyPanics(t *testing.T) {
	withCleanModuleRegistry(t)

	defer func() {
		if recover() == nil {
			t.Fatal("a module depending on itself was accepted")
		}
	}()
	Register(ModuleInfo{
		Name:         "ouroboros",
		Dependencies: []string{" Ouroboros "},
		Factory:      func(s *Scope) (any, error) { return 1, nil },
	})
}

func TestMissingDependencyFailsConstruction(t *testing.T) {
	withCleanModuleRegistry(t)

	registerModule(t, ModuleInfo{
		Name:         "orphan",
		Dependencies: []string{"never-registered"},
		Factory:      func(s *Scope) (any, error) { return 1, nil },
	})

	// Construction fails with an error rather than panicking: an unregistered
	// dependency is an import or build mistake, and the application's main()
	// has to be able to report it rather than inherit a panic.
	a, err := newTestAppNoBuild(t)
	if err == nil {
		a.Close()
		t.Fatal("a module with an unregistered dependency constructed anyway")
	}
	if !strings.Contains(err.Error(), "never-registered") {
		t.Errorf("error does not name the missing dependency: %v", err)
	}
}

func TestDisabledDependencyFailsConstruction(t *testing.T) {
	withCleanModuleRegistry(t)

	registerModule(t, ModuleInfo{Name: "store", Factory: func(s *Scope) (any, error) { return 1, nil }})
	registerModule(t, ModuleInfo{
		Name:         "reports",
		Dependencies: []string{"store"},
		Factory:      func(s *Scope) (any, error) { return 2, nil },
	})

	// The whitelist names reports but not its dependency. Silently dropping
	// reports would be a deployment missing a feature; loading it broken is
	// worse, so the answer is an error stating what to add.
	a, err := newTestAppNoBuild(t, WithModules("reports"))
	if err == nil {
		a.Close()
		t.Fatal("a module loaded without its dependency")
	}
	if !strings.Contains(err.Error(), "store") || !strings.Contains(err.Error(), "TESTAPP_MODULES") {
		t.Errorf("error should name the dependency and the variable that fixes it: %v", err)
	}
}

func TestEnabledModuleMustBeRegistered(t *testing.T) {
	withCleanModuleRegistry(t)

	registerModule(t, quietModule("present"))

	a, err := newTestAppNoBuild(t, WithModules("present", "typoed"))
	if err == nil {
		a.Close()
		t.Fatal("a whitelist entry naming nothing was accepted")
	}
	if !strings.Contains(err.Error(), "typoed") {
		t.Errorf("error does not name the unknown module: %v", err)
	}
	// The message lists what could be enabled, because the operator asking
	// "why won't it start" should not have to read source to find out.
	if !strings.Contains(err.Error(), "present") {
		t.Errorf("error does not list the registered modules: %v", err)
	}
}

func TestWhitelistOmitsAndKeeps(t *testing.T) {
	withCleanModuleRegistry(t)

	registerModule(t, quietModule("alpha"))
	registerModule(t, quietModule("beta"))
	registerModule(t, ModuleInfo{
		Name:     "always",
		Required: true,
		Factory:  func(s *Scope) (any, error) { return "always", nil },
	})

	a := newTestApp(t, WithModules("beta"))
	if got := strings.Join(a.LoadedModules(), ","); got != "always,beta" {
		t.Errorf("LoadedModules = %q, want \"always,beta\"", got)
	}
	if _, ok := a.Module("alpha"); ok {
		t.Error("a module excluded by the whitelist was constructed anyway")
	}
}

func TestScopeSelectsModules(t *testing.T) {
	withCleanModuleRegistry(t)

	registerModule(t, ModuleInfo{
		Name:    "webby",
		Scopes:  []string{"web"},
		Factory: func(s *Scope) (any, error) { return 1, nil },
	})
	registerModule(t, ModuleInfo{
		Name:    "workery",
		Scopes:  []string{"worker"},
		Factory: func(s *Scope) (any, error) { return 2, nil },
	})
	registerModule(t, quietModule("everywhere"))

	web := newTestApp(t, WithDeploymentScope("web"))
	if got := strings.Join(web.LoadedModules(), ","); got != "everywhere,webby" {
		t.Errorf("scope web loaded %q", got)
	}

	worker := newTestApp(t, WithDeploymentScope("WORKER"))
	if got := strings.Join(worker.LoadedModules(), ","); got != "everywhere,workery" {
		t.Errorf("scope WORKER loaded %q, want the same as worker", got)
	}

	// An unset scope loads everything, scope lists included: the default must
	// be the boring single-binary deployment, not a configuration step.
	all := newTestApp(t)
	if got := strings.Join(all.LoadedModules(), ","); got != "everywhere,webby,workery" {
		t.Errorf("no scope loaded %q, want every module", got)
	}
}

func TestDependencyAcrossScopesFails(t *testing.T) {
	withCleanModuleRegistry(t)

	registerModule(t, ModuleInfo{Name: "queue", Scopes: []string{"worker"}, Factory: func(s *Scope) (any, error) { return 1, nil }})
	registerModule(t, ModuleInfo{
		Name:         "dashboard",
		Dependencies: []string{"queue"},
		Factory:      func(s *Scope) (any, error) { return 2, nil },
	})

	a, err := newTestAppNoBuild(t, WithDeploymentScope("web"))
	if err == nil {
		a.Close()
		t.Fatal("a module in scope depends on one the scope excluded, and construction succeeded")
	}
	if !strings.Contains(err.Error(), "queue") || !strings.Contains(err.Error(), "web") {
		t.Errorf("error should name the dependency and the scope: %v", err)
	}
}

func TestLoadOrderIsDeterministic(t *testing.T) {
	withCleanModuleRegistry(t)

	// Registered in an order unrelated to any dependency, so that an
	// implementation sorting by registration order rather than by name fails
	// here instead of in a deployment where an import moved.
	for _, name := range []string{"c", "a", "b"} {
		registerModule(t, quietModule(name))
	}

	for i := 0; i < 25; i++ {
		a := newTestApp(t)
		if got := strings.Join(a.LoadedModules(), ","); got != "a,b,c" {
			t.Fatalf("iteration %d loaded %q, want a,b,c", i, got)
		}
		a.Close()
	}
}

// collabModule is what one module asserts another against: the consumer declares
// the shape it needs, so the dependency points from the consumer.
type collabModule interface {
	Greeting() string
}

type greeter struct{ greeting string }

func (g greeter) Greeting() string { return g.greeting }

func TestModulesAreInstantiatedInDependencyOrder(t *testing.T) {
	withCleanModuleRegistry(t)

	var steps []string
	record := func(step string) { steps = append(steps, step) }

	registerModule(t, ModuleInfo{
		Name:         "leaf",
		Dependencies: []string{"middle"},
		Factory: func(s *Scope) (any, error) {
			record("leaf")
			if _, err := s.MustDependency("middle"); err != nil {
				return nil, err
			}
			return "leaf", nil
		},
	})
	registerModule(t, ModuleInfo{
		Name:         "middle",
		Dependencies: []string{"root"},
		Factory: func(s *Scope) (any, error) {
			record("middle")
			return "middle", nil
		},
	})
	registerModule(t, ModuleInfo{
		Name:    "root",
		Factory: func(s *Scope) (any, error) { record("root"); return "root", nil },
	})

	a := newTestApp(t)
	if got := strings.Join(steps, ","); got != "root,middle,leaf" {
		t.Errorf("factories ran in order %q", got)
	}
	if v, ok := a.Module("LEAF"); !ok || v != "leaf" {
		t.Errorf("Module lookup = %v, %v", v, ok)
	}
}

func TestConsumerDeclaredInterfaceCollaboration(t *testing.T) {
	withCleanModuleRegistry(t)

	registerModule(t, ModuleInfo{
		Name:     "hello",
		Factory:  func(s *Scope) (any, error) { return greeter{greeting: "hi"}, nil },
		Required: true,
	})

	type consumer struct{ greeting string }

	registerModule(t, ModuleInfo{
		Name:         "consumer",
		Dependencies: []string{"hello"},
		Factory: func(s *Scope) (any, error) {
			c := &consumer{}
			value, _ := s.Get("hello")
			if h, ok := value.(collabModule); ok {
				c.greeting = h.Greeting()
			}
			return c, nil
		},
	})

	a := newTestApp(t)
	v, ok := a.Module("consumer")
	if !ok {
		t.Fatal("consumer not loaded")
	}
	if got := v.(*consumer).greeting; got != "hi" {
		t.Errorf("greeting through the asserted interface = %q", got)
	}
}

func TestMustDependencyNamesTheMissingModule(t *testing.T) {
	withCleanModuleRegistry(t)

	var inner error
	registerModule(t, ModuleInfo{
		Name: "needy",
		Factory: func(s *Scope) (any, error) {
			_, inner = s.MustDependency("absent")
			return &struct{}{}, inner
		},
	})

	if _, err := newTestAppNoBuild(t); err == nil {
		t.Fatal("a factory whose required dependency was absent constructed anyway")
	}
	if inner == nil || !strings.Contains(inner.Error(), "absent") {
		t.Errorf("MustDependency did not name the missing module: %v", inner)
	}
}

func TestDepsIncludesLoadedCollaborators(t *testing.T) {
	withCleanModuleRegistry(t)

	registerModule(t, ModuleInfo{
		Name:     "provider",
		Factory:  func(s *Scope) (any, error) { return "provided", nil },
		Required: true,
	})

	var seen map[string]any
	registerModule(t, ModuleInfo{
		Name:         "consumer",
		Dependencies: []string{"provider"},
		Factory: func(s *Scope) (any, error) {
			seen = s.Deps()
			return &struct{}{}, nil
		},
	})

	newTestApp(t)
	if got := seen["provider"]; got != "provided" {
		t.Errorf("Deps = %v, want the provider's value", seen)
	}
}

func TestFactoryErrorAbortsConstruction(t *testing.T) {
	withCleanModuleRegistry(t)

	boom := errors.New("cannot reach the queue")
	registerModule(t, ModuleInfo{
		Name:    "broken",
		Factory: func(s *Scope) (any, error) { return nil, boom },
	})

	a, err := newTestAppNoBuild(t)
	if err == nil {
		a.Close()
		t.Fatal("a failing factory produced an application")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error should wrap the factory's: %v", err)
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Errorf("error does not name the module: %v", err)
	}
}

func TestFactoryPanicBecomesConstructionError(t *testing.T) {
	withCleanModuleRegistry(t)

	registerModule(t, ModuleInfo{
		Name: "panicky",
		Factory: func(s *Scope) (any, error) {
			var rows []int
			return rows[3], nil
		},
	})

	a, err := newTestAppNoBuild(t)
	if err == nil {
		a.Close()
		t.Fatal("a factory panic escaped construction")
	}
	if !strings.Contains(err.Error(), "panicky") || !strings.Contains(err.Error(), "panic") {
		t.Errorf("error should name the module and say it panicked: %v", err)
	}
}

func TestNilModuleValueIsRejected(t *testing.T) {
	cases := map[string]func(s *Scope) (any, error){
		"untyped nil":   func(s *Scope) (any, error) { return nil, nil },
		"typed nil":     func(s *Scope) (any, error) { var g *greeter; return g, nil },
		"nil map":       func(s *Scope) (any, error) { var m map[string]int; return m, nil },
		"nil func":      func(s *Scope) (any, error) { var f func(); return f, nil },
		"nil slice":     func(s *Scope) (any, error) { var xs []int; return xs, nil },
		"nil interface": func(s *Scope) (any, error) { var c collabModule; return c, nil },
	}

	for name, factory := range cases {
		t.Run(name, func(t *testing.T) {
			withCleanModuleRegistry(t)
			registerModule(t, ModuleInfo{Name: "returnsnil", Factory: factory})

			a, err := newTestAppNoBuild(t)
			if err == nil {
				a.Close()
				t.Fatal("a factory returning nothing with no error was accepted")
			}
			// The message has to name the anti-pattern: whoever returns nil
			// believes it means "not applicable here".
			if !strings.Contains(err.Error(), "nil") {
				t.Errorf("error does not explain the nil: %v", err)
			}
		})
	}
}

func TestDependencyCycleIsReported(t *testing.T) {
	candidates := map[string]ModuleInfo{
		"a": {Name: "a", Dependencies: []string{"b"}},
		"b": {Name: "b", Dependencies: []string{"c"}},
		"c": {Name: "c", Dependencies: []string{"a"}},
	}

	order, err := topoSortModules(candidates)
	if err == nil {
		t.Fatalf("a cycle sorted anyway: %v", order)
	}
	for _, name := range []string{"a", "b", "c"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the cycle error omits %s: %v", name, err)
		}
	}
}

// moduleProbe exercises the capability interfaces.
type moduleProbe struct {
	routesRegistered bool
}

func (m *moduleProbe) Routes(r Router) {
	m.routesRegistered = true
	r.Get("/probe-module", func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, "from module")
	})
	r.Route("/probe-sub", func(sub Router) {
		sub.Get("/x", func(w http.ResponseWriter, req *http.Request) { fmt.Fprint(w, "sub") })
	})
}

func (m *moduleProbe) Templates() (string, fs.FS) {
	return "probe", mapFS(map[string]string{"page.p2": `module page for {{ who }}`})
}

func (m *moduleProbe) Nav() []NavItem {
	return []NavItem{{Weight: 70, Label: "Probe", Href: "/probe-module/", Perm: auth.P("probe", "view")}}
}

func TestModuleCapabilitiesAreApplied(t *testing.T) {
	withCleanModuleRegistry(t)

	probe := &moduleProbe{}
	registerModule(t, ModuleInfo{
		Name:        "probe",
		Factory:     func(s *Scope) (any, error) { return probe, nil },
		Permissions: []auth.Perm{auth.P("probe", "view")},
	})

	a := newTestApp(t)

	if !probe.routesRegistered {
		t.Error("Routes was never called")
	}
	if rec := a.get(t, "/probe-module"); rec.Code != http.StatusOK || rec.Body.String() != "from module" {
		t.Errorf("module route: %d %q", rec.Code, rec.Body.String())
	}
	if rec := a.get(t, "/probe-sub/x"); rec.Code != http.StatusOK || rec.Body.String() != "sub" {
		t.Errorf("nested module route: %d %q", rec.Code, rec.Body.String())
	}

	// A module's templates resolve through the same renderer the application
	// uses, under the module's own namespace.
	a.Route("/rendered", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, req *http.Request) {
			a.View(w, req, "@probe/page.p2", Context{"who": "viewer"})
		})
	})
	rec := a.get(t, "/rendered")
	if rec.Code != http.StatusOK {
		t.Fatalf("module template: status %d body %q", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "module page for viewer" {
		t.Errorf("module template body = %q", rec.Body.String())
	}

	// Navigation arrived and is still filtered per request by permission.
	if len(a.navItems) != 1 || a.navItems[0].Label != "Probe" {
		t.Fatalf("module nav missing: %+v", a.navItems)
	}
	if entries := navFor(requestFor(""), a.navItems); len(entries) != 0 {
		t.Error("an anonymous viewer sees a gated module nav item")
	}
	if entries := navFor(requestFor("ada", "PROBE:VIEW"), a.navItems); len(entries) != 1 {
		t.Error("the grant does not reveal the nav item")
	}

	// Declared permissions are queryable and name their owner.
	if perms := a.Permissions(); len(perms) != 1 || perms[0].String() != "PROBE:VIEW" {
		t.Errorf("Permissions = %v", perms)
	}
	if owner, ok := a.ModuleOwner(auth.P("probe", "view")); !ok || owner != "probe" {
		t.Errorf("ModuleOwner = %q, %v", owner, ok)
	}
	if owner, ok := a.ModuleOwner(auth.P("probe", "admin")); ok {
		t.Errorf("an undeclared permission has an owner: %q", owner)
	}
}

// guardedProbeModule registers gated routes, which is the case a guard that
// resolved the auth service when it was built gets wrong: modules register
// during construction, authentication is installed after it.
type guardedProbeModule struct{}

func (guardedProbeModule) Routes(r Router) {
	r.Get("/guarded-module", r.Guard(auth.P("guarded", "view"), okHandler))
	r.Get("/login-module", r.RequireLogin(okHandler))
}

func TestModuleGuardedRoutesSeeTheLaterInstall(t *testing.T) {
	withCleanModuleRegistry(t)

	registerModule(t, ModuleInfo{
		Name:        "guarded",
		Factory:     func(s *Scope) (any, error) { return guardedProbeModule{}, nil },
		Permissions: []auth.Perm{auth.P("guarded", "view")},
	})

	a := newTestApp(t)
	svc := newCoreAuthService(t)
	installAuth(t, a, svc)

	if rec := a.get(t, "/guarded-module"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous module route = %d, want 401", rec.Code)
	}
	holder := withCookie(httptest.NewRequest(http.MethodGet, "/guarded-module", nil),
		sessionCookieFor(t, svc, "ada", "GUARDED:VIEW"))
	if rec := a.send(t, holder); rec.Code != http.StatusOK {
		t.Fatalf("holder of the module's own permission = %d, want 200", rec.Code)
	}
	anyone := withCookie(httptest.NewRequest(http.MethodGet, "/login-module", nil),
		sessionCookieFor(t, svc, "ada"))
	if rec := a.send(t, anyone); rec.Code != http.StatusOK {
		t.Fatalf("login-gated module route = %d, want 200", rec.Code)
	}
}

// greasyModule adds middleware inside its own registration.
type greasyModule struct{}

func (greasyModule) Routes(r Router) {
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("X-Module-Mw", "greasy")
			next.ServeHTTP(w, req)
		})
	})
	r.Get("/greasy/thing", func(w http.ResponseWriter, req *http.Request) { fmt.Fprint(w, "greased") })
}

func TestModuleMiddlewareCannotLeak(t *testing.T) {
	withCleanModuleRegistry(t)

	registerModule(t, ModuleInfo{
		Name:    "greasy",
		Factory: func(s *Scope) (any, error) { return greasyModule{}, nil },
	})

	a := newTestApp(t)
	// Registered after the module, which is the direction a shared Use would
	// leak in.
	a.Route("/elsewhere", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, req *http.Request) { fmt.Fprint(w, "clean") })
	})

	if rec := a.get(t, "/greasy/thing"); rec.Header().Get("X-Module-Mw") == "" {
		t.Error("the module's middleware did not apply to its own route")
	}
	if rec := a.get(t, "/elsewhere"); rec.Header().Get("X-Module-Mw") != "" {
		t.Error("a module's middleware leaked onto another module's routes")
	}
}

func TestModuleTemplateNamespaceCollisionFailsStartup(t *testing.T) {
	withCleanModuleRegistry(t)

	registerModule(t, ModuleInfo{Name: "first", Factory: func(s *Scope) (any, error) { return &moduleProbe{}, nil }})
	registerModule(t, ModuleInfo{Name: "second", Factory: func(s *Scope) (any, error) { return &moduleProbe{}, nil }})

	a, err := newTestAppNoBuild(t)
	if err == nil {
		a.Close()
		t.Fatal("two modules mounted the same template namespace")
	}
	if !strings.Contains(err.Error(), "first") || !strings.Contains(err.Error(), "second") {
		t.Errorf("the error should name both claimants: %v", err)
	}
}

func TestModuleCannotClaimFrameworkNamespaces(t *testing.T) {
	for _, ns := range []string{"core", "dev"} {
		t.Run(ns, func(t *testing.T) {
			withCleanModuleRegistry(t)
			registerModule(t, ModuleInfo{
				Name:    "namegrabber",
				Factory: func(s *Scope) (any, error) { return nsHijacker{ns: ns}, nil },
			})

			a, err := newTestAppNoBuild(t)
			if err == nil {
				a.Close()
				t.Fatalf("a module mounted namespace %q", ns)
			}
			if !strings.Contains(err.Error(), "reserved") {
				t.Errorf("error should say the namespace is reserved: %v", err)
			}
		})
	}
}

type nsHijacker struct{ ns string }

func (h nsHijacker) Templates() (string, fs.FS) {
	return h.ns, mapFS(map[string]string{"evil.p2": "nope"})
}

func TestModuleNameCollisionWithApplicationTemplateRoot(t *testing.T) {
	withCleanModuleRegistry(t)

	registerModule(t, ModuleInfo{
		Name:    "overlap",
		Factory: func(s *Scope) (any, error) { return nsHijacker{ns: "claimed"}, nil },
	})

	a, err := newTestAppNoBuild(t, WithTemplates("claimed", mapFS(map[string]string{"x.p2": "app"})))
	if err == nil {
		a.Close()
		t.Fatal("a module mounted a namespace the application already had")
	}
	if !strings.Contains(err.Error(), "already claimed") {
		t.Errorf("error should say the namespace is taken: %v", err)
	}
}

func TestModulePermissionsConflictFailsStartup(t *testing.T) {
	withCleanModuleRegistry(t)

	registerModule(t, ModuleInfo{
		Name:        "one",
		Permissions: []auth.Perm{auth.P("shared", "edit")},
		Factory:     func(s *Scope) (any, error) { return 1, nil },
	})
	registerModule(t, ModuleInfo{
		Name:        "two",
		Permissions: []auth.Perm{auth.P("SHARED", "EDIT")},
		Factory:     func(s *Scope) (any, error) { return 2, nil },
	})

	a, err := newTestAppNoBuild(t)
	if err == nil {
		a.Close()
		t.Fatal("two modules own the same grant")
	}
	if !strings.Contains(err.Error(), "already owns") {
		t.Errorf("error should name the existing owner: %v", err)
	}
}

// moduleModel is a table a module owns.
type moduleModel struct {
	ID   uint
	Note string
}

// schemaModule implements both Component and Migrator, the same interfaces every
// other component implements, so there is one rule about what syncs and
// migrates rather than a second pair with similar signatures.
type schemaModule struct{ steps *[]string }

func (m schemaModule) Models() []any { return []any{moduleModel{}} }

func (m schemaModule) Migrate(ctx context.Context, d *db.DB) error {
	*m.steps = append(*m.steps, "migrated")

	// The module's own table has to be queryable from its own migration, which
	// is only true if its models were synced when it was constructed rather
	// than after every module had been.
	if _, err := db.G[moduleModel](d).Count(context.Background(), "id"); err != nil {
		return fmt.Errorf("the module's own table is not queryable from its migration: %w", err)
	}
	return nil
}

func TestModuleSchemaAndMigrationOrder(t *testing.T) {
	withCleanModuleRegistry(t)

	var steps []string

	registerModule(t, ModuleInfo{
		Name:     "earlier",
		Factory:  func(s *Scope) (any, error) { steps = append(steps, "built:earlier"); return &struct{}{}, nil },
		Required: true,
	})
	registerModule(t, ModuleInfo{
		Name:         "later",
		Dependencies: []string{"earlier"},
		Factory: func(s *Scope) (any, error) {
			steps = append(steps, "built:later")
			return schemaModule{steps: &steps}, nil
		},
	})

	newTestApp(t)
	if got := strings.Join(steps, ","); got != "built:earlier,built:later,migrated" {
		t.Errorf("construction and migration interleaved wrong: %q", got)
	}
}

func TestModuleMigrationFailureAbortsConstruction(t *testing.T) {
	withCleanModuleRegistry(t)

	registerModule(t, ModuleInfo{
		Name:    "unmigratable",
		Factory: func(s *Scope) (any, error) { return failingMigrator{}, nil },
	})

	a, err := newTestAppNoBuild(t)
	if err == nil {
		a.Close()
		t.Fatal("a module whose migration failed still started")
	}
	if !strings.Contains(err.Error(), "unmigratable") {
		t.Errorf("error does not name the module: %v", err)
	}
}

type failingMigrator struct{}

func (failingMigrator) Migrate(ctx context.Context, d *db.DB) error {
	return errors.New("extension unavailable")
}

func TestScopeExposesTheApplication(t *testing.T) {
	withCleanModuleRegistry(t)

	var got *db.DB
	var named string
	registerModule(t, ModuleInfo{
		Name: "scopeuser",
		Factory: func(s *Scope) (any, error) {
			got = s.DB()
			named = s.Name()
			if s.App() == nil || s.Config() == nil || s.Log() == nil {
				return nil, errors.New("Scope is missing something it promises")
			}
			s.Log().Info("from a module", "answer", 42)
			return &struct{}{}, nil
		},
	})

	a := newTestApp(t)
	if got != a.DB {
		t.Error("Scope.DB() is not the application's handle")
	}
	if named != "scopeuser" {
		t.Errorf("Scope.Name() = %q", named)
	}
}

func TestWithModuleIsApplicationLocal(t *testing.T) {
	withCleanModuleRegistry(t)

	a := newTestApp(t, WithModule(ModuleInfo{
		Name:    "local-only",
		Factory: func(s *Scope) (any, error) { return "local", nil },
	}))
	if _, ok := a.Module("local-only"); !ok {
		t.Fatal("WithModule did not load the module")
	}

	// The global registry is untouched, so another application in the same
	// process does not inherit this one's modules.
	b := newTestApp(t)
	if _, ok := b.Module("local-only"); ok {
		t.Error("an application-local module leaked to another application")
	}
	for _, info := range RegisteredModules() {
		if info.Name == "local-only" {
			t.Error("WithModule registered into the global registry")
		}
	}
}

func TestWithModuleValidates(t *testing.T) {
	one := ModuleInfo{Name: "hollow"}
	reserved := ModuleInfo{Name: "core", Factory: func(s *Scope) (any, error) { return 1, nil }}
	badPerm := ModuleInfo{
		Name:        "badperm",
		Permissions: []auth.Perm{{Module: "TASKS"}},
		Factory:     func(s *Scope) (any, error) { return 1, nil },
	}
	loop := ModuleInfo{
		Name:         "loop",
		Dependencies: []string{"loop"},
		Factory:      func(s *Scope) (any, error) { return 1, nil },
	}
	dupe := ModuleInfo{Name: "dupe", Factory: func(s *Scope) (any, error) { return 1, nil }}

	cases := map[string][]Option{
		"nil factory":    {WithModule(one)},
		"reserved name":  {WithModule(reserved)},
		"bad permission": {WithModule(badPerm)},
		"self loop":      {WithModule(loop)},
		"twice":          {WithModule(dupe, dupe)},
	}

	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			a, err := newTestAppNoBuild(t, opts...)
			if err == nil {
				a.Close()
				t.Fatalf("%s was accepted", name)
			}
			if !strings.Contains(err.Error(), "WithModule") {
				t.Errorf("error should name the option: %v", err)
			}
		})
	}
}

func TestSuppliedAndRegisteredNameConflict(t *testing.T) {
	withCleanModuleRegistry(t)

	registerModule(t, quietModule("dupe"))

	a, err := newTestAppNoBuild(t, WithModule(ModuleInfo{
		Name:    "dupe",
		Factory: func(s *Scope) (any, error) { return 1, nil },
	}))
	if err == nil {
		a.Close()
		t.Fatal("one name claimed by both the registry and the application")
	}
	if !strings.Contains(err.Error(), "dupe") {
		t.Errorf("error does not name the module: %v", err)
	}
}

func TestRegisteredModulesIsSortedAndACopy(t *testing.T) {
	withCleanModuleRegistry(t)

	registerModule(t, quietModule("zulu"))
	registerModule(t, quietModule("alpha"))

	got := RegisteredModules()
	if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "zulu" {
		t.Fatalf("RegisteredModules = %+v", got)
	}

	got[0].Name = "mutated"
	if RegisteredModules()[0].Name != "alpha" {
		t.Error("the returned slice shares state with the registry")
	}
}

func TestNoModulesIsNotAnError(t *testing.T) {
	withCleanModuleRegistry(t)

	a := newTestApp(t)
	if got := a.LoadedModules(); len(got) != 0 {
		t.Errorf("LoadedModules = %v", got)
	}
	if got := a.Permissions(); len(got) != 0 {
		t.Errorf("Permissions = %v", got)
	}
	if _, ok := a.Module("anything"); ok {
		t.Error("Module found something in an application with no modules")
	}
}

func TestModuleNamesAreCaseInsensitive(t *testing.T) {
	withCleanModuleRegistry(t)

	registerModule(t, ModuleInfo{
		Name:     "CaseSensitive",
		Factory:  func(s *Scope) (any, error) { return "value", nil },
		Required: true,
	})

	// A registered name is normalized, so the graph and the error messages use
	// one spelling.
	if got := RegisteredModules()[0].Name; got != "casesensitive" {
		t.Errorf("registered name = %q, want the canonical form", got)
	}

	a := newTestApp(t, WithModules("CASESENSITIVE"))
	if _, ok := a.Module("CaseSensitive"); !ok {
		t.Error("a whitelist differing only in case excluded the module")
	}
}

// TestModuleRoutesSurviveDisabledModules checks that a disabled module
// contributes nothing at all: no route, no nav, no namespace.
func TestDisabledModuleContributesNothing(t *testing.T) {
	withCleanModuleRegistry(t)

	registerModule(t, ModuleInfo{
		Name:    "off",
		Factory: func(s *Scope) (any, error) { return &moduleProbe{}, nil },
	})
	registerModule(t, quietModule("keeper"))

	a := newTestApp(t, WithModules("keeper"))
	if rec := a.get(t, "/probe-module"); rec.Code == http.StatusOK {
		t.Error("a disabled module's route was registered")
	}
	if len(a.navItems) != 0 {
		t.Errorf("a disabled module contributed navigation: %+v", a.navItems)
	}
	if a.registry.Mounted("probe") {
		t.Error("a disabled module's templates were mounted")
	}
}
