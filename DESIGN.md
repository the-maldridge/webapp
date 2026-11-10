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
| AuthN | `github.com/the-maldridge/authware` | User type and context keys, stateless encrypted session cookie format |
| Passwords | `golang.org/x/crypto/bcrypt` | Appropriate default work factor, no tuning required |
| Logging | `log/slog` | Stdlib, structured, no configuration |

Authentication is a `Service` owned by `webapp/auth`, configured through a mechanism chain rather
than a global registry:

```go
type Mechanism interface {
    Verify(ctx context.Context, identity, password string) error
    Groups(ctx context.Context, identity string) (map[string]struct{}, error)
    Name() string
}
```

`Verify` returns an error rather than a bool-and-nil so that "this mechanism does not know that
account" and "wrong password" stay distinguishable internally while collapsing to one indistinguishable
failure on the way out. Token resolution and password upgrades are separate optional interfaces,
so a backend that cannot write passwords does not have to pretend. The database mechanism is in the
chain by default and an application appends or replaces the set with options; see §7.6.

## 3. Package layout

```
webapp/                App, options, config, router, middleware, render funnel, errors, input handling, nav, static, modules
webapp/db/             driver selection, pooling, pragmas, AutoMigrate + versioned migrations, upsert, paging
webapp/auth/           service, mechanism chain, hardened cookies, CSRF, permissions, guards, login handlers
webapp/tmpl/           namespaced multi-loader, shared context, error adaptation, filters
webapp/themetest/      the theme contract, runnable from a theme's own test suite
webapp/theme/builtin/  default theme: base and login templates, error pages, nav fragment, static assets
webapp/webapptest/     test applications: disposable database, real auth, handler-driven client
webapp/testas/         identity stamped onto a request, for handlers tested alone
webapp/internal/       code shared between the core and its own default theme
examples/tasks/        a complete application built on the framework, in its own module
```

Dependency direction is strictly inward: `webapp` may import `db`, `auth`, `tmpl`, and
`theme/builtin`; none of those import `webapp`. Applications import `webapp` plus whichever of the
subpackages they need directly. `webapptest`, `testas`, and every theme other than the default are
the leaves: they import `webapp` and nothing imports them, which is what lets a test helper reach
an installed service, and a theme register itself, without the framework knowing either exists.

A theme is not required to live in this repository, and the Bulma theme does not: it is
`github.com/the-maldridge/webapp-bulma`, a separate module adjacent to this one on disk, importing
`webapp` for `RegisterTheme` and `RequiredTemplates` and importing `webapp/themetest` to run the
contract over itself. That direction is legal precisely because the leaf rule above already holds:
the framework imports no theme but its own default. The example application is in its own module for
the same reason one layer out, since it depends on a theme that depends on the framework, and an
example inside the framework's module would close that graph into a cycle between two modules.

## 4. Application surface

```go
func main() {
    a, err := webapp.New(
        webapp.WithEnvPrefix("TASKS"),
        webapp.WithDBOptions(db.WithAutoMigrate(Task{})),
        webapp.WithTemplates("tasks", templateFS), // //go:embed p2
    )
    if err != nil {
        return err
    }
    defer a.Close()

    // Authentication needs the database, which does not exist until New has
    // opened it, so it is installed rather than configured. Registering the
    // service as a component is what puts its tables in the schema.
    svc, err := auth.New(a.DB)
    if err != nil {
        return err
    }
    if err := a.RegisterComponent(svc); err != nil {
        return err
    }
    if err := a.InstallAuth(svc); err != nil {
        return err
    }

    a.Nav(webapp.NavItem{Weight: 80, Label: "Tasks", Href: "/ui/task/"})

    a.Route("/ui/task", func(r webapp.Router) {
        r.Use(a.RequireLogin())
        r.Get("/", listTasks)
        r.Get("/new", taskForm)
        r.Post("/new", taskFormSubmit)
        r.Get("/{id}/edit", taskForm)
        r.Post("/{id}/edit", taskFormSubmit)
        r.Post("/{id}/complete", r.Guard(auth.P("tasks", "complete"), completeTask))
    })

    return a.Run()
}
```

`InstallAuth` is a method rather than a construction option for the reason in its own comment: a
service needs a database handle, and the auth tables have to be part of the schema decided before
the first request. It splices the authentication, CSRF, and profile middleware into the chain
immediately after the security headers, installs the denial renderer, and puts `user`,
`isAuthenticated`, `csrfInput`, and `csrfToken` into every render context.

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

Two constraints follow from the engine and are the reason for the shape of the API:

- **Exactly one loader.** pongo2 resolves `extends`, `include`, and imports against the first
  loader in its set and ignores the rest, so mounting one loader per root would make every
  framework template unresolvable from inside an application template. The registry is one
  loader implementing the search above, which is also what makes `@ns/...` mandatory for
  framework files: it is the only form that cannot be answered by a different root. An override
  is therefore a second copy of a file that wins by mount order, not a fallback to a
  second root when the first misses.
- **`Abs` cannot report failure.** A name that resolves nowhere comes back unchanged, which for
  something plausible-looking like `@ghost/detail.p2` is indistinguishable from a resolved key.
  Anything that produces an error message or a startup check goes through
  `Lookup(base, name)`, which re-checks that the namespace is mounted and the file exists.

The consequence for applications: `{% extends "base.p2" %}` and
`{% include "fragments/nav.p2" %}` just work, at any depth, regardless of which package the
template was embedded from. Names that climb out of a mounted root are clamped to it, so a
template cannot reference a file that was never a template.

### 5.2 Themes and the compile check

