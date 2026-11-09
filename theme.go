package webapp

import (
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"

	"github.com/the-maldridge/webapp/theme/builtin"
	"github.com/the-maldridge/webapp/tmpl"
)

// Themes.
//
// A theme is the set of templates the framework renders itself, plus the
// stylesheet they link. The distinction from an application's own templates is
// that the framework renders these without being asked to: the base layout, the
// login form, the navigation fragment, and the three error pages.
//
// A theme replaces the framework's copies rather than sitting in front of them,
// which is why mounting is by substitution and not by overlay. Those templates
// name each other by absolute namespace ("@core/base.p2"), and an absolute name
// resolves inside exactly one root with no fallback, so an additional mounted
// root would be invisible to every one of them. Substitution is what makes the
// theme's base.p2 the one the framework's error pages extend.
//
// The consequence is that a theme is all-or-nothing, and that is deliberate: a
// theme is refused at startup unless it supplies every required template and
// they all compile. A theme that omitted errors/internal.p2 would not lose its
// styling on the error page, it would take the page away, and the page is what
// runs when the application is the thing that is broken.

// ThemeInfo describes a theme.
//
// The filesystems are functions rather than values because a theme's assets may
// need generating before they exist: a vendored CSS bundle is fetched by
// go:generate rather than committed, and a field of type fs.FS would force the
// embed directive to resolve when this package is compiled rather than when the
// theme is used.
type ThemeInfo struct {
	// Name identifies the theme in configuration. It is matched against
	// <PREFIX>_THEME and WithTheme, so it is a stable string once shipped.
	Name string

	// FS returns the theme's templates, rooted so that "base.p2" is the base
	// layout. Required names are resolved against it.
	FS func() fs.FS

	// Static returns the theme's assets, rooted at the framework's static
	// URL. A theme with nothing to serve returns nil, and a theme whose
	// base.p2 links a stylesheet is not such a theme.
	Static func() fs.FS

	// Required are the namespace-qualified templates the framework renders
	// itself and therefore must be able to compile: the base layout, the
	// login page, the navigation fragment, and the error pages. Every entry
	// has to name the framework's own namespace, because a bare name resolves
	// to whichever mounted root came first, so the startup check would pass
	// against an application's copy while the theme's was missing.
	Required []string
}

var (
	// themeMu guards the registry. Registration happens in package
	// initialisation; resolution reads it once during construction.
	themeMu sync.Mutex

	// themeRegistry holds every registered theme in registration order.
	themeRegistry []ThemeInfo
)

// init registers the framework's own theme.
//
// It is registered from here rather than from package builtin because builtin is
// the default and so must be reachable without an import by the application: a
// theme that has to be blank-imported to exist cannot also be the one that is
// there when nothing is imported. Third-party themes register themselves from
// their own init, which means the application's main imports them, which is the
// asymmetry: the default is linked, everything else is chosen.
func init() {
	RegisterTheme(ThemeInfo{
		Name:     BuiltinThemeName,
		FS:       builtin.FS,
		Static:   builtin.Static,
		Required: builtin.Required,
	})
}

// BuiltinThemeName is the name of the theme that ships with the framework, and
// the one used when configuration names no theme.
const BuiltinThemeName = "builtin"

// RegisterTheme adds a theme to the process-global registry so that
// <PREFIX>_THEME can select it by name.
//
// It is called from package initialisation, which is why it panics rather than
// returning an error: a theme that cannot be described correctly is a defect in
// the code being compiled, and the alternative is a configuration variable that
// silently never matches. An application that assembles its own themes should
// use WithTheme, which reports the same problems as errors.
//
// It panics on a duplicate name and on any defect validateTheme rejects.
func RegisterTheme(info ThemeInfo) {
	info, err := validateTheme(info)
	if err != nil {
		panic("webapp: RegisterTheme: " + err.Error())
	}

	themeMu.Lock()
	defer themeMu.Unlock()

	for _, existing := range themeRegistry {
		if existing.Name == info.Name {
			panic(fmt.Sprintf("webapp: RegisterTheme(%q): a theme with this name is already registered", info.Name))
		}
	}
	themeRegistry = append(themeRegistry, info)
}

