// Package tmpl resolves template names across several roots and renders them
// with pongo2.
//
// It exists because template loading is the layer where hand-wired
// applications are most fragile. Relative paths couple a template's identity
// to its depth in the tree, shared partials get duplicated because there is
// no stable name for them, and error templates get referenced before they are
// written and fail on first use — usually at the moment of an outage.
//
// A Registry holds named roots. One application embeds its own, the framework
// contributes its own, a theme or a developer's working directory can be put
// in front of both:
//
//	reg := tmpl.New()
//	reg.Mount("tasks", tasksP2) // the application's embedded p2 directory
//	reg.Mount("core", uiP2)     // framework templates, mounted last
//
//	// The loader implements pongo2.TemplateLoader; the renderer wraps it.
//	r, err := tmpl.NewRenderer(reg)
//
// A name is resolved in this order:
//
//  1. "@ns/rest" resolves inside that namespace and nowhere else. This is the
//     collision-proof form, and it is what the framework uses for its own
//     templates so that no application can shadow an error page.
//  2. A bare name is tried relative to the directory of the template doing the
//     referencing, so sibling includes work unchanged at any depth.
//  3. A bare name is then tried in each mounted root, in registration order.
//
// Abs returns the canonical "@ns/rest" key for whatever it resolved, and that
// key is what pongo2 caches under. One physical template therefore compiles
// once no matter which of the three spellings reached it.
//
// Get is the matching open. Abs never fails, so a name that resolves nowhere
// keeps the spelling it was asked for and the failure surfaces from Get, where
// it can name every place it looked.
package tmpl