The framework renders a handful of pages itself: the base layout, the login page, the navigation
fragment, and the three error pages. `theme/builtin` is the package that supplies them by default,
and it is a package because embedding requires one — the directory some package embeds has to be a
directory in the source tree, and the alternative is the framework's presentation files living
inside the application core.

`ThemeInfo.Required` lists the templates the framework renders itself. Whichever theme is active,
they are compiled through `Renderer.Verify` at startup and a missing or unparseable entry fails
construction:

```
@core/base.p2  @core/login.p2  @core/fragments/nav.p2
@core/errors/internal.p2  @core/errors/unauthorized.p2  @core/errors/notfound.p2
```

They are listed namespace-qualified, and that is load-bearing: a bare `errors/internal.p2`
resolves to whichever mounted root contains it first, so an application shipping its own copy
would make the framework's compile check pass while the framework still could not render its own
failures. An error template is rendered only when something has already gone wrong, which is the
worst possible moment to discover it is broken and the least likely moment to investigate it.

A theme is mounted *as* core rather than ahead of it. That is a change from the original design,
which described themes as mounting a root in front of the framework's own, and the reason is that
the mechanism it described could not work: absolute names resolve inside exactly one namespace
with no fallback (§5.1), so a second mounted root holding a different `base.p2` is invisible to
every template that asks for `@core/base.p2`. Substitution is what makes the theme's base layout
the one the framework's error pages extend, and the visible consequence is that an application's
own templates — which extend `@core/base.p2` because that is how they reach the framework's layout
— change appearance when the theme changes, without any of them being edited.

The cost of substitution is that a theme is all-or-nothing, and the check above is what makes that
safe rather than merely strict. A theme cannot supply a pretty login page and leave the error pages
to the default, because there is no default left to fall back to; it is refused at startup unless
it supplies all seven and they all compile. So the guarantee about the error page changes shape.
An application still cannot replace the framework's error page: `core` is a reserved namespace, an
application's templates arrive in their own, and no amount of application markup redirects a
framework render. A theme can, because a theme *is* what renders it — and cannot ship a half-empty
one, because construction fails first.

Selecting a theme is a name, not a path: `WithTheme` or `<PREFIX>_THEME`. The name has to match a
registered theme, and a theme outside the framework's own registers itself from its package's
`init`, which means the application imported it. A name matching nothing is a construction error
listing what is available, never a quiet fall back to the default, because a deployment that
believes it is serving a theme and is not has produced the visual defect no test notices and every
user does. The assets follow the same resolution: the active theme's static tree is served below
`/static/core`, so a theme's stylesheet URL is a stable shape and a template never mentions which
theme produced the page it is on.

A theme is therefore not required to live in this repository, and the two things a theme author
outside it would otherwise have to hardcode are supplied instead: `RequiredTemplates` returns the
set construction will demand, and `webapp/themetest` runs every contract rule against a theme from
inside the theme's own package. `themetest` was an internal package until that mattered, because an
internal package cannot be imported from another module, which meant the contract was checkable by
anyone except the audience the contract is written for. Both depend only on `tmpl` and the standard
library, so a theme pulls in no more of the framework than a theme should.

Three things an external theme author has to know without being told, and the framework can describe
but not prevent. Which template variables exist is documented in §5.4 and nowhere machine-checkable,
so a theme referencing `{{ user.Identity }}` on a page the framework renders outside the sign-in
flow finds out at render time. A filter registered from a theme's `init` competes in pongo2's
process-global filter map, so an unprefixed name can collide with another package's and the loser
learns at startup; the framework reports the collision, but prefixing avoids it. And a vendored
asset must be present when the theme is compiled, because generation cannot reach across modules
(§8.5). The behavioural contract, which is the CSRF field, the never-repopulated password, and the
undifferentiated login failure, is checked by the suite over the example application rather than by
anything a theme author can call, so a theme that loses one of them is caught by the application
using it rather than by its own tests.

The per-template contract, since a theme has to satisfy it and cannot read it off the framework:

| Template | Blocks a consumer may fill | Variables, and who supplies them |
|---|---|---|
| `base.p2` | `title`, `head`, `bodyclass`, `header`, `content`, `footer` | `siteTitle`, `buildVersion` (framework). Owns the `<link>` to its own stylesheet, and includes `@core/fragments/nav.p2` |
| `login.p2` | `title`, `content` | `loginError`, `action`, `username`, `next` (application), `csrfInput`, `siteTitle` (framework) |
| `fragments/nav.p2` | fragment, no blocks | `navElements` (`Base`/`Href`/`Icon`/`Label`/`Children`), `pageBase`, `isAuthenticated`, `csrfInput`, `user.Identity`, `signInURL`, `signOutURL` (framework) |
| `errors/base.p2` | `title`, `content`, `detail` (left empty, for its children) | `status`, `statusText`, `requestId`, `siteTitle` (framework), `errorDetail` (framework, debug only) |
| `errors/notfound.p2` | `detail` | none beyond the skeleton |
| `errors/unauthorized.p2` | `detail` | `isAuthenticated`, `user.Identity`, `requiredPerm`, `signInURL` |
| `errors/internal.p2` | `detail` | none beyond the skeleton |

Two entries in that table say something about the design rather than merely recording it. The login
page's variables come from the application, not the framework, because the framework renders the
form but whoever wires up sign-in decides the failure wording; the one requirement that survives the
cut is that a single message covers a wrong username and a wrong password alike, so the page cannot
become an oracle for which half of a credential is valid. And `errorDetail` is present only under
`Config.Debug`, which is why a theme may render it and must not depend on it: a theme that puts the
detail in a wrapper element only when it is non-empty is correct in both modes, and a theme that
assumes it is there will show an empty box in production.

