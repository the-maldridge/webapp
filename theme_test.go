package webapp

import (
	"io/fs"
	"net/http"
	"strings"
	"testing"

	"github.com/the-maldridge/webapp/themetest"
)

// withCleanThemeRegistry restores the process-global theme registry after a test
// that registered a theme, so the registration cannot leak into the next test in
// the package.
//
// The root package's tests cannot run in parallel for this reason, which is the
// same constraint the module registry imposes.
func withCleanThemeRegistry(t *testing.T) {
	t.Helper()

	themeMu.Lock()
	original := themeRegistry
	themeMu.Unlock()

	t.Cleanup(func() {
		themeMu.Lock()
		themeRegistry = original
		themeMu.Unlock()
	})
}

// testTheme builds a complete theme from a map, supplying the required set for
// tests that are about something other than the list itself.
func testTheme(name string, files map[string]string) ThemeInfo {
	return ThemeInfo{
		Name:     name,
		FS:       func() fs.FS { return mapFS(files) },
		Static:   func() fs.FS { return mapFS(map[string]string{}) },
		Required: append([]string(nil), testThemeRequired...),
	}
}

// testThemeRequired is the set every theme has to supply. It is spelled out
// here rather than read from package builtin so that a test asserting the theme's
// files won a render cannot pass by accident because the default happened to
// define the same names.
var testThemeRequired = []string{
	"@core/base.p2",
	"@core/login.p2",
	"@core/fragments/nav.p2",
	"@core/errors/notfound.p2",
	"@core/errors/unauthorized.p2",
	"@core/errors/internal.p2",
}

// minimalThemeFiles returns a complete theme tree. Each template says what it is
// so an assertion can tell which file produced the output.
//
// The block declarations are not decoration. A theme has to publish the blocks the
// framework and an application's templates fill, and construction now refuses one
// that does not, because pongo2 drops a filled block whose parent omitted it with
// no error at all. The fixtures therefore satisfy the contract the same way a real
// theme does, rather than being excused from it, which would mean the tests that
// build a theme were testing a theme that could not ship.
func minimalThemeFiles() map[string]string {
	return map[string]string{
		"base.p2": `theme-base[{% block title %}{% endblock %}{% block head %}{% endblock %}` +
			`{% block bodyclass %}{% endblock %}{% block header %}{% endblock %}` +
			`{% block content %}{% endblock %}{% block footer %}{% endblock %}]`,
		"login.p2":         `theme-login{% block title %}{% endblock %}{% block content %}{% endblock %}`,
		"fragments/nav.p2": `theme-nav`,
		"errors/base.p2": `theme-error[{% block title %}{% endblock %}{% block content %}{% endblock %}` +
			`{% block detail %}{% endblock %}]`,
		"errors/notfound.p2":     `{% extends "@core/errors/base.p2" %}{% block detail %}gone{% endblock %}`,
		"errors/unauthorized.p2": `{% extends "@core/errors/base.p2" %}{% block detail %}refused{% endblock %}`,
		"errors/internal.p2":     `{% extends "@core/errors/base.p2" %}{% block detail %}broke{% endblock %}`,
	}
}

// baseWithAsset returns a base layout declaring the full contract and linking the
// named stylesheet, for tests about assets rather than about blocks.
func baseWithAsset(url string) string {
	return `<html><head>{% block title %}{% endblock %}` +
		`<link rel="stylesheet" href="` + url + `">` +
		`{% block head %}{% endblock %}</head>` +
		`<body class="{% block bodyclass %}{% endblock %}{% block header %}{% endblock %}">` +
		`{% block content %}{% endblock %}{% block footer %}{% endblock %}</body></html>`
}

// TestDefaultThemeIsBuiltin pins the behaviour every existing application
// depends on: naming no theme selects the framework's own, and the framework's
// templates are the ones that render.
func TestDefaultThemeIsBuiltin(t *testing.T) {
	if got := themeNames(); len(got) == 0 || got[0] != BuiltinThemeName {
		t.Errorf("registered themes = %v, want the builtin theme among them", got)
	}

	a := newTestApp(t)
	if a.theme.Name != BuiltinThemeName {
		t.Errorf("default theme = %q, want %q", a.theme.Name, BuiltinThemeName)
	}
	if body := a.get(t, "/definitely-not-here").Body.String(); !strings.Contains(body, "Nothing is here") {
		t.Errorf("the default theme's 404 did not render:\n%s", body)
	}
}

