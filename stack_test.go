package webapp

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPatternMatches(t *testing.T) {
	t.Parallel()

	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"/", "/", true},
		{"/things", "/things", true},

		// A route mounted as "/" is walked as "/things/" but also answers
		// the unslashed path, so a trailing empty segment in the pattern is
		// optional. The reverse does not hold: a pattern registered with an
		// explicit path is matched exactly, and chi sends the unslashed
		// spelling to a 404 rather than to it.
		{"/things/", "/things", true},
		{"/things", "/things/", false},
		{"/things", "/other", false},
		{"/things", "/things/1", false},
		{"/things", "/thin", false},
		{"/things/{id}", "/things/7", true},
		{"/things/{id}", "/things/", false},
		{"/things/{id}", "/things", false},
		{"/things/{id}/parts", "/things/7/parts", true},
		{"/things/{id}/parts", "/things/7/parts/extra", false},
		{"/assets/*", "/assets/css/site.css", true},
		{"/assets/*", "/assets/", true},
		{"/assets/*", "/assets", false},
		{"/files/{dir}/*", "/files/a/b/c.txt", true},
		{"/files/{dir}/*", "/files/a", false},
	}

	for _, tc := range cases {
		if got := patternMatches(tc.pattern, tc.path); got != tc.want {
			t.Errorf("patternMatches(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestMethodNotAllowedSetsAllow(t *testing.T) {
	a := newTestApp(t)

	a.Route("/things", func(r Router) {
		r.Get("/", okHandler)
		r.Post("/", okHandler)
		r.Patch("/", okHandler)
		r.Delete("/", okHandler)
	})

	rec := a.send(t, httptest.NewRequest(http.MethodPut, "/things", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}

	// A 405 without Allow leaves the client guessing. The list must be
	// sorted, de-duplicated, and must include HEAD wherever GET appears.
	const want = "DELETE, GET, HEAD, PATCH, POST"
	if got := rec.Header().Get("Allow"); got != want {
		t.Errorf("Allow = %q, want %q", got, want)
	}
}

// TestAllowCoversOnlyMatchingPattern checks that the verbs advertised for a
// parameterized route are not leaked onto a sibling pattern that a client
// cannot use them on.
func TestAllowCoversOnlyMatchingPattern(t *testing.T) {
	a := newTestApp(t)

	a.Route("/things", func(r Router) {
		r.Post("/", okHandler)
		r.Patch("/", okHandler)
		r.Delete("/", okHandler)
		r.Get("/{id}", okHandler)
	})

	rec := a.postForm(t, "/things/7", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got, want := rec.Header().Get("Allow"), "GET, HEAD"; got != want {
		t.Errorf("Allow = %q, want %q: /things/7 is a different pattern than /things", got, want)
	}
}

func TestNotFoundHasNoAllowHeader(t *testing.T) {
	a := newTestApp(t)
	a.Route("/things", func(r Router) { r.Get("/", okHandler) })

	rec := a.get(t, "/nowhere")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "" {
		t.Errorf("Allow = %q on a 404, want none: nothing matches this path", got)
	}
}

// TestNoRoutesStillRunsMiddleware is the regression guard for chi's
// empty-mux short circuit: a mux with no routes answers its own not-found
// without running any of its middleware, which is why the framework's
// chain wraps the router instead of being installed with Use.
func TestNoRoutesStillRunsMiddleware(t *testing.T) {
	a := newTestApp(t)

	rec := a.get(t, "/anything")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Error("a 404 from an empty router must still carry a correlation ID")
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff on every response", got)
	}

	// Liveness must not depend on an application having mounted routes:
	// a process that failed to wire anything is still a live process,
	// and killing it on a routing bug turns a bad page into an outage.
	if rec := a.get(t, HeartbeatPath); rec.Code != http.StatusOK {
		t.Errorf("heartbeat on an empty router = %d, want 200", rec.Code)
	}
}

func TestBodyLimitRejectsOversizedPost(t *testing.T) {
	a := newTestApp(t, WithMaxBodyBytes(16))

	a.Route("/submit", func(r Router) {
		r.Post("/", func(w http.ResponseWriter, r *http.Request) {
			// The handler must never see an oversized body. Reading it is
			// what surfaces the limit, so this is where an application
			// would normally be parsing a form.
			if _, err := FormValues(r); err != nil {
				a.dispatchError(w, r, err)
				return
			}
			w.WriteHeader(http.StatusOK)
		})
	})

	body := strings.Repeat("a", 4096)
	rec := a.postForm(t, "/submit", "payload="+body)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}

	// The small case still works, which is what distinguishes a limit
	// from a broken route.
	if rec := a.postForm(t, "/submit", "payload=small"); rec.Code != http.StatusOK {
		t.Errorf("small POST status = %d, want 200", rec.Code)
	}
}

func TestMaxBytesErrorIsRecognized(t *testing.T) {
	t.Parallel()

	if !isBodyTooLarge(&http.MaxBytesError{Limit: 16}) {
		t.Error("isBodyTooLarge must recognize *http.MaxBytesError")
	}
	if isBodyTooLarge(errors.New("something else")) {
		t.Error("isBodyTooLarge must not match an unrelated error")
	}
	if isBodyTooLarge(nil) {
		t.Error("isBodyTooLarge(nil) must be false")
	}
}

func TestClientIPIgnoresSpoofedForwardedHeaders(t *testing.T) {
	a := newTestApp(t)

	var got string
	a.Route("/whoami", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			got = ClientIP(r)
			w.WriteHeader(http.StatusNoContent)
		})
	})

	const remote = "203.0.113.9:5555"

	req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	req.RemoteAddr = remote
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	req.Header.Set("X-Real-IP", "10.0.0.2")
	req.Header.Set("True-Client-IP", "10.0.0.3")

	if rec := a.send(t, req); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}

	// With no trusted proxy configured, every forwarded header is the
	// client's own claim, and the connection is the only honest source.
	if got != "203.0.113.9" {
		t.Errorf("ClientIP = %q, want the connection address 203.0.113.9; trusting a header any client can set makes audit logs and rate limiters worthless", got)
	}
}

