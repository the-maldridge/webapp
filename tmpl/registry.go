package tmpl

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
)

// NamespaceSeparator introduces an explicit namespace in a template name:
// "@core/errors/notfound.p2" is the core error page and nothing else. The
// character is not legal in a path segment any application is likely to
// invent, which is what makes the form collision-proof rather than merely
// conventional.
const NamespaceSeparator = "@"

// ErrUnknownNamespace reports a name whose namespace is not mounted.
var ErrUnknownNamespace = errors.New("tmpl: namespace is not mounted")

// mount is one root of the registry.
type mount struct {
	ns   string
	fsys fs.FS
}

// Registry is the set of template roots a loader searches.
//
// It is a value type guarded by nothing because it is built during startup
// and read concurrently afterwards; mounting after the first render is not
// supported and is not something the framework needs, since the roots are the
// embedded filesystems of the application and its modules, all of which exist
// before the first request.
type Registry struct {
	mounts []mount
}

// New returns an empty registry.
func New() *Registry { return &Registry{} }

// Mount adds a root under a namespace, searched after every root already
// registered.
//
// Order is the whole mechanism by which a theme overrides an application's
// template and an application overrides the framework's: the one registered
// first wins. The framework mounts its own root last for exactly that reason.
//
// A nil filesystem is ignored rather than fatal: a module that has no
// templates has nothing to mount, and requiring an empty FS just to satisfy a
// constructor produces embed directives over a directory of placeholder files.
func (r *Registry) Mount(ns string, fsys fs.FS) error {
	if fsys == nil {
		return nil
	}
	ns, err := cleanNamespace(ns)
	if err != nil {
		return err
	}
	if _, exists := r.lookup(ns); exists {
		return fmt.Errorf("tmpl: namespace %q is already mounted", ns)
	}
	r.mounts = append(r.mounts, mount{ns: ns, fsys: fsys})
	return nil
}

// MountFirst adds a root searched before every root already registered.
//
// It exists for the two cases that must win over everything embedded in the
// binary: a theme, and a developer's working directory in debug mode. Both are
// overlays, and an overlay that loses to the copy inside the binary is not an
// overlay.
func (r *Registry) MountFirst(ns string, fsys fs.FS) error {
	if fsys == nil {
		return nil
	}
	ns, err := cleanNamespace(ns)
	if err != nil {
		return err
	}
	if _, exists := r.lookup(ns); exists {
		return fmt.Errorf("tmpl: namespace %q is already mounted", ns)
	}
	r.mounts = append([]mount{{ns: ns, fsys: fsys}}, r.mounts...)
	return nil
}

// Namespaces returns the mounted namespaces in search order. It is the
// diagnostic that makes "which copy of this template am I looking at"
// answerable without reading startup code.
func (r *Registry) Namespaces() []string {
	names := make([]string, 0, len(r.mounts))
	for _, m := range r.mounts {
		names = append(names, m.ns)
	}
	return names
}

// Mounted reports whether a namespace is registered.
func (r *Registry) Mounted(ns string) bool {
	_, ok := r.lookup(ns)
	return ok
}

func (r *Registry) lookup(ns string) (mount, bool) {
	for _, m := range r.mounts {
		if m.ns == ns {
			return m, true
		}
	}
	return mount{}, false
}

// CleanNamespace validates a namespace and returns its canonical form.
//
// Exported so that a caller mounting a root can report a bad namespace at the
// point it was configured rather than at the point it was used.
func CleanNamespace(ns string) (string, error) { return cleanNamespace(ns) }

// cleanNamespace validates a namespace.
//
// The rules are short because the character set is what the resolver has to
// work with: a namespace containing the separator or a path separator would
// make a canonical key ambiguous to split, and an ambiguous key means a
// template that resolves differently than it was written.
func cleanNamespace(ns string) (string, error) {
	ns = strings.TrimSpace(ns)
	if ns == "" {
		return "", errors.New("tmpl: namespace must not be empty")
	}
	if strings.ContainsAny(ns, NamespaceSeparator+"/\\") {
		return "", fmt.Errorf("tmpl: namespace %q must not contain %q, a slash, or a backslash",
			ns, NamespaceSeparator)
	}
	if ns == "." || ns == ".." {
		return "", fmt.Errorf("tmpl: namespace %q is reserved", ns)
	}
	return ns, nil
}

// SplitKey divides a canonical "@ns/rest" key into its parts.
func SplitKey(key string) (ns, rest string, ok bool) {
	if !strings.HasPrefix(key, NamespaceSeparator) {
		return "", "", false
	}
	nsPart, restPart, found := strings.Cut(strings.TrimPrefix(key, NamespaceSeparator), "/")
	if !found || nsPart == "" || restPart == "" {
		return "", "", false
	}
	return nsPart, restPart, true
}

// Canonical returns the "@ns/rest" form of a name inside a namespace.
func Canonical(ns, rest string) string {
	return NamespaceSeparator + ns + "/" + strings.TrimPrefix(rest, "/")
}