// TestThemeReplacesCoreTemplates is the whole point of the feature: a theme's
// files become the ones the framework renders, including the pages the framework
// renders itself without being asked.
func TestThemeReplacesCoreTemplates(t *testing.T) {
	withCleanThemeRegistry(t)
	RegisterTheme(testTheme("testtheme", minimalThemeFiles()))

	a := newTestApp(t, WithTheme("testtheme"))

	if a.theme.Name != "testtheme" {
		t.Fatalf("active theme = %q", a.theme.Name)
	}
	// The error page is the important case. It is rendered by absolute name
	// from the framework's own code, so a theme reached there proves mounting
	// by substitution rather than overlay actually works.
	body := a.get(t, "/definitely-not-here").Body.String()
	if !strings.Contains(body, "theme-error") || !strings.Contains(body, "gone") {
		t.Errorf("the theme's error page did not render, got:\n%s", body)
	}
}

// TestThemeSuppliesEveryFrameworkPage checks the three page kinds a theme owns
// rather than one: a theme that got the error page right and lost the login page
// is a theme that fails at review time rather than deploy time.
func TestThemeSuppliesEveryFrameworkPage(t *testing.T) {
	withCleanThemeRegistry(t)
	files := minimalThemeFiles()
	files["probe.p2"] = `{% extends "@core/base.p2" %}{% block content %}page{% endblock %}`
	RegisterTheme(testTheme("paged", files))

	a := newTestApp(t, WithTheme("paged"))
	a.Route("/probe", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.View(w, r, "probe.p2", Context{})
		})
	})
	a.Route("/login", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.View(w, r, "@core/login.p2", Context{})
		})
	})

	if body := a.get(t, "/probe").Body.String(); !strings.Contains(body, "theme-base[page]") {
		t.Errorf("a page extending the theme's base rendered:\n%s", body)
	}
	if body := a.get(t, "/login").Body.String(); !strings.Contains(body, "theme-login") {
		t.Errorf("the theme's login page did not render:\n%s", body)
	}
}

// TestIncompleteThemeFailsConstruction is the guarantee that replaces the one
// this framework used to state as "an application cannot replace the framework's
// error page". A theme may replace it. What it may not do is omit it.
//
// The failure has to be at construction: an error page is rendered when something
// has already gone wrong, so discovering at that moment that the theme never
// provided one converts a diagnosable failure into an unexplained one.
func TestIncompleteThemeFailsConstruction(t *testing.T) {
	withCleanThemeRegistry(t)

	files := minimalThemeFiles()
	delete(files, "errors/internal.p2")
	delete(files, "login.p2")
	RegisterTheme(testTheme("partial", files))

	_, err := newTestAppNoBuild(t, WithTheme("partial"))
	if err == nil {
		t.Fatal("a theme missing two required templates was accepted")
	}
	for _, want := range []string{"internal.p2", "login.p2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should name %q so the author knows what to add, got: %v", want, err)
		}
	}
}

// TestUnknownThemeNamesWhatExists pins that a mistyped theme is a loud failure
// naming the alternatives. A silent fallback to the default is the worse
// behaviour: the deployment looks healthy and serves the wrong design.
func TestUnknownThemeNamesWhatExists(t *testing.T) {
	withCleanThemeRegistry(t)
	RegisterTheme(testTheme("present", minimalThemeFiles()))

	_, err := newTestAppNoBuild(t, WithTheme("typoed"))
	if err == nil {
		t.Fatal("an unregistered theme name was accepted")
	}
	for _, want := range []string{"typoed", BuiltinThemeName, "present"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should name %q, got: %v", want, err)
		}
	}

	// WithTheme validates at option-application time as well, so a typo is not
	// delayed until the rest of construction has run.
	if _, err := newTestAppNoBuild(t, WithTheme("typoed")); err == nil {
		t.Error("WithTheme accepted an unregistered name")
	}
}

// TestThemeEnvVarSelectsTheme checks the operator's path, which is what makes one
// binary serve two designs.
func TestThemeEnvVarSelectsTheme(t *testing.T) {
	withCleanThemeRegistry(t)
	RegisterTheme(testTheme("fromenv", minimalThemeFiles()))

	t.Setenv("TESTAPP_THEME", "fromenv")

	a := newTestApp(t)
	if a.theme.Name != "fromenv" {
		t.Errorf("theme from environment = %q, want fromenv", a.theme.Name)
	}
}

