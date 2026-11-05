# webapp: a batteries-included Go web framework

Module: `github.com/the-maldridge/webapp`

`webapp` is a framework for building server-rendered Go applications. It targets the shape of
program that Go is constantly asked to build — a CRUD-ish internal tool, a moderation queue, a
tracker, a management console — where 90% of the code is routing, templates, database plumbing,
and "is this person allowed to do that". Those four concerns are solved the same way in almost
every such application, and getting them right the first time, once, in a framework, is worth
more than solving them a twentieth time in an application.

The design goal is that an application author writes handlers, models, and templates, and gets
everything else from the framework: connection setup, migrations, session handling, CSRF,
authorization, pagination, form parsing, error pages, logging, static assets, graceful
shutdown.

## 1. Goals and non-goals

**Goals**

- Batteries included: database, server-side templating, routing, authentication and
  authorization, form handling, configuration, logging, static assets.
- Small application: a working, authenticated, templated CRUD app is under ~200 lines.
- Escape hatches everywhere: chi routers, `*gorm.DB`, and `http.HandlerFunc` are all directly
  reachable. Nothing the framework does is inaccessible to an application that wants to do it
  differently.
- Secure by default, and explicitly verifiable: the defaults for sessions, CSRF, password
  hashing, and error disclosure are correct without the application author knowing they matter.
- Boot must be boring and fail fast: configuration errors, uncompilable templates, and
  migration failures surface at startup with useful messages, not as a 500 on the first click.

**Non-goals**

- Being a dependency injection framework. The application struct is the container.
- Owning the frontend. No JS toolchain, no component model, no client-side router. Templates
  emit HTML; applications may vendor CSS/JS however they like.
- Being a microservice toolkit. No service discovery, no gRPC, no tracing backend.
- Supporting every database. SQLite and PostgreSQL in v1, because those cover the realistic
  deployment range; the driver registry is open, the query layer is not portable-SQL-purist.
- Hot reloading. A debug mode that re-reads templates from disk is in scope; a supervised
  file-watcher/restarter is not.

## 2. Stack

The stack is fixed rather than pluggable. Frameworks that make every layer swappable end up
with no layer being well-integrated.

| Concern | Choice | Rationale |
|---|---|---|
| Routing | `github.com/go-chi/chi/v5` | Stdlib-compatible, `net/http` middleware, composable route trees, no framework lock-in |
| Templates | `github.com/flosch/pongo2/v6` | Django-syntax templates, `fs.FS` loading, template inheritance, autoescaping |
| Database | `gorm.io/gorm`, generic API (`gorm.G[T]`) | Compile-checked, context-first queries; migrations; association loading |
| Drivers | `gorm.io/driver/sqlite` (default), `gorm.io/driver/postgres` | Covers single-binary deploys through real multi-instance deploys |
| AuthN | `github.com/the-maldridge/authware` | Pluggable authenticator registry, stateless encrypted session cookies |
| Passwords | `golang.org/x/crypto/bcrypt` | Appropriate default work factor, no tuning required |
| Logging | `log/slog` | Stdlib, structured, no configuration |

Authware's `Authenticator` interface is the seam that keeps authentication open without the
framework needing an abstraction of its own:

```go
type Authenticator interface {
    AuthUserPassword(context.Context, string, string) error
    UserGroups(context.Context, string) (map[string]struct{})
    Name() string
}
```

Backends register themselves by name and a colon-separated environment variable
(`AUTHWARE_BASIC_MECHS="htpasswd:myapp"`) selects and orders the chain, so an operator can move
an application from a password file to LDAP to a database of local users without a code change.

## 3. Package layout

```
webapp/            App, options, config, router, middleware, render funnel, errors, input handling, nav, static, modules
webapp/db/         driver selection, pooling, pragmas, AutoMigrate + versioned migrations, upsert, paging
webapp/auth/       authware wiring, hardened cookies, CSRF, permissions, guards, database-backed backend
webapp/tmpl/       namespaced multi-loader, shared context, error adaptation, filters
webapp/ui/         embedded core UI: base and login templates, error pages, nav fragment, static assets
examples/tasks/    a complete application built on the framework
```

Dependency direction is strictly inward: `webapp` may import `db`, `auth`, `tmpl`, and `ui`;
none of those import `webapp`. Applications import `webapp` plus whichever of the subpackages
they need directly.

## 4. Application surface

