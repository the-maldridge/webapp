package webapp

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"

	"github.com/the-maldridge/webapp/auth"
	"github.com/the-maldridge/webapp/db"
	"github.com/the-maldridge/webapp/tmpl"
	"github.com/the-maldridge/webapp/ui"
)

// Module registry.
//
// A module is a feature area that can be turned on and off by configuration
// without editing the binary that contains it. The registry is process-global
// because that is what package-initialised registration means: a package
// registers itself when it is imported, and the application decides which of
// what it linked to actually load.
//
// What a module may contribute is a fixed list — routes, templates, static
// assets, navigation, permissions, schema — rather than an open-ended hook
// set. A fixed list is what makes "what does enabling this module do?"
// answerable, and it keeps a module from reaching into the application in ways
// the application cannot see from its own source.
//
// Everything the registry rejects at registration or resolution time is a
// defect that would otherwise surface as a wrong page, a missing route, or a
// nil dereference inside a handler on the first request.

// ModuleInfo describes one module.
//
// The value is a description rather than an object with behaviour: the
// behaviour is the value Factory returns, and which of the capability
// interfaces that value happens to implement decides what the application does
// with it.
type ModuleInfo struct {
	// Name identifies the module in configuration, in the dependency
	// graph, and in error messages. It is compared case-insensitively and
	// doubles as the default namespace for the module's templates and
	// assets, so its character rules are the template namespace's.
	Name string

	// Scopes restrict the module to a deployment mode, such as "web" or
	// "worker". Empty means every deployment.
	//
	// A scope is a deployment fact, not a permission: it answers "is this
	// process the kind that serves this feature?" for an application that
	// ships one binary running several roles. Who may use the feature is
	// the permissions' business.
	Scopes []string

	// Dependencies are the names of modules that must be loaded for this one
	// to work. They are instantiated first, and their values are handed to
	// this module's Factory.
	//
	// Depending on a module is not how a module calls another module: the
	// value is asserted against an interface the consumer declares, which
	// keeps the dependency pointing from the consumer.
	Dependencies []string

	// Required marks a module that configuration cannot disable. A required
	// module may depend only on other required modules, because a whitelist
	// that could switch off a required module's collaborator would produce a
	// module that is required and broken at the same time.
	Required bool

	// Permissions are the grants this module owns. Declaring them rather
	// than having them discovered makes "what can this application be asked
	// to allow?" answerable without walking handlers, and stops two modules
	// quietly claiming the same grant.
	Permissions []auth.Perm

	// Factory builds the module. It runs once, during construction, after
	// the database is open and migrated and before any request is served.
	//
	// A failure must be reported as an error. Returning a nil value with a
	// nil error is the specific bug this signature exists to make
	// unaffordable: a typed-nil satisfies any interface, so it registers,
	// wires up, and then panics on first use, three layers away from the
	// code that actually failed.
	Factory func(s *Scope) (any, error)
}

// Capability interfaces. A module's value is asked which of these it is, and
// every one it satisfies is applied at startup.
//
// They are separate interfaces rather than one Module interface because a
// module with no templates should not have to write an empty Templates method
// to satisfy a contract, and because an application can assert only the one it
// needs.
//
// Schema contribution is deliberately not repeated here: a module that owns
// tables implements the same Component and Migrator interfaces as every other
// component, so there is one rule about what syncs and migrates rather than a
// parallel pair whose two signatures nobody can keep straight.

// RouteContributor registers routes. The Router it receives is the
// application's, and the routes appear exactly as though the application had
// registered them itself.
type RouteContributor interface {
	Routes(r Router)
}

// Templater contributes a root of templates under a namespace.
//
// The namespace is validated rather than trusted: it has to be legal to write
// in a template name, must not be the framework's, and must not collide with
// another module's. Returning the module's own name is the normal case.
type Templater interface {
	Templates() (namespace string, fsys fs.FS)
}

// Staticer contributes static assets under a namespace, served at
// /static/<namespace>/ so that two modules shipping a file of the same name
// cannot overwrite each other.
type Staticer interface {
	Static() (namespace string, fsys fs.FS)
}

