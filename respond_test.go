package webapp

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestWantsJSON(t *testing.T) {
	t.Parallel()

	cases := []struct {
		accept string
		want   bool
	}{
		{"", false},
		{"*/*", false},
		{"application/json", true},
		{"application/json; charset=utf-8", true},
		{"application/vnd.api+json", true},
		{"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", false},
		{"text/html;q=0.9,application/json", false},

		// Preference order wins, which is what makes the browser case work
		// without special-casing it: a browser lists text/html first, so it
		// gets a page, and a client that lists json first asked for json.
		{"application/json,text/html", true},
		{"image/png", false},
	}

	for _, tc := range cases {
		if got := wantsJSON(acceptHeader(tc.accept)); got != tc.want {
			t.Errorf("wantsJSON(%q) = %v, want %v", tc.accept, got, tc.want)
		}
	}
}

func TestWantsHTML(t *testing.T) {
	t.Parallel()

	cases := []struct {
		accept string
		want   bool
	}{
		// No stated preference is not a request for a page, which is what
		// keeps this the mirror of wantsJSON rather than a second way of
		// asking the same question: an unnamed default stays whatever the
		// response layer already defaults to.
		{"", false},
		{"*/*", false},
		{"application/json", false},
		{"image/png", false},
		{"text/html", true},
		{"text/html; charset=utf-8", true},
		{"application/xhtml+xml", true},

		// The same preference-order rule as wantsJSON: a browser lists
		// text/html first even though its Accept mentions json and xml, and
		// the first type is the one that describes what it will show.
		{"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", true},
		{"application/json,text/html", false},
		{"text/html;q=0.9,application/json", true},
	}

	for _, tc := range cases {
		if got := wantsHTML(acceptHeader(tc.accept)); got != tc.want {
			t.Errorf("wantsHTML(%q) = %v, want %v", tc.accept, got, tc.want)
		}
	}
}

func TestSanitizeConvertsErrorsToStrings(t *testing.T) {
	// An error in a render context is routine: reporting why something
	// failed is what a page does. The engine autoescapes strings but calls
	// methods on other values, so the error must become a string before it
	// gets anywhere near a template.
	ctx := Context{
		"err":     errors.New(`<script>alert(1)</script> failed`),
		"nested":  Context{"err": fmt.Errorf("wrapped %v", errors.New("boom"))},
		"asMap":   map[string]any{"err": errors.New("inner")},
		"slice":   []any{errors.New("first"), "plain"},
		"keepMe":  "untouched",
		"number":  42,
		"nothing": (*int)(nil),
	}

	sanitized := sanitize(ctx, 0)

	for _, key := range []string{"err", "nested", "asMap", "slice"} {
		if _, ok := sanitized[key].(error); ok {
			t.Errorf("%q is still an error after sanitize", key)
		}
	}

	if got, ok := sanitized["err"].(string); !ok {
		t.Errorf("err = %T, want string", sanitized["err"])
	} else if !strings.Contains(got, "failed") {
		t.Errorf("err = %q, want the error text preserved", got)
	}

	if nested, ok := sanitized["nested"].(Context); !ok {
		t.Errorf("nested = %T, want Context", sanitized["nested"])
	} else if _, ok := nested["err"].(string); !ok {
		t.Errorf("nested.err = %T, want string", nested["err"])
	}

	if asMap, ok := sanitized["asMap"].(Context); !ok {
		t.Errorf("asMap = %T, want Context", sanitized["asMap"])
	} else if _, ok := asMap["err"].(string); !ok {
		t.Errorf("asMap.err = %T, want string", asMap["err"])
	}

	list, ok := sanitized["slice"].([]any)
	if !ok {
		t.Fatalf("slice = %T, want []any", sanitized["slice"])
	}
	if _, ok := list[0].(string); !ok {
		t.Errorf("slice[0] = %T, want string", list[0])
	}

	if got, ok := sanitized["keepMe"].(string); !ok || got != "untouched" {
		t.Errorf("keepMe = %v, want it left alone", sanitized["keepMe"])
	}
	if got, ok := sanitized["number"].(int); !ok || got != 42 {
		t.Errorf("number = %v, want it left alone", sanitized["number"])
	}
	if sanitized["nothing"] != nil {
		t.Errorf("nothing = %v, want a nil pointer to become nil", sanitized["nothing"])
	}
}

func TestSanitizeStopsAtDepth(t *testing.T) {
	// A render context that references itself would hang an unbounded
	// walk. The bound is a design statement, and this pins it.
	ctx := Context{}
	current := ctx
	for i := 0; i < maxSanitizeDepth+4; i++ {
		next := Context{}
		current["child"] = next
		current = next
	}
	current["err"] = errors.New("deep")

	done := make(chan struct{})
	go func() {
		defer close(done)
		sanitize(ctx, 0)
	}()

	select {
	case <-done:
	case <-timeoutAfterTest():
		t.Fatal("sanitize did not terminate on a deeply nested context")
	}
}

// TestRenderFailureAfterHeadersOnlyLogs covers the case where a template
// fails partway through: the status is already sent, so the only correct
// action is to record the cause and stop writing.
func TestRenderFailureAfterHeadersOnlyLogs(t *testing.T) {
	a := newTestApp(t)
	rec := &recorder{}
	a.Config.Logger = slogWith(rec)
	a.Renderer = &stubRenderer{err: errors.New("template blew up"), prefix: "<html>partial"}

	a.Route("/broken", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.View(w, r, "page.p2", Context{})
		})
	})

	out := a.get(t, "/broken")
	if out.Code != http.StatusOK {
		t.Errorf("status = %d, want the already-committed 200; a status cannot be changed after the body started", out.Code)
	}
	if !strings.Contains(out.Body.String(), "<html>partial") {
		t.Errorf("body = %q, want the partial output left as it was", out.Body.String())
	}
	if strings.Contains(out.Body.String(), "template blew up") {
		t.Error("the template error must not be written into the page")
	}
	if _, ok := rec.find("template execution failed"); !ok {
		t.Error("a render failure must be logged with its cause")
	}
}

func TestViewWithNoRendererFails(t *testing.T) {
	a := newTestApp(t)
	a.Renderer = nil

	a.Route("/page", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.View(w, r, "page.p2", Context{})
		})
	})

	rec := a.get(t, "/page")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500: a missing renderer is a server fault", rec.Code)
	}
}

func TestRejectionWithNoRendererKeepsClientStatus(t *testing.T) {
	a := newTestApp(t)
	a.Renderer = nil

	a.Route("/tasks", func(r Router) {
		r.PostErr("/new", func(w http.ResponseWriter, r *http.Request) error {
			return &Rejected{Template: "task_form.p2", Reasons: []string{"title is required"}}
		})
	})

	rec := a.postForm(t, "/tasks/new", "")
	// The submission was refused on its own merits. Falling back to 500
	// because the re-render could not happen blames the server for the
	// client's input.
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}
}

func TestLevelForStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status int
		want   string
	}{
		{0, "INFO"},
		{200, "INFO"},
		{303, "INFO"},
		{400, "WARN"},
		{404, "WARN"},
		{500, "ERROR"},
		{503, "ERROR"},
	}

	for _, tc := range cases {
		if got := levelForStatus(tc.status).String(); got != tc.want {
			t.Errorf("levelForStatus(%d) = %s, want %s", tc.status, got, tc.want)
		}
	}
}