// TestThemeEnvVarRejectsUnknownName is the environment equivalent of the typo
// check. A bad variable has to stop startup rather than be logged and ignored.
func TestThemeEnvVarRejectsUnknownName(t *testing.T) {
	t.Setenv("TESTAPP_THEME", "not registered anywhere")

	_, err := newTestAppNoBuild(t)
	if err == nil {
		t.Fatal("an unregistered theme from the environment was accepted")
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestOptionOutranksEnvVar matches every other setting in the configuration: an
// option wins, and the environment fills the gap.
func TestOptionOutranksEnvVar(t *testing.T) {
	withCleanThemeRegistry(t)
	RegisterTheme(testTheme("fromoption", minimalThemeFiles()))
	RegisterTheme(testTheme("fromenv", minimalThemeFiles()))

	t.Setenv("TESTAPP_THEME", "fromenv")

	a := newTestApp(t, WithTheme("fromoption"))
	if a.theme.Name != "fromoption" {
		t.Errorf("theme = %q, want the one named by the option", a.theme.Name)
	}
}

// TestDebugOverlayOutranksTheme keeps template debugging useful to whoever is
// writing a theme, which is the case where it matters most: iterating on a page
// without a rebuild, against a tree that is not the framework's.
//
// The overlay wins a bare name. It does not win an absolute "@core/..." name,
// which is the pre-existing rule that keeps the framework's error pages
// unreplaceable from outside and which theming does not change: the overlay is
// mounted as its own namespace, not as the theme's.
func TestDebugOverlayOutranksTheme(t *testing.T) {
	withCleanThemeRegistry(t)
	RegisterTheme(testTheme("overlay", minimalThemeFiles()))

	dir := t.TempDir()
	writeTemplate(t, dir, "login.p2", "from disk")

	a := newTestApp(t, WithTheme("overlay"), WithTemplateDebug(dir))

	a.Route("/login", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.View(w, r, "login.p2", Context{})
		})
	})
	if body := a.get(t, "/login").Body.String(); !strings.Contains(body, "from disk") {
		t.Errorf("the debug overlay did not outrank the theme:\n%s", body)
	}

	// The same page by absolute name still comes from the theme, proving the
	// overlay is an additional root rather than a replacement for it.
	a.Route("/abslogin", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.View(w, r, "@core/login.p2", Context{})
		})
	})
	if body := a.get(t, "/abslogin").Body.String(); body != "theme-login" {
		t.Errorf("the debug overlay replaced an absolute @core name:\n%s", body)
	}
}

// TestApplicationRootStillOutranksTheme is the boundary that survives theming: an
// application can still shadow a bare template name, and still cannot reach the
// absolute @core/ names the framework renders by. A theme changes who fills
// @core/, not whether it can be shadowed from outside.
func TestApplicationRootStillOutranksTheme(t *testing.T) {
	withCleanThemeRegistry(t)
	RegisterTheme(testTheme("appwins", minimalThemeFiles()))

	a := newTestApp(t,
		WithTheme("appwins"),
		WithTemplates("app", mapFS(map[string]string{
			"base.p2":            `application base`,
			"errors/notfound.p2": `application 404`,
		})))

	a.Route("/page", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.View(w, r, "base.p2", Context{})
		})
	})

	if body := a.get(t, "/page").Body.String(); strings.TrimSpace(body) != "application base" {
		t.Errorf("an application root stopped winning a bare name:\n%s", body)
	}
	body := a.get(t, "/definitely-not-here").Body.String()
	if strings.Contains(body, "application 404") {
		t.Error("an application replaced the error page through the theme's namespace")
	}
	if !strings.Contains(body, "theme-error") {
		t.Errorf("the active theme's error page did not render:\n%s", body)
	}
}