// Navigator contributes navigation entries. They are filtered per request by
// permission like every other entry, so a module states its gate once.
type Navigator interface {
	Nav() []NavItem
}

var (
	// moduleMu guards the registry. Registration happens in package
	// initialisation and in tests; resolution reads it once during
	// construction.
	moduleMu sync.Mutex

	// moduleRegistry holds every registered module in registration order.
	// Order is not load order — resolution decides that — but it is the
	// order duplicates and conflicts are reported in.
	moduleRegistry []ModuleInfo
)

// Register adds a module to the registry. It is called from package
// initialisation, which is why it panics rather than returning an error: an
// invalid registration is a defect in the code being compiled, and every place
// a package's init could report one is a function the application author never
// called. An application that assembles modules itself should use WithModule,
// which reports the same problems as errors.
//
// It panics on a duplicate name, on a malformed name or permission, on a
// missing factory, and on a required module depending on an optional one.
// Conflicts that depend on what else is registered are checked in both
// directions, so they are caught whichever module initialises first.
func Register(info ModuleInfo) {
	info, err := validateModuleInfo(info)
	if err != nil {
		panic("webapp: Register: " + err.Error())
	}

	moduleMu.Lock()
	defer moduleMu.Unlock()

	for _, existing := range moduleRegistry {
		if existing.Name == info.Name {
			panic(fmt.Sprintf("webapp: Register(%q): a module with this name is already registered", info.Name))
		}
	}

	// A required module depending on an optional one is only satisfiable by
	// accident. Checking it at registration makes it the module author's
	// problem rather than an operator's at deploy time.
	if info.Required {
		for _, dep := range info.Dependencies {
			other, found := lookupModuleLocked(dep)
			if found && !other.Required {
				panic(fmt.Sprintf("webapp: Register(%q): required module cannot depend on optional module %q, "+
					"which configuration could disable", info.Name, dep))
			}
		}
	}
	for _, other := range moduleRegistry {
		if !other.Required || info.Required {
			continue
		}
		for _, dep := range other.Dependencies {
			if dep == info.Name {
				panic(fmt.Sprintf("webapp: Register(%q): optional module cannot be a dependency of required module %q, "+
					"which configuration could not load", info.Name, other.Name))
			}
		}
	}

	moduleRegistry = append(moduleRegistry, info)
}

// validateModuleInfo checks a description in isolation and normalizes its
// names. It is shared by Register and WithModule so that the two cannot drift
// into accepting different things.
func validateModuleInfo(info ModuleInfo) (ModuleInfo, error) {
	info.Name = normalizeModuleName(info.Name)

	switch {
	case info.Name == "":
		return info, errors.New("module name may not be empty")
	case !validModuleName(info.Name):
		return info, fmt.Errorf("module name %q may not contain %q, a slash, a backslash, or whitespace",
			info.Name, tmpl.NamespaceSeparator)
	case info.Name == ui.Namespace:
		return info, fmt.Errorf("module name %q is reserved for the framework", info.Name)
	case info.Name == debugNamespace:
		return info, fmt.Errorf("module name %q is reserved for the template debug overlay", info.Name)
	case info.Factory == nil:
		return info, fmt.Errorf("module %q has no Factory and would contribute nothing", info.Name)
	}

	// Slices are copied before normalization so that a caller mutating what
	// it passed in cannot change what the application validated.
	info.Scopes = append([]string(nil), info.Scopes...)
	info.Dependencies = append([]string(nil), info.Dependencies...)
	info.Permissions = append([]auth.Perm(nil), info.Permissions...)

	for i, scope := range info.Scopes {
		scope = strings.ToLower(strings.TrimSpace(scope))
		if scope == "" {
			return info, fmt.Errorf("module %q: scope #%d is empty", info.Name, i)
		}
		info.Scopes[i] = scope
	}

	for i, dep := range info.Dependencies {
		dep = normalizeModuleName(dep)
		switch {
		case dep == "":
			return info, fmt.Errorf("module %q: dependency #%d is empty", info.Name, i)
		case dep == info.Name:
			return info, fmt.Errorf("module %q depends on itself", info.Name)
		}
		info.Dependencies[i] = dep
	}

	owning := map[string]bool{}
	for _, p := range info.Permissions {
		if p.Module == "" || p.Grant == "" {
			return info, fmt.Errorf("module %q: permission %q is not MODULE:GRANT", info.Name, p.String())
		}
		if owning[p.String()] {
			return info, fmt.Errorf("module %q: permission %q is declared twice", info.Name, p.String())
		}
		owning[p.String()] = true
	}

	return info, nil
}

