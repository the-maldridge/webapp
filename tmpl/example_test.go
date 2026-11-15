// Examples for the namespaced template loader. The resolver is pure string work
// over a mounted tree, so its examples run against an in-memory filesystem and
// assert real output.
//
// A canonical template key is "@namespace/path": the namespace marker leads, so
// a name carrying no marker is a bare path that has to be searched for.
package tmpl_test

import (
	"fmt"
	"io"
	"log"
	"testing/fstest"

	"github.com/the-maldridge/webapp/tmpl"
)

// A theme and a module both mount a list.p2, which is the case the namespace and
// the mount order exist to make unambiguous.
func newRegistry() *tmpl.Registry {
	reg := tmpl.New()
	if err := reg.Mount("core", fstest.MapFS{
		"base.p2":          &fstest.MapFile{Data: []byte("base")},
		"list.p2":          &fstest.MapFile{Data: []byte("core list")},
		"partials/head.p2": &fstest.MapFile{Data: []byte("head")},
	}); err != nil {
		panic(err)
	}
	if err := reg.Mount("tasks", fstest.MapFS{
		"list.p2":         &fstest.MapFile{Data: []byte("tasks list")},
		"detail.p2":       &fstest.MapFile{Data: []byte("detail")},
		"partials/row.p2": &fstest.MapFile{Data: []byte("row")},
	}); err != nil {
		panic(err)
	}
	return reg
}

func ExampleCanonical() {
	fmt.Println(tmpl.Canonical("core", "base.p2"))
	fmt.Println(tmpl.Canonical("core", "/base.p2"))

	// Output:
	// @core/base.p2
	// @core/base.p2
}

func ExampleSplitKey() {
	ns, rest, ok := tmpl.SplitKey("@tasks/detail.p2")
	fmt.Println(ns, rest, ok)

	// A bare name is not a key: it carries no namespace, and a caller has to
	// resolve it rather than assume one.
	ns, rest, ok = tmpl.SplitKey("detail.p2")
	fmt.Printf("%q %q %v\n", ns, rest, ok)

	// Output:
	// tasks detail.p2 true
	// "" "" false
}

func ExampleRegistry_Abs() {
	reg := newRegistry()

	// A bare name is searched for in mount order, so core wins the tie.
	fmt.Println(reg.Abs("", "list.p2"))

	// A qualified name says which root to look in, and a miss comes back
	// unchanged so the caller's error names what was asked for.
	fmt.Println(reg.Abs("", "@tasks/detail.p2"))
	fmt.Println(reg.Abs("", "@tasks/missing.p2"))

	// A relative name resolves against the referencing template first, inside
	// that template's own namespace: detail.p2 lives in tasks, so list.p2 means
	// the module's copy and not the core's.
	fmt.Println(reg.Abs("@tasks/detail.p2", "list.p2"))
	fmt.Println(reg.Abs("@tasks/partials/row.p2", "row.p2"))

	// A climb out of the mounted root is a miss rather than a read. The name
	// comes back with the climb intact and nothing resolves it, which is why
	// {% include "../secrets.p2" %} fails instead of escaping.
	fmt.Println(reg.Abs("@core/base.p2", "../secrets.p2"))

	// Output:
	// @core/list.p2
	// @tasks/detail.p2
	// @tasks/missing.p2
	// @tasks/list.p2
	// @tasks/partials/row.p2
	// ../secrets.p2
}

func ExampleRegistry_Lookup() {
	reg := newRegistry()

	key, ok := reg.Lookup("@core/base.p2", "partials/head.p2")
	fmt.Println(key, ok)

	key, ok = reg.Lookup("@core/base.p2", "list.p2")
	fmt.Println(key, ok)

	// Output:
	// @core/partials/head.p2 true
	// @core/list.p2 true
}

func ExampleRegistry_Namespaces() {
	reg := newRegistry()
	fmt.Println(reg.Namespaces())

	// MountFirst puts a root in front of the others, which is how an application
	// overrides one of a theme's templates without copying the theme: mount an
	// overlay root first and only the names it defines win.
	if err := reg.MountFirst("override", fstest.MapFS{
		"list.p2": &fstest.MapFile{Data: []byte("mine")},
	}); err != nil {
		panic(err)
	}
	fmt.Println(reg.Namespaces())
	fmt.Println(reg.Abs("", "list.p2"))

	// Output:
	// [core tasks]
	// [override core tasks]
	// @override/list.p2
}

func ExampleRegistry_Get() {
	reg := newRegistry()

	// Get takes a canonical key. An fs.FS root cannot be handed a path outside
	// itself, so nothing here reads a file that was never a template.
	f, err := reg.Get("@core/base.p2")
	if err != nil {
		log.Fatal(err)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s\n", b)

	// Output:
	// base
}

func ExampleCleanNamespace() {
	name, err := tmpl.CleanNamespace(" core ")
	fmt.Println(name, err)

	// A namespace holding the separator would make a canonical key ambiguous to
	// split, which is the one thing a resolver cannot survive.
	_, err = tmpl.CleanNamespace("core/sub")
	fmt.Println(err)

	// Output:
	// core <nil>
	// tmpl: namespace "core/sub" must not contain "@", a slash, or a backslash
}

// ExampleNewRenderer shows a renderer built over a mounted tree, compiled, and
// then asked for a page. It is compiled, not run.
func ExampleNewRenderer() {
	reg := tmpl.New()
	if err := reg.Mount("core", fstest.MapFS{
		"base.p2": &fstest.MapFile{Data: []byte(`<title>{{ SiteTitle }}</title>{% block content %}{% endblock %>`)},
	}); err != nil {
		log.Fatal(err)
	}

	// The globals are what every template sees without a handler passing them:
	// the site title, the current user, the CSRF field, and the permission check
	// a navigation fragment calls.
	r, err := tmpl.NewRenderer(reg,
		tmpl.WithGlobals("SiteTitle", "Tasks"),
		tmpl.WithGlobals("Can", tmpl.Perms(func(perm string) bool {
			return perm == "TASKS:VIEW"
		})),
	)
	if err != nil {
		log.Fatal(err)
	}

	// Verify compiles the pages the framework renders itself. Doing it at
	// startup is the point: an error page that does not compile is discovered at
	// the worst possible moment.
	if err := r.Verify("@core/base.p2"); err != nil {
		log.Fatal(err)
	}

	// Resolve answers "which copy am I actually rendering", which is otherwise a
	// question about mount order.
	key, ok := r.Resolve("base.p2")
	fmt.Println(key, ok)
}