// TestThemeStaticServedUnderFrameworkRoot checks the asset half of a theme, which
// has to land at the same URL the default theme used so a theme's base layout
// needs no special knowledge of being a theme.
func TestThemeStaticServedUnderFrameworkRoot(t *testing.T) {
	withCleanThemeRegistry(t)

	info := testTheme("withassets", minimalThemeFiles())
	info.Static = func() fs.FS {
		return mapFS(map[string]string{
			"css/theme.css": "body { color: red; }",
		})
	}
	RegisterTheme(info)

	a := newTestApp(t, WithTheme("withassets"))

	rec := a.get(t, "/static/core/css/theme.css")
	if rec.Code != http.StatusOK {
		t.Errorf("theme asset status = %d, want 200", rec.Code)
	}
	// The default theme's stylesheet must not be reachable once replaced: two
	// stylesheets at one path is how a themed page ends up half-unstyled.
	if rec := a.get(t, "/static/core/css/app.css"); rec.Code == http.StatusOK {
		t.Error("the default theme's stylesheet was served underneath another theme")
	}
}

// TestThemeMissingAssetFailsConstruction covers the half of a theme that the
// required-template check cannot see: a layout linking an asset its tree does not
// contain.
//
// This is the failure a theme whose stylesheet is generated rather than committed
// produces for anyone who imports it, and before this check it was invisible:
// every page answered 200, the markup was correct, and the document was unstyled.
// A construction error is the only signal available, because the response itself
// does not distinguish an unstyled page from a theme that shipped no stylesheet.
func TestThemeMissingAssetFailsConstruction(t *testing.T) {
	withCleanThemeRegistry(t)

	files := minimalThemeFiles()
	files["base.p2"] = baseWithAsset("/static/core/css/generated.min.css")
	RegisterTheme(testTheme("ungenerated", files))

	_, err := newTestAppNoBuild(t, WithTheme("ungenerated"))
	if err == nil {
		t.Fatal("a theme whose layout links an asset it does not ship was accepted; " +
			"every page would render unstyled while reporting itself healthy")
	}
	for _, want := range []string{"ungenerated", "/static/core/css/generated.min.css", "go generate"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should name %q so the operator knows what is missing and which theme "+
				"omitted it, got: %v", want, err)
		}
	}
}

// TestThemeAssetWithCacheStampResolves covers the query string a stamped URL
// carries. A check that treated the stamp as part of the filename would refuse
// every correctly stamped theme, which is a worse defect than the one it guards
// because it cannot be worked around.
func TestThemeAssetWithCacheStampResolves(t *testing.T) {
	withCleanThemeRegistry(t)

	files := minimalThemeFiles()
	files["base.p2"] = baseWithAsset("/static/core/css/theme.css?v=1.2.3")

	info := testTheme("stamped", files)
	info.Static = func() fs.FS {
		return mapFS(map[string]string{"css/theme.css": "body{}"})
	}
	RegisterTheme(info)

	if _, err := newTestAppNoBuild(t, WithTheme("stamped")); err != nil {
		t.Errorf("a theme whose asset link carries a cache stamp was refused: %v", err)
	}
}

// TestThemeWithoutStylesheetIsAccepted states the other half of the rule: a theme
// that ships no assets and links none is a decision, not a defect. Refusing it
// would make a minimal theme impossible, and a theme that deliberately inherits
// its application's styling is legitimate.
func TestThemeWithoutStylesheetIsAccepted(t *testing.T) {
	withCleanThemeRegistry(t)

	info := testTheme("bare", minimalThemeFiles())
	info.Static = func() fs.FS { return nil }
	RegisterTheme(info)

	if _, err := newTestAppNoBuild(t, WithTheme("bare")); err != nil {
		t.Errorf("a theme with no stylesheet and no asset links was refused: %v", err)
	}
}

// TestThemeMissingBlockFailsConstruction covers the failure pongo2 cannot report:
// a child template filling a block its parent never declared renders the parent
// and throws the filled block away, with no error at parse time or render time.
//
// The error skeleton is the case that matters. Its three children all fill
// `detail`, so a base that omitted the block produces three error pages whose
// entire content, the reason for the failure, has been silently removed. That is
// a defect invisible to a test that only checks the page renders, and the page is
// the one that appears when something else has already gone wrong.
func TestThemeMissingBlockFailsConstruction(t *testing.T) {
	withCleanThemeRegistry(t)

	files := minimalThemeFiles()
	files["errors/base.p2"] = `theme-error[{% block title %}{% endblock %}{% block content %}{% endblock %}]`
	RegisterTheme(testTheme("nodetail", files))

	_, err := newTestAppNoBuild(t, WithTheme("nodetail"))
	if err == nil {
		t.Fatal("a theme whose error skeleton omits the block its children fill was accepted")
	}
	for _, want := range []string{"nodetail", "errors/base.p2", "detail"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should name %q so the theme author knows which file lacks which block, "+
				"got: %v", want, err)
		}
	}
}

