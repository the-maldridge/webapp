# tasks

A task list built on the `webapp` framework. It is the example application in
DESIGN.md §13: small enough to read in one sitting, and it uses every layer of
the framework the way a real application does rather than the way a snippet does.

## Run it

```sh
go run ./examples/tasks
```

Open <http://localhost:8080>.

The first boot has no accounts, so tell it who the first one is:

```sh
TASKS_BOOTSTRAP_IDENTITY=ada \
TASKS_BOOTSTRAP_PASSWORD=choose-something-long \
  go run ./examples/tasks
```

Sign in as `ada`. That account is given the `TASKS:VIEW`, `TASKS:EDIT`,
`TASKS:DELETE` and `TASKS:ADMIN` groups, so it sees everything the example does,
including the administrative totals page.

The bootstrap variables are optional and only ever additive: an existing account
keeps its password, and a restart does not revoke a group an administrator
removed.

## What is worth looking at

| File | The thing it demonstrates |
|---|---|
| `module.go` | One module contributing routes, templates, assets, schema, migrations, permissions and navigation |
| `model.go` | A model whose table is created by the module that owns it |
| `main.go` | Authentication installed *after* construction, and why that order is forced |
| `p2/list.p2` | Permission-gated controls, a pager, and the `can("…")` callable |
| `p2/form.p2` | A rejected submission re-rendered with what was typed still in it |
| `static/css/tasks.css` | A module's own asset namespace, served at `/static/tasks/` |
| `theme_test.go` | The same pages rendered by a different theme, unedited |

Three behaviours are worth trying by hand, because they are the three an
application usually gets wrong:

1. **Ownership.** Sign in as a second account with no `TASKS:ADMIN` group and try
   to reach `POST /ui/task/1/delete` for a task you did not create. The answer is
   404, not 403: a stranger is not told the row exists.
2. **Validation.** Submit a task with no title. The response is `422` and the page
   comes back with your notes still in the textarea.
3. **Permissions.** Remove a group from the account and reload. The links
   disappear and the routes refuse the request, from the same declaration.

## Configuration

Environment only, prefix `TASKS_`. Everything the framework reads is documented
in DESIGN.md §11; these are the ones this example adds or cares about.

| Variable | Meaning | Default |
|---|---|---|
| `TASKS_ADDR` | listen address | `:8080` |
| `TASKS_DEBUG` | verbose error pages, template reload | unset |
| `TASKS_PRODUCTION` | hardening; requires a session key | unset |
| `TASKS_DB_DRIVER` | `sqlite` or `postgres` | `sqlite` |
| `TASKS_DB` | SQLite file path | `tasks.db` |
| `TASKS_DB_DSN` | PostgreSQL DSN | unset |
| `TASKS_MODULES` | comma-separated modules to load | all linked |
| `TASKS_SCOPE` | deployment role this process serves | all roles |
| `TASKS_THEME` | theme the framework's own pages render with | `builtin` |
| `TASKS_TEMPLATE_DEBUG` | directory of templates that win over the embedded ones | unset |
| `TASKS_BOOTSTRAP_IDENTITY` | account to create and grant the task groups to | unset |
| `TASKS_BOOTSTRAP_PASSWORD` | password for that account, required with the identity | unset |
| `AUTHWARE_SESSION_KEY` | session signing key, 32+ bytes | required in production |
| `AUTHWARE_SESSION_LIFETIME` | session lifetime | `1h` |

### Switching theme

The example links the alternative theme, so its appearance is configuration:

```sh
make generate          # vendors the theme's stylesheet, once per clone
TASKS_THEME=bulma TASKS_BOOTSTRAP_IDENTITY=ada TASKS_BOOTSTRAP_PASSWORD=... \
  go run ./examples/tasks
```

No template in this directory mentions a theme. They all extend
`@core/base.p2`, which is the framework's own name for its base layout, and a
theme is mounted *as* that name rather than in front of it, so the layout, the
navbar, the login page and the error pages all change while `list.p2` does not
move. That is the whole point of the mechanism, and `theme_test.go` is the test
that says so.

The vendored stylesheet is generated rather than committed, which is why a fresh
clone needs `make generate` before it builds. `theme/bulma/bulma.lock` pins the
release and its digests; the generator refuses to write anything that does not
match them.

One consequence is worth stating plainly, because it is the one that bites:
`static/css/tasks.css` declares its own custom properties and its own button
modifiers. The framework publishes no CSS variables for an application to
borrow, because a variable some theme defined is a value that disappears when
the theme changes, and a stylesheet whose colours fall back to transparent looks
perfectly correct in the one theme it was written against.

### PostgreSQL

The application code is identical. Only the environment changes:

```sh
TASKS_DB_DSN='postgres://user:pass@localhost:5432/tasks?sslmode=disable' \
  go run ./examples/tasks
```

Both drivers are worth trying at least once, because they disagree about the one
thing that surprises people: PostgreSQL enforces foreign keys when a table is
created, SQLite does not enforce them at all.

### Narrowing what loads

`TASKS_MODULES` is a whitelist of module names, and it is the operator's half of
the registry: the same binary, some features off.

Two of its rules are worth knowing before relying on it, and both are covered by
tests in `app_test.go`:

- An empty value means "no whitelist", exactly like not setting it. A variable
  that defaults to unset and one deliberately emptied cannot be told apart in a
  deployment, and reading the second as "load nothing" would take a feature out
  of production because somebody wrote `MODULES=` into a unit file. So this
  example, which links one module, cannot be emptied into a login-only
  application; an application that links several narrows them by naming the ones
  that stay.
- A name that matches nothing registered stops the process rather than being
  ignored. The failure mode of ignoring it is a deployment quietly missing a
  feature, which is discovered from a complaint rather than from a startup log.

What a disabled module loses: its routes 404, its navigation items are gone, its
templates and assets stop being mounted, its permissions stop being offered, and
its table stops being created on a fresh database. Nothing else changes, because
nothing outside the module referenced it. The application's own front door at
`/login` keeps working, since the application owns it rather than the module.

### Edit templates without rebuilding

```sh
TASKS_DEBUG=1 TASKS_TEMPLATE_DEBUG=./examples/tasks/p2 go run ./examples/tasks
```

Names in that directory win over the embedded copies and are re-read on every
request. Anything the directory does not contain still comes from the binary.
The option is refused when `TASKS_PRODUCTION` is set: a writable directory ahead
of the templates inside a deployed binary is a code-execution surface.

## Tests

```sh
CGO_ENABLED=1 go test ./examples/tasks
```

The suite drives the real handler chain with the real session codec and the real
CSRF check, through `webapptest`, so it covers the ordering that unit tests of
handlers cannot: a route registered by a module during construction, guarded by a
permission from a service installed after it.

Set `WEBAPPTEST_PG_DSN` to run the same suite against PostgreSQL.