The block column is enforced at startup, and unlike the variable column it can be, which is why it
is worth the paragraph. Pongo2 resolves a `{% block %}` a child fills against the parent it
extends, and when the parent never declared that block, the render succeeds and the filled block is
dropped. No parse error, no render error, and the output is indistinguishable from a template that
never mentioned the block. `errors/base.p2` makes this serious rather than merely untidy: all three
of its children fill `detail`, so a skeleton that lost the declaration during a refactor renders
three error pages with the reason for the failure removed, which is the whole page, silently, on the
page that appears when something else is already wrong. A test that asserts the error page "renders"
passes, because it does.

So construction reads the active theme's layouts and refuses one that omits a block anything fills,
naming the template and the block. The required surface is the one documented above, which is what
makes the table a contract rather than a description: an application that writes
`{% block footer %}` has been told the framework's layouts have somewhere to put it, and a theme now
cannot quietly take that away. A theme with no footer region is not forbidden, it declares an empty
block, which costs one line and says something true.

Because a theme owns its base layout, it owns its markup entirely. Bulma expects
`.field > .label + .control`; the default theme's login form writes `<label class="field">`. No
amount of vendored CSS reconciles the two, so a theme is a rewrite in the target framework's idiom
rather than the default markup with a stylesheet bolted on, and the framework's own class names are
not part of what a theme must preserve. What a theme must preserve is behaviour, which is the part
that is not styling: the CSRF field, the password that is never repopulated, and the single failure
message that does not say which half of the credentials was wrong.

### 5.3 Debug mode

`WithTemplateDebug(dir)` mounts an `os.DirFS` root before every embedded root and turns on
pongo2's non-caching mode, so an edit is visible on the next request without a rebuild. The two
effects are one option rather than two settings: an overlay whose edits never appear is an
overlay the server is ignoring, and an engineer editing a file nothing reads has no reason to
suspect the option they can see.

Names the overlay does not contain fall back to the embedded copies, because a directory with one
file in it is what debugging one page looks like. The option is refused when the application is
configured for production: a writable directory ahead of the templates inside the binary is a
code-execution surface on any host where the deployment user can write, and a startup warning
about template precedence is a warning nobody reads at three in the morning.

### 5.4 Context and filters

`View` is the only render entrypoint and injects ambient context: `user`, `isAuthenticated`,
`navElements`, `can`, `csrfInput`, `pageBase`, `requestId`, `buildVersion`, `debug`,
`siteTitle`. Applications add their own with `a.Share("key", fn)`, called per request, which is
how application-wide navigation and feature flags reach templates without every handler
remembering them. Render context keys must match `[a-zA-Z0-9_]+`; a key a template could not name
is rejected at the boundary with the offending key named, rather than at render depth with an
error pointing at a line of a template.

Three rules are enforced rather than documented:

- Errors placed in a render context are converted to strings before rendering, and
  `template.HTML` is converted to a value the engine treats as already escaped. pongo2 does not
  know the `html/template` types, so without that conversion a login page renders its CSRF input
  as visible markup; and a `fmt.Stringer` reaching a template bypasses autoescaping, which is an
  XSS vector with the error message of a failed database read as the payload.
- Custom filters are registered once, in `init()`, and must be pure. pongo2's filter registry is
  a process-global map, so a filter registered from a constructor is a data race in any
  application that builds two renderers, and a filter closing over request state is a race
  between requests. A registration that collided with another package's name is remembered and
  reported at renderer construction, because a template's "unknown filter" error is otherwise a
  question about an unrelated package.
- Framework filters are `jsonify`, `keyValue`, `pluralizeEN`, and `hasPermission`.
  `pluralizeEN` is named for the language it implements: a filter called `pluralize` that knows
  only English is a promise broken for whoever localizes the application. `keyValue` sorts a map
  because Go maps do not iterate in order, and an unsorted map in a template is a document that
  differs on every request. Anything that needs the current request is not a filter at all:
  `can("MODULE:GRANT")` is a per-request callable built for one request and unreachable from any
  other.


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

Authentication is a `Service` over the application's database: the mechanism chain, stateless
AES-GCM encrypted session cookies, and `User{Identity, Groups, AuthedBy}` delivered through the
request context. The cookie format is authware's, so an existing deployment's users survive the
switch and a fleet of replicas agrees who is signed in without sharing anything; the package
borrows authware's type aliases rather than defining its own user.

The package returns plain `func(http.Handler) http.Handler` middleware and reports refusals
through an injected `DenialHandler`. It does not import `webapp` and renders nothing, because it
cannot know whether a client wanted a page or JSON, and does not own the error templates. The
dependency points inward: everything framework-shaped lives in the core's auth wiring.

### 7.1 Sessions and cookies

The framework issues sessions through its own codec with hardened attributes: `HttpOnly`,
`Path=/`, `SameSite=Lax`, and `Secure` unless explicitly disabled for a plain-HTTP deployment
behind a TLS-terminating proxy. Session name and lifetime are configuration, not constants, and
the name carries the application's environment prefix so that two applications sharing a host
cannot read each other's sessions.

`SameSite` is `Lax` rather than `Strict` deliberately: `Strict` drops the session cookie on
ordinary navigation from an external link, which breaks the return-from-payment-provider and
click-a-link-from-an-email flows that every application eventually has. `Lax` still withholds the
cookie on cross-site POSTs, which is the request CSRF actually cares about.

Session key policy is enforced at startup: a production boot without an explicit session key is
an error rather than a generated one. A per-process random key silently invalidates every session
on restart and makes horizontal scaling impossible, which is the kind of failure that gets
attributed to the network.