// TestThemeMissingApplicationBlockFailsConstruction covers `head`, the block an
// application's pages fill to add a stylesheet. Dropping it costs a themed
// deployment its application's CSS with no other symptom, which is the same class
// of silent failure as the error skeleton and arrives the same way: a theme
// refactor that removed a block nobody rendered in a test.
func TestThemeMissingApplicationBlockFailsConstruction(t *testing.T) {
	withCleanThemeRegistry(t)

	files := minimalThemeFiles()
	files["base.p2"] = `theme-base[{% block title %}{% endblock %}{% block bodyclass %}{% endblock %}` +
		`{% block header %}{% endblock %}{% block content %}{% endblock %}{% block footer %}{% endblock %}]`
	RegisterTheme(testTheme("nohead", files))

	_, err := newTestAppNoBuild(t, WithTheme("nohead"))
	if err == nil {
		t.Fatal("a theme whose base layout omits the head block was accepted")
	}
	if !strings.Contains(err.Error(), "head") {
		t.Errorf("the error should name the missing block, got: %v", err)
	}
}

// TestThemeNilStaticWithLinksFails covers the combination that would otherwise
// dereference a nil filesystem: a theme returning no asset tree at all while its
// layout links from it.
//
// The distinction matters because "returns nil" is legal and means nothing to
// serve, whereas the layout asking for a file from nothing is the same defect as
// a missing file, and must produce the same error rather than a panic in
// somebody's startup.
func TestThemeNilStaticWithLinksFails(t *testing.T) {
	withCleanThemeRegistry(t)

	files := minimalThemeFiles()
	files["base.p2"] = baseWithAsset("/static/core/css/theme.css")

	info := testTheme("nostatic", files)
	info.Static = func() fs.FS { return nil }
	RegisterTheme(info)

	_, err := newTestAppNoBuild(t, WithTheme("nostatic"))
	if err == nil {
		t.Fatal("a theme linking a stylesheet from a tree it does not provide was accepted")
	}
	if !strings.Contains(err.Error(), "/static/core/css/theme.css") {
		t.Errorf("the error should name the missing asset, got: %v", err)
	}
}

// TestRequiredTemplatesIsTheContractEveryThemeIsCheckedAgainst pins that the
// exported accessor hands out exactly what construction enforces, so a theme in
// another module that builds its Required from it cannot disagree with the startup
// check it is supposed to predict.
func TestRequiredTemplatesIsTheContractEveryThemeIsCheckedAgainst(t *testing.T) {
	got := strings.Join(RequiredTemplates(), "\n")
	want := strings.Join(testThemeRequired, "\n")
	if got != want {
		t.Errorf("RequiredTemplates() disagrees with what the theme contract enforces:\n"+
			"RequiredTemplates:\n%s\ncontract:\n%s", got, want)
	}
}

// TestRequiredTemplatesReturnsACopy covers the mutation a caller would otherwise
// inflict on every later check: writing to the slice it was handed would change
// what the framework believes it renders, for the whole process.
func TestRequiredTemplatesReturnsACopy(t *testing.T) {
	first := RequiredTemplates()
	if len(first) == 0 {
		t.Fatal("RequiredTemplates() returned nothing")
	}
	first[0] = "tampered"

	if second := RequiredTemplates(); second[0] == "tampered" {
		t.Errorf("the slice handed to a caller is the registry's own, so a theme could rewrite " +
			"the contract for everyone")
	}
}

// TestReservedThemeNamesRejected covers the names that would make a template
// resolve differently than it was written.
func TestReservedThemeNamesRejected(t *testing.T) {
	withCleanThemeRegistry(t)

	for _, name := range []string{templateNamespace, debugNamespace, "", "  ", "a/b", "a\\b", "a b", "@x"} {
		t.Run("name="+name, func(t *testing.T) {
			if _, err := validateTheme(testTheme(name, minimalThemeFiles())); err == nil {
				t.Errorf("theme name %q was accepted", name)
			}
		})
	}
}

