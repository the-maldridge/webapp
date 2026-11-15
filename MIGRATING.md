# Migrating to webapp

Two separate migrations live here. Pick the one that describes where you are
coming from; they do not overlap.

- [From `net/http`](#from-nethttp) — you have a hand-built server and want the
  framework around it.
- [From `authware`](#from-authware) — you already use
  `github.com/the-maldridge/authware` for authentication and are adopting this
  framework's `auth` package on top of it.

## From `net/http`

The framework does not replace your handlers. It replaces the parts around them
that each application re-implements differently: configuration, the database
handle, the middleware order, the error page, and the way a bad request becomes a
response.

### Construct an App instead of a server

`http.ListenAndServe` and its argument-laden friends go away. `webapp.New` reads
configuration from the environment under a prefix, opens the database, builds a
fixed middleware stack, and returns an `*App` you register routes on:

```go
func main() {
    if err := run(); err != nil {
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }
}

func run() error {
    a, err := webapp.New(
        webapp.WithEnvPrefix("TASKS"),
        webapp.WithDBOptions(db.WithAutoMigrate(Task{})),
    )
    if err != nil {
        return err
    }
    defer a.Close()

    a.Route("/ui/task", func(r webapp.Router) {
        r.GetErr("/", handleList)
        r.PostErr("/new", handleCreate)
    })
    return a.Run()
}
```

Two ordering rules are worth internalizing, both because the database handle does
not exist until `New` has opened it:

1. Models have to be known *before* the database opens, so they are passed in as
   options (`WithComponent`, or `db.WithAutoMigrate`) rather than registered after.
2. Authentication is installed *after* `New` returns, because a service needs that
   handle: `a.InstallAuth(svc)`, then continue registering routes. Guards resolve
   the service per request, so a module's `r.Guard(...)` works without knowing when
   the install happened.

Nothing in construction calls `os.Exit` or panics. A bad configuration is an error
returned to `main`, which is the only place in the process allowed to decide what
to exit with.

### Return your errors

Handlers can stay `http.HandlerFunc`. The preferred form is `HandlerErr`, which
returns an error:

```go
func (a *app) handleList(w http.ResponseWriter, r *http.Request) error {
    id, err := webapp.ParamUint(r, "id")
    if err != nil {
        return err // becomes 400, logged with a correlation ID
    }
    a.View(w, r, "@tasks/detail.p2", webapp.Context{"ID": id})
    return nil
}
```

`View`, `Fail`, and `SeeOther` write the response and return nothing; whether one of
them was the last thing a handler did is recorded for the log line.

Route them with `GetErr`, `PostErr`, `MethodErr`, `HandleErr`. A returned error
reaches one responder that sets an accurate status code, logs at error level with
the request's correlation ID, renders detail only in debug, and returns a generic
page plus an ID in production. The hand-rolled `if err != nil { http.Error(w,
err.Error(), 500) }` blocks are what this replaces: they leak internals to users
and log nothing.

Two errors are load-bearing rather than fatal:

- `webapp.ErrNotFound` renders the 404 page.
- A `*webapp.Rejected` is the user's input being refused, not the server failing.
  Return it with the template, the submitted values, and the reasons, and the form
  is re-rendered with what the person typed still in it, logged at info level.

### Render through the framework

`a.View(w, r, name, ctx)` resolves a namespaced template, injects the current user
and the CSRF field, wraps the page in the active theme's layout, and turns a render
failure into a real error. Template names are namespaced: `@core/base.p2` is the
framework's layout, `@tasks/list.p2` is yours.

### Stop discarding parse errors

The helpers in `webapp` return an error for every value they parse: `ParamUint`,
`QueryInt`, `QueryBool`, `QueryDate`, `Page`, `FormString`, `FormInt`, `FormBool`,
`FormDate`, `FormValues`. The reason they are not convenience wrappers returning a
bare zero is that the zero is indistinguishable from an input that legitimately was
zero, and a handler that silently accepts `?page=abc` as page 1 reports no problem
forever.

## From `authware`

This is the easier migration, because this package is built on authware's
contracts rather than a competing one.

### What carries over unchanged

- `auth.User` **is** `authware.User`. No conversion, in either direction.
- `auth.Mechanism` **is** `authware.Authenticator`, and `auth.TokenMechanism`
  **is** `authware.TokenAuthenticator`. Every shipped backend (`htpasswd`, `ldap`,
  `pam`, `apitoken`, `netauth`) passes in with no adapter, and a mechanism you
  already wrote compiles as-is.
- The session payload is the same shape, the same serializer, and the same
  encryption scheme. A cookie minted by authware is accepted here.
- The environment variables keep their names: `AUTHWARE_SESSION_KEY` and
  `AUTHWARE_SESSION_LIFETIME`. Do not rename them in your deployment; this package
  reads those.
- A bearer token in an `Authorization` header resolves the same way.

**One thing to check before deploying: the session cookie name.** authware always
uses `session`. This framework derives the name from the application's environment
prefix, so a prefix of `TASKS` produces `tasks_session`, and every logged-in user
silently has to log in again. The name stays `session` when the prefix is unset or
is the default `WEBAPP`. If you want the prefix and the sessions, set the prefix for
everything else and pin the cookie:

```go
svc, err := auth.New(d, auth.WithSessionKey(os.Getenv("AUTHWARE_SESSION_KEY")))
// ...
a.InstallAuth(svc)
// InstallAuth applies its own policy, so re-assert the name afterwards.
svc.SetCookiePolicy(auth.CookiePolicy{Name: "session", Secure: true,
    SameSite: http.SameSiteLaxMode, Lifetime: svc.SessionLifetime()})
```

### Wiring is a constructor argument, not a registry name

authware builds a middleware from the environment:

```go
am, err := authware.NewAuth()          // reads AUTHWARE_BASIC_MECHS, AUTHWARE_TOKEN_MECHS
mux.Use(am.BasicHandler)
```

Here you build the mechanism and hand it to the service. `AUTHWARE_BASIC_MECHS` and
`AUTHWARE_TOKEN_MECHS` have **no equivalent**: the factory registry takes a
zero-argument constructor, which cannot be given the `*db.DB` an application's
mechanism usually needs, so the choice of mechanism moves into `main` where it is
visible:

```go
dir, err := htpasswd.New()              // an authware.Authenticator
if err != nil {
    return err
}

svc, err := auth.New(a.DB,
    auth.WithSessionKey(os.Getenv("AUTHWARE_SESSION_KEY")),
    auth.WithOnlyMechanisms(dir),        // replaces the database password chain
)
if err != nil {
    return err
}
if err := a.InstallAuth(svc); err != nil {
    return err
}
```

`auth.WithTokenMechanism` and `auth.WithOnlyTokenMechanisms` do the same for the
bearer-token chain. The two chains are independent on purpose: verifying passwords
against a directory while machine tokens stay revocable rows in your own database is
the normal configuration, not an inconsistency.

### Middleware mapping

| authware | webapp |
| --- | --- |
| `BasicHandler` | `a.InstallAuth(svc)`, which installs the stack below |
| `MultiAuthHandler` | `svc.Middleware()` (the default; tries session, bearer, basic) |
| `OptionalAuthHandler` | `svc.Optional()` — authenticate if a credential is present, never deny |
| `TokenAuthHandler` | The chain built by `auth.WithTokenMechanism` / `WithOnlyTokenMechanisms`, reached through `svc.Middleware()` |
| `LoginFormHandler` | `svc.LoginHandler(auth.LoginOptions{...})` |
| `LogoutHandler` | `svc.LogoutHandler(redirectTo)` |
| `authware.UserFromContext` | `auth.UserFrom(r)`, `auth.UserFromContext(ctx)`, `auth.MustIdentity(r)` |
| nothing | `svc.Require(perm)`, `svc.RequireLogin()`, `r.Guard(perm)` |

Per-route authorization is the significant addition. In authware a request that
authenticated was let through and each handler decided what it could do; here the
route declares the grant and the framework denies before the handler runs.

### Behavior that gets stricter

These are the places where a migration changes what a user or a log sees. None of
them need code on your side unless noted.

- **CSRF is now enforced** on unsafe methods. authware has none. Requests carrying a
  verified `Authorization` header are exempt; a browser form post needs the token,
  which `auth.CSRFInput(r)` renders and which templates get injected automatically.
  A CSRF failure is 403, distinct from an unauthenticated 401.
- **Login and logout handlers are POST-only.** authware's form handler answered any
  method.
- **`?next=` goes through `auth.SafeNext`.** It rejects absolute URLs, protocol-relative
  URLs, backslashes, and control characters, including the percent-decoded form of
  each. authware checked only that it started with a single `/`.
- **One failure message.** A wrong username and a wrong password are
  indistinguishable in the response. Account-disabled is not reported as such.
- **Cookies are hardened.** `Secure` unless debug is on, `SameSite=Lax`, and
  multiple cookies of the same name are rejected rather than first-one-wins.
- **Identity is normalized.** `TrimSpace` then `ToLower`. A mechanism that asserts
  `Alice` and a row stored as `alice` are the same user, consistently, on every path.
- **401 versus 403 is decided in one place.** No identity is 401; an identity without
  the grant is 403; a CSRF failure is 403. Each renders a real page.
- **Revocation lags one session.** Groups travel inside the cookie, so dropping
  someone from a group takes effect at their next login. `svc.HasPerm` is the
  store-backed check when that lag is wrong for a given route.

`CHANGELOG.md` lists the same items from the release-history point of view, and
`go doc github.com/the-maldridge/webapp/auth` is the authoritative signature list.