A session carries the groups the user held when it was issued, so taking a group away takes effect
when the session expires, not immediately. Making revocation instant would mean a database read on
every request; an application that needs it calls `Service.HasPerm(ctx, identity, p)`, which reads
through, rather than paying the round trip everywhere.

### 7.2 CSRF

Double-submit, no server state: a `csrf_token` cookie signed with a key derived from the session
key, compared in constant time against the `X-CSRF-Token` header or a `csrf_token` form field.
The token's payload contains the identity it was minted for, so a token minted for one user cannot
be replayed as another's, and it expires with the session. The derived key is HMAC over the
session key with a domain-separating label, never the session key itself; see §12.

Enforced on unsafe methods for cookie-authenticated requests, and for anonymous unsafe requests
that arrived with the CSRF cookie — the condition a browser satisfies and an API client does not,
which keeps anonymous JSON posts working while a cross-site login attempt is still checked.
Skipped for requests whose `Authorization` header *verified*: a cross-site form cannot set that
header, and a cross-origin fetch that could is stopped by CORS before the handler. The skip is
gated on verification, because a request presenting a credential that failed has proved nothing
about who sent it.

Safe methods always ensure a token exists, including for anonymous requests, so the login form has
one; a check that issues tokens only to signed-in users cannot protect the endpoint where a forged
request is worth the most. The token a page renders comes from the request context, which is how a
first-time visitor — who has no CSRF cookie — gets a form that submits successfully instead of an
empty field and a guaranteed 403.

Templates emit the hidden field with `{{ csrfInput }}`; the core wraps the already-escaped markup
from `auth.CSRFInput` in `template.HTML` once, so no template needs a `|safe` filter and no token
can carry markup. The failure mode is **403**. A forged request is not malformed input — every
field in it is well formed, and the reason it is refused is authorization — and a 400 that
re-renders the form hands whoever submitted it a description of what was expected.

This is on by default for any route that authenticates by cookie. It is the single most common
critical vulnerability in small server-rendered Go applications, and none of them should have to
opt in to it.

### 7.3 Permissions

Authorization is a two-part string, module and grant:

```go
type Perm struct{ Module, Grant string } // e.g. {"TASKS", "ADMIN"}

func P(module, grant string) Perm        // upper-cases both halves
func (p Perm) String() string            // "TASKS:ADMIN"
func ParsePerm(s string) (Perm, error)   // strict: both halves required
```

A user satisfies a permission by exact match, `MODULE:ADMIN` for that module, or `CORE:ADMIN` for
anything (`auth.CoreAdmin()`). There are no wildcards, no inheritance between grants, and no
prefix matching on the module half: each of those rules has produced a grant broader than whoever
wrote it intended, and "narrow and explodable in one direction only" is the property worth having
here. An empty grant in a `Perm.String()` is malformed rather than "all grants in this module",
because an accidentally-empty `Grant` field is a plausible bug and a bug that widens authority is
worse than one that denies.

Four ways to use a permission, all reading the same data:

```go
r.Use(a.Guard(auth.P("tasks", "admin")))            // route group
r.Post("/nuke", r.Guard(auth.P("tasks", "admin"), handler)) // single handler
if auth.HasPerm(r, auth.P("tasks", "edit")) { }     // inside a handler
{% if user|hasPermission:"TASKS:EDIT" %}            // template
```

`a.RequireLogin()` and `r.RequireLogin(h)` are the identity-only form of the same check.

Navigation entries carry a required permission and are filtered per request, so users are never
offered a link that will deny them.

Ownership — the "may this user touch *this* row" question that a permission model does not answer —
gets one helper, `auth.RequireOwnership(r, ownerProfileID)`, used by every mutating handler. The
owner is read from the request context and never from the body, which is what makes the check
unspoofable by a form field that says otherwise. Per-handler ad hoc ownership comparisons are how
such applications end up with exactly one endpoint that forgot the check.

### 7.4 Denials

The auth package classifies a refusal — no identity, forbidden, CSRF — into a `Denial` carrying
the kind and the permission that was required, and hands it to a `DenialHandler`. `webapp` installs
its own at `InstallAuth` time, so denials are content-negotiated and status-correct: 401 when no
user is present, 403 when a user lacks the grant. If the request prefers `application/json`, the
body is `{"error": "permission denied", "required": "TASKS:ADMIN"}`; otherwise the unauthorized
template renders with the required permission named. A 401 carries `WWW-Authenticate`. Returning an
HTML page or a 200 to an API caller produces a client that believes a rejected write succeeded, and
an auth package that decided the response format could not be tested without a render funnel.

Applications without an installed service deny with 500 rather than passing through: a gate
configured on an application that has no identity behind it is a configuration bug, and "protected
routes are quietly open depending on which call site forgot what" is the wrong direction for that
to fail in.

### 7.5 Profiles

`Profile` is a framework-owned row keyed on the authenticated identity, created on first request
by the profile middleware and reachable through a checked accessor that never panics. Application
data hangs off the profile's ID. Applications that need extra fields keep their own model joined
to it rather than extending the core one. This gives every application a stable local primary key
for "the current user" while identity itself remains owned by the authentication backend, and it
means there is no registration flow to write: the first request provisions the row.

### 7.6 Database-backed accounts

`auth.New(d, opts...)` builds a service over the application's database with a working account
store: bcrypt-hashed passwords, group membership, opaque API tokens stored only as digests, and
the profile row above. An application gets real accounts without a password file, and swaps in
another backend by supplying a mechanism:

```go
svc, err := auth.New(d, auth.WithMechanism(ldapMechanism{})) // database stays as fallback
svc, err := auth.New(d, auth.WithOnlyMechanisms(ldapMechanism{})) // or drop local accounts
```

