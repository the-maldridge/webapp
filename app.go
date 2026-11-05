package webapp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/the-maldridge/webapp/auth"
	"github.com/the-maldridge/webapp/db"
	"github.com/the-maldridge/webapp/tmpl"
)

// App is a configured application: a database handle, a router with the
// framework middleware installed, and the response helpers handlers use.
//
// Handlers are normally methods on an application struct that captured
// what they need; App holds no per-request state.
type App struct {
	// Config is the resolved configuration the App was built with.
	Config *Config

	// DB is the application's database handle.
	DB *db.DB

	// Renderer renders templates. The templating layer installs it;
	// exporting the field lets an application substitute its own, and
	// lets tests substitute one that records what was asked for.
	Renderer Renderer

	// Handler is the HTTP entrypoint: the router with the middleware
	// stack. It is exported so an application can wrap the whole thing
	// (mutual TLS, a proxy-protocol listener, a test server) without
	// going through Serve.
	Handler http.Handler

	// shared are per-render providers added with Share.
	shared map[string]Provider

	// navItems are the navigation entries registered by the application and
	// its modules, filtered per request by permission.
	navItems []NavItem

	// registry is the mounted template roots. It is kept so that a module
	// installed between construction and the first request can add its own;
	// compilation is lazy, so nothing else has to be rebuilt. Mounting while
	// requests are in flight is not supported.
	registry *tmpl.Registry

	// injections are internal context contributors, used by the
	// authentication and templating layers to add user, CSRF, and
	// navigation values without the core depending on them.
	injections []func(*http.Request, Context)

	// errorPage renders error responses; installed by SetErrorPage.
	errorPage func(http.ResponseWriter, *http.Request, int, error, string)

	// auth is the installed authentication service, nil when the
	// application has none. InstallAuth sets it.
	auth *auth.Service

	// extensions are middleware appended with Use, spliced into the chain
	// after the security headers and before the router.
	extensions []func(http.Handler) http.Handler

	// server is set by ServeListener and read by Shutdown.
	server *http.Server

	// mux is the router that Handler dispatches to after the middleware
	// chain. It is kept separate because the chain wraps it rather than
	// being installed on it.
	mux chi.Router

	mu       sync.Mutex
	shutting bool

	// buildVersion is stamped into every render context so a page can
	// be tied to the build that produced it.
	buildVersion string

	// ownsDB records whether Close should close the database.
	ownsDB bool
}

// Component is anything that contributes database models, such as a
// package that owns an accounts table.
//
// The hook exists because a component's schema has to be in place before
// the first request. Registering models after the database opened would
// otherwise mean the application has to remember to migrate, and the
// failure mode of forgetting is a runtime error on first use rather than
// a startup error.
type Component interface {
	Models() []any
}

// Migrator is a component whose schema needs a step beyond syncing
// structs: seeding rows, creating a database extension, backfilling a
// column. Migrate runs after models are synced, in registration order.
type Migrator interface {
	Migrate(ctx context.Context, d *db.DB) error
}

// New builds an application.
//
// Construction follows a fixed order and returns on the first failure,
// which is the point of centralizing it: options are applied, the
// environment fills the gaps, the result is validated as one aggregate
// error, the database opens and migrates, components contribute schema,
// and only then are the middleware stack and router built. Nothing here
// exits the process or panics, so the application decides how to report
// a bad configuration rather than inheriting an exit code from a library.
func New(opts ...Option) (*App, error) {
	config, err := newConfig(nil, opts...)
	if err != nil {
		return nil, err
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}

	a := &App{
		Config:       config,
		shared:       map[string]Provider{},
		buildVersion: readBuildVersion(),
	}

	// Components contribute models before the database opens, so a
	// component's tables exist on the first boot rather than the second.
	var models []any
	for _, comp := range config.Components {
		if c, ok := comp.(Component); ok {
			models = append(models, c.Models()...)
			continue
		}
		models = append(models, comp)
	}

	dbOpts := append([]db.Option{db.WithEnvPrefix(config.EnvPrefix)}, config.DBOptions...)
	if len(models) > 0 {
		dbOpts = append(dbOpts, db.WithAutoMigrate(models...))
	}

	handle, err := db.New(dbOpts...)
	if err != nil {
		return nil, fmt.Errorf("webapp: database unavailable: %w", err)
	}
	a.DB = handle
	a.ownsDB = true

	if err := a.migrateComponents(context.Background(), config.Components); err != nil {
		handle.Close()
		return nil, err
	}

	a.installStack()

	// Templating is installed last so that an application's routes, which may
	// be registered by components, are already described by the time a render
	// is possible. It is part of construction rather than an opt-in step
	// because the error page depends on it: an application that forgot to wire
	// templating would still start, and would discover the omission on the first
	// request that failed for a different reason.
	if err := a.installTemplates(); err != nil {
		handle.Close()
		return nil, err
	}
	return a, nil
}

