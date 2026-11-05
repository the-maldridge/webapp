package tmpl

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/flosch/pongo2/v6"
)

// Renderer turns a template name and a data map into a document.
//
// It is the only place pongo2 is touched. The application core holds it behind
// its own one-method interface, which is what lets the core's render funnel
// stay independent of the engine: a different engine is a different
// implementation of that interface, not a change of vocabulary everywhere a
// template is rendered.
type Renderer struct {
	set      *pongo2.TemplateSet
	registry *Registry

	// compiled records the canonical keys this renderer has already been
	// asked to compile, which is what makes Verify's report a list rather than
	// a first-failure error.
	compiled map[string]bool
	mu       sync.Mutex
}

// Option configures a Renderer.
type Option func(*Renderer) error

// WithDebug turns on pongo2's non-caching mode, so a template edited on disk is
// picked up by the next request rather than by the next build.
//
// It costs a parse per render, which is why it is a flag rather than something
// inferred from the presence of a directory root: a filesystem root mounted in
// production must not quietly become a debugging configuration.
func WithDebug(enabled bool) Option {
	return func(r *Renderer) error {
		r.set.Debug = enabled
		return nil
	}
}

// WithGlobals adds values available to every template without every handler
// passing them. They are the static part of the ambient context; the
// per-request part is the core's job.
func WithGlobals(name string, value any) Option {
	return func(r *Renderer) error {
		r.set.Globals[name] = value
		return nil
	}
}

// NewRenderer builds a renderer over a registry.
//
// The filters this package contributes are registered in init(), not here,
// because pongo2's filter registry is process-global and a registration
// performed from a constructor is a data race the moment two renderers are
// built concurrently — which is exactly what an application with a mail
// template set and a web template set does. A registration that failed because
// something else already claimed the name is reported here instead, since that
// is the first moment there is anywhere to report it to.
func NewRenderer(reg *Registry, opts ...Option) (*Renderer, error) {
	if reg == nil {
		return nil, errors.New("tmpl: NewRenderer requires a registry")
	}
	if len(reg.mounts) == 0 {
		return nil, errors.New("tmpl: registry has no template roots mounted")
	}

	r := &Renderer{
		set:      pongo2.NewSet("webapp", reg),
		registry: reg,
		compiled: map[string]bool{},
	}
	r.set.Globals = pongo2.Context{}

	for _, opt := range opts {
		if err := opt(r); err != nil {
			return nil, err
		}
	}
	if err := filterRegistrationError(); err != nil {
		return nil, err
	}
	return r, nil
}

// Render writes the named template to out.
//
// Names accept every form the registry resolves, including the bare form an
// application writes in a handler. A name that resolves nowhere is an error
// naming the namespaces searched, and nothing is written to out in that case:
// the caller decides whether a failure becomes an error page, and a renderer
// that already emitted half a document takes that choice away.
func (r *Renderer) Render(_ context.Context, out io.Writer, name string, data map[string]any) error {
	tpl, err := r.templateFor(name)
	if err != nil {
		return err
	}

	ctx, err := r.context(data)
	if err != nil {
		return err
	}

	if err := tpl.ExecuteWriter(ctx, out); err != nil {
		return fmt.Errorf("tmpl: %s: %w", name, err)
	}
	return nil
}

// RenderString renders a template to a string. It exists for the compile check
// and for anything that renders to a buffer — mail bodies, PDF preambles — and
// is deliberately not what the render funnel calls.
func (r *Renderer) RenderString(name string, data map[string]any) (string, error) {
	tpl, err := r.templateFor(name)
	if err != nil {
		return "", err
	}
	ctx, err := r.context(data)
	if err != nil {
		return "", err
	}
	out, err := tpl.Execute(ctx)
	if err != nil {
		return "", fmt.Errorf("tmpl: %s: %w", name, err)
	}
	return out, nil
}