// Abs resolves a template name to the canonical key to open, and is the
// method pongo2 uses to name a template.
//
// Three forms, in order of specificity:
//
//  1. "@ns/rest" resolves inside that namespace alone. A name that says where
//     it came from means what it says, which is what the framework relies on
//     for its own error pages: an application or theme cannot shadow them,
//     and a missing core template is an error rather than a surprise
//     substitution.
//  2. A bare name is resolved relative to the directory of the template that
//     referenced it, so {% include "sidebar.p2" %} finds the sibling the author
//     meant regardless of how deep the tree is.
//  3. A bare name is tried in each mounted root in registration order.
//
// A name that resolves nowhere is returned unchanged. Abs has no way to report
// failure, and inventing a path that does not exist is better than the
// alternative — returning a different template's key — because the caller's
// next move is Get, which can name every root it searched.
func (r *Registry) Abs(base, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return name
	}

	if strings.HasPrefix(name, NamespaceSeparator) {
		ns, rest, ok := SplitKey(name)
		if !ok {
			return name
		}
		if m, mounted := r.lookup(ns); mounted && hasFile(m, rest) {
			return Canonical(ns, rest)
		}
		return name
	}

	clean := pathClean(name)

	// Relative to the referencing template. base is itself a canonical key,
	// because pongo2 passes the parent's name and every name this loader hands
	// back is canonical.
	//
	// The name is joined before it is cleaned, which is the part that matters:
	// cleaning first resolves "../x.p2" against the current directory, which
	// this loader has no notion of, and turns a climb out of the referencing
	// template's directory into a climb out of the root.
	if bns, brest, ok := SplitKey(base); ok {
		if m, mounted := r.lookup(bns); mounted {
			if rel := pathJoin(pathDir(brest), name); hasFile(m, rel) {
				return Canonical(bns, rel)
			}
		}
	}

	for _, m := range r.mounts {
		if hasFile(m, clean) {
			return Canonical(m.ns, clean)
		}
	}

	return name
}

// Lookup resolves a name and reports truthfully whether a template was found.
//
// Abs has no way to report failure, so a name that resolved nowhere comes back
// from it looking exactly like a name that resolved somewhere — including the
// plausible-looking "@ghost/x.p2". Anything that has to distinguish the two,
// which is everything that produces an error message or a startup check, goes
// through here rather than trusting what Abs returned.
func (r *Registry) Lookup(base, name string) (string, bool) {
	key := r.Abs(base, name)
	ns, rest, ok := SplitKey(key)
	if !ok {
		return "", false
	}
	m, mounted := r.lookup(ns)
	if !mounted || !hasFile(m, rest) {
		return "", false
	}
	return key, true
}

// Get opens a resolved key.
//
// A key that is not canonical is probed as a bare name, because pongo2 calls
// Get with the result of Abs and a name Abs could not resolve arrives here
// unchanged. The error then lists the namespaces that were searched, which is
// the difference between a thirty-second fix and a guess about mount order.
func (r *Registry) Get(path string) (io.Reader, error) {
	ns, rest, ok := SplitKey(path)
	if !ok {
		ns, rest, ok = SplitKey(r.Abs("", path))
		if !ok {
			return nil, fmt.Errorf("tmpl: cannot resolve %q: no mounted namespace contains it (%s)",
				path, r.describe())
		}
	}

	m, exists := r.lookup(ns)
	if !exists {
		return nil, fmt.Errorf("%w: %q while resolving %q (%s)",
			ErrUnknownNamespace, ns, path, r.describe())
	}

	f, err := m.fsys.Open(rest)
	if err != nil {
		return nil, fmt.Errorf("tmpl: %s: %w", Canonical(ns, rest), err)
	}
	return f, nil
}

// describe names the mounted roots in an error message.
func (r *Registry) describe() string {
	if len(r.mounts) == 0 {
		return "no template roots are mounted"
	}
	return "searched " + strings.Join(r.Namespaces(), ", ")
}

// hasFile reports whether a path is present in a root.
//
// Stat rather than Open: Open on a directory would report success for a name
// that is a directory, and a directory is not a template.
func hasFile(m mount, name string) bool {
	if name == "" {
		return false
	}
	f, err := fs.Stat(m.fsys, name)
	return err == nil && !f.IsDir()
}

// pathClean normalizes a name inside a root: "./x.p2" and "a/../x.p2" become
// "x.p2", and a leading slash that an fs.FS would reject is dropped.
//
// Cleaning against a virtual root is the point: path.Clean("/"+"../x") is
// "/x", so a name that climbs above the root is clamped to it rather than
// returned with the climb intact.
func pathClean(name string) string {
	return strings.TrimPrefix(path.Clean("/"+name), "/")
}

// pathJoin joins within a root, clamping at the root's top.
//
// The clamp is why this is not path.Join: {% include "../secrets.p2" %} from
// inside a mounted root has to be a miss rather than a read of a file that was
// never a template. With an embedded directory the file would simply not be
// there, so without the clamp the safety of the behaviour would depend on how
// the application happened to be packaged rather than on the application.
func pathJoin(dir, name string) string {
	return pathClean(dir + "/" + name)
}

// pathDir is the directory of a path within a root, "" for a top-level name.
func pathDir(name string) string {
	i := strings.LastIndexByte(name, '/')
	if i < 0 {
		return ""
	}
	return name[:i]
}
