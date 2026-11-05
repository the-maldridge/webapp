package tmpl

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/flosch/pongo2/v6"
)

func TestFilterRegistrationSucceeded(t *testing.T) {
	// A collision is only interesting if the framework noticed. Registration
	// failures are recorded in init() and surfaced at renderer construction, so
	// a build in which another package claimed "jsonify" fails here rather than
	// shipping a template that errors at parse time.
	if err := filterRegistrationError(); err != nil {
		t.Fatalf("filterRegistrationError: %v", err)
	}
}

func TestRegisterReportsTakenNames(t *testing.T) {
	// register is this package's wrapper around pongo2's global registry, so a
	// real collision is the only honest way to test it. The name is one this
	// package does not ship, and the recorded failure is restored afterwards:
	// leaving it set would make every other test in the package fail at
	// NewRenderer, which is a confusing way to report a passed test.
	if err := pongo2.RegisterFilter("tmplTestOnlyFilter", filterJSONify); err != nil {
		t.Fatalf("could not claim a test filter name: %v", err)
	}

	defer func(saved error) { registrationFailure = saved }(registrationFailure)
	register("tmplTestOnlyFilter", filterJSONify)

	if registrationFailure == nil {
		t.Fatal("register did not record a collision")
	}
	if !errors.Is(registrationFailure, ErrFilterUnavailable) {
		t.Errorf("collision error %v does not wrap ErrFilterUnavailable", registrationFailure)
	}
	if !strings.Contains(registrationFailure.Error(), "tmplTestOnlyFilter") {
		t.Errorf("collision error %q does not name the filter", registrationFailure)
	}
}

func TestJSONifyFilter(t *testing.T) {
	r := newTestRenderer(t, map[string]string{
		"p.p2": `{{ value|jsonify }}`,
	})

	out := render(t, r, "p.p2", map[string]any{"value": map[string]string{"a": "<b>"}})
	// Escaped for HTML context, and not escaped again by autoescaping.
	if out != `{"a":"\u003cb\u003e"}` {
		t.Errorf("got %q", out)
	}

	if out := render(t, r, "p.p2", map[string]any{"value": nil}); out != "null" {
		t.Errorf("nil: got %q", out)
	}
}

func TestJSONifyRejectsUnencodableValues(t *testing.T) {
	r := newTestRenderer(t, map[string]string{"p.p2": `{{ value|jsonify }}`})

	var buf strings.Builder
	err := r.Render(t.Context(), &buf, "p.p2", map[string]any{"value": make(chan int)})
	if err == nil {
		t.Fatal("jsonify accepted a value it cannot encode")
	}
	if !strings.Contains(err.Error(), "JSON") {
		t.Errorf("error %q does not explain the failure", err)
	}
}

func TestKeyValueSorts(t *testing.T) {
	r := newTestRenderer(t, map[string]string{"p.p2": `{{ value|keyValue }}`})

	// The map literal is written out of order on purpose; the output must not
	// depend on Go's randomized iteration.
	out := render(t, r, "p.p2", map[string]any{
		"value": map[string]string{"zebra": "1", "apple": "2", "mango": "3"},
	})
	// Autoescaping turns the joiner into &amp;, which is what HTML wants in
	// text and in an href alike; the filter's job is the ordering, not the
	// escaping decision.
	if out != "apple=2&amp;mango=3&amp;zebra=1" {
		t.Errorf("got %q", out)
	}
}

func TestKeyValueHandlesOtherMapTypes(t *testing.T) {
	r := newTestRenderer(t, map[string]string{"p.p2": `{{ value|keyValue:"/" }}`})

	out := render(t, r, "p.p2", map[string]any{"value": map[string]int{"b": 2, "a": 1}})
	if out != "a/1&amp;b/2" {
		t.Errorf("custom separator: got %q", out)
	}

	if out := render(t, r, "p.p2", map[string]any{"value": nil}); out != "" {
		t.Errorf("nil map: got %q", out)
	}
}

func TestKeyValueRejectsNonMaps(t *testing.T) {
	r := newTestRenderer(t, map[string]string{"p.p2": `{{ value|keyValue }}`})

	err := r.Render(t.Context(), &strings.Builder{}, "p.p2", map[string]any{"value": 42})
	if err == nil {
		t.Fatal("keyValue accepted a non-map")
	}
	if !strings.Contains(err.Error(), "expected a map") {
		t.Errorf("error %q does not say what it wanted", err)
	}
}

func TestPluralizeEN(t *testing.T) {
	r := newTestRenderer(t, map[string]string{
		"one.p2": `{{ n|pluralizeEN:"task" }}`,
		"irr.p2": `{{ n|pluralizeEN:"person,people" }}`,
	})

	tests := []struct{ tpl, count, want string }{
		{"one.p2", "1", "task"},
		{"one.p2", "2", "tasks"},
		{"one.p2", "0", "tasks"},
		{"irr.p2", "1", "person"},
		{"irr.p2", "9", "people"},
	}
	for _, tc := range tests {
		n, err := strconv.Atoi(tc.count)
		if err != nil {
			t.Fatalf("bad count %q: %v", tc.count, err)
		}
		out := render(t, r, tc.tpl, map[string]any{"n": n})
		if out != tc.want {
			t.Errorf("%s with %s = %q, want %q", tc.tpl, tc.count, out, tc.want)
		}
	}
}

type checker map[string]bool

func (c checker) HasPermission(perm string) bool { return c[perm] }

func TestHasPermissionFilter(t *testing.T) {
	r := newTestRenderer(t, map[string]string{
		"p.p2": `{% if user|hasPermission:"TASKS:EDIT" %}yes{% else %}no{% endif %}`,
	})

	out := render(t, r, "p.p2", map[string]any{"user": checker{"TASKS:EDIT": true}})
	if out != "yes" {
		t.Errorf("held permission: got %q", out)
	}
	out = render(t, r, "p.p2", map[string]any{"user": checker{"TASKS:VIEW": true}})
	if out != "no" {
		t.Errorf("unheld permission: got %q", out)
	}
	out = render(t, r, "p.p2", map[string]any{"user": nil})
	if out != "no" {
		t.Errorf("anonymous: got %q", out)
	}
}

func TestHasPermissionRejectsUnanswerableSubjects(t *testing.T) {
	r := newTestRenderer(t, map[string]string{
		"p.p2": `{{ user|hasPermission:"TASKS:EDIT" }}`,
	})

	err := r.Render(t.Context(), &strings.Builder{}, "p.p2", map[string]any{"user": "not-a-user"})
	if err == nil {
		t.Fatal("a subject that cannot answer was accepted")
	}
	if !strings.Contains(err.Error(), "PermissionChecker") {
		t.Errorf("error %q does not name the interface to implement", err)
	}
}
