package webapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/the-maldridge/webapp/tmpl"
)

// Templating glue.
//
// The tmpl package knows how to resolve and compile templates. This package
// knows what a render is: a response, with a status, inside a request, to a
// client that may want JSON instead of a page. Neither fact can move into the
// other without the dependency pointing the wrong way — tmpl must not import
// the core that uses it — so everything needing both lives here.
//
// That includes the error page. An error response is rendered from a template
// and is also what happens when rendering fails, and the only sane resolution
// of that circularity is a page that cannot fail: the built-in fallback in
// respond.go is plain formatting over escaped values, and the template path
// below is allowed to lose to it but never to take the request with it.

// templateRoot is one mounted template filesystem.
type templateRoot struct {
	namespace string
	fsys      fs.FS
}

// templateNamespace is the namespace the active theme's templates are mounted
// under. A theme template refers to its own files through it,
// "@core/errors/internal.p2", so that a template an application mounts can
// never be the one an error response renders.
//
// It belongs to the core rather than to any theme because it is the core's
// addressing scheme: the framework renders against it whatever theme is
// selected, and every theme's required templates are named in its terms.
const templateNamespace = "core"

// WithTemplates mounts a set of templates under a namespace.
//
// It may be given more than once; roots are searched in the order given, and
// the first one containing a name wins. The active theme's root is mounted
// after everything an application mounts, so an application replaces a
// framework template by providing a file of the same name and never has to know
// which files those are in order to avoid them.
//
// A namespace is validated here rather than at first render, because a
// namespace that cannot be written in a template name is only reachable by the
// one form another mounted root can shadow.
func WithTemplates(namespace string, fsys fs.FS) Option {
	return func(c *Config) error {
		if fsys == nil {
			return fmt.Errorf("WithTemplates(%q): filesystem may not be nil", namespace)
		}
		clean, err := tmpl.CleanNamespace(namespace)
		if err != nil {
			return fmt.Errorf("WithTemplates: %w", err)
		}
		if clean == templateNamespace {
			return fmt.Errorf("WithTemplates: %q is reserved for the active theme's templates",
				templateNamespace)
		}
		c.templateRoots = append(c.templateRoots, templateRoot{namespace: clean, fsys: fsys})
		return nil
	}
}

// WithTemplateDebug mounts a directory of templates ahead of every embedded root
// and recompiles on every request.
//
// The two effects are one option rather than two settings, because an overlay
// that is never re-read is an overlay whose edits are invisible, and that is the
// confusing combination: an engineer editing a file the server is ignoring has
// no way to suspect the thing they can see.
//
// It is refused in production. Template debugging re-parses a file per request
// and, more importantly, gives a writable directory precedence over the contents
// of the deployed binary, which is a code-execution surface on any host where
// the deployment user can write.
func WithTemplateDebug(dir string) Option {
	return func(c *Config) error {
		if strings.TrimSpace(dir) == "" {
			return fmt.Errorf("WithTemplateDebug: directory may not be empty")
		}
		c.TemplateDebugDir = dir
		return nil
	}
}

// WithNoTemplates builds an application with no templating at all.
//
// It exists for the API-only case and for tests that want to assert what the
// core does when a render is asked for and nothing is installed. Without it,
// "no renderer" would be an internal state no application could reach
// deliberately, and "rendering is broken" would be indistinguishable from
// "nothing was ever installed" in the logs instead of in the configuration.
func WithNoTemplates() Option {
	return func(c *Config) error {
		if c.TemplatesDisabled {
			return nil
		}
		c.TemplatesDisabled = true
		return nil
	}
}

// WithSiteTitle names the application the way its pages name it.
//
// The default is the environment prefix, which is a reasonable string and always
// the wrong one to show a customer; this exists so the framework's own base
// layout can say something the application chose.
func WithSiteTitle(title string) Option {
	return func(c *Config) error {
		if strings.TrimSpace(title) == "" {
			return fmt.Errorf("WithSiteTitle: title may not be empty")
		}
		c.SiteTitle = title
		return nil
	}
}

