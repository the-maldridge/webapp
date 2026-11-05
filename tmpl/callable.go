package tmpl

import (
	"html/template"

	"github.com/flosch/pongo2/v6"
)

// Perms exposes a permission check as a value a template can call:
//
//	inject(ctx, "can", tmpl.Perms(req.Can))
//
//	{% if can "TASKS:EDIT" %}<a href="/tasks/new">New task</a>{% endif %}
//
// It exists because a template that wants to hide a control the viewer cannot
// use needs an answer from the authorization layer, and the two honest ways to
// give it one are a filter over a subject value and a per-request callable. A
// filter cannot reach the current request without closing over it, and a filter
// that closes over request state is a data race in a process-global registry,
// so the callable form is the one that is safe by construction: it is built for
// one request, captures that request, and is unreachable from any other.
//
// The returned value's signature is the one pongo2 requires of a callable in a
// render context, which is why this helper is here rather than a plain closure
// in the application core: pongo2 rejects a func(string) bool with an error
// about argument types that names neither the caller nor the fix.
func Perms(fn func(perm string) bool) any {
	if fn == nil {
		fn = func(string) bool { return false }
	}
	return func(perm *pongo2.Value) *pongo2.Value {
		return pongo2.AsValue(fn(perm.String()))
	}
}

// Constants exposes a string map for templates that iterate over it. It is a
// convenience for one call site in the core and is exported because a package
// that must not import pongo2 otherwise cannot produce a callable at all.
func Constant(value string) any {
	return func(*pongo2.Value) *pongo2.Value { return pongo2.AsValue(value) }
}

// SafeValue marks already-escaped markup so that a template prints it verbatim.
//
// It exists because pongo2 does not know the html/template types: a
// template.HTML in a render context is, to pongo2, a string, and its
// autoescaping turns a rendered <input> tag into visible markup. Anything this
// framework puts into a render context that is markup rather than text goes
// through here.
//
// The parameter type is html/template.HTML rather than a bare string
// deliberately: the caller has to be holding a value that html/template already
// considers trusted, which is a harder mistake to make than passing along a
// string that came out of a request body.
func SafeValue(v template.HTML) any { return pongo2.AsSafeValue(string(v)) }
