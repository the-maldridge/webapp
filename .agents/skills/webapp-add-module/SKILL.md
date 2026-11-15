---
name: webapp-add-module
description: Use when adding a feature area to a webapp application as a module, when making an existing package toggleable by configuration, or when a module needs routes, templates, assets, navigation, models, or migrations.
---

# Add a module to a webapp application

A module is a feature area that configuration can turn off without editing the
binary. `go doc github.com/the-maldridge/webapp.ModuleInfo` is the contract;
`DESIGN.md` §9 is the reasoning. `examples/tasks` has working ones.

## Procedure

1. Decide the module's name. It is what `<PREFIX>_MODULES` lists, what the loader
   reports, and what a dependency refers to, so pick it once.
2. Declare the permissions the module owns in `ModuleInfo.Permissions`, using
   `auth.P(module, grant)`. This is what lets an administration screen offer only
   the grants that do something in the running deployment.
3. Write the `Factory func(*webapp.Scope) (any, error)`. Dependencies come from the
   scope: `s.DB()`, `s.Config()`, `s.MustDependency("name")`. Return one value; the
   framework decides what it contributes by which interfaces it implements.
4. Contribute only through the fixed capability list. Which ones you implement is
   the whole API surface: `RouteContributor`, `Templater`, `Staticer`, `Navigator`,
   `Component`, `Migrator`. Anything else belongs behind a constructor argument, not
   in a new hook.
5. Register it. `webapp.Register` from the package's `init` means "this binary
   contains the feature"; `webapp.WithModule` assembles it explicitly. Both take the
   same `ModuleInfo` and are validated the same way.
6. Declare `Scopes` and, if it needs another module loaded first, `Dependencies`.
   Load order is dependencies first, ties alphabetical, because modules seed rows and
   contribute navigation.
7. Set `Required: true` if turning it off would leave the application incoherent.

## Gotchas

- A guarded route works even though the auth service is installed after `New`:
  guards resolve the service per request. Do not add an initialization dance.
- Assets a module serves come from `Staticer` and appear at `/static/<namespace>/`.
  Templates come from `Templater` under the module's own namespace; write
  `@<ns>/<file>.p2` to name one unambiguously.
- Naming a module in `<PREFIX>_MODULES` that is not registered is a startup error,
  not a silent skip.

## Verify

`make test` (both modules), then boot with the module excluded from
`<PREFIX>_MODULES` and confirm the application still starts and its navigation has
lost the entry. A module that cannot actually be switched off has not earned the
name.
