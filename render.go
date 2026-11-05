package webapp

import (
	"context"
	"io"
)

// Context is the data handed to a template on render.
type Context map[string]any

// Renderer turns a template name and a context into a response body.
// Package tmpl provides the pongo2 implementation; the indirection lets
// tests substitute a renderer that records what was asked for.
//
// Implementations must not write to w on error: the caller decides
// whether the failure becomes a 500, and a renderer that already wrote
// a partial body makes that impossible.
type Renderer interface {
	// Render writes the named template to out. Names use the
	// templating package's resolution rules, so "@core/errors/
	// notfound.p2" is namespace-explicit and "detail.p2" resolves
	// relative to the caller and then through the mounted roots.
	Render(ctx context.Context, out io.Writer, name string, data Context) error
}

// Shared values are functions called per request to produce a render
// context entry. They exist so that navigation, feature flags, and the
// current user reach every template without every handler remembering
// them.
//
// A Provider runs once per render, in the goroutine handling the
// request, and receives that request's context. It must not block on
// anything unbounded; a provider that queries the database should be
// the kind of query that belongs on a page load.
type Provider func(ctx context.Context) (any, error)

// ErrSkip tells a Provider to contribute nothing to the context rather
// than to fail the render. Returning an error other than ErrSkip logs
// at warning and renders without the key, because a missing sidebar
// entry is not a reason for a page to 500.
var ErrSkip = errSkip{}

type errSkip struct{}

func (errSkip) Error() string { return "webapp: provider skipped" }