// TestThemeRequiredMustBeQualified covers the rule the default theme's own tests
// enforce, from the side that can produce a mistake: an unqualified entry would
// let startup verification pass against an application's copy of a filename while
// the theme's copy was absent.
func TestThemeRequiredMustBeQualified(t *testing.T) {
	withCleanThemeRegistry(t)

	for _, required := range [][]string{
		{"base.p2"},
		{"app/base.p2"},
		{"@other/base.p2"},
	} {
		info := testTheme("badrequired", minimalThemeFiles())
		info.Required = required
		if _, err := validateTheme(info); err == nil {
			t.Errorf("required entry %v was accepted", required)
		}
	}

	empty := testTheme("norequired", minimalThemeFiles())
	empty.Required = nil
	if _, err := validateTheme(empty); err == nil {
		t.Error("a theme declaring no required templates was accepted")
	}
}

// TestRegisterThemePanicsOnDefects matches the module registry: a defect visible
// at package-initialisation time is a panic, because init has no error to return
// and a theme that silently never matched is worse than a crash at startup.
func TestRegisterThemePanicsOnDefects(t *testing.T) {
	withCleanThemeRegistry(t)

	RegisterTheme(testTheme("dupe", minimalThemeFiles()))

	for name, info := range map[string]ThemeInfo{
		"duplicate": testTheme("dupe", minimalThemeFiles()),
		"nil FS": {
			Name:     "nilfs",
			Required: append([]string(nil), testThemeRequired...),
		},
		"reserved name": testTheme(templateNamespace, minimalThemeFiles()),
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("RegisterTheme accepted a defective theme")
				}
			}()
			RegisterTheme(info)
		})
	}
}

// TestValidateThemeCopiesRequired proves the copy: a caller that mutated the
// slice it passed in must not be able to change what was validated after the
// fact, which would let a theme pass a check it no longer satisfies.
func TestValidateThemeCopiesRequired(t *testing.T) {
	withCleanThemeRegistry(t)

	required := append([]string(nil), testThemeRequired...)
	info := testTheme("copied", minimalThemeFiles())
	info.Required = required

	clean, err := validateTheme(info)
	if err != nil {
		t.Fatalf("validateTheme: %v", err)
	}
	required[0] = "@core/destroyed.p2"

	if clean.Required[0] != testThemeRequired[0] {
		t.Errorf("the validated Required slice was changed through the caller's slice: %q", clean.Required[0])
	}
}

// TestRegisteredThemesSatisfyTheContract is the backstop that catches a theme
// added to this repository without its own contract test. It runs the same rules
// the theme's package runs, over whatever is actually registered, so a theme
// cannot be reachable from configuration while failing the checks.
func TestRegisteredThemesSatisfyTheContract(t *testing.T) {
	themeMu.Lock()
	registered := append([]ThemeInfo(nil), themeRegistry...)
	themeMu.Unlock()

	if len(registered) == 0 {
		t.Fatal("no themes are registered; the framework could not render anything")
	}
	for _, info := range registered {
		info := info
		t.Run(info.Name, func(t *testing.T) {
			themetest.Check(t, info.Name, info.FS(), info.Static(), info.Required)
		})
	}
}

// TestThemeFSSetTwiceIsNotMountedTwice confirms substitution cannot produce two
// roots under one namespace. Registry.Mount rejects a duplicate, which is what
// keeps "exactly one theme is active" true even if the mount code grew a loop.
func TestThemeFSSetTwiceIsNotMountedTwice(t *testing.T) {
	withCleanThemeRegistry(t)

	info := testTheme("doublemount", minimalThemeFiles())
	RegisterTheme(info)

	a := newTestApp(t, WithTheme("doublemount"))
	names := a.registry.Namespaces()

	seen := 0
	for _, n := range names {
		if n == templateNamespace {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("the theme namespace is mounted %d times, want exactly 1: %v", seen, names)
	}
}

// TestThemeErrorsAreConstructionErrors guards against a theme problem surfacing
// as a panic, which would take the process down inside a supervisor restart loop
// instead of reporting the reason and exiting.
func TestThemeErrorsAreConstructionErrors(t *testing.T) {
	withCleanThemeRegistry(t)

	info := testTheme("brokenfs", minimalThemeFiles())
	info.FS = func() fs.FS { return nil }
	RegisterTheme(info)

	_, err := New(baseTestOptions(t, WithTheme("brokenfs"))...)
	if err == nil {
		t.Fatal("a theme with no templates was accepted")
	}
	if !strings.Contains(err.Error(), "brokenfs") {
		t.Errorf("the error should name the theme, got: %v", err)
	}
}
