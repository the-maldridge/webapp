// Package themetest checks the contract every theme has to satisfy.
//
// It is a package rather than a test helper because the checks have to run
// against a theme twice: once from the core, over every theme that happens to be
// registered, and once from inside the theme package itself, where a theme is
// checked without any application being constructed. A theme that only ever ran
// one of the two would pass while the other found the problem, and the
// reproduction would depend on which package the engineer happened to test.
//
// The checks exist because a theme replaces the templates the framework renders
// when something has already gone wrong. A theme missing an error page is not a
// visual defect, it is a framework that cannot explain a failure, discovered at
// the worst possible moment by whoever is least able to investigate it.
package themetest

import (
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/the-maldridge/webapp/tmpl"
)

// Namespace is the namespace a theme's required templates are named in. It
// duplicates the core's unexported constant because a test helper must not drag
// in the whole core to state a name, and the core's own registry test asserts
// every theme's entries use the same one, so the two cannot disagree quietly.
const Namespace = "core"

// Excused lists templates that are shipped but not required, because they are
// extended by a required template rather than rendered. errors/base.p2 is the
// shared error skeleton, compiled through its three children.
var Excused = map[string]bool{
	"errors/base.p2": true,
}

// assetRe finds the assets a base layout loads from the framework's static root,
// whether as a stylesheet or as a script. The capture group is the URL, so the
// attribute name does not have to be stripped off the result afterwards.
var assetRe = regexp.MustCompile(`(?:href|src)="(` + regexp.QuoteMeta("/static/"+Namespace) + `/[^"]+)"`)

// Check runs every contract rule against one theme and reports each failure with
// the theme's name, so a suite covering several themes names the offender rather
// than the rule.
func Check(t *testing.T, name string, tree fs.FS, static fs.FS, required []string) {
	t.Helper()

	if tree == nil {
		t.Errorf("%s: theme has no template tree", name)
		return
	}

	t.Run(name+"/required-qualified", func(t *testing.T) {
		checkQualified(t, name, required)
	})
	t.Run(name+"/required-present", func(t *testing.T) {
		checkPresent(t, name, tree, required)
	})
	t.Run(name+"/required-compile", func(t *testing.T) {
		checkCompile(t, name, tree, required)
	})
	t.Run(name+"/nothing-unverified", func(t *testing.T) {
		checkNothingUnverified(t, name, tree, required)
	})
	if static != nil {
		t.Run(name+"/stylesheets-resolve", func(t *testing.T) {
			checkStylesheets(t, name, tree, static)
		})
	}
}

// checkQualified asserts every required name is absolute in the theme namespace.
//
// A bare name resolves to whichever mounted root came first, so a required entry
// written without the prefix would let the startup check pass against an
// application's copy of the same filename while the theme's own copy was missing
// entirely, which is the one thing the check exists to prevent.
func checkQualified(t *testing.T, name string, required []string) {
	t.Helper()

	if len(required) == 0 {
		t.Error("theme declares no required templates")
	}
	for _, entry := range required {
		ns, _, ok := tmpl.SplitKey(entry)
		if !ok {
			t.Errorf("required entry %q is not namespace-qualified; a bare name resolves to whichever "+
				"mounted root came first, so the check would pass against an application's copy while the "+
				"theme's is missing", entry)
			continue
		}
		if ns != Namespace {
			t.Errorf("required entry %q names namespace %q, not %q; the framework renders against %q, "+
				"so an entry naming anything else is never what a render opens",
				entry, ns, Namespace, Namespace)
		}
	}
}

// checkPresent asserts every required template exists in the tree.
func checkPresent(t *testing.T, name string, tree fs.FS, required []string) {
	t.Helper()

	reg := tmpl.New()
	if err := reg.Mount(Namespace, tree); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	for _, entry := range required {
		if _, found := reg.Lookup("", entry); !found {
			t.Errorf("required template %q is not in theme %q's tree", entry, name)
		}
	}
}

// checkCompile asserts the required set parses, which is what the core does at
// startup. A template that is present but does not parse has exactly the same
// effect as one that is missing: no error page when something goes wrong.
func checkCompile(t *testing.T, name string, tree fs.FS, required []string) {
	t.Helper()

	reg := tmpl.New()
	if err := reg.Mount(Namespace, tree); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	r, err := tmpl.NewRenderer(reg)
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	if err := r.Verify(required...); err != nil {
		t.Errorf("theme %q's templates do not compile: %v", name, err)
	}
}

// checkNothingUnverified fails when a template ships without being required or
// explicitly excused.
//
// The required list is short on purpose, so the way it goes stale is a file being
// added and quietly never checked, which is worse than never adding it: the file
// looks covered because it is in the directory.
func checkNothingUnverified(t *testing.T, name string, tree fs.FS, required []string) {
	t.Helper()

	present := map[string]bool{}
	err := fs.WalkDir(tree, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || path.Ext(p) != ".p2" {
			return nil
		}
		present[p] = true
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}

	listed := map[string]bool{}
	for entry := range Excused {
		listed[entry] = true
	}
	for _, entry := range required {
		_, rest, ok := tmpl.SplitKey(entry)
		if !ok {
			continue
		}
		listed[rest] = true
	}

	var missing []string
	for p := range present {
		if !listed[p] {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("theme %q ships templates nothing verifies; add them to Required or to themetest.Excused: %v",
			name, missing)
	}
}

// checkStylesheets asserts every asset the base layout loads from the framework's
// static root is actually present in the theme's asset tree.
//
// This is the drift that no renderer notices: a base layout referencing an asset
// that was renamed, or in a vendored bundle that was never generated, produces a
// page that renders perfectly and is unstyled. The check reads the layout rather
// than a hardcoded filename precisely so that a theme may organize its own assets
// as it likes.
func checkStylesheets(t *testing.T, name string, tree fs.FS, static fs.FS) {
	t.Helper()

	src, err := fs.ReadFile(tree, "base.p2")
	if err != nil {
		t.Errorf("theme %q has no base.p2 to read its asset links from: %v", name, err)
		return
	}

	prefix := "/static/" + Namespace + "/"
	links := assetRe.FindAllStringSubmatch(string(src), -1)
	if len(links) == 0 {
		t.Errorf("theme %q's base.p2 loads no asset below /static/%s/; a theme is expected to "+
			"provide its own styling, and a page that ships unstyled by accident is indistinguishable "+
			"from one that does it on purpose", name, Namespace)
		return
	}

	for _, match := range links {
		target := match[1]
		// A query string is a cache stamp, not part of the filename.
		target, _, _ = strings.Cut(target, "?")
		rel := strings.TrimPrefix(target, prefix)
		if _, err := fs.Stat(static, rel); err != nil {
			t.Errorf("theme %q's base.p2 loads %q, which its asset tree does not contain (%v); "+
				"if the bundle is generated rather than committed, run go generate ./...", name, target, err)
		}
	}
}