```go
func main() {
    a, err := webapp.New(
        webapp.WithEnvPrefix("TASKS"),
        webapp.WithDBOptions(db.WithAutoMigrate(Task{})),
        webapp.WithAuth(),
        webapp.WithPermissions(PermTasksAdmin),
    )
    if err != nil {
        return err
    }

    a.RegisterTemplates("tasks", templateFS) // //go:embed p2
    a.Nav(webapp.NavItem{Weight: 80, Label: "Tasks", Href: "/ui/task/"})

    a.Route("/ui/task", func(r webapp.Router) {
        r.Use(a.RequireLogin())
        r.Get("/", listTasks)
        r.Get("/new", taskForm)
        r.Post("/new", taskFormSubmit)
        r.Get("/{id}/edit", taskForm)
        r.Post("/{id}/edit", taskFormSubmit)
        r.Post("/{id}/complete", a.Guard(auth.Perm("TASKS", "COMPLETE"), completeTask))
    })

    return a.Run()
}
```

Handlers are ordinary `http.HandlerFunc` values or methods on the application struct, which
captures its own dependencies lexically. One addition is provided: `webapp.HandlerErr`, a
`func(http.ResponseWriter, *http.Request) error`, where a non-nil returned error is handed to
the framework's error responder.

```go
type HandlerErr func(http.ResponseWriter, *http.Request) error

func (a *App) View(w http.ResponseWriter, r *http.Request, name string, ctx webapp.Context)
func (a *App) Fail(w http.ResponseWriter, r *http.Request, status int, err error)
func (a *App) SeeOther(w http.ResponseWriter, r *http.Request, url string)
```

`Run()` installs signal handlers, starts the listener, and performs a bounded graceful shutdown;
`Serve(addr)` and `Shutdown(ctx)` remain separately callable for applications that want to own
their lifecycle, embed the handler in a test, or run alongside other listeners.

## 5. Templating (`webapp/tmpl`)

### 5.1 Namespaced loading

Template loading is the layer where hand-wired applications are most fragile. Relative
`extends` paths (`{% extends "../../base.p2" %}`) couple a template's identity to its depth in
the tree, shared partials must be duplicated because there is no stable name for them, and
error templates get referenced before they are written and fail on first use.

`tmpl` presents pongo2 with a single loader backed by a registry of named roots.

```go
reg := tmpl.New()
reg.Mount("tasks", tasksP2) // the application's embedded p2 directory
reg.Mount("core", uiP2)     // framework templates, always mounted last
```

A template name is resolved in this order:

1. `@ns/rest` resolves inside that namespace and nowhere else. This is the collision-proof form
   and is what the framework itself uses internally (`@core/errors/internal.p2`).
2. A bare name is tried relative to the directory of the template doing the referencing, so
   sibling includes work unchanged.
3. A bare name is then tried in each mounted root, in registration order.

`Abs(base, name)` performs that probe and returns the canonical `@ns/rest` key; `Get(path)`
splits the key and opens the underlying `fs.FS`. The canonical key doubles as pongo2's cache
key, so one physical template is compiled once regardless of which name reached it.

The consequence for applications: `{% extends "base.p2 %}` and
`{% include "fragments/nav.p2" %}` just work, at any depth, regardless of which package the
template was embedded from.

### 5.2 Core UI and the compile check

`webapp/ui` ships the base layout, the login page, the error pages, the navigation fragment, and
the framework stylesheet. At startup the framework compiles the required set and fails if any of
it is missing or broken:

```
base.p2  login.p2  fragments/nav.p2
errors/internal.p2  errors/unauthorized.p2  errors/notfound.p2
```

Themes are supported by mounting the theme root *ahead of* core, so a theme only needs to
provide the templates it actually wants to override, and an incomplete theme is a startup error
rather than a runtime one.

### 5.3 Debug mode

`WithTemplateDebug(dir)` mounts an `os.DirFS` root before the embedded roots and enables
pongo2's non-caching debug mode, so template edits are visible on the next request. The
fallback to embedded content means files that exist only in the binary keep rendering, so a
partially extracted theme remains functional.

### 5.4 Context and filters

`View` is the only render entrypoint and injects ambient context: `user`, `profile`,
`navElements`, `csrfInput`, `pageBase`, `buildVersion`. Applications add their own with
`a.Share("key", fn)`, called per request, which is how application-wide navigation and feature
flags reach templates without every handler remembering them.

Two rules are enforced rather than documented:

- Errors placed in a render context are converted to HTML-escaped strings before rendering
  (`fmt.Stringer` values reaching a template bypass autoescaping and are an XSS vector).
