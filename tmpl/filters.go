package tmpl

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/flosch/pongo2/v6"
)

// Filters.
//
// pongo2's filter registry is a process-global map. That single fact dictates
// everything below: filters are registered in init(), never in a constructor,
// because a constructor-registered filter is a data race the moment two
// renderers are built concurrently, and it is a race that only appears in a
// test binary that builds renderers in parallel — which is to say, in the place
// it is least likely to be investigated.
//
// The same global map means a filter name is one name for the whole process.
// Framework filters therefore avoid names pongo2 already owns, and a
// registration that collides is remembered and reported at renderer
// construction rather than dropped: a missing filter in a template is an
// "unknown filter" error at parse time, and an engineer debugging that error
// deserves to be told the truth about why the filter is absent.
//
// Purity is a requirement, not a style preference. A filter that closes over
// request state is reachable from any goroutine rendering any request, and the
// race detector will find it eventually, on someone else's machine, in
// production-adjacent conditions. Anything that needs the current user, the
// current request, or the database belongs in the render context as a function
// the core provides, where it is called with the request it belongs to.

// registrationFailure holds the first filter registration error, if any. It is
// written only by init() and read afterwards, which is the ordering Go
// guarantees, so no locking is needed to make it visible.
var registrationFailure error

func init() {
	register("jsonify", filterJSONify)
	register("keyValue", filterKeyValue)
	register("pluralizeEN", filterPluralizeEN)
	register("hasPermission", filterHasPermission)
}

func register(name string, fn pongo2.FilterFunction) {
	if err := pongo2.RegisterFilter(name, fn); err != nil && registrationFailure == nil {
		registrationFailure = fmt.Errorf("%w: cannot register %q: %v", ErrFilterUnavailable, name, err)
	}
}

// filterRegistrationError reports a filter that could not be registered.
//
// Reported at renderer construction because that is the earliest point with
// somewhere to send it: init() has no error return, and a panic in init() is
// indistinguishable from a panic in a dependency's init() when the binary
// refuses to start.
func filterRegistrationError() error {
	return registrationFailure
}

// ErrFilterUnavailable reports a framework filter whose name another package
// had already claimed. Use errors.Is to test for it.
var ErrFilterUnavailable = errors.New("tmpl: filter name was already taken")

// filterJSONify renders a value as JSON.
//
// It returns a safe value because the output is markup-relevant only inside an
// attribute or a <script> block, and in both places the caller has already made
// the escaping decision that matters. The payload is escaped for HTML context
// regardless — Go's JSON encoder does that by default, and it is not turned
// off: a template that writes {{ record|jsonify|safe }} into a script tag is
// one hostile title away from executing whoever reads it.
func filterJSONify(in *pongo2.Value, param *pongo2.Value) (*pongo2.Value, *pongo2.Error) {
	if in.IsNil() {
		return pongo2.AsSafeValue("null"), nil
	}

	var out strings.Builder
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(true)
	if err := enc.Encode(in.Interface()); err != nil {
		return nil, &pongo2.Error{
			Sender:    "filter:jsonify",
			OrigError: fmt.Errorf("value of type %T cannot be encoded as JSON: %w", in.Interface(), err),
		}
	}
	return pongo2.AsSafeValue(strings.TrimSuffix(out.String(), "\n")), nil
}

// filterKeyValue renders a map as "key=value" pairs in sorted order.
//
// Sorting is the whole point. A Go map iterates in randomized order, and a
// template that renders one — a query string, a list of labels, a diff — gets a
// different document on every request. That defeats caching, makes page
// snapshots incomparable, and produces a test failure that appears roughly
// never on the machine that wrote it.

// kvPair is one entry of a map that is about to be printed in sorted order.
type kvPair struct {
	key   string
	value string
}

