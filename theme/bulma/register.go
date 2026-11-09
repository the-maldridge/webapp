package bulma

import "github.com/the-maldridge/webapp"

// init registers this theme with the framework.
//
// Registration lives here rather than in the file holding the templates so that
// the choice to depend on the framework is visible as the package's only such
// dependency: everything else in the theme is data, and only this file knows it
// is plugging into anything.
//
// An application selects the theme by importing it for its effect, after which
// <PREFIX>_THEME=bulma or webapp.WithTheme("bulma") resolves to it. Without the
// import nothing happens, which is the point: a theme an application never
// mentioned is not a theme it should be able to be switched into by an
// environment variable, because nobody checked that it was deployed with one.
func init() {
	webapp.RegisterTheme(webapp.ThemeInfo{
		Name:     "bulma",
		FS:       FS,
		Static:   Static,
		Required: Required,
	})
}