- Custom filters are registered once, in `init()`, with namespaced names, and must be pure. A
  filter that closes over request state is a data race, because pongo2's filter registry is
  process-global. Framework filters: `hasPermission`, `keyValue`, `jsonify`, `linebreaksbr`.

## 6. Database (`webapp/db`)

```go
d, err := db.New(
    db.WithEnvPrefix("TASKS"),
    db.WithDriver(db.DriverSQLite),
    db.WithAutoMigrate(Profile{}, Task{}),
    db.WithMigrations(backfillOwnerlessTasks),
    db.WithPool(4, time.Hour),
    db.WithSlowQuery(250*time.Millisecond),
)
```

### 6.1 Drivers, pragmas, pooling

Driver selection is by option, then environment (`TASKS_DB_DRIVER`, `TASKS_DB` for the SQLite
path, `TASKS_DB_DSN` for PostgreSQL), so the same binary runs as a single-file tool or against a
database server. Configuration that applications reliably forget is applied here:

- SQLite: `journal_mode=WAL`, `busy_timeout=5000`, `foreign_keys=ON`, supplied as DSN `_pragma`
  parameters. Without these, concurrent writers error immediately and every foreign key
  constraint in the schema is silently unenforced.
- Connection pool limits and `ConnMaxLifetime` are set rather than left at defaults, with
  different defaults per driver (SQLite wants a small write pool; PostgreSQL wants lifetimes
  short enough to survive connection recycling).
- `PrepareStmt: true` and `SkipDefaultTransaction: true`, which measurably helps the
  single-row-dominant access pattern these applications have.
- A slow-query threshold, logged at Warn with the elapsed time.

### 6.2 Migrations

Two layers, because they solve different problems.

`AutoMigrate` handles schema-as-code for ordinary additive evolution. It runs on every boot and
is idempotent, which means a crash mid-startup is safe to retry and a fresh deployment needs no
operator step. Module-owned models migrate with the module.

Versioned migrations handle everything `AutoMigrate` cannot express: renames, data backfills,
index creation after a de-duplication pass, repair of rows written by an older version. They run
in order, each in its own transaction where the driver allows it, tracked in a
`schema_migrations` table so each runs exactly once.

```go
type Migration struct {
    Version string // ordered lexicographically; "2026-11-04-backfill-owners"
    Up      func(tx *gorm.DB) error
}
```

There is intentionally no down-migration and no DSL. Ordered, transactional `Up` functions in Go
cover the realistic cases and keep a migration reviewable as ordinary code.

Seeding and repair that must stay current (reference rows, static option lists) belongs in a
module's `Migrate()`, which is re-run every boot and reconciles rather than insert-only.

### 6.3 Query layer and helpers

Applications use GORM's generic API, which is context-first and returns typed results instead of
a mutated builder whose `Error` field must be remembered:

```go
tasks, err := db.G[Task](a.DB).
    Where("owner_id = ?", profile.ID).
    Preload("Tags", nil).
    Order("created_at desc").
    Find(r.Context())
```

Promoted helpers: `db.InsertOrUpdate[T]` (upsert on primary-key conflict), `db.Transaction(ctx,
fn)`, `db.Paginate(page, size)` as a scope, and `db.G[T]` as an alias so applications import one
package for all database access.

The non-obvious behaviours that cost hours when rediscovered are documented on the package, not
in tribal knowledge:

- `gorm.G[T]()` starts a fresh session with `NewDB: true`. Conditions applied to a raw
  `*gorm.DB` *before* calling it are silently dropped; filters and limits must be part of the
  generic chain.
- `Find` does not report not-found as an error; only `First`, `Last`, and `Take` do. Treating a
  `Find` error as "no rows" hides real database failures.
- Zero values in struct conditions are omitted, so queries for a zero-valued column need
  explicit `Where("x = ?", 0)` conditions.
- PostgreSQL enforces foreign keys at DDL time, so parents must migrate before children, and it
  rejects some identifiers that SQLite accepts; reserved words are avoided in column names.

## 7. Authentication and authorization (`webapp/auth`)

Authentication stays with authware: the mechanism chain, stateless AES-GCM encrypted session
cookies, and `User{Identity, Groups, AuthedBy}` delivered through the request context.

### 7.1 Sessions and cookies

The framework issues sessions through the same codec but with hardened attributes: `HttpOnly`,
`Path=/`, `SameSite=Lax`, and `Secure` unless explicitly disabled for a plain-HTTP deployment
behind a TLS-terminating proxy. Session name and lifetime are configuration, not constants.