func filterKeyValue(in *pongo2.Value, param *pongo2.Value) (*pongo2.Value, *pongo2.Error) {
	sep := "="
	if param != nil && !param.IsNil() && param.IsString() {
		sep = param.String()
	}

	raw := in.Interface()
	if raw == nil {
		return pongo2.AsValue(""), nil
	}

	// Reflection rather than a type switch on map[string]any, because the maps
	// applications want printed are usually map[string]string or
	// map[string]int, and a filter that works on only one map type invites a
	// template that renders nothing at all when handed a different one.
	rv := reflect.ValueOf(raw)
	if rv.Kind() != reflect.Map {
		return nil, &pongo2.Error{
			Sender:    "filter:keyValue",
			OrigError: fmt.Errorf("expected a map, got %T", raw),
		}
	}

	pairs := make([]kvPair, 0, rv.Len())
	for _, k := range rv.MapKeys() {
		v := rv.MapIndex(k)
		value := ""
		if v.IsValid() && v.CanInterface() {
			value = fmt.Sprint(v.Interface())
		}
		pairs = append(pairs, kvPair{key: fmt.Sprint(k.Interface()), value: value})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].key < pairs[j].key })

	text := make([]string, 0, len(pairs))
	for _, p := range pairs {
		text = append(text, p.key+sep+p.value)
	}
	return pongo2.AsValue(strings.Join(text, "&")), nil
}

// filterPluralizeEN chooses a singular or plural noun for a count.
//
// {{ count|pluralizeEN:"task" }} renders "1 task" or "3 tasks". English
// pluralization is not attempted beyond appending an s, and the plural form is
// an argument so the template can supply the irregular one:
// {{ count|pluralizeEN:"person,people" }}.
//
// It is named for the language it implements because a framework that claims a
// filter called "pluralize" and implements only English is making a promise it
// will break for whoever localizes the application.
func filterPluralizeEN(in *pongo2.Value, param *pongo2.Value) (*pongo2.Value, *pongo2.Error) {
	singular, plural := "", "s"
	if param != nil && !param.IsNil() {
		parts := strings.Split(param.String(), ",")
		switch len(parts) {
		case 1:
			singular, plural = parts[0], parts[0]+"s"
		default:
			singular, plural = parts[0], parts[1]
		}
	}
	if in.IsNil() {
		return pongo2.AsValue(singular), nil
	}
	if in.Integer() == 1 {
		return pongo2.AsValue(singular), nil
	}
	return pongo2.AsValue(plural), nil
}

// PermissionChecker is what the hasPermission filter asks of the value a
// template hands it.
//
// An interface rather than a concrete user type, because this package must not
// import the authentication layer, and a copy of that layer's user made here
// would be a second definition of "holds a permission" — the kind of
// duplication that survives exactly long enough for the two copies to disagree
// about a grant. The application core supplies the implementation, since that
// is the one place both the user and the rules are available.
type PermissionChecker interface {
	HasPermission(perm string) bool
}

// filterHasPermission reports whether a value holds a named grant.
//
// The subject is the filter's input rather than something read from a global,
// which is what keeps this filter legal in a process-global registry:
// {{ user|hasPermission:"TASKS:EDIT" }} asks about the user the render context
// was built with, so two requests rendering at the same time cannot see each
// other's identity.
//
// A nil subject holds nothing, which is how anonymous requests behave in
// handlers. A non-nil subject that cannot answer the question is an error and
// not a quiet false: a navigation link that vanishes because a type stopped
// implementing an interface is a bug that hides for months, and it hides best
// in production, where nobody expects the link to be there at all.
func filterHasPermission(in *pongo2.Value, param *pongo2.Value) (*pongo2.Value, *pongo2.Error) {
	if in.IsNil() || param == nil || param.IsNil() {
		return pongo2.AsValue(false), nil
	}

	checker, ok := in.Interface().(PermissionChecker)
	if !ok {
		return nil, &pongo2.Error{
			Sender: "filter:hasPermission",
			OrigError: fmt.Errorf(
				"value of type %T cannot answer permission questions: it must implement tmpl.PermissionChecker",
				in.Interface()),
		}
	}
	return pongo2.AsValue(checker.HasPermission(param.String())), nil
}