`WithMechanism` appends, leaving the database backend in the chain so a local administrator can
still get in when a directory is down — which is precisely when an emergency account matters.
`WithOnlyMechanisms` replaces it, for deployments where a local table is a liability rather than a
fallback. A mechanism is three methods — `Verify`, `Groups`, `Name` — with token resolution and
password upgrades as optional interfaces, so an LDAP directory that cannot write passwords declares
fewer capabilities rather than stubbing out ones it cannot honor.

Mechanisms are constructor arguments, not names in a global registry. The alternative — a package
`Factory func() (Authenticator, error)` keyed by name, selected by an environment variable — has
one fatal signature: it takes no arguments, so it cannot be handed a database handle, and the only
ways out are a captured closure, an ambient global, or widening the signature into a
dependency-carrying bag. Passing the handle to the constructor makes the requirement visible in the
type, gives a service exactly the mechanisms it was told to have, and removes both the
first-registration-wins race and the configuration variable that names a backend nobody registered.
An operator still chooses the backend; it is the application's `main` that reads the variable and
passes the mechanism.

Login is a handler rather than a flow to write: `svc.LoginHandler(opts)` parses the form, calls
`Authenticate`, issues the session, and redirects with 303 so a refresh cannot resubmit. Failures
are indistinguishable by construction — identical status, identical body, no cookie — with the
mechanism-specific reasons going to a debug log line; `errors.Is` on the returned sentinel works
because every failure path returns that one value. `auth.SafeNext` resolves the redirect target
and refuses scheme-relative, backslash, encoded, and control-character variants. Logout is POST
only.

Password handling: bcrypt at cost 12, with input truncated to the 72 bytes bcrypt actually reads so
that the visible password is the password. A login against an unknown account pays a real hash
anyway, so a timing difference cannot enumerate who exists. Hashes recorded at an older cost are
rewritten on successful login, which is the only moment the plaintext exists.

API tokens carry 256 bits of entropy and are stored as SHA-256 digests, not bcrypt hashes: bcrypt's
cost is a defense against low-entropy input, and paying it on every token-authenticated request
would only slow the server down. The plaintext is shown once at creation, alongside a short display
prefix.

Two properties of the upstream session cookie are overridden rather than inherited. It is named
`session`, and it is set with `HttpOnly` alone: no `Secure`, no `SameSite`. A cookie without
`SameSite` is sent on cross-site requests in every current browser, which is what a CSRF token is
there to compensate for, and a cookie without `Secure` crosses plain HTTP. The framework reissues
the cookie with `Secure` (unless `Debug`), `SameSite=Lax`, and a name carrying the application
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
→ Compress → Heartbeat("/-/alive") → SecureHeaders → ResolveIdentity → CSRF → Profile → router
```

The identity resolver runs before the CSRF check, so a token can be bound to the identity this
request was authenticated as rather than resolving the credential a second time, and the profile
middleware runs last because provisioning a row needs the identity the first one established. All
three are spliced in by `InstallAuth`, between the security headers and the router, which is the
only position where a denial still gets hardened headers and a route guard can read an identity.

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

Three error templates ship, because those are the three cases a viewer can act on differently:
`@core/errors/notfound.p2` for 404 and 410, `@core/errors/unauthorized.p2` for 401 and 403,
`@core/errors/internal.p2` for everything from 500 up. Everything else — a 405, a 418, a code
nobody designed a page for — uses the built-in page, which is plain formatting over escaped
values and therefore cannot fail. A refusal names the grant it needed, taken from a typed denial
rather than parsed back out of an error string, and states it to an anonymous viewer as well as
to a signed-in one: the person best placed to fix a missing permission is often the one who
cannot reach the page to see it.

Error pages are rendered from namespace-qualified names, and that is what keeps an application
mid-deploy from replacing the page that explains itself. `SetErrorPage` replaces the renderer for
applications with their own error design.

The status of an error response is written only when a body exists to carry it. A render that
fails having produced nothing — the usual case, and the required behaviour of a renderer — is
replaced by the built-in page at the same status rather than being preceded by a `200` that no
error page can follow. An error page that itself fails is logged together with the original
cause, because the pair is one story and either half alone reads like a mystery.

### 8.4 Navigation

`NavItem{Weight, Label, Href, Icon, Perm, Children}`, registered with `a.Nav(...)` by the
application and by modules. Higher weights sort first; ties break alphabetically so ordering is
deterministic and the order of `Nav` calls cannot change what a page looks like. Weight values
are documented as coarse bands (90+ for admin, 50–89 for primary sections, below 50 for
auxiliary) so a module author does not need to know what else is installed to place themselves
sensibly.

Items are filtered per request, not at registration, because the answer depends on who is asking:
an item whose `Perm` is set is hidden from viewers who do not hold it, and hiding it through the
same `auth.HasPerm` that guards the route behind it is what keeps a visible link and a refused
destination from disagreeing. The zero `Perm` means no gate, which is the right default for a home
page and the wrong one for anything else — a module that leaves it unset is saying out loud that
the link is public.

The fragment receives `navElements` already filtered and ordered, with each entry's `Base`
derived from the first segment of its `Href`; `pageBase`, injected into every render from the
request path, is what marks the active item without any handler declaring which page it is.

### 8.5 Static assets

The active theme's assets ship embedded and are served at `/static/core/`, which is what lets a
template link its stylesheet without knowing which theme it is on. A module's are served at
`/static/<namespace>/`, which is what lets two modules ship `app.css` without either overwriting
the other; a namespace is validated at startup against the framework's, and templates and assets
may share a name because they are different address spaces.

Embedded files have no modification time, so the file server has no validator to offer and a
browser re-downloads a stylesheet forever. The framework therefore fingerprints each asset tree at
startup and sets an `ETag` per request, which makes `If-None-Match` answer 304; `Cache-Control:
public, max-age=3600` is set deliberately rather than falling out of the transport, and `Asset`
and `ModuleAsset` stamp the URL with the build version so a browser that cached the previous
deploy asks again. Query-parameter stamping rather than filename rewriting because the assets are
embedded: there is no build step in which `app.a4f3c1.css` could be produced.

Third-party CSS is committed by the theme that vendors it. The Bulma theme, which is where any of
this framework's CSS comes from, is a separate module and keeps `bulma.min.css` and its licence in
its own repository. That was not the original arrangement, and the reason it changed is worth
recording because the failure was silent.

The bundle used to be fetched by `go:generate`, verified against a `bulma.lock` naming a release and
the SHA-256 of both the tarball and each file taken from it, with the generator refusing to write
anything whose digest did not match. The lock was the only record of the version, so there was no
constant that could drift from the file it verified, and `-check` answered whether what was on disk
was what the lock said without fetching anything. A digest mismatch was fatal rather than a warning
because the failure it guarded was specific: an artifact at a registry that is not the one reviewed,
served to every application that embeds the theme. All of that remains true and the mechanism moved
with the theme.

What did not survive was the conclusion that a generated bundle can be the arrangement at all. `go
generate` is a main-module operation: it runs over the packages of the module being built and refuses
every package belonging to a dependency. An application that imported the framework as a library
therefore could not run the generator, and could not obtain the bundle by any route it controlled,
because the fetcher sat under `internal/` and was not importable either. The bundle was
committed-by-checkout inside this repository and absent-by-default everywhere else, which meant a
theme could be present, selected, and completely unstyled in exactly the deployments that had done
nothing wrong. Committing the asset is the fix, and it is also what let the theme leave.

The startup check stays, because a theme can always vendor a generated asset badly, and the failure
is invisible in every way a rendered page can be inspected. The framework reads the active theme's
base layout at startup and checks every asset it links against the theme's own asset tree, in the
same place it already compiles the theme's required templates. A theme missing an asset its layout
links is a construction error naming the theme and the path. The alternative is a deployment that
answers 200 on every page, is visibly unstyled, and reports nothing about it in any log line, and the
first thing anyone inspects when that happens is the templates, which are correct.

The framework publishes no CSS custom properties for an application to consume. Its default theme
declares the variables it uses, but they are that theme's private business, in the same way its
class names are: borrowing them means the page picks its colours from whichever theme is loaded,
which looks correct in exactly one of them. An application's stylesheet declares its own.


## 9. Modules

For applications large enough to have independent feature areas, a registry lets a package
contribute routes, templates, assets, schema, permissions, and navigation, and be enabled or disabled
by configuration.

```go
type ModuleInfo struct {
    Name         string   // identity in configuration, the dependency graph, and errors
    Scopes       []string // restrict to a deployment role; empty means every role
    Dependencies []string
    Required     bool
    Permissions  []auth.Perm
    Factory      func(s *webapp.Scope) (any, error)
}