Session key policy is enforced at startup: `WithProduction()` refuses to boot without an
explicit session key. A per-process random key silently invalidates every session on restart and
makes horizontal scaling impossible, which is the kind of failure that gets attributed to the
network.

### 7.2 CSRF

Double-submit, no server state: a `csrf_token` cookie signed with the session key, compared in
constant time against the `X-CSRF-Token` header or a `csrf_token` form field. Enforced on unsafe
methods for cookie-authenticated requests; skipped for HTTP Basic and bearer-token requests,
which are not subject to cross-site forgery. Templates emit the hidden field with
`{{ csrfInput|safe }}`; the framework's CSRF middleware reads the header or field, and the
failure mode is a 400 with the form re-rendered rather than a state change.

This is on by default for any route that authenticates by cookie. It is the single most common
critical vulnerability in small server-rendered Go applications, and none of them should have to
opt in to it.

### 7.3 Permissions

Authorization is a two-part string, module and grant:

```go
type Perm struct{ Module, Grant string } // e.g. {"TASKS", "ADMIN"}

func P(module, grant string) Perm
func (p Perm) String() string // "TASKS:ADMIN"
```

A user satisfies a permission with an exact match, `Module:ADMIN` for that module, or
`CORE:ADMIN` for anything. Required permissions are declared at construction — globally with
`WithPermissions`, per module with `ModuleInfo.Permissions` — and installed idempotently at
boot, so the permission universe is inspectable without reading route definitions.

Four ways to use a permission, all reading the same data:

```go
r.Use(a.RequirePermission(auth.Perm("TASKS", "ADMIN")))         // route group
r.Post("/nuke", a.Guard(auth.Perm("TASKS", "ADMIN"), handler))  // single handler
if auth.Has(ctx, auth.Perm("TASKS", "EDIT")) { }                // inside a handler
{% if user|hasPermission:"TASKS:EDIT" %}                        // template
```

Navigation entries carry a required permission and are filtered per request, so users are never
offered a link that will deny them.

Ownership — the "may this user touch *this* row" question that a permission model does not
answer — gets one helper, `a.RequireOwnership(ctx, ownerID)`, used by every mutating handler.
Per-handler ad hoc ownership comparisons are how such applications end up with exactly one
endpoint that forgot the check.

### 7.4 Denials

Denials are content-negotiated and status-correct: 401 when no user is present, 403 when a user
lacks the grant. If the request prefers `application/json`, the body is
`{"error": "permission denied", "required": "TASKS:ADMIN"}`; otherwise the unauthorized template
renders with the required permission named. Returning an HTML page or a 200 to an API caller
produces a client that believes a rejected write succeeded.

### 7.5 Profiles

`Profile` is a framework-owned row keyed on the authenticated identity, created on first request
by the profile middleware and reachable through a checked accessor that never panics. Application
data hangs off the profile's ID. Applications that need extra fields keep their own model joined
to it rather than extending the core one. This gives every application a stable local primary key
for "the current user" while identity itself remains owned by the authentication backend, and it
means there is no registration flow to write: the first request provisions the row.

### 7.6 Database-backed accounts

`auth.NewDBBackend(d, "myapp")` provides an `Authenticator` and `TokenAuthenticator` over a local
`User` table: bcrypt-hashed secrets, created and reset by an administrator, plus opaque API
tokens that are stored only as SHA-256 digests with a short display prefix and shown once at
creation. Applications get real accounts without a password file, while file- and
directory-based backends remain selectable by an environment variable.

The backend lives in `webapp/auth`, not in authware, because of one signature. authware resolves
mechanisms through `Factory func() (Authenticator, error)`, which takes no arguments and therefore
cannot be handed a database handle. Registration is by name and first-one-wins, so the framework
registers a closure that has already captured it:

```go
authware.RegisterFactory("database", func() (authware.Authenticator, error) {
    return newDBAuthenticator(handle), nil
})
authware.RegisterTokenFactory("database", func() (authware.TokenAuthenticator, error) {
    return newDBTokenAuthenticator(handle), nil
})
```

Making the closure capture the handle is the right shape rather than a workaround, with one
condition worth stating: the handle must exist before mechanisms are registered, which is exactly
the order `webapp.New` already guarantees, since the database opens before anything that could
serve a request. The alternative considered was widening authware's `Factory` to accept a
context, which would push a dependency-carrying bag through the one interface that keeps the
backends independent of each other.

Registration being idempotent and first-wins means a second registration silently does nothing
rather than failing. That is acceptable here and only here: the framework registers once, during
construction, and an application that wants a different backend configures one rather than racing
the framework for the name.