// migrateComponents runs each component's Migrate in registration order.
// A component that cannot migrate is a startup failure: a partially
// installed schema serves wrong answers, and "it started" is the wrong
// signal to look for.
func (a *App) migrateComponents(ctx context.Context, components []any) error {
	for i, comp := range components {
		m, ok := comp.(Migrator)
		if !ok {
			continue
		}
		if err := m.Migrate(ctx, a.DB); err != nil {
			return fmt.Errorf("webapp: %s failed to migrate: %w", componentName(comp, i), err)
		}
	}
	return nil
}

// componentName names a component in an error message, preferring its
// type over a bare index.
func componentName(comp any, i int) string {
	if comp == nil {
		return fmt.Sprintf("component #%d", i)
	}
	t := reflect.TypeOf(comp)
	return fmt.Sprintf("component %s (%s)", t, t.Kind())
}

// RegisterComponent takes on a component after construction, syncing its
// models and running its migration immediately.
//
// The work happens here rather than lazily, because a component whose
// tables appear on first use serves errors until it is used.
func (a *App) RegisterComponent(comp any) error {
	if comp == nil {
		return errors.New("webapp: cannot register a nil component")
	}
	if a.DB == nil {
		return errors.New("webapp: cannot register a component without a database")
	}

	if c, ok := comp.(Component); ok {
		if models := c.Models(); len(models) > 0 {
			if err := a.DB.Raw().AutoMigrate(models...); err != nil {
				return fmt.Errorf("webapp: %s models failed to sync: %w", componentName(comp, 0), err)
			}
		}
	}

	if m, ok := comp.(Migrator); ok {
		if err := m.Migrate(context.Background(), a.DB); err != nil {
			return fmt.Errorf("webapp: %s failed to migrate: %w", componentName(comp, 0), err)
		}
	}
	return nil
}

// Route registers a sub-tree of routes. The callback receives a Router
// that carries the application, so gates and guards are reachable from
// it.
func (a *App) Route(prefix string, fn func(r Router)) {
	a.router().Route(prefix, func(r chi.Router) {
		fn(Router{inner: r, app: a})
	})
}

// Mount attaches an arbitrary handler below a path.
func (a *App) Mount(path string, h http.Handler) {
	a.router().Mount(path, h)
}

// Handle registers a handler for every method at a pattern.
func (a *App) Handle(pattern string, h http.HandlerFunc) {
	a.router().Handle(pattern, h)
}

// HandleErr registers an error-returning handler for every method at a
// pattern.
func (a *App) HandleErr(pattern string, h HandlerErr) {
	a.router().Handle(pattern, a.WrapErr(h))
}

// WrapErr converts an error-returning handler into an http.HandlerFunc
// whose returned errors reach the error responder.
//
// It exists because the alternative is the per-branch "log it, set a
// status, render the error page, remember to return" block that this
// style of application accumulates and, more often than not, gets one of
// those four steps wrong.
func (a *App) WrapErr(h HandlerErr) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			a.dispatchError(w, r, err)
		}
	}
}

// router returns the application's router. Calling it after Serve has
// installed a different entrypoint, or on an application whose Handler was
// replaced, cannot add routes to anything; that is a bug in the
// application, so it fails loudly and immediately rather than with a nil
// dereference inside a request.
func (a *App) router() chi.Router {
	if a.mux == nil {
		panic("webapp: routes cannot be registered on an application whose router was replaced")
	}
	return a.mux
}

// Serve starts listening and blocks until the server is shut down or
// fails. An empty address uses the configured one.
//
// An orderly shutdown returns nil: Serve is not the place to distinguish
// "the process was asked to stop" from "nothing is listening anymore",
// and Run is what turns a signal into an exit status.
func (a *App) Serve(addr string) error {
	if addr == "" {
		addr = a.Config.Addr
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("webapp: could not listen on %s: %w", addr, err)
	}
	return a.ServeListener(ln)
}

// newServer builds the http.Server that ServeListener will run. It is a
// separate method so that the timeouts a Config produced can be inspected
// without starting a listener.
func (a *App) newServer() *http.Server {
	return &http.Server{
		Handler: a.Handler,

		// These come from Config and default to non-zero values. An
		// unset read header timeout is a slow-loris primitive, and an
		// unset write timeout lets one stalled client hold a connection,
		// a goroutine, and whatever transaction that handler opened
		// indefinitely.
		ReadHeaderTimeout: a.Config.Timeouts.ReadHeaderTimeout,
		ReadTimeout:       a.Config.Timeouts.ReadTimeout,
		WriteTimeout:      a.Config.Timeouts.WriteTimeout,
		IdleTimeout:       a.Config.Timeouts.IdleTimeout,

		// net/http reports its own failures, including slow-client
		// resets, through the standard log package. Bridging them into
		// slog keeps one stream with one format.
		ErrorLog: log.New(&slogWriter{logger: a.log()}, "", 0),
	}
}