// RegisteredModules returns every registered module, sorted by name. It is the
// diagnostic that answers "what is linked into this binary?" without reading
// import lists.
func RegisteredModules() []ModuleInfo {
	moduleMu.Lock()
	defer moduleMu.Unlock()

	out := make([]ModuleInfo, len(moduleRegistry))
	copy(out, moduleRegistry)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func lookupModuleLocked(name string) (ModuleInfo, bool) {
	for _, info := range moduleRegistry {
		if info.Name == name {
			return info, true
		}
	}
	return ModuleInfo{}, false
}

// normalizeModuleName canonicalizes a module name for comparison. Names arrive
// from source and from an operator's environment variable, and a whitelist that
// silently missed a module because of a capital letter reads as a bug in the
// module rather than in the configuration.
func normalizeModuleName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// validModuleName applies the template namespace's rules, because a module name
// is used as a namespace and the resolver's constraints are not negotiable, and
// adds the one rule the registry needs on top: a name is also a field in a
// comma-separated environment variable, so whitespace in it is a name nobody
// can type correctly twice.
func validModuleName(name string) bool {
	if strings.ContainsAny(name, " \t\n\r") {
		return false
	}
	_, err := tmpl.CleanNamespace(name)
	return err == nil
}

// Scope is what a module's Factory is handed: the application, its database,
// its logger, and the values of the modules it depends on.
//
// It is a parameter object rather than four arguments because the alternative
// is a factory signature that changes whenever the framework gains something
// worth giving a module, and every change to that signature breaks every
// application.
type Scope struct {
	app    *App
	info   ModuleInfo
	values map[string]any
}

// Name returns the module being constructed. A factory that logs should name
// itself with this rather than a literal, so that a module registered twice is
// at least honest about which registration was asked for.
func (s *Scope) Name() string { return s.info.Name }

// App returns the application under construction.
//
// It is not yet serving, and is not yet finished: the module's own routes,
// templates, and navigation are installed after its Factory returns, so calling
// Route or Nav directly from a factory duplicates what the capability
// interfaces do and skips the ordering they guarantee.
func (s *Scope) App() *App { return s.app }

// DB returns the application's database handle.
func (s *Scope) DB() *db.DB { return s.app.DB }

// Config returns the resolved configuration.
func (s *Scope) Config() *Config { return s.app.Config }

// Log returns a logger that names this module on every line, so that a module's
// output can be attributed without every call site remembering to.
func (s *Scope) Log() *moduleLogger { return &moduleLogger{app: s.app, module: s.info.Name} }

// moduleLogger is a logger scoped to one module.
type moduleLogger struct {
	app    *App
	module string
}

// Debug logs at debug level with the module named.
func (l *moduleLogger) Debug(msg string, args ...any) {
	l.app.log().Debug(msg, append([]any{"module", l.module}, args...)...)
}

// Info logs at info level with the module named.
func (l *moduleLogger) Info(msg string, args ...any) {
	l.app.log().Info(msg, append([]any{"module", l.module}, args...)...)
}

// Warn logs at warning level with the module named.
func (l *moduleLogger) Warn(msg string, args ...any) {
	l.app.log().Warn(msg, append([]any{"module", l.module}, args...)...)
}

// Error logs at error level with the module named.
func (l *moduleLogger) Error(msg string, args ...any) {
	l.app.log().Error(msg, append([]any{"module", l.module}, args...)...)
}

// Deps returns the values of the modules this one declared as dependencies,
// keyed by module name, in load order.
//
// The map is shared and must not be written to. A missing dependency is not a
// runtime condition: resolution fails construction when a declared dependency
// is absent, so a factory never sees a partial map.
func (s *Scope) Deps() map[string]any { return s.values }

// Get returns a dependency's value.
//
// A module collaborating with an optional module asserts the value against an
// interface it declared itself:
//
//	type indexer interface{ Index(ctx context.Context, t Task) error }
//
//	if ix, ok := s.Get("search").(indexer); ok {
//	    m.searcher = ix
//	}
//
// The consumer declaring the interface makes the dependency point from the
// consumer to the collaborator rather than between two modules' concrete
// types, and its absence degrades to a missing capability rather than a
// startup failure that takes a stack trace to decode.
func (s *Scope) Get(name string) (any, bool) {
	v, ok := s.values[normalizeModuleName(name)]
	return v, ok
}

// MustDependency returns a dependency's value or an error naming the module
// that is missing. It exists so that a factory which genuinely cannot work
// without a collaborator says which one rather than nil-dereferencing on it.
func (s *Scope) MustDependency(name string) (any, error) {
	v, ok := s.Get(name)
	if !ok {
		return nil, fmt.Errorf("webapp: module %q requires module %q, which is not loaded", s.info.Name, name)
	}
	return v, nil
}

// moduleState is what an application remembers about the modules it loaded.
type moduleState struct {
	// order is load order, which is the dependency order resolution produced.
	order []string

	// values maps a module name to the value its Factory returned.
	values map[string]any

	// perms maps a declared grant to the module that owns it.
	perms map[auth.Perm]string
}

// moduleStatic is one module's asset tree, waiting to be mounted.
type moduleStatic struct {
	namespace string
	fsys      fs.FS
}

// planModules resolves the registry against the configuration: scope filter,
// whitelist, dependency validation, then a topological order with alphabetical
// tie-breaking.
//
// Deterministic order is not a nicety. Modules seed rows and register
// navigation, so an order that varies between boots of the same binary means
// two deployments of identical code can seed data in a different sequence and
// render different navigation, which makes "the seed is wrong" depend on luck
// to reproduce.
func planModules(cfg *Config) ([]ModuleInfo, error) {
	moduleMu.Lock()
	registry := make([]ModuleInfo, len(moduleRegistry))
	copy(registry, moduleRegistry)
	moduleMu.Unlock()

	// Modules the application supplied directly are merged with the global
	// registry rather than replacing it, and a name claimed in both is an
	// error: two definitions of one module is exactly the ambiguity a name is
	// supposed to remove.
	for _, local := range cfg.Modules {
		info, err := validateModuleInfo(local)
		if err != nil {
			return nil, err
		}
		if _, dup := lookupIn(registry, info.Name); dup {
			return nil, fmt.Errorf("module %q is registered and also supplied by the application", info.Name)
		}
		registry = append(registry, info)
	}

	want := strings.ToLower(strings.TrimSpace(cfg.DeploymentScope))
	allowed := map[string]bool{}
	for _, name := range cfg.EnabledModules {
		allowed[normalizeModuleName(name)] = true
	}

	var errs []error

	// Pass one: which registered modules are candidates?
	inScope := make(map[string]bool, len(registry))
	candidates := make(map[string]ModuleInfo, len(registry))
	for _, info := range registry {
		if want != "" && len(info.Scopes) > 0 && !scopeMatches(info.Scopes, want) {
			continue
		}
		if len(allowed) > 0 && !info.Required && !allowed[info.Name] {
			continue
		}
		inScope[info.Name] = true
		candidates[info.Name] = info
	}

	// A whitelist entry naming nothing registered is a typo, and the failure
	// mode of ignoring it is a deployment silently missing a feature. A
	// whitelist naming a module that exists but is out of scope is not an
	// error: that is one variable shared across roles.
	for _, name := range sortedKeys(allowed) {
		if _, found := lookupIn(registry, name); !found {
			errs = append(errs, fmt.Errorf("module %q is enabled but not registered; registered modules: %s",
				name, strings.Join(moduleNames(registry), ", ")))
		}
	}

	// Pass two: every dependency has to be loaded, or the dependent module is
	// being asked to work without the thing it needs.
	for _, info := range sortedModules(candidates) {
		for _, dep := range info.Dependencies {
			if inScope[dep] {
				continue
			}
			other, found := lookupIn(registry, dep)
			switch {
			case !found:
				errs = append(errs, fmt.Errorf("module %q depends on %q, which is not registered", info.Name, dep))
			case want != "" && len(other.Scopes) > 0 && !scopeMatches(other.Scopes, want):
				errs = append(errs, fmt.Errorf("module %q depends on %q, which is not in scope %q", info.Name, dep, want))
			default:
				errs = append(errs, fmt.Errorf("module %q depends on %q, which is not enabled; add it to %s%s",
					info.Name, dep, cfg.EnvPrefix, suffixModules))
			}
		}
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return topoSortModules(candidates)
}

// topoSortModules orders modules so each appears after its dependencies,
// breaking ties alphabetically.
//
// The tie-break is what makes the result a function of the graph rather than of
// Go's map iteration order. It is Kahn's algorithm with the candidate set
// resorted each round; the module counts at which the extra passes matter are
// the counts at which a monolith gets decomposed instead.
func topoSortModules(candidates map[string]ModuleInfo) ([]ModuleInfo, error) {
	names := make([]string, 0, len(candidates))
	for name := range candidates {
		names = append(names, name)
	}
	sort.Strings(names)

	loaded := make(map[string]bool, len(candidates))
	order := make([]ModuleInfo, 0, len(candidates))

	for len(order) < len(candidates) {
		progress := false
		for _, name := range names {
			if loaded[name] {
				continue
			}
			ready := true
			for _, dep := range candidates[name].Dependencies {
				if !loaded[dep] {
					ready = false
					break
				}
			}
			if !ready {
				continue
			}
			loaded[name] = true
			order = append(order, candidates[name])
			progress = true
		}
		if !progress {
			var stuck []string
			for _, name := range names {
				if !loaded[name] {
					stuck = append(stuck, name)
				}
			}
			return nil, fmt.Errorf("module dependency cycle: %s", strings.Join(stuck, ", "))
		}
	}
	return order, nil
}

func scopeMatches(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}

func lookupIn(registry []ModuleInfo, name string) (ModuleInfo, bool) {
	for _, info := range registry {
		if info.Name == name {
			return info, true
		}
	}
	return ModuleInfo{}, false
}

func moduleNames(registry []ModuleInfo) []string {
	names := make([]string, 0, len(registry))
	for _, info := range registry {
		names = append(names, info.Name)
	}
	sort.Strings(names)
	return names
}

// sortedModules returns the map's values ordered by name, so that an error
// listing several problems lists them in the same order every run.
func sortedModules(candidates map[string]ModuleInfo) []ModuleInfo {
	out := make([]ModuleInfo, 0, len(candidates))
	for _, info := range candidates {
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// installModules resolves the registry and installs everything the loaded
// modules contribute.
//
// The order within is stated rather than implied:
//
//  1. Every module is instantiated in dependency order, and its models are
//     synced and its migration run before the next one starts, so a module may
//     read a table its dependency seeded.
//  2. Templates and static assets are mounted, which has to happen before the
//     renderer is built.
//  3. Navigation is contributed.
//  4. Routes are registered last, so a module that resolves a collaborator
//     while wiring a route finds every collaborator constructed.
//
// A failure at any step aborts construction with the database closed. Loading
// half a module set would serve a site whose navigation points at routes that
// were never registered, and that is a failure an operator learns about from a
// complaint rather than from a startup log.
func (a *App) installModules() error {
	plan, err := planModules(a.Config)
	if err != nil {
		return err
	}

	state := &moduleState{
		values: make(map[string]any, len(plan)),
		perms:  make(map[auth.Perm]string, 8),
	}
	a.modules = state

	// Namespace claims, kept per mount type. One map for both would make the
	// ordinary case — a module mounting templates and assets under its own
	// name — look like a collision, because a template namespace and a URL
	// prefix are different address spaces that happen to be spelled alike.
	templateClaims := map[string]string{ui.Namespace: "the framework"}
	if a.Config.TemplateDebugDir != "" {
		templateClaims[debugNamespace] = "the template debug overlay"
	}
	for _, root := range a.Config.templateRoots {
		templateClaims[root.namespace] = "an application template root"
	}
	staticClaims := map[string]string{ui.Namespace: "the framework"}

	for _, info := range plan {
		scope := &Scope{app: a, info: info, values: state.values}

		value, err := a.callFactory(scope)
		if err != nil {
			return err
		}
		if err := rejectNilValue(info.Name, value); err != nil {
			return err
		}

		state.values[info.Name] = value
		state.order = append(state.order, info.Name)

		if err := registerModulePerms(state, info); err != nil {
			return err
		}
		if err := a.syncModuleSchema(info.Name, value); err != nil {
			return err
		}
	}

	for _, info := range plan {
		value := state.values[info.Name]

		if t, ok := value.(Templater); ok {
			ns, fsys := t.Templates()
			if fsys == nil {
				continue
			}
			clean, err := claimNamespace(info.Name, ns, templateClaims)
			if err != nil {
				return err
			}
			a.Config.templateRoots = append(a.Config.templateRoots, templateRoot{namespace: clean, fsys: fsys})
		}

		if s, ok := value.(Staticer); ok {
			ns, fsys := s.Static()
			if fsys == nil {
				continue
			}
			clean, err := claimNamespace(info.Name, ns, staticClaims)
			if err != nil {
				return err
			}
			a.moduleStatics = append(a.moduleStatics, moduleStatic{namespace: clean, fsys: fsys})
		}

		if n, ok := value.(Navigator); ok {
			a.Nav(n.Nav()...)
		}
	}

	for _, info := range plan {
		rc, ok := state.values[info.Name].(RouteContributor)
		if !ok {
			continue
		}
		if err := a.registerModuleRoutes(info.Name, rc); err != nil {
			return err
		}
	}

	if len(state.order) > 0 {
		a.log().Debug("webapp: modules loaded", "modules", strings.Join(state.order, ","))
	}
	return nil
}

// claimNamespace validates a namespace a module wants to mount, records the
// claim, and returns its canonical form.
//
// Modules choose their own namespace because the alternative — the framework
// deriving one — would make a module's own template names depend on what it was
// registered as, which is exactly the coupling that lets a template that works
// in one deployment resolve differently in another.
func claimNamespace(module, ns string, claimed map[string]string) (string, error) {
	clean, err := tmpl.CleanNamespace(ns)
	if err != nil {
		return "", fmt.Errorf("webapp: module %q: %w", module, err)
	}
	switch clean {
	case ui.Namespace:
		return "", fmt.Errorf("webapp: module %q: namespace %q is reserved for the framework", module, ui.Namespace)
	case debugNamespace:
		return "", fmt.Errorf("webapp: module %q: namespace %q is reserved for the template debug overlay", module, debugNamespace)
	}
	if owner, taken := claimed[clean]; taken {
		return "", fmt.Errorf("webapp: module %q: namespace %q is already claimed by %s", module, clean, owner)
	}
	claimed[clean] = "module " + module
	return clean, nil
}

// callFactory runs a module factory, turning a panic into an error.
//
// A panic is not hypothetical: a factory that touches the database can hit a
// nil map write, a bad assertion, or an index the author believed total.
// Construction is the last moment at which that is a log line and an exit code
// rather than a dead process in a restart loop.
func (a *App) callFactory(scope *Scope) (value any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("webapp: module %q panicked while being constructed: %v", scope.info.Name, r)
		}
	}()

	value, err = scope.info.Factory(scope)
	if err != nil {
		return nil, fmt.Errorf("webapp: module %q could not be constructed: %w", scope.info.Name, err)
	}
	return value, nil
}

// rejectNilValue reports a factory that returned nothing without saying so.
//
// The message names the anti-pattern rather than only the fact, because the
// person debugging it is usually certain that returning nil was how to say
// "not applicable here".
func rejectNilValue(module string, value any) error {
	if value == nil {
		return fmt.Errorf("webapp: module %q factory returned a nil value with no error; "+
			"a nil satisfies every interface and panics on first use", module)
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.UnsafePointer:
		if rv.IsNil() {
			return fmt.Errorf("webapp: module %q factory returned a nil %s with no error; "+
				"a nil satisfies every interface and panics on first use", module, rv.Type())
		}
	}
	return nil
}

// registerModulePerms records the grants a module owns.
func registerModulePerms(state *moduleState, info ModuleInfo) error {
	for _, p := range info.Permissions {
		if owner, taken := state.perms[p]; taken {
			return fmt.Errorf("webapp: module %q declares permission %q, which module %q already owns",
				info.Name, p.String(), owner)
		}
		state.perms[p] = info.Name
	}
	return nil
}

// syncModuleSchema brings a module's tables into existence and runs its
// migration.
//
// It happens per module, immediately after that module is constructed, rather
// than after every module is constructed. A module's migration is where it
// seeds rows, and a seed that runs before a collaborator's tables exist either
// fails or has to be deferred to a later hook for reasons nobody can
// reconstruct afterwards.
func (a *App) syncModuleSchema(module string, value any) error {
	if c, ok := value.(Component); ok {
		if models := c.Models(); len(models) > 0 {
			if err := a.DB.Raw().AutoMigrate(models...); err != nil {
				return fmt.Errorf("webapp: module %q models failed to sync: %w", module, err)
			}
		}
	}
	if m, ok := value.(Migrator); ok {
		if err := m.Migrate(context.Background(), a.DB); err != nil {
			return fmt.Errorf("webapp: module %q failed to migrate: %w", module, err)
		}
	}
	return nil
}

// registerModuleRoutes applies a module's routes.
//
// They go into a Group rather than onto the router directly. Group is what
// gives a module's middleware a scope it cannot leak out of: chi's Use on the
// shared router would apply whatever a module installed to every route
// registered after it, by another module, in an order no author can see from
// their own file. Chi's duplicate-path panic is converted into a construction
// error for the same reason a factory's panic is.
func (a *App) registerModuleRoutes(module string, rc RouteContributor) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("webapp: module %q failed to register routes: %v", module, r)
		}
	}()

	a.router().Group(func(cr chi.Router) {
		rc.Routes(Router{inner: cr, app: a})
	})
	return nil
}

