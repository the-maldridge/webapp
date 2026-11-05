package tmpl

import (
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

// mapFS is a shorthand for the in-memory roots the tests mount.
func mapFS(files map[string]string) fs.FS {
	return fstest.MapFS(mks(files))
}

func mks(files map[string]string) fstest.MapFS {
	out := fstest.MapFS{}
	for name, body := range files {
		out[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return out
}

func mustMount(t *testing.T, r *Registry, ns string, files map[string]string) {
	t.Helper()
	if err := r.Mount(ns, mapFS(files)); err != nil {
		t.Fatalf("Mount(%q): %v", ns, err)
	}
}

func TestSplitKey(t *testing.T) {
	tests := []struct {
		key  string
		ns   string
		rest string
		ok   bool
	}{
		{"@core/errors/notfound.p2", "core", "errors/notfound.p2", true},
		{"@app/x.p2", "app", "x.p2", true},
		{"core/x.p2", "", "", false},
		{"@core", "", "", false},
		{"@/x.p2", "", "", false},
		{"@core/", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range tests {
		ns, rest, ok := SplitKey(tc.key)
		if ns != tc.ns || rest != tc.rest || ok != tc.ok {
			t.Errorf("SplitKey(%q) = %q, %q, %v; want %q, %q, %v", tc.key, ns, rest, ok, tc.ns, tc.rest, tc.ok)
		}
	}
}

func TestAbsPrefersExplicitNamespace(t *testing.T) {
	r := New()
	mustMount(t, r, "app", map[string]string{"shared.p2": "app"})
	mustMount(t, r, "core", map[string]string{"shared.p2": "core"})

	if got := r.Abs("", "@core/shared.p2"); got != "@core/shared.p2" {
		t.Errorf("explicit namespace resolved to %q", got)
	}
	if got := r.Abs("", "shared.p2"); got != "@app/shared.p2" {
		t.Errorf("bare name resolved to %q, want the first mounted root", got)
	}
}

func TestAbsUnknownNamespaceIsUnresolved(t *testing.T) {
	r := New()
	mustMount(t, r, "app", map[string]string{"x.p2": "x"})

	// Abs returns the name unchanged when it cannot resolve, so the returned
	// string still looks like a canonical key even though nothing is behind
	// it. Lookup is what says whether it resolved.
	if got := r.Abs("", "@other/x.p2"); got != "@other/x.p2" {
		t.Errorf("unmounted namespace resolved to %q, want the name returned unchanged", got)
	}
	if _, ok := r.Lookup("", "@other/x.p2"); ok {
		t.Error("unmounted namespace reported as resolved")
	}
}

func TestAbsRelativeToReferencingTemplate(t *testing.T) {
	r := New()
	mustMount(t, r, "app", map[string]string{
		"deep/nested/page.p2": "page",
		"deep/nested/side.p2": "side",
		"deep/other.p2":       "other",
		"top.p2":              "top",
	})

	if got := r.Abs("@app/deep/nested/page.p2", "side.p2"); got != "@app/deep/nested/side.p2" {
		t.Errorf("sibling include = %q, want the file next to the referencing template", got)
	}
	if got := r.Abs("@app/deep/nested/page.p2", "../other.p2"); got != "@app/deep/other.p2" {
		t.Errorf("parent-relative include = %q", got)
	}
	// A bare name still falls through to the search order when the
	// referencing template's own directory does not contain it.
	if got := r.Abs("@app/deep/nested/page.p2", "top.p2"); got != "@app/top.p2" {
		t.Errorf("fallthrough to root = %q, want @app/top.p2", got)
	}
}

func TestAbsClampsEscapingNames(t *testing.T) {
	r := New()
	mustMount(t, r, "app", map[string]string{
		"deep/page.p2": "page",
		"secret.p2":    "secret",
	})

	// Clamped to the root, so this is the top-level file rather than an
	// escape attempt that happens to succeed.
	if got := r.Abs("@app/deep/page.p2", "../../secret.p2"); got != "@app/secret.p2" {
		t.Errorf("escaping include = %q, want the clamped @app/secret.p2", got)
	}

	// A name that escapes and then names something that does not exist at
	// the clamped path must be a miss, not a read.
	empty := New()
	mustMount(t, empty, "app", map[string]string{"deep/page.p2": "page"})
	if got := empty.Abs("@app/deep/page.p2", "../gone.p2"); strings.HasPrefix(got, NamespaceSeparator) {
		t.Errorf("escape to a missing file resolved to %q, want a miss", got)
	}
}

func TestGetNamesSearchedNamespaces(t *testing.T) {
	r := New()
	mustMount(t, r, "app", map[string]string{"x.p2": "x"})
	mustMount(t, r, "core", map[string]string{"y.p2": "y"})

	_, err := r.Get("missing.p2")
	if err == nil {
		t.Fatal("Get of a missing template returned no error")
	}
	msg := err.Error()
	for _, want := range []string{"app", "core", "missing.p2"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

func TestGetUnknownNamespace(t *testing.T) {
	r := New()
	mustMount(t, r, "app", map[string]string{"x.p2": "x"})

	_, err := r.Get(Canonical("ghost", "x.p2"))
	if !errors.Is(err, ErrUnknownNamespace) {
		t.Fatalf("error %v does not wrap ErrUnknownNamespace", err)
	}
}

func TestMountFirstOverrides(t *testing.T) {
	r := New()
	mustMount(t, r, "core", map[string]string{"base.p2": "embedded"})
	if err := r.MountFirst("theme", mapFS(map[string]string{"base.p2": "theme"})); err != nil {
		t.Fatalf("MountFirst: %v", err)
	}

	if got := r.Abs("", "base.p2"); got != "@theme/base.p2" {
		t.Errorf("override = %q, want the first-mounted copy", got)
	}
	body := read(t, r, r.Abs("", "base.p2"))
	if body != "theme" {
		t.Errorf("override body = %q", body)
	}
	if got := r.Namespaces(); len(got) != 2 || got[0] != "theme" || got[1] != "core" {
		t.Errorf("Namespaces = %v, want [theme core]", got)
	}
}

func TestMountRejectsDuplicateNamespace(t *testing.T) {
	r := New()
	mustMount(t, r, "app", map[string]string{"x.p2": "x"})

	if err := r.Mount("app", mapFS(map[string]string{"y.p2": "y"})); err == nil {
		t.Error("duplicate Mount returned no error")
	}
	if err := r.MountFirst("app", mapFS(map[string]string{"y.p2": "y"})); err == nil {
		t.Error("duplicate MountFirst returned no error")
	}
}

func TestMountIgnoresNilFS(t *testing.T) {
	r := New()
	if err := r.Mount("empty", nil); err != nil {
		t.Fatalf("Mount(nil): %v", err)
	}
	if len(r.mounts) != 0 {
		t.Errorf("a nil filesystem was mounted: %v", r.Namespaces())
	}
}

func TestMountRejectsUnusableNamespaces(t *testing.T) {
	for _, ns := range []string{"", "  ", "a/b", `a\b`, "@core", ".", ".."} {
		r := New()
		if err := r.Mount(ns, mapFS(map[string]string{"x.p2": "x"})); err == nil {
			t.Errorf("Mount(%q) accepted an unusable namespace", ns)
		}
	}
}

func TestNamespacesAndMounted(t *testing.T) {
	r := New()
	mustMount(t, r, "app", map[string]string{"placeholder.p2": ""})
	mustMount(t, r, "core", map[string]string{"placeholder.p2": ""})

	if got := r.Namespaces(); strings.Join(got, ",") != "app,core" {
		t.Errorf("Namespaces = %v", got)
	}
	if !r.Mounted("core") || r.Mounted("ghost") {
		t.Error("Mounted disagrees with the mounted set")
	}
}

func read(t *testing.T, r *Registry, key string) string {
	t.Helper()
	f, err := r.Get(key)
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	buf := new(strings.Builder)
	if _, err := io.Copy(buf, f); err != nil {
		t.Fatalf("read %q: %v", key, err)
	}
	return buf.String()
}