Two properties of the upstream session cookie are overridden rather than inherited. It is named
`session`, and it is set with `HttpOnly` alone: no `Secure`, no `SameSite`. A cookie without
`SameSite` is sent on cross-site requests in every current browser, which is what a CSRF token is
there to compensate for, and a cookie without `Secure` crosses plain HTTP. The framework reissues
the cookie with `Secure` (unless `Debug`), `SameSite=Strict`, and a name carrying the application
prefix, so the hardening is a property of the framework rather than of whichever backend happened
to be selected.


## 8. Routing, middleware, and errors (`webapp`)

### 8.1 Routing

`App.Route(prefix, func(r Router))` mirrors chi, and `Router` adds `Use`, `Guard`,
`RequireLogin`, and `Public`. Route parameters use chi's syntax and are read with typed helpers
(`webapp.ParamUint(r, "id")`), never a parse that silently returns zero for malformed input — an
ID that quietly becomes `0` is a "record not found" in one handler and an unintended create in
another.

The split between public and authenticated surfaces is expressed structurally, as a property of
the route tree, rather than as a check inside handlers:

```go
a.Route("/ui/public", func(r webapp.Router) { r.Get("/list/{id}", publicList) })
a.Route("/ui", func(r webapp.Router) {
    r.Use(a.RequireLogin())
    // everything here is behind the gate, by construction
})
```

The global chain runs best-effort authentication (`OptionalAuth`), so public pages can render
differently for a logged-in user without requiring one, and privacy is opt-in per subtree. Raw
chi routers can be attached with `a.Mount(path, chiRouter)`, and non-UI endpoints
(health streams, websockets, prometheus) with `a.Handle`.

### 8.2 Middleware stack

The chain wraps the router; it is not installed on it with chi's `Use`. Two behaviours make that
necessary rather than stylistic. `Use` panics once a route exists, so a stack that any component
can extend cannot be assembled in any other order. And a mux with no routes short-circuits to its
own not-found handler without running any middleware, so a fully configured application that has
not mounted anything yet would serve 404s with no correlation ID, no logging, no security headers,
and a liveness endpoint that is quietly absent. A test builds an empty application and asserts the
headers are on the 404.

Installed in order, outermost first, each individually replaceable:

```
tracker → RequestID → client IP → Recoverer → slog request logger → body limit
→ Compress → Heartbeat("/-/alive") → SecureHeaders → CSRF → OptionalAuth → Profile
```

The tracker goes outside everything, including `RequestID`, because the error path needs to know
whether a response had already begun, and a writer installed partway down the chain is invisible
to everything outside it.

Client IP is taken from the connection unless the application names a header its proxy overwrites
(`WithClientIPHeader`), enumerates proxy networks (`WithTrustedProxies`), or states a hop count
(`WithTrustedProxyCount`). Configured networks are a gate, not only a skip list: a request that
did not arrive over one of them cannot influence its own recorded address. chi's `RealIP` is not
used, and not only because it is deprecated — it takes the leftmost `X-Forwarded-For` entry, or
`True-Client-IP` / `X-Real-IP`, unconditionally, and those values are chosen by the client.

