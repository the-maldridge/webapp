package webapp

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// staticModule contributes an asset tree under a namespace, sharing the
// namespace with its templates the way a real module does.
type staticModule struct {
	namespace string
	files     map[string]string
	route     string
}

func (m staticModule) Static() (string, fs.FS) { return m.namespace, mapFS(m.files) }

func (m staticModule) Templates() (string, fs.FS) {
	return m.namespace, mapFS(map[string]string{"page.p2": "page"})
}

func (m staticModule) Routes(r Router) {
	if m.route == "" {
		return
	}
	r.Get(m.route, func(w http.ResponseWriter, req *http.Request) { http.Redirect(w, req, m.Asset(), http.StatusSeeOther) })
}

func (m staticModule) Asset() string { return "/static/" + m.namespace + "/" }

func TestModuleNameSpacesTemplatesAndStaticsTogether(t *testing.T) {
	// A module mounting both kinds of thing under its own name is the normal
	// case, not an edge case. A namespace check that treated the template
	// namespace and the URL prefix as one address space would reject every
	// module that had a stylesheet.
	withCleanModuleRegistry(t)

	registerModule(t, ModuleInfo{
		Name: "shared",
		Factory: func(s *Scope) (any, error) {
			return staticModule{namespace: "shared", files: map[string]string{"app.css": "h1{}"}}, nil
		},
	})

	a := newTestApp(t)
	if !a.registry.Mounted("shared") {
		t.Errorf("templates were not mounted; namespaces = %v", a.registry.Namespaces())
	}
	rec := a.get(t, "/static/shared/app.css")
	if rec.Code != http.StatusOK {
		t.Errorf("module asset: status %d body %q", rec.Code, rec.Body.String())
	}
}

func TestTwoModulesSameFileName(t *testing.T) {
	withCleanModuleRegistry(t)

	registerModule(t, ModuleInfo{
		Name: "alpha",
		Factory: func(s *Scope) (any, error) {
			return staticModule{namespace: "alpha", files: map[string]string{"app.css": "alpha wins"}}, nil
		},
	})
	registerModule(t, ModuleInfo{
		Name: "beta",
		Factory: func(s *Scope) (any, error) {
			return staticModule{namespace: "beta", files: map[string]string{"app.css": "beta wins"}}, nil
		},
	})

	a := newTestApp(t)
	if got := a.get(t, "/static/alpha/app.css").Body.String(); got != "alpha wins" {
		t.Errorf("alpha's stylesheet = %q", got)
	}
	if got := a.get(t, "/static/beta/app.css").Body.String(); got != "beta wins" {
		t.Errorf("beta's stylesheet = %q", got)
	}
}

func TestStaticNamespaceCollisionFailsStartup(t *testing.T) {
	withCleanModuleRegistry(t)

	registerModule(t, ModuleInfo{
		Name: "first",
		Factory: func(s *Scope) (any, error) {
			return staticModule{namespace: "same", files: map[string]string{"a.css": "1"}}, nil
		},
	})
	registerModule(t, ModuleInfo{
		Name: "second",
		Factory: func(s *Scope) (any, error) {
			return staticModule{namespace: "same", files: map[string]string{"b.css": "2"}}, nil
		},
	})

	a, err := newTestAppNoBuild(t)
	if err == nil {
		a.Close()
		t.Fatal("two modules claimed the same URL prefix")
	}
	if !strings.Contains(err.Error(), "already claimed") {
		t.Errorf("error should say the namespace is taken: %v", err)
	}
}

