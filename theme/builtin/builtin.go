/*
Package builtin is the framework's default theme: the base layout, the login
form, the navigation fragment, the three error pages, and the stylesheet they
link.

It is a theme rather than part of the core for one reason. A template has to
live in the directory some package embeds, and putting these files in the
application core would mean the core embeds a directory of presentation, which
is the kind of layering that makes a framework's own error design impossible to
replace. Here the templates are a theme package, mounted the same way any other
theme is, under the namespace the framework renders against.

Nothing in this package renders anything, and nothing in it knows it is the
default. The core mounts it when no other theme is selected, compiles Required
at startup, and never mounts a second theme alongside it, so a missing or broken
template is a startup failure rather than a surprise at the moment something
goes wrong.

The stylesheet is this theme's own business. Its custom properties are named
for the pages that use them and are not a contract any other theme is obliged
to provide; an application stylesheet that wants to survive a theme change has
to declare the properties it consumes.
*/
package builtin

import (
	"embed"
	"io/fs"
)

//go:embed p2
var embedded embed.FS

//go:embed static
var embeddedStatic embed.FS

// FS returns the theme's templates with the p2 directory as its root, which is
// what a template registry wants: names like "base.p2" rather than
// "p2/base.p2".
func FS() fs.FS {
	sub, err := fs.Sub(embedded, "p2")
	if err != nil {
		// Unreachable: the embed directive above references the directory
		// literally, so a build in which "p2" is absent does not compile.
		panic("builtin: embedded template directory is missing: " + err.Error())
	}
	return sub
}

// Static returns the theme's static assets rooted at "static", which is what
// the core serves under the framework's static root.
func Static() fs.FS {
	sub, err := fs.Sub(embeddedStatic, "static")
	if err != nil {
		panic("builtin: embedded static directory is missing: " + err.Error())
	}
	return sub
}

// Required are the templates that must compile for the framework to serve
// anything useful. The core checks them at startup.
//
// They are listed namespace-explicit on purpose. "errors/internal.p2" would
// also resolve, and would resolve to whichever mounted root happened to be
// registered first, so an application that shipped its own copy of an error page
// would find the compile check passing while the framework still could not
// render its own failures.
//
// The namespace is spelled as a literal rather than assembled from a
// constant because a theme must not import the core that mounts it. The core's
// own test asserts every theme's entries carry the prefix it expects, so the
// two cannot drift apart silently.
var Required = []string{
	"@core/base.p2",
	"@core/login.p2",
	"@core/fragments/nav.p2",
	"@core/errors/notfound.p2",
	"@core/errors/unauthorized.p2",
	"@core/errors/internal.p2",
}
