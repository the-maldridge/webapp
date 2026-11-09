package bulma_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/the-maldridge/webapp"
	"github.com/the-maldridge/webapp/internal/themetest"
	"github.com/the-maldridge/webapp/theme/builtin"
	"github.com/the-maldridge/webapp/theme/bulma"
)

// bundlePath is the vendored stylesheet this theme's base layout links, below
// the package's own static root.
const bundlePath = "static/css/bulma.min.css"

// licensePath is the bundle's own licence, which travels with it.
const licensePath = "static/css/LICENSE"

// navScript is the one script this theme writes itself, and unlike the bundle it
// is committed.
const navScript = "static/js/navbar.js"

// lockPath is the committed record of which release the bundle is.
const lockPath = "bulma.lock"

// minBundleBytes and maxBundleBytes bracket a real Bulma release. The bounds are
// loose on purpose: they exist to catch a truncated download, an HTML error page
// saved as a stylesheet, and a bundle that grew by an order of magnitude because
// something other than the minified CSS landed here.
const (
	minBundleBytes = 100_000
	maxBundleBytes = 2_000_000
)

// TestThemeContract runs the rules every theme has to satisfy against this one.
//
// It is the same checker the core runs over registered themes, run here so that
// a theme with an incomplete required set fails `go test ./theme/bulma` rather
// than only someone's application startup.
//
// The whole test skips when the bundle has not been generated, because the
// checker statfs base.p2's stylesheet links against the asset tree, and the one
// explanation an engineer needs at that moment is the command to run.
func TestThemeContract(t *testing.T) {
	requireBundle(t)
	themetest.Check(t, "bulma", bulma.FS(), bulma.Static(), bulma.Required)
}

// TestRequiredMatchesBuiltin asserts this theme supplies exactly the templates
// the default theme does.
//
// The required set is fixed by what the framework renders, so a theme adding to
// or dropping from it is either papering over a framework change or discovering
// a page the framework renders that neither theme knew about. Both are worth a
// failing test rather than a quiet divergence.
func TestRequiredMatchesBuiltin(t *testing.T) {
	want := strings.Join(builtin.Required, "\n")
	got := strings.Join(bulma.Required, "\n")
	if want != got {
		t.Errorf("Required differs from the default theme's, which is fixed by what the framework renders:\n"+
			"builtin:\n%s\nbulma:\n%s", want, got)
	}
}

// TestVendoredBundle checks the generated stylesheet is what this theme thinks it
// is linking.
func TestVendoredBundle(t *testing.T) {
	requireBundle(t)

	data, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) < minBundleBytes || len(data) > maxBundleBytes {
		t.Errorf("%s is %d bytes, outside the %d..%d band for a Bulma release; "+
			"a truncated download or an error page saved as a stylesheet looks exactly like this",
			bundlePath, len(data), minBundleBytes, maxBundleBytes)
	}

	// The dark scheme is the part of the bundle most likely to be missing from a
	// partial build, and it is a rule a minifier would never drop on its own.
	if !strings.Contains(string(data), "prefers-color-scheme") {
		t.Error("bundle contains no prefers-color-scheme rule, so it is not the CSS build it claims to be")
	}
	for _, marker := range []string{"--bulma-scheme-main", ".navbar-burger", ".button.is-primary"} {
		if !strings.Contains(string(data), marker) {
			t.Errorf("bundle does not define %s, which this theme's templates depend on", marker)
		}
	}
}

// TestLockMatchesBundle asserts the committed lock file describes the bundle on
// disk, so that a regenerated bundle and the record of what it should be cannot
// drift apart while tests still pass.
func TestLockMatchesBundle(t *testing.T) {
	requireBundle(t)

	version := lockValue(t, "version")
	if version == "" {
		t.Fatalf("%s records no version", lockPath)
	}
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(version) {
		t.Errorf("%s version %q is not a release version", lockPath, version)
	}

	// The bundle is the release's own file, so the version has to appear in it:
	// Bulma stamps a header comment, and a bundle built from a different release
	// is the failure this check is for.
	data, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), version) {
		t.Errorf("%s pins %s but %s does not contain that version string", lockPath, version, bundlePath)
	}
}

// TestLicenseVendored asserts the bundle's licence travels with it.
//
// The templates are this repository's own work, but the stylesheet below them is
// not, and a theme that vendors CSS without the text has distributed it
// unlawfully in every application that imports it.
func TestLicenseVendored(t *testing.T) {
	requireBundle(t)

	data, err := os.ReadFile(licensePath)
	if err != nil {
		t.Fatalf("%s is missing; the vendored bundle cannot be redistributed without it: %v", licensePath, err)
	}
	if !strings.Contains(string(data), "MIT") {
		t.Errorf("%s does not read like the licence the bundle was released under", licensePath)
	}
}