func Register(ModuleInfo)             // panics: a bad registration is a defect in the code compiled
func WithModule(...ModuleInfo) Option // reports the same problems as errors
```

Two entry points, one validator. `Register` is what a module's own package initialisation calls, so
it panics: initialisation has no error to return, and the alternative is a `MustRegister` that the
application author either never calls or ignores. `WithModule` is what `main()` calls to assemble
modules only this application sees, where a defective description is properly an error from `New`.
Both run the same validation, because the two drifting apart would produce a module that is valid in
one binary and invalid in another. Names are compared case-insensitively, and a name claimed both in
the global registry and by `WithModule` is an error rather than a precedence: two definitions of one
module is exactly the ambiguity a name exists to remove.

### 9.1 Capabilities

A module's value is asked which of these it is, and every one it satisfies is applied at startup:

```go
type RouteContributor interface{ Routes(r webapp.Router) }
type Templater interface{ Templates() (namespace string, fsys fs.FS) }
type Staticer interface{ Static() (namespace string, fsys fs.FS) }
type Navigator interface{ Nav() []webapp.NavItem }
```

They are separate interfaces rather than one `Module` interface because a module with no templates
should not write an empty `Templates` method to satisfy a contract, and because an application
asserting one capability should not have to know the others exist.

Schema contribution is deliberately absent from the list. A module that owns tables implements the
same `Component` (`Models() []any`) and `Migrator` (`Migrate(ctx, *db.DB) error`) interfaces every
other component already implements, so there is one rule about what syncs and what migrates instead
of a parallel pair whose two signatures nobody can keep straight. This design previously listed a
module `Router` and a module `Migrator` here; the first collides with the existing `webapp.Router`
and the second would only ever be confused with the component one, so neither exists. `Staticer` was
missing from the design and is here because assets need the same namespace discipline as
templates (§8.5).

Namespaces are validated rather than trusted: legal to write in a template name, never `core` (the
framework's) or `dev` (the debug overlay's), never already claimed. Template namespaces and static
URL prefixes are checked against separate claim maps, because they are different address spaces that
happen to be spelled alike; sharing one made the ordinary case, a module mounting both templates and
assets under its own name, look like a collision. That was a real startup failure, and a test pins it
now.

### 9.2 Resolution and order

Resolution is four steps: filter by scope, apply the `<PREFIX>_MODULES` whitelist, validate
dependencies, topologically sort with alphabetical tie-breaking.

A scope is a deployment fact, not a permission. It answers "is this process the kind that serves this
feature?" for an application shipping one binary in several roles, which is otherwise expressed by
build tags nobody outside the release engineer can reproduce; who may use the feature remains the
permissions' business. Matching is case-insensitive and an unset scope loads everything, which is the
right default for the single-role deployment most applications are. A whitelist entry naming nothing
registered is an error listing what is, because the failure mode of ignoring it is a deployment
quietly missing a feature; a whitelist entry naming a module that exists but is out of scope is not
an error, because that is one variable shared correctly across roles. Required modules survive any
whitelist.

Dependency validation reports three distinct causes, not registered, not in scope, and not enabled,
because "add it to `APP_MODULES`" is wrong advice for the first two. A required module may depend
only on required modules, checked in both directions at registration so whichever initialises first
still catches it, rather than at deploy time by an operator who cannot tell which of two modules is
the mistake.

The sort is Kahn's algorithm with the candidate set re-sorted alphabetically each round, which makes
the result a function of the graph rather than of Go's map iteration order. Determinism is not
cosmetic: seed rows written during migration and the order navigation appears in are both derived
from it, and a site whose menu shuffles between boots looks broken whether or not anything is.

Startup then makes three passes over the plan. The first instantiates each module and immediately
syncs its models and runs its migration before the next module starts. The second mounts template and
asset namespaces and collects navigation. The third registers routes, last, so a module that resolves
a collaborator while wiring a route finds every collaborator constructed. Interleaving instantiation
with schema, rather than all factories and then all migrations, is what lets a module's migration
read a table its dependency seeded. A failure in any pass aborts construction with the database
closed: half a module set means navigation pointing at routes that were never registered, and that is
a failure an operator learns about from a complaint rather than from a startup log.

Routes are registered into a router group, which is what gives a module's middleware a scope it
cannot leak out of. Applied to the shared router, a module's `Use` would attach its handler to every
route registered after it, by another module, in an order no author can read from their own file. A
duplicate path panics inside the router; that panic is converted into a construction error for the
same reason a factory panic is.

### 9.3 Collaboration

A factory receives a `*Scope`: `Name`, `App`, `DB`, `Config`, `Log` (with the module name already
stamped on every record), `Deps`, `Get(name)`, and `MustDependency(name)`.

Modules do not import each other in order to call each other. A module that needs a collaborator
declares an interface containing the methods it uses and asserts the value from the dependency map:

```go
type indexer interface{ Index(ctx context.Context, doc Doc) error }