`http.Server` gets `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, and `IdleTimeout` from
`WithTimeouts`; the framework default is a conservative non-zero set, because an unset read header
timeout is a slow-loris primitive and an unset write timeout lets one stalled client hold a
connection, a goroutine, and whatever transaction that handler opened. `SecureHeaders` sets
`X-Content-Type-Options`, `Referrer-Policy`, and `X-Frame-Options`; applications needing a CSP set
it explicitly. State-changing request bodies are capped by `WithMaxBodyBytes`, and exceeding the
cap is a 413 rather than an out-of-memory condition.

### 8.3 Errors

`a.Fail(w, r, status, err)` is the single error exit. It logs at Error with the request ID,
correlation ID, method, path, and error; sets the real status code; and renders the appropriate
template. Detailed error text reaches the client only when debug mode is on — in production the
user sees a generic page and an ID that matches a log line. `HandlerErr` returns route here
automatically, which removes the per-branch "render the internal error template and remember to
return" boilerplate that such applications accumulate and, more often, forget.

`NotFound` and `MethodNotAllowed` render templates rather than plain text. Template execution
failures that occur *after* headers are written are logged and abort the stream; that hook is
overridable, since it is the one error path an application may want to customize (e.g. emitting
a partial-page placeholder for an htmx fragment).

### 8.4 Navigation

`NavItem{Weight, Label, Href, Icon, Perm, Children}`, registered by the application and by
modules. Higher weights sort first; ties break alphabetically so ordering is deterministic.
`pageBase`, injected into every render, lets the nav fragment mark the active item without every
handler saying which page it is. Weight values are documented as coarse bands (90+ for admin,
50–89 for primary sections, below 50 for auxiliary) so a module author does not need to know
what else is installed to place themselves sensibly.

### 8.5 Static assets

Core assets ship embedded and are served at `/static/*` with ETags and a modest
`Cache-Control`. Applications register their own with a namespace, which becomes a URL prefix, so
module assets cannot collide. An `Asset("css/x.css")` helper returns a build-versioned URL for
cache-busting. Third-party CSS/JS is expected to be vendored via `go:generate` and gitignored, so
builds work without network access.

## 9. Modules

For applications large enough to have independent feature areas, a registry lets a package
contribute routes, templates, migrations, permissions, and navigation, and be enabled or disabled
by configuration.

```go
type ModuleInfo struct {
    Name         string
    Scopes       []string   // restrict to a deployment mode; empty means all
    Dependencies []string
    Required     bool
    Permissions  []auth.Perm
    Factory      func(s *webapp.Scope) (any, error)
}

func Register(ModuleInfo) // panics on duplicate name, or a required module depending on an optional one
```

A module's value satisfies, by assertion, whichever of these it implements:

```go
type Router interface{ Routes(r webapp.Router) }
type Migrator interface{ Migrate() error }
type Templater interface{ Templates() (string, fs.FS) } // namespace + root
type Navigator interface{ Nav() []webapp.NavItem }
```

Resolution: filter by scope → apply the `APP_MODULES` whitelist → validate dependencies →
topological sort with alphabetical tie-breaking → instantiate in order. Deterministic ordering
matters because seed data and navigation order should be reproducible across boots and
machines.

Rules for the registry, learned from the hard way: factories must return an error, and startup
aborts on it. A factory that returns `nil, nil` on failure yields a typed-nil interface that
compiles, registers, and panics on first use three layers away from the actual problem. Modules
that want another module's behaviour depend on a consumer-declared interface and type-assert the
deps map, which keeps dependencies acyclic and lets a module degrade gracefully when an optional
collaborator is absent.

## 10. Input handling (`webapp`)

Hand-written form parsing is where such applications carry the most latent defects: return values
from `ParseForm` discarded, booleans inferred from non-empty strings, multi-value fields read as
single values, and no validation at all beyond a nil check.

The obvious fix is a struct-tag binder: declare fields once, reflect over them, bind and validate.
This design does not do that, and the reason is not dependency cost. Reflection is already linked
into every build here, once through the template engine and once through the ORM, so a binder
would add no weight that is not already paid. The reason is legibility at exactly the point where
it matters.

A field that decides whether a request is authorized has to be readable as such at the call site.
In `if err := bindForm(r, &f); err != nil` the `owner_id` that a handler is about to trust is
somewhere inside `f`, and nothing about the line says whether it came from a hidden form input, a
URL parameter, or the authenticated user. Every application that grows an admin surface eventually
produces the handler that bound an `IsAdmin` or `OwnerID` straight off the request body, and the
grep for it is a grep for a tag that looks identical to the innocent ones. Naming each value that
is read, at the place it is read, keeps that decision in the diff where a reviewer can see it.

The invariant this supports, and that the framework itself relies on everywhere: the identity and
ownership of a record come from the request context, never from its body.

What the core provides instead:

| Helper | Behaviour |
|---|---|
| `ParamUint(r, "id")` | required path parameter; parse failure is an error, never `0` |
| `QueryUint`, `QueryUintDefault` | optional unsigned filter; absent is zero, malformed is an error |
| `QueryInt`, `QueryBool` | signed and boolean query values; `"false"` is false, junk is an error |
| `QueryDate` | RFC 3339 or `YYYY-MM-DD`, whole-string match |
| `Page(r)` | `(page, size)` clamped by `db.ClampPage` |
| `FormString` | body value with the parse error surfaced rather than discarded |
| `FormInt`, `FormUint`, `FormBool`, `FormDate` | typed body fields; a required field left blank is an error, not zero |

Blank means different things in the two places, and the helpers differ on purpose. An absent query
parameter is a filter that was not applied, so zero is the right answer; a blank numeric input is a
required field the user did not fill in, and substituting zero answers a question nobody asked.
Query and date parsing share one layout list, so a value a filter accepts is never rejected by the
form carrying the same value.

```go
func (a *App) createTask(w http.ResponseWriter, r *http.Request) error {
    title, err := webapp.FormString(r, "title")
    if err != nil {
        return err
    }
    points, err := webapp.FormInt(r, "points")
    if err != nil {
        return err
    }
    if points < 0 {
        return &webapp.Rejected{
            Template: "views/task_form.p2",
            Values:   webapp.Context{"title": title, "points": points},
            Reasons:  []string{"points must be at least 0"},
        }
    }
    // owner comes from the session, never from the form
    return a.store.Create(r.Context(), currentUser(r), title, points)
}
```

Two error types carry the whole contract. `BadInput` means the request was unusable as given, and
reaches the client as 400. `Rejected` means the values were well-formed but refused, and re-renders
the form at 422 with what was typed plus why it was refused, so nothing has to be entered again.
Both are ordinary errors: a handler returns one and the responder decides status, log level, and
whether the response is a page or an error.

Unchecked checkboxes are the one case a binder handles better by default, and it is handled here by
`QueryBool` using `ParseBool` rather than a non-empty test, which is the difference between a
hidden `<input value="false">` meaning false and meaning true.

`Page` clamps to the same bounds as `db.Paginate`, so a request for 100000 rows cannot ask the
database for them, and a list page gets its window of page links computed in Go rather than by
arithmetic in a template.

Successful mutations end in a 303 redirect via `a.SeeOther`; failures render rather than redirect,
so the message survives the round trip.

If a binder is ever added it will be an optional layer over these helpers, generating calls to
them rather than a second mechanism for reading a request, and it will refuse by default to bind
any field whose name matches a column that authorization depends on.


## 11. Configuration

One `Config` struct, functional options, environment fallbacks, in that precedence order:
explicit option, then environment variable, then documented default. `WithEnvPrefix("TASKS")`
sets the namespace for everything the framework reads:

```
TASKS_ADDR            listen address                  default ":8080"
TASKS_DEBUG           verbose errors, template debug  unset
TASKS_PRODUCTION      hardening, no auto-secrets      unset
TASKS_DB_DRIVER       sqlite | postgres               sqlite
TASKS_DB              sqlite path                     <app>.db
TASKS_DB_DSN          postgres DSN                    required for postgres
TASKS_COOKIE_SECURE   Secure cookie attribute         true unless TASKS_DEBUG
TASKS_MODULES         enabled optional modules        all
AUTHWARE_BASIC_MECHS  auth mechanism chain            htpasswd
AUTHWARE_SESSION_KEY  session signing key             required in production
AUTHWARE_SESSION_LIFETIME session lifetime            1h
```

`Config.Validate()` runs inside `New()` and returns an aggregate error. Constructors never call
`os.Exit`: an application that cannot be configured should return the error to `main`, which
decides how to report it and what to exit with. `WithProduction()` is the switch that turns
"safe in a demo" into "safe on the internet": Secure cookies, required session key, generic error
pages, no default administrator password, no secret generation logged to stdout.

The remaining suffixes cover the transport rather than the application: `_READ_TIMEOUT`,
`_WRITE_TIMEOUT`, `_IDLE_TIMEOUT`, `_MAX_BODY_BYTES`, `_CLIENT_IP_HEADER`,
`_TRUSTED_PROXY_CIDRS`, and `_TRUSTED_PROXY_COUNT`. Booleans are read with three states —
absent, true, false — because "unset" and "false" resolve to different defaults here: cookies
are Secure unless `Debug` is set, and an explicit `false` has to be distinguishable from neither
being mentioned.

`Debug` and `Production` set together are a boot error rather than a precedence rule. A process
started with contradictory intent should not guess.

## 12. Failure modes this design closes

These are the recurring defects in small server-rendered Go applications. Each is a default, not
a documented recommendation.

| Failure mode | Mitigation |
|---|---|
| Error pages served with status 200, echoing internal error text to clients | `a.Fail` sets the status; details only in debug; correlation ID in logs |
| No CSRF protection on cookie-authenticated state-changing requests | HMAC double-submit middleware, on by default |
| Session cookie missing `Secure` / `SameSite` | hardened issuance, configurable for proxied TLS |
| Session key generated per process, so sessions die on restart or multiply replicas | boot failure in production without an explicit key |
| No panic recovery; one bad request resets a connection or takes down the process | global `Recoverer` with stack logging |
| Error templates referenced but never written | core UI owns them; startup compiles the required set |
| No request logging or correlation IDs | `RequestID` plus a slog request logger |
| No `http.Server` timeouts | `WithTimeouts` with non-zero defaults |
| SQLite without WAL, busy timeout, or foreign keys | DSN pragmas applied unconditionally |
| Authorization checks copy-pasted per handler, one of them missing | guards, `RequireOwnership`, boot-time permission installation |
| Global filter registries mutated from constructors, racing at boot | framework-owned pure filters registered in `init()` |
| Module factories swallowing errors into typed-nil interfaces | `Factory` must return an error; startup aborts |
| `os.Exit` called from constructors; dropped context cancel functions | errors returned; `Run()` owns the lifecycle |
| Relative `extends` paths coupling templates to directory depth | namespace-aware loader |
| ID parsing that silently yields zero | typed parameter helpers that report parse errors |
| Client-controlled `X-Forwarded-For` treated as the source address | trust from the connection unless a proxy is configured; configured networks are a gate |
| Unbounded request body buffered into memory | `WithMaxBodyBytes` on state-changing methods, reported as 413 |
| Middleware silently absent on an application with no routes | chain wraps the router; a test asserts the headers on an empty app's 404 |
| `405` with no `Allow`, leaving the client to guess | allowed methods computed from the route table, `HEAD` implied by `GET` |

## 13. Testing

`webapp.NewTestApp(t, opts...)` returns an application with a temp-directory SQLite database (or
PostgreSQL when `APP_TEST_PG_DSN` is set, which is how foreign-key ordering and dialect
differences get caught), compiled templates, and an `http.Client` wired directly to the handler —
no listener, no ports, no sleeps.

`testas.As(t, "alice", "admins")` injects an authware user into the request context, so
authorization is tested without a login round trip. The framework's own test suite asserts the
security-relevant invariants explicitly: 401 versus 403 on denials, JSON versus HTML negotiation,
CSRF rejection of unsafe methods, escaped rendering of error payloads (a `<script>` in an error
must never round-trip), migration idempotency across two boots of the same database, and the
required-template compile check.

The example application in `examples/tasks` is the integration test: authentication, ownership,
permissions, pagination, form validation, theming, and both database drivers.

## 14. Milestones

**M1 — spine.** `db` with driver selection, pragmas, pooling, AutoMigrate, versioned migrations,
and helpers. `webapp.New` with config, options, the middleware stack, the render funnel, typed
input handling, pagination, and `Run`. Bare `View` over an embedded FS.

**M2 — auth.** authware wiring, hardened sessions, session key policy, CSRF, permissions,
denial negotiation, profile middleware, database-backed backend.

**M3 — templating.** Namespaced loader, core UI, startup compile check, debug mode, filters,
`Share`, navigation.

**M4 — modules and polish.** Registry, scopes, capability interfaces, static asset namespacing,
`NewTestApp`/`testas`, example application, package documentation.

M4 was originally "forms": binding, validation, re-render helpers, typed parameters, pagination.
Typed parameters, pagination, and the re-render contract turned out to belong to the request spine
rather than to a separate layer — they are how a handler learns what it was asked for, and a
render funnel that cannot re-render with values cannot report a refusal. Splitting them out would
have produced a package whose only job was to be imported by the thing that already does the work.
What remains of the original M4 is the registry, namespacing, test helpers, and the example.

## 15. Open questions

1. Whether the framework ships an administrative user-management UI. It is genuinely useful and
   genuinely security-sensitive. Proposed: an optional module, mounted only behind `CORE:ADMIN`.
2. Whether a lightweight in-process event bus with a websocket endpoint belongs in v1. It is
   load-bearing for dashboard-style applications but not for CRUD. Proposed: v1.1, as
   `webapp/rt`, with a documented gap-detection contract for clients.
3. Whether to offer a pure-Go SQLite driver as an option for `CGO_ENABLED=0` builds, at the cost
   of some pragmas and performance. Proposed: accept a driver injection option rather than
   depending on one.

Closed since the design was written:

- ~~`HandlerErr` adds a second handler shape.~~ Built, and both shapes exist with `HandlerErr`
  recommended. The cost turned out to be lower than expected: `WrapErr` converts in one direction,
  so an application never has to choose in order to interoperate, and the framework's own error
  path is only reachable through the returning shape.
- ~~Whether to build a struct-tag form binder.~~ No. See §10: the argument was legibility of
  authorization-relevant fields, not dependency cost, and it holds. The helpers that replaced it
  are in the core and cover path parameters, query values, dates, paging, and form fields.
- ~~How a database-backed authware backend gets a handle through an argument-less `Factory`.~~
  A registered closure captures it, and construction order guarantees the handle exists first.
  See §7.6.

