---
name: webapp-port-app
description: Use when moving an existing Go application onto the webapp framework, or when converting handlers, routing, configuration, or authentication from net/http or authware to webapp.
---

# Port an application onto webapp

Read `MIGRATING.md` first: it has both migrations (from `net/http`, from
`authware`) written out, including the cookie-name trap that logs every user out
once. Then `AGENTS.md` for the invariants. Do not infer the API from `DESIGN.md`.

## Procedure

1. Decide the environment prefix. It names the config keys and the session cookie,
   and changing it later forces a re-login for everyone.
2. Replace server construction with `webapp.New(...)`, then `defer a.Close()`.
   Nothing in the framework exits the process; `main` keeps that decision.
3. Move models into options (`WithComponent`, `db.WithAutoMigrate`). They must be
   known before the database opens, so registering them after `New` is too late.
4. Install authentication after construction and before routes:
   `auth.New(a.DB, ...)` then `a.InstallAuth(svc)`.
5. Convert handlers route by route. Prefer the error-returning forms (`GetErr`,
   `PostErr`, `MethodErr`); a returned error reaches the one responder that sets a
   real status and logs a correlation ID.
6. Replace hand-written error blocks with `a.Fail` and, for refused input,
   `*webapp.Rejected` so the form comes back filled in.
7. Replace discarded parse calls (`chi.URLParam` + `strconv`, `r.FormValue`) with
   the typed helpers in `go doc github.com/the-maldridge/webapp`, and handle the
   error they return.
8. Render through `a.View` so the theme's layout, the current user, and the CSRF
   field arrive without each handler remembering them.

## Verify

Run `make vet` and `make test`, not just the root module: `examples/` is a separate
module. Then check the things a compile cannot catch: that a guarded route still
returns 401 anonymous and 403 forbidden, that a POST without a CSRF token is
refused, and that a login failure does not reveal which half of the credentials was
wrong. `webapptest` drives all of that through the real middleware.

`go doc github.com/the-maldridge/webapp/webapptest` is the harness; `testas` is for
handlers tested alone and deliberately cannot satisfy a guard.