if found, ok := s.Get("search"); ok {
    if ix, ok := found.(indexer); ok {
        svc.indexer = ix
    }
}
```

The dependency arrow then points from the consumer, which keeps the graph acyclic by construction and
lets a module degrade when an optional collaborator is absent rather than refuse to start.
`MustDependency` is for the cases where degrading is the wrong answer; its error names the missing
module, so the factory's failure says what to fix.

For an application that wants to call a module rather than only route to it, `App.Module(name)`
returns the value a factory produced and `App.LoadedModules()` the order they loaded in, while
`App.Permissions()` and `App.ModuleOwner(perm)` answer what the running application can be asked to
allow and which module owns each grant. The interface asserted on the result is the application's to
declare, which is the same rule a module follows with its own collaborators.

### 9.4 What aborts startup

| Condition | Why it cannot be allowed to start |
|---|---|
| Factory returns `nil, nil`, or a typed-nil pointer, map, slice, func, or channel | A typed-nil satisfies every interface, registers, wires up, and panics on first use three layers from the cause |
| Factory returns an error, or panics | A panic is reported as an error naming the module; neither becomes a half-built application |
| Dependency missing, disabled, or excluded by scope | The dependent module would serve requests it cannot answer |
| Whitelist names an unregistered module | A typo in an enable list otherwise reads as success |
| Namespace collision, or `core` / `dev` | Two sources for one template name resolves differently per deployment |
| Two modules own the same permission | The grant means different things depending on load order |
| Required module depends on optional | Required and broken at the same time |
| Dependency cycle | Reported with the set of modules stuck in it, not by the sort hanging |

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
TASKS_SCOPE           deployment role this process serves  unset (all roles)
TASKS_MODULES         enabled optional modules        all
TASKS_THEME           templates the framework renders builtin
AUTHWARE_SESSION_KEY  session signing key             required in production
AUTHWARE_SESSION_LIFETIME session lifetime            1h
```

The two `AUTHWARE_` variables are deliberately outside the application prefix and are the same
for every application on a host, because they are the credential the session cookie format is
defined against: one key in the environment is much better than two that somebody has to keep
equal, and a prefix on a signing key would make a rolling deploy invalidate sessions that the new
process can still read. `AUTHWARE_SESSION_KEY` is the root authority for identity — anyone holding
it can mint a session for any identity with any groups — so a production boot that has to
manufacture one fails.

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
| Error templates referenced but never written | the theme owns them; startup compiles the required set |
| No request logging or correlation IDs | `RequestID` plus a slog request logger |
| No `http.Server` timeouts | `WithTimeouts` with non-zero defaults |
| SQLite without WAL, busy timeout, or foreign keys | DSN pragmas applied unconditionally |
| Authorization checks copy-pasted per handler, one of them missing | guards applied at route registration, `RequireOwnership`, identity read only from the context |
| Session and CSRF signing keys the same bytes, so a signature obtainable for one purpose serves the other | CSRF key derived from the session key by HMAC with a domain-separating label, never the key itself |
| Logout reachable by `GET`, triggerable by an `<img>` tag | logout and login handlers are POST-only, answering 405 with `Allow` |
| Login failures that disclose whether an account exists, by message or by timing | one sentinel error on every failure path, identical response body, a real bcrypt round for unknown accounts |
| A session identity taken from a body or query field, so "whose row is this" is whoever posted said | identity and profile ID come from the request context; `RequireOwnership` reads only there |
| Global filter registries mutated from constructors, racing at boot | framework-owned pure filters registered in `init()` |
| Module factories swallowing errors into typed-nil interfaces | `Factory` must return an error; a nil or typed-nil value aborts startup (§9.4) |
| `os.Exit` called from constructors; dropped context cancel functions | errors returned; `Run()` owns the lifecycle |
| Relative `extends` paths coupling templates to directory depth | namespace-aware loader |
| ID parsing that silently yields zero | typed parameter helpers that report parse errors |
| Client-controlled `X-Forwarded-For` treated as the source address | trust from the connection unless a proxy is configured; configured networks are a gate |
| Unbounded request body buffered into memory | `WithMaxBodyBytes` on state-changing methods, reported as 413 |
| Middleware silently absent on an application with no routes | chain wraps the router; a test asserts the headers on an empty app's 404 |
| `405` with no `Allow`, leaving the client to guess | allowed methods computed from the route table, `HEAD` implied by `GET` |