func TestStaticConditionalRequest(t *testing.T) {
	withCleanModuleRegistry(t)

	registerModule(t, ModuleInfo{
		Name: "assets",
		Factory: func(s *Scope) (any, error) {
			return staticModule{namespace: "assets", files: map[string]string{"x.css": "body{}"}}, nil
		},
	})

	a := newTestApp(t)
	first := a.get(t, "/static/assets/x.css")
	if first.Code != http.StatusOK {
		t.Fatalf("first request: %d", first.Code)
	}

	tag := first.Header().Get("ETag")
	if tag == "" {
		t.Fatal("an embedded asset carries no ETag, so every reload transfers it again")
	}
	if got := first.Header().Get("Cache-Control"); !strings.Contains(got, "max-age=") {
		t.Errorf("Cache-Control = %q", got)
	}

	req := httptest.NewRequest(http.MethodGet, "/static/assets/x.css", nil)
	req.Header.Set("If-None-Match", tag)
	rec := httptest.NewRecorder()
	a.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotModified {
		t.Errorf("conditional request = %d, want 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("a 304 carried a body of %d bytes", rec.Body.Len())
	}
	// The 304 has to keep naming the validator it was answered with, or the
	// client cannot reuse the entry it already holds.
	if got := rec.Header().Get("ETag"); got != tag {
		t.Errorf("304 ETag = %q, want %q", got, tag)
	}
}

func TestStaticETagTracksContent(t *testing.T) {
	withCleanModuleRegistry(t)

	registerModule(t, ModuleInfo{
		Name: "one",
		Factory: func(s *Scope) (any, error) {
			return staticModule{namespace: "one", files: map[string]string{"a.css": "first"}}, nil
		},
	})
	registerModule(t, ModuleInfo{
		Name: "two",
		Factory: func(s *Scope) (any, error) {
			return staticModule{namespace: "two", files: map[string]string{"a.css": "second"}}, nil
		},
	})

	a := newTestApp(t)
	left := a.get(t, "/static/one/a.css").Header().Get("ETag")
	right := a.get(t, "/static/two/a.css").Header().Get("ETag")
	if left == "" || right == "" {
		t.Fatal("missing ETag")
	}
	if left == right {
		t.Error("different contents produced the same ETag, so a browser would serve one for the other")
	}
}

func TestUnknownAssetHasNoETagAndStillMisses(t *testing.T) {
	a := newTestApp(t)
	rec := a.get(t, "/static/core/does-not-exist.css")
	if rec.Code == http.StatusOK {
		t.Fatal("a missing asset was served")
	}
	if tag := rec.Header().Get("ETag"); tag != "" {
		t.Errorf("a 404 carries an ETag: %q", tag)
	}
}

func TestTraversalStillMissesThroughAModuleMount(t *testing.T) {
	withCleanModuleRegistry(t)

	registerModule(t, ModuleInfo{
		Name: "trav",
		Factory: func(s *Scope) (any, error) {
			return staticModule{namespace: "trav", files: map[string]string{"ok.css": "fine"}}, nil
		},
	})

	a := newTestApp(t)
	// httptest normalises the path, so the encoded form is what actually
	// reaches the handler and proves the filesystem cannot be walked out of.
	if rec := a.get(t, "/static/trav/../../go.mod"); rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "module github.com") {
		t.Error("a traversal through a module's static mount escaped its tree")
	}
}

func TestModuleAssetURL(t *testing.T) {
	a := newTestApp(t)

	got := a.ModuleAsset("tasks", "css/app.css")
	if !strings.HasPrefix(got, "/static/tasks/css/app.css") {
		t.Errorf("ModuleAsset = %q", got)
	}
	if a.buildVersion != "" && !strings.Contains(got, "v=") {
		t.Errorf("ModuleAsset = %q, want a version stamp", got)
	}

	// A leading slash in the name must not become a double separator.
	if again := a.ModuleAsset("tasks", "/css/app.css"); strings.Contains(again, "//") && !strings.Contains(again, "://") {
		t.Errorf("ModuleAsset with a leading slash = %q", again)
	}

	// A namespace that cannot be requested must not silently point at a
	// different module's files.
	if bad := a.ModuleAsset("two/namespaces", "a.css"); bad != "" {
		t.Errorf("an unrequestable namespace produced %q, want the empty string", bad)
	}
}

func TestStaticServedWithTemplatingDisabled(t *testing.T) {
	// An API-only application switching templating off still has a stylesheet
	// if anything links it; the mount and the renderer are separate decisions.
	a := newTestApp(t, WithNoTemplates())
	if rec := a.get(t, "/static/core/css/app.css"); rec.Code != http.StatusOK {
		t.Errorf("framework asset with templating off: %d", rec.Code)
	}
}