// installTemplates mounts every root, builds the renderer, and installs the
// render-side plumbing that depends on one existing.
//
// Mount order is the whole override mechanism: the debug directory first, then
// the application's roots in the order they were given, then the active theme's.
// A bare name therefore resolves to whichever copy the operator or the
// application intended to win, and the theme's copy is the last suggestion
// rather than a fallback that hides a broken override.
//
// The theme is mounted under the framework's own namespace rather than
// alongside it. That is substitution, not overlay, and it has to be: the
// framework's templates name each other as "@core/base.p2", an absolute name
// resolves inside one root with no fallback, and an overlay would therefore be
// invisible to every template the framework renders.
//
// The active theme's required templates are compiled before this returns. They
// are rendered when something has already gone wrong, so a defect in one
// surfaces at the worst possible moment: a broken error page turns a missing
// record into an unexplained 500, and whoever is debugging the 500 has no idea
// they are reading a symptom of something else. Because a theme replaces these
// files outright, the check is what keeps "use a different theme" from becoming
// "have no error page", and it fails construction rather than degrading.
func (a *App) installTemplates() error {
	a.installNav()

	if a.Config.TemplatesDisabled {
		return nil
	}

	reg := tmpl.New()
	var mounted []string

	if dir := a.Config.TemplateDebugDir; dir != "" {
		if a.Config.Production {
			return fmt.Errorf("webapp: template debugging is refused in production: %s would take "+
				"precedence over the templates inside the binary", dir)
		}
		if err := reg.MountFirst(debugNamespace, os.DirFS(dir)); err != nil {
			return fmt.Errorf("webapp: could not mount template directory %s: %w", dir, err)
		}
		mounted = append(mounted, debugNamespace+" ("+dir+")")
	}

	for _, root := range a.Config.templateRoots {
		if err := reg.Mount(root.namespace, root.fsys); err != nil {
			return fmt.Errorf("webapp: could not mount templates %q: %w", root.namespace, err)
		}
		mounted = append(mounted, root.namespace)
	}

	themeFS := a.theme.FS()
	if themeFS == nil {
		return fmt.Errorf("webapp: theme %q returned no templates", a.theme.Name)
	}
	if err := reg.Mount(templateNamespace, themeFS); err != nil {
		return fmt.Errorf("webapp: could not mount theme %q: %w", a.theme.Name, err)
	}
	mounted = append(mounted, templateNamespace+" (theme "+a.theme.Name+")")
	a.registry = reg

	// Debug follows the overlay rather than Config.Debug. An application in
	// debug mode that mounted only embedded roots gains nothing from parsing a
	// file again that cannot have changed since it was compiled.
	opts := []tmpl.Option{tmpl.WithGlobals("siteTitle", a.siteTitle())}
	if a.Config.TemplateDebugDir != "" {
		opts = append(opts, tmpl.WithDebug(true))
	}

	render, err := tmpl.NewRenderer(reg, opts...)
	if err != nil {
		return fmt.Errorf("webapp: could not build the template renderer (mounted: %s): %w",
			strings.Join(mounted, ", "), err)
	}
	if err := render.Verify(a.theme.Required...); err != nil {
		return fmt.Errorf("webapp: theme %q cannot serve without its templates: %w", a.theme.Name, err)
	}

	a.Renderer = &templateRenderer{inner: render}
	a.SetErrorPage(a.renderErrorPage)

	a.log().Debug("webapp: templating installed",
		"theme", a.theme.Name,
		"namespaces", strings.Join(reg.Namespaces(), ","),
		"template_debug", a.Config.TemplateDebugDir != "")
	return nil
}

// debugNamespace is the namespace of the debug overlay. It is not chosen by the
// application because no template should name it: an include written against the
// overlay resolves to nothing in every other environment.
const debugNamespace = "dev"