## 13. Testing

`webapptest.NewApp(t, opts...)` returns an application with a temp-directory SQLite database (or
PostgreSQL when `WEBAPPTEST_PG_DSN` is set, which is how foreign-key ordering and dialect
differences get caught), migrated templates, and an `http.Client` wired directly to the handler —
no listener, no ports, no sleeps. Cookies are kept across requests and redirects are not followed,
so a 303 stays under test instead of silently becoming a test of the page it landed on.

Authentication is opt-in: `InstallAuth` builds a service over the application's own database with a
test session key, and `Account` and `SignIn` produce a working account and a real session cookie,
so an authorization test exercises the same middleware a browser does.

`testas.As(req, "alice", "admins")` stamps an identity into a request context instead, for a handler
or a template tested on its own. It is not a session and does not satisfy a route guard, which is
the distinction the two helpers exist to draw: an identity that passes a guard because a test helper
imported the right package is a test that cannot fail.

The framework's own test suite asserts the security-relevant invariants explicitly: 401 versus 403
on denials, JSON versus HTML negotiation, CSRF rejection of unsafe methods, a token that survives a
real cookie round trip, login failures that are byte-for-byte identical, escaped rendering of error
payloads (a `<script>` in an error must never round-trip), migration idempotency across two boots of
the same database, and the required-template compile check.

The example application in `examples/tasks` is the integration test: a module contributing routes,
templates, a stylesheet, a table, a migration, permissions, and navigation, wrapped in an
application that owns sign-in, the root redirect, and a configuration-only bootstrap account. Its
suite drives the real handler chain with the real session codec, CSRF check, and database, which
pins what a mocked test cannot — a module's guarded routes working despite authentication being
installable only after construction; another account's row answering 404 rather than 403, with one
override confined to the function that loads the row; a refused submission re-rendered at 422 with
what was typed still in it; a disabled module stopping route, asset, and permission serving while
the application's own front door keeps working; and either database driver from environment alone.

## 14. Milestones

**M1 — spine.** `db` with driver selection, pragmas, pooling, AutoMigrate, versioned migrations,
and helpers. `webapp.New` with config, options, the middleware stack, the render funnel, typed
input handling, pagination, and `Run`. Bare `View` over an embedded FS.

**M2 — auth.** Service over the database with a pluggable mechanism chain, hardened stateless
sessions, session key policy, CSRF bound to the identity, permissions and guards, denial
negotiation delegated to the core, profile middleware, database-backed accounts and API tokens,
login and logout handlers.

**M3 — templating.** Namespaced loader, core UI, startup compile check, debug mode, filters,
`Share`, navigation.

**M4 — modules and polish.** Registry, scopes, capability interfaces, static asset namespacing,
`NewTestApp`/`testas`, example application, package documentation. Built: the registry
(`Register`/`WithModule`), scope selection, the four capability interfaces, per-module template and
asset namespaces with `ETag`s (§9), `webapptest`/`testas`, the example application in
`examples/tasks`, and the package documentation pass.

M4 was originally "forms": binding, validation, re-render helpers, typed parameters, pagination.
Typed parameters, pagination, and the re-render contract turned out to belong to the request spine
rather than to a separate layer — they are how a handler learns what it was asked for, and a
render funnel that cannot re-render with values cannot report a refusal. Splitting them out would
have produced a package whose only job was to be imported by the thing that already does the work.
What the original M4 became is the registry, namespacing, test helpers, and the example, all of
which are built.

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

- ~~Whether themes are a thin overlay over vendored CSS or a rewrite.~~ A rewrite. A theme owns the
  markup, and the vendored CSS only provides it: Bulma wants `.field > .label + .control`, the
  default theme's login form writes `<label class="field">`, and no stylesheet reconciles the two.
  What the framework guarantees about those pages is behaviour, not class names.

- ~~`HandlerErr` adds a second handler shape.~~ Built, and both shapes exist with `HandlerErr`
  recommended. The cost turned out to be lower than expected: `WrapErr` converts in one direction,
  so an application never has to choose in order to interoperate, and the framework's own error
  path is only reachable through the returning shape.
- ~~Whether to build a struct-tag form binder.~~ No. See §10: the argument was legibility of
  authorization-relevant fields, not dependency cost, and it holds. The helpers that replaced it
  are in the core and cover path parameters, query values, dates, paging, and form fields.
- ~~How a database-backed backend gets a handle through an argument-less mechanism factory.~~ It
  does not use a factory: the service is constructed with the handle, and the mechanism chain
  replaced the registry indirection that had the problem. See §7.6.

