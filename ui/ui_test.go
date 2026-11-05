package ui_test

import (
	"io/fs"
	"path"
	"sort"
	"strings"
	"testing"

	"github.com/the-maldridge/webapp/tmpl"
	"github.com/the-maldridge/webapp/ui"
)

// TestRequiredTemplatesExist is the cheap half of the startup check the core
// performs.
//
// A Required entry naming a file that no longer exists is a startup failure for
// every application embedding this framework, discovered by whoever runs the
// build rather than by whoever made the change — and a package with no test
// binary has no build that fails. This test is where that lands.
func TestRequiredTemplatesExist(t *testing.T) {
	tree := ui.FS()

	for _, name := range ui.Required {
		if _, _, ok := tmpl.SplitKey(name); !ok {
			t.Errorf("Required entry %q is not namespace-qualified; a bare name "+
				"resolves to whichever mounted root came first, so the check would "+
				"pass against an application's copy while the framework's is missing", name)
			continue
		}
		if _, found := requiredLookup(t, tree, name); !found {
			t.Errorf("Required template %q is not in the embedded filesystem", name)
		}
	}
}

// TestRequiredTemplatesCompile builds a renderer over the embedded tree and
// verifies the required set, which is what the core does at startup. A
// template that is present but does not parse has the same effect: no error
// page when something goes wrong.
func TestRequiredTemplatesCompile(t *testing.T) {
	reg := tmpl.New()
	if err := reg.Mount(ui.Namespace, ui.FS()); err != nil {
		t.Fatalf("Mount: %v", err)
	}

	r, err := tmpl.NewRenderer(reg)
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	if err := r.Verify(ui.Required...); err != nil {
		t.Errorf("the framework's own templates do not compile: %v", err)
	}
}

// TestRequiredCoversEveryRenderableTemplate fails when a template is added to
// the embedded tree without being listed as required or explicitly excused.
//
// The list is short on purpose — the framework renders a handful of pages — so
// the way it goes stale is a file being added and quietly never checked.
func TestRequiredCoversEveryRenderableTemplate(t *testing.T) {
	present := map[string]bool{}
	err := fs.WalkDir(ui.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || path.Ext(name) != ".p2" {
			return nil
		}
		present[ui.Namespace+"/"+name] = true
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}

	// errors/base.p2 is extended by the three error pages rather than rendered
	// directly, so it is compiled through them and is not required on its own.
	listed := map[string]bool{"core/errors/base.p2": true}
	for _, name := range ui.Required {
		listed[strings.TrimPrefix(name, tmpl.NamespaceSeparator)] = true
	}

	var missing []string
	for name := range present {
		if !listed[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("templates shipped but never verified; add them to ui.Required "+
			"or to this test's excused list: %v", missing)
	}
}

// TestStaticAssetsAreEmbedded keeps the base layout's stylesheet link and the
// asset tree from drifting apart in a package that has no renderer to notice.
func TestStaticAssetsAreEmbedded(t *testing.T) {
	if _, err := fs.Stat(ui.Static(), "css/app.css"); err != nil {
		t.Errorf("the framework stylesheet is not embedded: %v", err)
	}
}

// requiredLookup resolves a namespace-qualified name against the embedded tree
// through the real registry, so the test checks resolution rather than string
// prefixing.
func requiredLookup(t *testing.T, tree fs.FS, name string) (string, bool) {
	t.Helper()

	reg := tmpl.New()
	if err := reg.Mount(ui.Namespace, tree); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	return reg.Lookup("", name)
}