// templateRenderer adapts the tmpl renderer to the core's one-method interface.
//
// The two signatures are structurally identical, so this type looks pointless.
// It exists because it is what keeps tmpl out of the core's public surface:
// without it the core would name a type from tmpl in an exported interface, and
// every implementation of that interface — including the stub this package's own
// tests use — would import a template engine it does not use.
type templateRenderer struct{ inner *tmpl.Renderer }

// Render implements Renderer. The context type is the same map, so nothing is
// copied and the cost is one call.
func (r *templateRenderer) Render(ctx context.Context, out io.Writer, name string, data Context) error {
	return r.inner.Render(ctx, out, name, data)
}

// renderErrorPage renders an error page from the framework's own templates.
//
// The template is named namespace-explicit for one reason: this is the code that
// runs when an application is already failing, and a page whose name is resolved
// by searching the application's mounted roots could be resolved by an
// application mid-deploy. "@core/errors/internal.p2" is the framework's file or
// it is a miss, and a miss falls through to the built-in page.
//
// A render failure is not reported by rendering something else at another
// status. The status the caller chose is the correct one, and the built-in
// fallback exists precisely because it cannot fail.
func (a *App) renderErrorPage(w http.ResponseWriter, r *http.Request, status int, err error, id string) {
	name, ok := errorTemplate(status)
	if !ok || a.Renderer == nil {
		a.defaultErrorPage(w, r, status, err, id)
		return
	}

	ctx := a.baseContext(r)
	ctx["status"] = status
	ctx["statusText"] = http.StatusText(status)
	ctx["requestId"] = id
	if a.Config.Debug && err != nil {
		ctx["errorDetail"] = errString(err)
	}
	if perm := requiredPerm(err); perm != "" {
		ctx["requiredPerm"] = perm
	}

	// The tmpl renderer buffers, so a template that fails mid-write leaves the
	// body empty rather than half a document with a status already stamped on
	// it. Whatever reaches the client is complete, and a page that could not be
	// produced is replaced by one that cannot fail.
	gate := &renderGate{w: w, status: status}
	gate.Header().Set("Cache-Control", "no-store")
	if rerr := a.Renderer.Render(r.Context(), gate, name, ctx); rerr != nil {
		slog.ErrorContext(r.Context(), "error page could not be rendered",
			"request_id", id,
			"template", name,
			"status", status,
			"original_error", errString(err),
			"error", rerr)

		if !gate.committed {
			// Nothing has been sent, so the built-in page — plain formatting
			// over escaped values, with no template and no filesystem in its
			// way — can still tell the viewer what happened and hand them the
			// reference ID. This is the only place the status of an error
			// response could be changed, and it is not: the caller's status
			// was correct and is passed along unchanged.
			a.defaultErrorPage(w, r, status, err, id)
		}
		return
	}
	gate.commit()
}

// errorTemplate maps a status to the framework template that explains it.
//
// Only three pages ship, because those are the three cases a viewer can act on
// differently: something is missing, something is refused, something broke. A
// distinct template per status code is a promise the framework cannot keep for
// codes nobody designed a page for, so the rest fall through to the built-in
// page, which renders anything.
func errorTemplate(status int) (string, bool) {
	switch {
	case status == http.StatusNotFound, status == http.StatusGone:
		return "@core/errors/notfound.p2", true
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return "@core/errors/unauthorized.p2", true
	case status >= http.StatusInternalServerError:
		return "@core/errors/internal.p2", true
	default:
		return "", false
	}
}

// requiredPerm returns the permission a denial named, or the empty string.
//
// The denial type is declared with the auth layer's renderer, which wraps the
// permission into the error it hands the core; reading it back with errors.As
// rather than out of the error's text is what keeps the page working when the
// wording changes.
func requiredPerm(err error) string {
	var denial *permDenial
	if !errors.As(err, &denial) {
		return ""
	}
	return denial.perm.String()
}

// siteTitle is the name templates give the application.
func (a *App) siteTitle() string {
	if a.Config.SiteTitle != "" {
		return a.Config.SiteTitle
	}
	return a.Config.EnvPrefix
}
