# Instructions for coding agents

This repository is **webapp**, a batteries-included server-rendered web framework
in Go: `chi` for routing, `pongo2` for templates, `gorm` for the database.
Applications import it; nothing here is a runnable service.

Before editing anything nontrivial, read `README.md`, then the package doc
(`go doc github.com/the-maldridge/webapp`). `DESIGN.md` is the *why* document,
twelve hundred lines of it: use the index below to jump into it, do not read it
end to end.

## Commands

```bash
gofmt -l .                              # must print nothing
go vet ./...                            # root module only
make vet                                # BOTH modules: root and examples
CGO_ENABLED=1 go test -count=1 ./...    # full suite
make all                                # fmt, vet, build, test
make tidy                               # per-module go mod tidy
```

`CGO_ENABLED=1` is required: the tests open real SQLite databases in
`TempDir`. A run without it fails in a way unrelated to your change.

**Never call a change green after testing one module.** `examples/` is a separate
Go module (`github.com/the-maldridge/webapp/examples`) and only `make vet`,
`make build`, and `make test` reach it. It exists as its own module because it
depends on a theme that depends on the framework; inlining it would close a cycle.

## Module graph

- `github.com/the-maldridge/webapp` — this repository: `webapp`, `db`, `auth`,
  `tmpl`, `themetest`, `webapptest`, `testas`, `theme/builtin`, `internal`.
- `github.com/the-maldridge/webapp/examples` — the example application, own module.
- `github.com/the-maldridge/webapp-bulma` — the Bulma theme, **a different repo**.
  Its templates and vendored CSS are not findable from this checkout.
- `github.com/the-maldridge/webapp-pico` — the Picocss theme, **a different repo**
  as well. Same rule: nothing in it is reachable from this checkout by name.
- `github.com/the-maldridge/authware` — the authentication contract: `Authenticator`,
  `TokenAuthenticator`, `User`, and the backends (`htpasswd`, `ldap`, `pam`,
  `apitoken`, `netauth`). `auth.Mechanism` and `auth.TokenMechanism` are aliases
  of authware's interfaces, so a backend drops in with no adapter.

Dependency direction is inward: `webapp` imports `db`/`auth`/`tmpl`/`theme/builtin`;
none of those import `webapp`. `webapptest`, `testas`, and third-party themes are
leaves. `go.work` is gitignored on purpose; without it the theme resolves by
published version.

## Task index

| Task | Start here |
| --- | --- |
| Add a page or handler | `go doc github.com/the-maldridge/webapp.App.View`, then DESIGN §4, §8.1, §10 |
| Guard a route | `go doc github.com/the-maldridge/webapp.Router.Guard`, then DESIGN §7.3, §7.4 |
| Add a module | DESIGN §9 and `go doc github.com/the-maldridge/webapp.ModuleInfo` |
| Add an auth mechanism | `go doc github.com/the-maldridge/webapp/auth.WithMechanism` and `...auth.Service.TokenMechanisms`, then DESIGN §7.6 |
| Query or migrate | `go doc github.com/the-maldridge/webapp/db`, then DESIGN §6 |
| Write a test | `go doc github.com/the-maldridge/webapp/webapptest` and `.../testas`, then DESIGN §13 |
| Author or change a theme | DESIGN §5.2 and `go doc github.com/the-maldridge/webapp/themetest` |
| See a call shape the build checks | `<package>/example_test.go` (`go doc` does not render examples) |
| Understand a config key | `go doc github.com/the-maldridge/webapp.Config`, then DESIGN §11 |
| Work out why startup failed | DESIGN §12, §9.4 |

`examples/tasks/` is a complete application and its tests are the integration
suite the framework does not otherwise have. When a question is "how does this fit
together in real code", read it before inventing an answer.

## Invariants that change behavior

These are enforced in code and are easy to break by accident.

Identity normalization: every identity is `TrimSpace` then `ToLower`, including
the identity an authware mechanism asserts. Comparisons downstream assume it.

Guards decide from `AuthInfo`, resolved session → bearer → basic. Stamping a user
with `auth.ContextWithUser` by hand still yields 401, because nothing verified it.
Same reason `testas.As` cannot satisfy a guard, and that is deliberate.

The password chain and the token chain are independent. `WithOnlyMechanisms`
drops local passwords only; bearer tokens still resolve against the database, and
`WithOnlyTokenMechanisms` does not touch the password chain. Orthogonality is the
feature (directory passwords plus revocable database tokens).

Mechanism errors are split: an error answering "is this credential correct"
(`ErrUnknownAccount`, `ErrBadCredential`, `ErrAccountDisabled`, authware's
`ErrUnauthenticated`/`ErrDoesNotExist`) is skipped silently. Anything else, e.g. a
dead LDAP directory, logs a warning. See `expectedRejection` in `auth/service.go`.

Sessions carry groups, so group changes take effect one session lifetime later.
`Service.HasPerm` is the store-backed alternative when that lag is wrong.

`AUTHWARE_SESSION_KEY` is root authority. Production refuses to boot without it.
The session cookie is named from the environment prefix (`session` with the default
prefix), is `Secure` unless debug is on, and is `SameSite=Lax`.

CSRF is skipped only for a request carrying a *verified* `Authorization` header.
CSRF failure is 403, missing identity is 401. Login and logout handlers are
POST-only, fail with one indistinguishable message, and `?next=` must pass through
`auth.SafeNext`.

Models must be registered before the database opens: `WithComponent`,
`RegisterComponent`, or `auth.Service.Models()`. `App.InstallAuth` runs after
`New` and before routes; guards resolve the service per request, which is why that
ordering is not a bug.

## Don'ts

- Don't hand-edit generated or vendored theme assets; run `make generate`.
- Don't add a new mechanism interface shaped like authware's but named
  differently. That divergence is what the aliases removed; don't reintroduce it.
- Don't push to a remote or cut a tag without being asked.
- Don't read all of `DESIGN.md` to answer an API question: `go doc` is generated
  from the source and cannot be stale.

## Verification expectations

An API claim in a doc comment must compile. Prose in a doc comment is the only kind
of documentation that can quietly become false; the `Example` functions in each
package's `example_test.go` are the form that cannot, because `go test` runs the ones
with an `// Output:` comment and compiles the rest. `go doc` does not render
examples, so open those files to see a call shape the build has checked. Change a
public signature and the example that shows it belongs in the same commit.

For authware interop specifically, verify by compiling out of tree against the
published backend rather than trusting any prose, including this file.