// ServeListener serves on an already-bound listener. It exists for a
// socket-activated process, a TLS listener assembled elsewhere, and a
// test that bound port 0 and needs to learn which port it got.
func (a *App) ServeListener(ln net.Listener) error {
	srv := a.newServer()

	a.mu.Lock()
	if a.shutting {
		a.mu.Unlock()
		ln.Close()
		return http.ErrServerClosed
	}
	a.server = srv
	a.mu.Unlock()

	slog.Info("webapp: listening", "addr", ln.Addr().String(), "production", a.Config.Production)

	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops the server gracefully: the listener closes immediately
// and in-flight requests are given until ctx expires. It is safe to call
// more than once, and safe to call before Serve, which makes signal
// handling in an application that failed to start simple.
func (a *App) Shutdown(ctx context.Context) error {
	a.mu.Lock()
	srv := a.server
	a.shutting = true
	a.mu.Unlock()

	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

// Close releases the application's resources, currently the database
// handle. It is separate from Shutdown so an application can stop serving
// and still finish work, and so that tests can release a temporary
// database deterministically.
func (a *App) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.ownsDB || a.DB == nil {
		return nil
	}
	a.ownsDB = false
	return a.DB.Close()
}

// Run serves until the process is asked to stop by SIGINT or SIGTERM,
// then performs a bounded graceful shutdown and closes the database.
//
// The shutdown deadline cannot be turned off: a request that cannot
// finish within it is not going to, and a process that waits forever is
// indistinguishable from one that ignored the signal. An orchestrator
// sends SIGKILL after its own grace period regardless.
func (a *App) Run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- a.Serve(a.Config.Addr) }()

	select {
	case err := <-errCh:
		// Serve returned by itself: a listen failure, or the server
		// died. Do not wait for a signal that is never coming.
		if cerr := a.Close(); cerr != nil {
			a.log().Error("could not close database", "error", cerr)
		}
		return err
	case <-ctx.Done():
	}

	slog.Info("webapp: shutting down", "timeout", a.Config.Timeouts.ShutdownTimeout)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), a.Config.Timeouts.ShutdownTimeout)
	defer cancel()

	serr := a.Shutdown(shutdownCtx)
	cerr := a.Close()

	switch {
	case errors.Is(serr, context.DeadlineExceeded):
		a.log().Error("shutdown timed out with requests in flight; exiting anyway")
	case serr != nil:
		return serr
	}
	return cerr
}

// Share registers a value provider called on every render. It is how
// application-wide data such as navigation, feature flags, or the current
// user reaches templates without every handler remembering to pass them.
//
// A provider returning an error other than ErrSkip logs a warning and
// omits the key: a missing sidebar entry is not a reason to fail a page.
func (a *App) Share(key string, p Provider) {
	if p == nil {
		delete(a.shared, key)
		return
	}
	a.shared[key] = p
}

// inject adds an internal context contributor, used by the authentication
// and templating layers.
func (a *App) inject(fn func(*http.Request, Context)) {
	a.injections = append(a.injections, fn)
}

// baseContext assembles what every render receives: shared providers,
// internal injections, and the values that are always correct.
func (a *App) baseContext(r *http.Request) Context {
	ctx := Context{
		"buildVersion": a.buildVersion,
		"debug":        a.Config.Debug,
		"pageBase":     pageBase(r),
		"requestId":    a.requestID(r),
	}

	for _, inject := range a.injections {
		inject(r, ctx)
	}

	for key, provider := range a.shared {
		v, err := provider(r.Context())
		switch {
		case errors.Is(err, ErrSkip):
			continue
		case err != nil:
			a.log().WarnContext(r.Context(), "shared value unavailable",
				"key", key, "request_id", a.requestID(r), "error", err)
			continue
		}
		ctx[key] = v
	}

	return ctx
}

// pageBase is the leading path segment of the request, which is what a
// navigation fragment uses to decide which item is active. Deriving it
// here means no handler has to declare which page it is, and a handler
// that forgets cannot highlight the wrong item.
func pageBase(r *http.Request) string {
	path := strings.Trim(r.URL.Path, "/")
	if path == "" {
		return ""
	}
	if i := strings.IndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return path
}

// requestID returns the correlation ID for the request.
func (a *App) requestID(r *http.Request) string {
	return middleware.GetReqID(r.Context())
}

// log returns the configured logger, defaulting to the process logger.
func (a *App) log() *slog.Logger {
	if a != nil && a.Config != nil && a.Config.Logger != nil {
		return a.Config.Logger
	}
	return slog.Default()
}

// slogWriter bridges the standard log package into slog, for net/http's
// own diagnostics.
type slogWriter struct{ logger *slog.Logger }

func (w *slogWriter) Write(p []byte) (int, error) {
	w.logger.Log(context.Background(), slog.LevelWarn, "http server",
		"message", strings.TrimSpace(string(p)))
	return len(p), nil
}

// readBuildVersion determines the running build from the binary itself,
// so that "which build produced this page?" is a question with an answer
// in the page.
func readBuildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 7 {
			return s.Value[:7]
		}
	}
	return "unknown"
}