// Module returns the value a loaded module's Factory returned, by name.
//
// It is the seam for an application that wants to call a module rather than
// only route to it. The interface it asserts is the application's to declare,
// which is the same rule a module follows with its own collaborators.
func (a *App) Module(name string) (any, bool) {
	if a.modules == nil {
		return nil, false
	}
	v, ok := a.modules.values[normalizeModuleName(name)]
	return v, ok
}

// LoadedModules returns the loaded module names in load order, which is
// dependency order. It is empty when no modules were registered.
func (a *App) LoadedModules() []string {
	if a.modules == nil {
		return nil
	}
	return append([]string(nil), a.modules.order...)
}

// Permissions returns every grant the loaded modules declared, sorted by
// canonical name.
//
// This is the answer to "what should the administration screen offer?", and its
// being derived from the modules actually loaded is the point: an option that
// grants nothing because its module is disabled is a defect that no amount of
// testing on a fully-loaded development machine will surface.
func (a *App) Permissions() []auth.Perm {
	if a.modules == nil {
		return nil
	}
	perms := make([]auth.Perm, 0, len(a.modules.perms))
	for p := range a.modules.perms {
		perms = append(perms, p)
	}
	sort.Slice(perms, func(i, j int) bool { return perms[i].String() < perms[j].String() })
	return perms
}

// ModuleOwner returns the module that declared a permission.
func (a *App) ModuleOwner(p auth.Perm) (string, bool) {
	if a.modules == nil {
		return "", false
	}
	owner, ok := a.modules.perms[p]
	return owner, ok
}
