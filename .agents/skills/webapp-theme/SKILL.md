---
name: webapp-theme
description: Use when authoring or changing a webapp theme, when a theme's templates or stylesheets need to work with the framework, or when a theme's own test suite needs themetest.Check.
---

# Author or change a webapp theme

A theme is the set of pages the framework renders itself: the base layout, the login
form, the navigation fragment, and the three error pages. `github.com/the-maldridge/
webapp-bulma` is a real one in its own module; read it before inventing a structure.
`DESIGN.md` §5.2 explains every check construction runs.

## Procedure

1. Create a package whose `init` calls `webapp.RegisterTheme(webapp.ThemeInfo{...})`
   with `Name`, `FS`, `Static`, and `Required: webapp.RequiredTemplates()`. Templates
   and assets are `embed.FS` behind the `FS`/`Static` closures.
2. Use the six required template names, namespace-qualified, exactly as
   `webapp.RequiredTemplates()` lists them. A bare name resolves to whichever mounted
   root has it first, which is how a theme can pass a compile check while the
   framework still cannot render its own failures.
3. Declare every block the framework and application pages fill. On the base layout:
   `title`, `head`, `bodyclass`, `header`, `content`, `footer`. On the error skeleton:
   `detail`. Pongo2 drops a filled block whose parent never declared it, with no error
   at parse or render time, so a skeleton missing `detail` renders three error pages
   with the reason for the failure removed.
4. Link the stylesheet so it resolves under `/static/core`, and commit the file.
   Construction reads the layout and refuses a theme whose links do not resolve, and
   an application importing the theme cannot produce what the theme did not ship,
   because `go generate` runs only in the module that owns the package.
5. Prefix any template filter name the theme registers. Pongo2's filter registry is
   one process-global map, so an unprefixed name can collide with one another package
   claimed first and the loser finds out at startup.
6. Do not lose anything in the login form: the CSRF field, the password never
   repopulated, and the single failure message that does not reveal which half of the
   credentials was wrong.

## Verify

Call `themetest.Check(t, name, tree, static, required)` from the theme's own test
package. It runs every rule construction applies, with no application built. It is
the only honest check a theme has: the framework cannot see a theme until it is
imported, and by then startup has already decided.

Then check by eye that an application page extending `@core/base.p2` picks the theme
up without editing anything, since that is the contract the namespace exists to make
work.