// TestClientIPIgnoresForwardedHeadersFromUnlistedConnection checks that a
// configured proxy network is a gate and not only a skip list: a request
// that did not arrive over one of those networks cannot influence its own
// recorded address.
func TestClientIPIgnoresForwardedHeadersFromUnlistedConnection(t *testing.T) {
	a := newTestApp(t, WithTrustedProxies("10.0.0.0/8"))

	var got string
	a.Route("/whoami", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			got = ClientIP(r)
			w.WriteHeader(http.StatusNoContent)
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	req.RemoteAddr = "198.51.100.7:4444"
	req.Header.Set("X-Forwarded-For", "10.0.0.1, 10.0.0.2")
	a.send(t, req)

	if got != "198.51.100.7" {
		t.Errorf("ClientIP = %q, want 198.51.100.7 when the connection is not a trusted proxy", got)
	}
}

func TestClientIPFromTrustedProxy(t *testing.T) {
	a := newTestApp(t, WithTrustedProxies("10.0.0.0/8"))

	var got string
	a.Route("/whoami", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			got = ClientIP(r)
			w.WriteHeader(http.StatusNoContent)
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	req.RemoteAddr = "10.0.0.5:4444"
	req.Header.Set("X-Forwarded-For", "198.51.100.23")
	a.send(t, req)

	if got != "198.51.100.23" {
		t.Errorf("ClientIP = %q, want 198.51.100.23 from a trusted proxy's X-Forwarded-For", got)
	}
}

// TestClientIPFromTrustedProxyChain checks that the trusted network is
// skipped rather than reported, which is the difference between showing an
// operator their own load balancer and showing them the client.
func TestClientIPFromTrustedProxyChain(t *testing.T) {
	a := newTestApp(t, WithTrustedProxies("10.0.0.0/8"))

	var got string
	a.Route("/whoami", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			got = ClientIP(r)
			w.WriteHeader(http.StatusNoContent)
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	req.RemoteAddr = "10.0.0.5:4444"
	req.Header.Set("X-Forwarded-For", "203.0.113.5, 10.0.0.9")
	a.send(t, req)

	if got != "203.0.113.5" {
		t.Errorf("ClientIP = %q, want 203.0.113.5 with trusted hops skipped", got)
	}
}

func TestClientIPFromConfiguredHeader(t *testing.T) {
	a := newTestApp(t, WithClientIPHeader("X-Real-IP"))

	var got string
	a.Route("/whoami", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			got = ClientIP(r)
			w.WriteHeader(http.StatusNoContent)
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	req.RemoteAddr = "10.0.0.5:4444"
	req.Header.Set("X-Real-IP", "203.0.113.11")
	a.send(t, req)

	if got != "203.0.113.11" {
		t.Errorf("ClientIP = %q, want 203.0.113.11 from the header the application named", got)
	}
}

func TestClientIPWithProxyCount(t *testing.T) {
	a := newTestApp(t, WithTrustedProxyCount(2))

	var got string
	a.Route("/whoami", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			got = ClientIP(r)
			w.WriteHeader(http.StatusNoContent)
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	req.RemoteAddr = "10.0.0.5:4444"
	req.Header.Set("X-Forwarded-For", "203.0.113.5, 198.51.100.2, 192.0.2.1")
	a.send(t, req)

	// Two trusted hops means the client is the second entry from the right.
	if got != "198.51.100.2" {
		t.Errorf("ClientIP = %q, want 198.51.100.2", got)
	}
}
