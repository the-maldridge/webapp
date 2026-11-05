package tmpl

import (
	"bytes"
	"errors"
	"html/template"
	"strings"
	"testing"
)

func newTestRenderer(t *testing.T, files map[string]string, opts ...Option) *Renderer {
	t.Helper()
	reg := New()
	if err := reg.Mount("app", mapFS(files)); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	r, err := NewRenderer(reg, opts...)
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	return r
}

func render(t *testing.T, r *Renderer, name string, data map[string]any) string {
	t.Helper()
	var buf bytes.Buffer
	if err := r.Render(t.Context(), &buf, name, data); err != nil {
		t.Fatalf("Render(%q): %v", name, err)
	}
	return buf.String()
}

func TestNewRendererRequiresRegistryWithRoots(t *testing.T) {
	if _, err := NewRenderer(nil); err == nil {
		t.Error("NewRenderer(nil) returned no error")
	}
	if _, err := NewRenderer(New()); err == nil {
		t.Error("NewRenderer over an empty registry returned no error")
	}
}

func TestRenderResolvesBareName(t *testing.T) {
	r := newTestRenderer(t, map[string]string{"page.p2": "hello {{ name }}"})

	if got := render(t, r, "page.p2", map[string]any{"name": "world"}); got != "hello world" {
		t.Errorf("got %q", got)
	}
	if got := render(t, r, "@app/page.p2", map[string]any{"name": "world"}); got != "hello world" {
		t.Errorf("canonical name: got %q", got)
	}
}

func TestExtendsAndIncludeResolveAtAnyDepth(t *testing.T) {
	r := newTestRenderer(t, map[string]string{
		"base.p2":          `[{% block body %}{% endblock %}]`,
		"a/b/c/page.p2":    `{% extends "../../base.p2" %}{% block body %}page{% include "side.p2" %}{% endblock %}`,
		"a/b/c/side.p2":    `side`,
		"deep/page2.p2":    `{% extends "base.p2" %}{% block body %}two{% endblock %}`,
		"fragments/nav.p2": `nav`,
		"deep/sub/uses.p2": `{% include "fragments/nav.p2" %}`,
		"explicit.p2":      `{% include "@app/fragments/nav.p2" %}`,
		"relativeclimb.p2": `{% include "a/b/c/side.p2" %}`,
		"escapes.p2":       `{% include "../outside.p2" %}`,
	})

	if got := render(t, r, "a/b/c/page.p2", nil); got != "[pageside]" {
		t.Errorf("nested page = %q", got)
	}
	if got := render(t, r, "deep/page2.p2", nil); got != "[two]" {
		t.Errorf("root-relative extends = %q", got)
	}
	if got := render(t, r, "deep/sub/uses.p2", nil); got != "nav" {
		t.Errorf("root-relative include = %q", got)
	}
	if got := render(t, r, "explicit.p2", nil); got != "nav" {
		t.Errorf("explicit namespace include = %q", got)
	}
	if got := render(t, r, "relativeclimb.p2", nil); got != "side" {
		t.Errorf("explicit relative include = %q", got)
	}
}

func TestIncludeEscapingRootIsAMiss(t *testing.T) {
	r := newTestRenderer(t, map[string]string{
		"escapes.p2": `{% include "../outside.p2" %}`,
	})

	var buf bytes.Buffer
	err := r.Render(t.Context(), &buf, "escapes.p2", nil)
	if err == nil {
		t.Fatal("including above the root succeeded")
	}
	if buf.Len() != 0 {
		t.Errorf("partial output written for a failed render: %q", buf.String())
	}
}

func TestRenderMissingTemplateWritesNothing(t *testing.T) {
	r := newTestRenderer(t, map[string]string{"page.p2": "hi"})

	var buf bytes.Buffer
	err := r.Render(t.Context(), &buf, "nope.p2", nil)
	if err == nil {
		t.Fatal("missing template returned no error")
	}
	if buf.Len() != 0 {
		t.Errorf("renderer wrote %q before failing", buf.String())
	}
	// The error has to say what was searched, or the fix is a guess.
	if !strings.Contains(err.Error(), "app") {
		t.Errorf("error %q does not name the mounted namespace", err)
	}
}

func TestAutoescapeAndSafeValues(t *testing.T) {
	r := newTestRenderer(t, map[string]string{
		"p.p2": `{{ plain }}|{{ markup }}|{{ err }}`,
	})

	out := render(t, r, "p.p2", map[string]any{
		"plain":  "<b>bold</b>",
		"markup": template.HTML("<b>bold</b>"),
		"err":    errors.New("boom <script>"),
	})

	want := "&lt;b&gt;bold&lt;/b&gt;|<b>bold</b>|boom &lt;script&gt;"
	if out != want {
		t.Errorf("got  %q\nwant %q", out, want)
	}
}

func TestSafeValueHelper(t *testing.T) {
	r := newTestRenderer(t, map[string]string{"p.p2": "{{ csrf }}"})

	out := render(t, r, "p.p2", map[string]any{
		"csrf": SafeValue(template.HTML(`<input name="csrf_token" value="x">`)),
	})
	if !strings.Contains(out, `<input name="csrf_token"`) {
		t.Errorf("SafeValue output was escaped: %q", out)
	}
}

