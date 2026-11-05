/*
Package ui holds the templates the framework itself renders: the base layout,
the login form, the navigation fragment, and the error pages.

It exists as its own package for one reason — embedding. A template has to live
in the directory some package embeds, and putting these files in the application
core would mean the core embeds a directory of presentation, which is the kind
of layering that makes a framework's own error design impossible to replace.
Here the templates are the package, and the core mounts them the same way it
mounts an application's, under the namespace "core".

Nothing in this package renders anything. The core mounts FS() into its template
registry and compiles Required at startup, so a missing or broken core template
is a startup failure rather than a surprise at the moment something goes wrong.
*/
package ui

import (
	"embed"
	"io/fs"
)

// Namespace is the namespace the core's templates are mounted under. A core
// template refers to its own files through it — "@core/errors/internal.p2" —
// so that a template an application mounts can never be the one an error
// response renders.
const Namespace = "core"

//go:embed p2
var embedded embed.FS

//go:embed static
var embeddedStatic embed.FS

// FS returns the core templates with the p2 directory as its root, which is
// what a template registry wants: names like "base.p2" rather than
// "p2/base.p2".
func FS() fs.FS {
	sub, err := fs.Sub(embedded, "p2")
	if err != nil {
		// Unreachable: the embed directive above references the directory
		// literally, so a build in which "p2" is absent does not compile.
		panic("ui: embedded template directory is missing: " + err.Error())
	}
	return sub
}

// Static returns the framework's static assets rooted at "static", which is
// what the core serves under /static/core/.
func Static() fs.FS {
	sub, err := fs.Sub(embeddedStatic, "static")
	if err != nil {
		panic("ui: embedded static directory is missing: " + err.Error())
	}
	return sub
}

// Required are the templates that must compile for the framework to serve
// anything useful. The core checks them at startup.
//
// They are listed namespace-explicit on purpose. "errors/internal.p2" would
// also resolve, and would resolve to whichever mounted root happened to be
// registered first — so an application that shipped its own copy of an error
// page would find the framework's compile check passing while the framework
// still could not render its own failures.
var Required = []string{
	"@core/base.p2",
	"@core/login.p2",
	"@core/fragments/nav.p2",
	"@core/errors/notfound.p2",
	"@core/errors/unauthorized.p2",
	"@core/errors/internal.p2",
}