// TestNoFrameworkCustomProperties asserts this theme does not reach for the
// default theme's stylesheet variables.
//
// Nothing in a theme may depend on another theme's custom properties, because
// they are that theme's private business. The mistake is easy to make and hard
// to see, since the page looks right in whichever theme it was written against.
func TestNoFrameworkCustomProperties(t *testing.T) {
	seen := map[string]string{}
	err := fs.WalkDir(bulma.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(p) != ".p2" {
			return nil
		}
		src, err := fs.ReadFile(bulma.FS(), p)
		if err != nil {
			return err
		}
		if i := strings.Index(string(src), "--wa-"); i >= 0 {
			seen[p] = string(src)[i:min(len(string(src)), i+40)]
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	for p, ctx := range seen {
		t.Errorf("%s references the default theme's custom properties (%s); a theme styles with its "+
			"own framework's vocabulary and defines whatever else it needs itself", p, ctx)
	}
}

// TestRegistrationRuns asserts importing this package registers the theme under
// the name configuration uses.
//
// The check is that WithTheme accepts the name, because that is the whole of what
// an application does with it, and it is the failure mode a missing register.go
// produces: templates that exist, assets that exist, and a configuration variable
// that matches nothing.
func TestRegistrationRuns(t *testing.T) {
	var cfg webapp.Config
	if err := webapp.WithTheme("bulma")(&cfg); err != nil {
		t.Fatalf("WithTheme(\"bulma\"): %v; the theme's init must register it", err)
	}
}

// requireBundle skips unless the generated bundle is on disk.
func requireBundle(t *testing.T) {
	t.Helper()

	if _, err := os.Stat(bundlePath); err != nil {
		t.Skipf("%s is missing (%v); it is vendored rather than committed, so run go generate ./...",
			bundlePath, err)
	}
}

// TestBundledScriptIsServed asserts the theme's own JavaScript is in the asset
// tree the core serves, which is what makes the link in base.p2 resolve.
//
// It is checked here because the failure is silent in every sense a test normally
// catches: a page that loads with a 200, looks correct, and has a navigation menu
// that does not open on a phone.
func TestBundledScriptIsServed(t *testing.T) {
	data, err := fs.ReadFile(bulma.Static(), "js/navbar.js")
	if err != nil {
		t.Fatalf("%s is missing from the asset tree (%v), so base.p2 links a script that answers 404",
			navScript, err)
	}
	if !strings.Contains(string(data), "wa-nav-toggle") {
		t.Errorf("%s does not reference the toggle base.p2 renders, so the two have drifted apart",
			navScript)
	}
}

// TestNoInlineHandlers asserts the theme ships no inline JavaScript or event
// attributes.
//
// The framework deliberately publishes no Content-Security-Policy, because a
// useful one is specific to the application, which means an application is free to
// set script-src 'self'. A theme whose layout carried an onclick attribute would
// quietly break navigation for exactly the deployments that got their policy
// right, and the theme author would never see it.
func TestNoInlineHandlers(t *testing.T) {
	src, err := fs.ReadFile(bulma.FS(), "base.p2")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	for _, forbidden := range []string{" onclick=", " onfocus=", " onload=", "<script>"} {
		if strings.Contains(string(src), forbidden) {
			t.Errorf("base.p2 contains %q; the theme must not require inline script, because an "+
				"application setting Content-Security-Policy: script-src 'self' would lose its "+
				"navigation", forbidden)
		}
	}
}

// lockValue reads one key from the lock file.
func lockValue(t *testing.T, key string) string {
	t.Helper()

	src, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	for _, line := range strings.Split(string(src), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if k == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// TestLockFileSize is a smoke test that the lock file is parseable in the same
// way the generator reads it, including its byte counts where recorded.
func TestLockFileShape(t *testing.T) {
	src, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	seen := map[string]bool{}
	for _, line := range strings.Split(string(src), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Errorf("%s has a malformed line %q; the generator refuses to read a lock file it cannot "+
				"parse in full, and so does this", lockPath, line)
			continue
		}
		seen[k] = true
		if k == "version" {
			if _, err := strconv.Atoi(strings.Split(v, ".")[0]); err != nil {
				t.Errorf("version %q does not start with a number: %v", v, err)
			}
		}
		if strings.HasPrefix(k, "sha256:") && len(v) != 64 {
			t.Errorf("digest for %s is %d characters, not 64", k, len(v))
		}
	}
	for _, key := range []string{"version", "tarball_sha256"} {
		if !seen[key] {
			t.Errorf("%s records no %s", lockPath, key)
		}
	}
}