// Verify compiles every named template and returns one error describing all of
// them that failed.
//
// This is what turns "the error page is missing" from a runtime surprise into a
// startup failure. That matters more than it looks: an error template is
// rendered only when something has already gone wrong, so the handful of
// circumstances in which it is broken coincide with the handful in which
// whoever is debugging needs it and can least afford to discover it.
func (r *Renderer) Verify(names ...string) error {
	var missing, broken []string
	for _, name := range names {
		key, found := r.registry.Lookup("", name)
		if !found {
			missing = append(missing, fmt.Sprintf("%s (searched: %s)", name, r.registry.describe()))
			continue
		}
		if _, err := r.set.FromCache(key); err != nil {
			broken = append(broken, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		r.mark(key)
	}

	if len(missing) == 0 && len(broken) == 0 {
		return nil
	}

	var b strings.Builder
	b.WriteString("tmpl: required templates unavailable")
	if len(missing) > 0 {
		sort.Strings(missing)
		fmt.Fprintf(&b, "\n  not found:\n    %s", strings.Join(missing, "\n    "))
	}
	if len(broken) > 0 {
		sort.Strings(broken)
		fmt.Fprintf(&b, "\n  failed to compile:\n    %s", strings.Join(broken, "\n    "))
	}
	return errors.New(b.String())
}

// Namespaces returns the mounted roots, in search order.
func (r *Renderer) Namespaces() []string { return r.registry.Namespaces() }

// Resolve reports the canonical key a name resolves to, and whether it resolved
// at all. It is the diagnostic for "why is this the copy of the template I am
// seeing", which is otherwise a question about mount order.
func (r *Renderer) Resolve(name string) (string, bool) {
	return r.registry.Lookup("", name)
}

// templateFor compiles, or fetches from pongo2's cache, the template behind a
// name.
func (r *Renderer) templateFor(name string) (*pongo2.Template, error) {
	key, found := r.registry.Lookup("", name)
	if !found {
		return nil, fmt.Errorf("tmpl: template %q not found: %s", name, r.registry.describe())
	}

	tpl, err := r.set.FromCache(key)
	if err != nil {
		return nil, fmt.Errorf("tmpl: %s: %w", key, err)
	}
	r.mark(key)
	return tpl, nil
}

func (r *Renderer) mark(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.compiled[key] = true
}

// Compiled returns the canonical keys this renderer has compiled, sorted. It
// answers "was this template ever used" without instrumenting handlers.
func (r *Renderer) Compiled() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	keys := make([]string, 0, len(r.compiled))
	for k := range r.compiled {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// identPattern is the shape pongo2 requires of a context key. It is reproduced
// rather than borrowed because the engine's copy is unexported and runs at
// render depth, where the failure is a template error pointing at a line of a
// template instead of at the handler that built the map.
var identPattern = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// checkIdentifiers rejects keys no template could name.
//
// Keys are visited in sorted order so that a context with two bad keys reports
// the same one every time; an error whose text changes between runs of the same
// binary is a test that fails on one machine.
func checkIdentifiers(ctx pongo2.Context) error {
	keys := make([]string, 0, len(ctx))
	for k := range ctx {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		if !identPattern.MatchString(k) {
			return fmt.Errorf("tmpl: render context key %q is not usable in a template: keys must match %s",
				k, identPattern)
		}
	}
	return nil
}

// context converts the data map into pongo2's context.
//
// Two conversions, both about escaping. Markup the application already escaped
// arrives as template.HTML — the core wraps the CSRF input that way — and
// pongo2 does not know that type, so it would escape it again and print a
// visible <input> tag; converting it to pongo2's own safe value carries the
// intent across the boundary. Errors are the mirror case: an engine calls
// String on a non-string and inserts the result unescaped, and an error's text
// routinely quotes a request body. The core already replaces errors with
// strings; doing it here too means a renderer used outside the core cannot
// become the XSS.
func (r *Renderer) context(data map[string]any) (pongo2.Context, error) {
	ctx := make(pongo2.Context, len(data))
	for k, v := range data {
		ctx[k] = prepareValue(v, 0)
	}
	if err := checkIdentifiers(ctx); err != nil {
		return nil, err
	}
	return ctx, nil
}

// maxPrepareDepth bounds the walk. Render contexts are shallow by construction
// and a cyclic one is a bug worth a stopped render rather than a hung request.
const maxPrepareDepth = 12

func prepareValue(v any, depth int) any {
	if depth > maxPrepareDepth {
		return v
	}

	switch t := v.(type) {
	case template.HTML:
		return pongo2.AsSafeValue(string(t))
	case *pongo2.Value:
		return v
	case error:
		return t.Error()
	case map[string]any:
		return prepareMap(t, depth+1)
	case []any:
		out := make([]any, 0, len(t))
		for _, elem := range t {
			out = append(out, prepareValue(elem, depth+1))
		}
		return out
	}
	return v
}

func prepareMap(m map[string]any, depth int) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = prepareValue(v, depth)
	}
	return out
}
