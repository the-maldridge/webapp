/*
Package bulma is an alternative theme: the framework's base layout, login form,
navigation fragment, and error pages written in Bulma's markup instead of the
default theme's.

It is a rewrite rather than a wrapper. Bulma's components are a contract about
structure, not about class names sprinkled on markup chosen elsewhere: a
`.field` expects a `.label` and a `.control` beneath it, and the default theme's
`<label class="field">` is not that, however much of Bulma is loaded. So this
package owns its templates outright and vendors only the stylesheet.

The stylesheet is fetched by `go generate` rather than committed, and verified
against the digests in bulma.lock; see DESIGN.md §8.5 for why the digests are
fatal rather than advisory. Importing this package registers the theme, after
which WithTheme("bulma") or <PREFIX>_THEME=bulma selects it.
*/
package bulma

import (
	"embed"
	"io/fs"
)

//go:generate go run ./internal/fetchbulma

//go:embed p2
var embedded embed.FS

//go:embed static
var embeddedStatic embed.FS

// FS returns the theme's templates with the p2 directory as its root.
func FS() fs.FS {
	sub, err := fs.Sub(embedded, "p2")
	if err != nil {
		// Unreachable: the embed directive above references the directory
		// literally, so a build in which "p2" is absent does not compile.
		panic("bulma: embedded template directory is missing: " + err.Error())
	}
	return sub
}

// Static returns the vendored Bulma bundle, served by the core under its static
// root so the base layout can link it the way any theme's base layout does.
func Static() fs.FS {
	sub, err := fs.Sub(embeddedStatic, "static")
	if err != nil {
		panic("bulma: embedded static directory is missing: " + err.Error())
	}
	return sub
}

// Required are the templates the framework renders itself, which this theme
// therefore has to supply in full.
//
// They are identical in name to the default theme's, and that is the contract: a
// theme replaces the framework's pages rather than adding optional ones, so the
// set is fixed by the framework's rendering code and not by the theme's taste.
var Required = []string{
	"@core/base.p2",
	"@core/login.p2",
	"@core/fragments/nav.p2",
	"@core/errors/notfound.p2",
	"@core/errors/unauthorized.p2",
	"@core/errors/internal.p2",
}
