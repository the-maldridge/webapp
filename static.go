package webapp

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/the-maldridge/webapp/internal/themeassets"
)

// Static assets.
//
// The framework's own assets and every module's are embedded, and embedded
// files have no modification time. That single fact drives everything here:
// http.FileServer has no validator to offer, so conditional requests always
// transfer the whole body, and the expiry that ships is a deliberate number
// rather than something the transport produced by accident.
//
// Validators are therefore computed rather than read. Walking an asset tree at
// startup and hashing each file is affordable because the trees are the ones
// compiled into the binary, and it buys the one thing a version stamp in a
// query string cannot: a browser holding a cached copy finds out it is still
// valid without downloading it again.

// StaticRoot is the URL prefix the active theme's static assets are served
// from. The name says "core" because the assets belong to the namespace the
// framework renders against, not to whichever theme is filling it: a theme's
// base layout links its own stylesheet from below this prefix, so switching
// themes changes what is served here without changing any URL a template has to
// know. Leaving the top of the namespace below a free directory means a theme
// organises its assets as it likes.
const StaticRoot = "/static/core"

// staticNamespacePrefix is the URL prefix every module's assets are served
// below. Namespacing by URL is what lets two modules ship a file named
// app.css without either overwriting the other.
const staticNamespacePrefix = "/static"

// installStatic serves the active theme's assets and every module's.
//
// The theme's mount is not optional: its base layout links its own stylesheet,
// so a build that forgot would render every page unstyled, which is the kind of
// defect that survives review because nobody looks at the network tab during
// code review. A theme with no assets at all is allowed and simply mounts
// nothing, which is the difference between a theme that renders unstyled on
// purpose and one whose stylesheet was forgotten: the former is a decision, the
// latter is invisible.
func (a *App) installStatic() {
	if tree := a.theme.Static(); tree != nil {
		a.Mount(StaticRoot, staticHandler(StaticRoot, tree))
	}

	for _, mod := range a.moduleStatics {
		prefix := staticNamespacePrefix + "/" + mod.namespace
		a.Mount(prefix, staticHandler(prefix, mod.fsys))
	}
}

// themeLayout is the base layout whose asset links are checked at startup. It is
// also the first entry of every theme's Required set, which is what makes it safe
// to read here: the template check that precedes this one has already proved it
// exists and compiles.
const themeLayout = "base.p2"

// verifyThemeAssets confirms every asset the active theme's base layout loads is
// actually in the tree installStatic mounted.
//
// The template half of a theme is verified at startup and its asset half was not,
// which left exactly one failure mode open and made it invisible: a theme whose
// stylesheet is generated rather than committed produces a binary in which every
// page answers 200, renders correct markup, and is unstyled, because the link in
// the layout resolves to a route with no file behind it. Nothing in the response
// distinguishes that from a theme that shipped no stylesheet on purpose. Nobody
// discovers it from a log line; they discover it from a screenshot, and by then
// the first thing anyone inspects is the templates, which are fine.
//
// The check reads the layout rather than a list of filenames because a theme owns
// its own assets, so the layout is the only thing that knows what a given theme
// needs. It is a construction error rather than a log warning for the same reason
// Required is one: the alternative is a deployment that reports itself healthy
// while serving an unstyled page, and "it started" is the wrong signal to look
// for.
func (a *App) verifyThemeAssets() error {
	static := a.theme.Static()
	if static == nil {
		// An empty tree rather than nil, so that a theme linking a stylesheet from
		// a tree it never provided reports the missing file instead of nothing.
		static = emptyStatic{}
	}

	missing, err := themeassets.Missing(a.theme.FS(), static, themeLayout, StaticRoot+"/")
	if err != nil {
		// Not fatal on its own: the required-template check has already refused a
		// theme with no base layout, and says so more precisely than this can.
		return nil
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("webapp: theme %q is missing %s, which its base layout links, so every page it "+
		"renders would answer 200 unstyled. Rebuild with the theme's assets present, or select another "+
		"theme",
		a.theme.Name, strings.Join(missing, ", "))
}

// emptyStatic is an asset tree with nothing in it.
type emptyStatic struct{}

// Open always reports the file is absent.
func (emptyStatic) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }

// staticHandler serves an asset tree mounted at prefix.
//
// The prefix is stripped before the filesystem is consulted, which is not
// decoration: a mounted chi route passes the whole request path down to the
// handler, so without the strip the file server looks for
// "static/core/css/app.css" inside a tree whose root is already below that
// prefix, and every asset answers 404 while the router reports the route as
// registered.
//
// ETags are computed once, here, over the whole tree, and set per request from
// the map. Computing one per request would mean reading and hashing the file on
// every load to prove it has not changed, which costs more than the transfer it
// was meant to avoid.
//
// Setting the header before delegating is enough for conditional requests:
// http.ServeContent evaluates If-None-Match against the ETag the caller already
// set, and answers 304 itself.
func staticHandler(prefix string, tree fs.FS) http.Handler {
	files := http.FileServer(http.FS(tree))
	stripped := http.StripPrefix(strings.TrimSuffix(prefix, "/"), files)

	etags, err := etagManifest(tree)
	if err != nil {
		// A tree that cannot be walked is not a reason to serve nothing, and
		// the failure is not a reason to serve it unvalidated either: the
		// handler degrades to the behaviour this function had before validators
		// existed, which is correct, merely slower.
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "public, max-age=3600")
			w.Header().Set("Vary", "Accept-Encoding")
			stripped.ServeHTTP(w, r)
		})
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("Vary", "Accept-Encoding")

		// The lookup name is the path with the mount prefix removed, which is
		// what the file server below will be handed. Stripping it here rather
		// than reading the path as-is matters: the map is keyed by the tree's
		// own names, and "static/core/css/app.css" is not one of them, so a
		// handler that forgot the strip would set no validator on any file and
		// report no problem doing it.
		name := strings.TrimPrefix(r.URL.Path, strings.TrimSuffix(prefix, "/"))
		if name = strings.Trim(path.Clean(name), "/"); name != "" && name != "." {
			if tag, ok := etags[name]; ok {
				w.Header().Set("ETag", tag)
			}
		}
		stripped.ServeHTTP(w, r)
	})
}

// etagManifest fingerprints every file in a tree.
//
// The hash is of the contents rather than of the name and size, so a deploy that
// changed nothing produces the same validators and a browser's cached copy stays
// usable. A file over maxHashBytes is skipped rather than hashed: an asset tree
// with something that large in it is a deployment mistake, and the price of
// being wrong about that is a missing validator, not a wrong answer.
func etagManifest(tree fs.FS) (map[string]string, error) {
	const maxHashBytes = 8 << 20

	tags := map[string]string{}
	err := fs.WalkDir(tree, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		f, err := tree.Open(name)
		if err != nil {
			return nil
		}
		defer f.Close()

		info, err := d.Info()
		if err != nil || info.Size() > maxHashBytes {
			return nil
		}

		h := sha256.New()
		if _, err := io.Copy(h, io.LimitReader(f, maxHashBytes)); err != nil {
			return nil
		}
		tags[name] = `"` + base64.RawURLEncoding.EncodeToString(h.Sum(nil)[:12]) + `"`
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("webapp: could not fingerprint static assets: %w", err)
	}
	return tags, nil
}

// Asset returns the URL for one of the framework's static assets, stamped with
// the build version so a browser that cached the file from the previous deploy
// asks for the new one.
//
// The stamp is a query parameter rather than a filename rewrite because the
// assets are embedded: there is no build step in which a file named
// app.a4f3c1.css could be produced, and a cache-busting scheme that needs one is
// broken the first time somebody runs the binary directly.
func (a *App) Asset(name string) string {
	return stamp(StaticRoot+"/"+strings.TrimPrefix(name, "/"), a.buildVersion)
}

// ModuleAsset returns the URL for a static asset contributed by a module,
// stamped the same way Asset stamps the framework's.
//
// The namespace is a separate argument rather than a prefix inside the name
// because "css/app.css" below one module and below another are different files,
// and a form that invites callers to guess which slash separates what is how
// two modules end up serving each other's stylesheets.
func (a *App) ModuleAsset(namespace, name string) string {
	ns, err := cleanStaticNamespace(namespace)
	if err != nil {
		// A namespace that cannot be written cannot be requested either, so
		// the honest answer is a URL that misses rather than one that points at
		// a different module's file.
		return ""
	}
	return stamp(staticNamespacePrefix+"/"+ns+"/"+strings.TrimPrefix(name, "/"), a.buildVersion)
}

// cleanStaticNamespace validates a namespace used in an asset URL.
func cleanStaticNamespace(ns string) (string, error) {
	ns = strings.TrimSpace(ns)
	if ns == "" {
		return "", fmt.Errorf("webapp: static namespace must not be empty")
	}
	if strings.ContainsAny(ns, `/@`) {
		return "", fmt.Errorf("webapp: static namespace %q must not contain a slash or %q", ns, "@")
	}
	return ns, nil
}

// stamp appends the version parameter, preserving any query the caller already
// put on the URL.
func stamp(url, version string) string {
	if version == "" {
		return url
	}
	if strings.Contains(url, "?") {
		return url + "&v=" + version
	}
	return url + "?v=" + version
}