func TestNestedMapsAndSlicesArePrepared(t *testing.T) {
	r := newTestRenderer(t, map[string]string{
		"p.p2": `{{ row.cell }}|{% for item in list %}{{ item }};{% endfor %}`,
	})

	out := render(t, r, "p.p2", map[string]any{
		"row":  map[string]any{"cell": template.HTML("<i>ok</i>")},
		"list": []any{errors.New("e1"), template.HTML("<i>2</i>")},
	})
	if out != "<i>ok</i>|e1;<i>2</i>;" {
		t.Errorf("got %q", out)
	}
}

func TestContextKeysAreValidated(t *testing.T) {
	r := newTestRenderer(t, map[string]string{"p.p2": "x"})

	var buf bytes.Buffer
	err := r.Render(t.Context(), &buf, "p.p2", map[string]any{"not an identifier": 1})
	if err == nil {
		t.Fatal("an unusable context key was accepted")
	}
	if !strings.Contains(err.Error(), "not an identifier") {
		t.Errorf("error %q does not name the offending key", err)
	}
}

func TestGlobals(t *testing.T) {
	reg := New()
	if err := reg.Mount("app", mapFS(map[string]string{"p.p2": "{{ appName }}"})); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	r, err := NewRenderer(reg, WithGlobals("appName", "Tasks"))
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	if got := render(t, r, "p.p2", nil); got != "Tasks" {
		t.Errorf("globals: got %q", got)
	}
}

func TestWithDebugRecompiles(t *testing.T) {
	reg := New()
	if err := reg.Mount("app", mapFS(map[string]string{"p.p2": "one"})); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	r, err := NewRenderer(reg, WithDebug(true))
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	if !r.set.Debug {
		t.Error("WithDebug did not reach the template set")
	}
}

func TestVerifyReportsMissingAndBroken(t *testing.T) {
	r := newTestRenderer(t, map[string]string{
		"good.p2":   "fine",
		"broken.p2": "{% for x in %}",
	})

	err := r.Verify("good.p2", "missing.p2", "broken.p2", "@ghost/also-missing.p2")
	if err == nil {
		t.Fatal("Verify accepted missing and broken templates")
	}
	msg := err.Error()
	for _, want := range []string{"missing.p2", "broken.p2", "also-missing.p2", "not found", "failed to compile"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Verify error does not mention %q:\n%s", want, msg)
		}
	}
	// A template that compiled is still recorded.
	found := false
	for _, k := range r.Compiled() {
		if k == "@app/good.p2" {
			found = true
		}
	}
	if !found {
		t.Errorf("Compiled() = %v, want it to include @app/good.p2", r.Compiled())
	}
}

func TestVerifyPassesForExistingTemplates(t *testing.T) {
	r := newTestRenderer(t, map[string]string{"a.p2": "a", "b.p2": "b"})
	if err := r.Verify("a.p2", "b.p2"); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

func TestResolveAndCompiled(t *testing.T) {
	r := newTestRenderer(t, map[string]string{"z.p2": "z", "a.p2": "a"})

	if _, ok := r.Resolve("z.p2"); !ok {
		t.Error("Resolve could not resolve an existing template")
	}
	if _, ok := r.Resolve("gone.p2"); ok {
		t.Error("Resolve resolved a missing template")
	}

	for _, name := range []string{"z.p2", "a.p2"} {
		if err := r.Render(t.Context(), &bytes.Buffer{}, name, nil); err != nil {
			t.Fatalf("Render(%q): %v", name, err)
		}
	}
	compiled := r.Compiled()
	if len(compiled) != 2 || compiled[0] != "@app/a.p2" || compiled[1] != "@app/z.p2" {
		t.Errorf("Compiled() = %v, want sorted canonical keys", compiled)
	}
}

func TestOverrideWinsByMountOrder(t *testing.T) {
	reg := New()
	if err := reg.Mount("app", mapFS(map[string]string{"base.p2": "app"})); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if err := reg.MountFirst("theme", mapFS(map[string]string{"base.p2": "theme"})); err != nil {
		t.Fatalf("MountFirst: %v", err)
	}
	r, err := NewRenderer(reg)
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	if got := render(t, r, "base.p2", nil); got != "theme" {
		t.Errorf("override = %q, want the theme copy", got)
	}
}

func TestRenderString(t *testing.T) {
	r := newTestRenderer(t, map[string]string{"p.p2": "hi {{ x }}"})

	out, err := r.RenderString("p.p2", map[string]any{"x": 2})
	if err != nil {
		t.Fatalf("RenderString: %v", err)
	}
	if out != "hi 2" {
		t.Errorf("got %q", out)
	}
	if _, err := r.RenderString("gone.p2", nil); err == nil {
		t.Error("RenderString of a missing template returned no error")
	}
}

func TestNamespacesFromRenderer(t *testing.T) {
	reg := New()
	mustMount(t, reg, "app", map[string]string{"p.p2": ""})
	mustMount(t, reg, "core", map[string]string{"q.p2": ""})
	r, err := NewRenderer(reg)
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	if got := strings.Join(r.Namespaces(), ","); got != "app,core" {
		t.Errorf("Namespaces = %q", got)
	}
}