// WithTheme selects the theme this application renders with, by name.
//
// The name must match a registered theme: builtin, or one registered by a
// package the application imported. A name that matches nothing is a
// construction error listing what is available, never a fall back to the
// default, because a deployment that believes it is serving a theme and is not
// is a visual defect that no test notices and every user does.
//
// Selecting a theme does not enable templating; WithNoTemplates still removes it,
// and with it the theme's templates and the framework's error pages alike.
func WithTheme(name string) Option {
	return func(c *Config) error {
		name = strings.TrimSpace(name)
		if name == "" {
			return errors.New("WithTheme: theme name may not be empty")
		}
		if _, err := lookupTheme(name); err != nil {
			return fmt.Errorf("WithTheme: %w", err)
		}
		c.Theme = name
		return nil
	}
}

// lookupTheme finds a registered theme by name.
//
// The error names the themes that are registered, because the usual cause is a
// theme package that was not imported, and "not found" without a list of what
// was found is not a hint anyone can act on.
func lookupTheme(name string) (ThemeInfo, error) {
	themeMu.Lock()
	defer themeMu.Unlock()

	for _, info := range themeRegistry {
		if info.Name == name {
			return info, nil
		}
	}
	return ThemeInfo{}, fmt.Errorf("theme %q is not registered (registered: %s; a theme outside the "+
		"framework's own must be imported for its registration to run)", name, strings.Join(lockedThemeNames(), ", "))
}

// themeNames returns the registered theme names, sorted.
func themeNames() []string {
	themeMu.Lock()
	defer themeMu.Unlock()
	return lockedThemeNames()
}

// lockedThemeNames reads the names without taking the lock. Callers hold it.
func lockedThemeNames() []string {
	names := make([]string, 0, len(themeRegistry))
	for _, info := range themeRegistry {
		names = append(names, info.Name)
	}
	sort.Strings(names)
	return names
}

// validateTheme checks a description in isolation and normalizes it. It is
// shared by RegisterTheme and WithTheme's lookup so that the two cannot drift
// into accepting different things.
func validateTheme(info ThemeInfo) (ThemeInfo, error) {
	info.Name = normalizeModuleName(info.Name)

	switch {
	case info.Name == "":
		return info, errors.New("theme name may not be empty")
	case !validModuleName(info.Name):
		return info, fmt.Errorf("theme name %q may not contain %q, a slash, a backslash, or whitespace",
			info.Name, tmpl.NamespaceSeparator)
	case info.Name == templateNamespace:
		return info, fmt.Errorf("theme name %q is the namespace themes are mounted under, "+
			"and cannot also be a name a theme is selected by", templateNamespace)
	case info.Name == debugNamespace:
		return info, fmt.Errorf("theme name %q is reserved for the template debug overlay", debugNamespace)
	case info.FS == nil:
		return info, fmt.Errorf("theme %q has no FS and would contribute no templates", info.Name)
	case len(info.Required) == 0:
		return info, fmt.Errorf("theme %q declares no required templates; every theme has to supply the "+
			"framework's error pages", info.Name)
	}

	// The slice is copied so that a caller mutating what it passed in cannot
	// change what was validated, which is the same guarantee modules give.
	info.Required = append([]string(nil), info.Required...)
	for _, name := range info.Required {
		ns, _, ok := tmpl.SplitKey(name)
		if !ok || ns != templateNamespace {
			return info, fmt.Errorf("theme %q: required template %q must be namespace-qualified as %q%s/... ; "+
				"a bare name resolves to whichever mounted root came first, so the startup check would pass "+
				"against an application's copy while the theme's is missing",
				info.Name, name, tmpl.NamespaceSeparator, templateNamespace)
		}
	}

	return info, nil
}

// resolveTheme decides which theme this application renders with and returns it.
//
// Empty configuration means the default. It is resolved once, here, rather than
// at each mount point, because the templates and the assets have to agree about
// which theme is active: a base layout from one theme linking a stylesheet from
// another is a page that renders and looks wrong, which is the class of defect
// that survives review because nobody checks the network tab during code review.
func resolveTheme(c *Config) (ThemeInfo, error) {
	name := c.Theme
	if name == "" {
		name = BuiltinThemeName
	}
	info, err := lookupTheme(name)
	if err != nil {
		return ThemeInfo{}, fmt.Errorf("webapp: %w", err)
	}
	return info, nil
}
