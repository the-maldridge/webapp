package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// denialCapture is a DenialHandler that records denials instead of writing
// them, so a test can assert on what the guard decided rather than on how the
// core happens to render it.
type denialCapture struct {
	denials []Denial
}

func (d *denialCapture) ServeDenial(w http.ResponseWriter, r *http.Request, denial Denial) {
	d.denials = append(d.denials, denial)
	w.WriteHeader(denial.Kind.Status())
}

// handler is the DenialHandler shape: Go needs the explicit conversion because
// a method value on a struct pointer is not a named func type.
func (d *denialCapture) handler() DenialHandler {
	return DenialHandler(d.ServeDenial)
}

// sessionRequest builds a request carrying a valid session cookie for a user.
func sessionRequest(t *testing.T, method, target string, u User) *http.Request {
	t.Helper()

	req := httptest.NewRequest(method, target, nil)
	req.AddCookie(&http.Cookie{
		Name:  DefaultCookieName,
		Value: sealSession(t, testSessionKey, u, time.Now().Add(time.Hour)),
	})
	return req
}

func TestRequireLoginRefusesAnonymous(t *testing.T) {
	svc := newTestService(t)

	rec := serve(svc.Middleware()(svc.RequireLogin()(okHandler("ok"))),
		httptest.NewRequest(http.MethodGet, "/private", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if rec.Body.String() == "ok" {
		t.Fatal("the downstream handler ran")
	}
	// A 401 without a challenge is a dead end for a client that could
	// authenticate by header.
	if got := rec.Header().Get("WWW-Authenticate"); got == "" {
		t.Error("a 401 should say how to authenticate")
	}
}

func TestRequireLoginAdmitsSession(t *testing.T) {
	svc := newTestService(t)

	req := sessionRequest(t, http.MethodGet, "/private", User{Identity: "ada"})
	rec := serve(svc.Middleware()(svc.RequireLogin()(okHandler("ok"))), req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestRequireRefusesWrongGroup(t *testing.T) {
	svc := newTestService(t)
	captured := &denialCapture{}
	svc.SetDenialHandler(captured.handler())

	req := sessionRequest(t, http.MethodGet, "/admin", User{
		Identity: "ada",
		Groups:   map[string]struct{}{"TASKS:VIEW": {}},
	})
	rec := serve(svc.Middleware()(svc.Require(P("tasks", "edit"))(okHandler("ok"))), req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if len(captured.denials) != 1 {
		t.Fatalf("denials = %+v, want exactly one", captured.denials)
	}
	d := captured.denials[0]
	if d.Kind != DenialForbidden {
		t.Errorf("kind = %v", d.Kind)
	}
	if d.Perm != P("tasks", "edit") {
		t.Errorf("perm = %v", d.Perm)
	}
	// What the caller held is recorded for logs and never rendered, so the
	// test asserts it is present on the struct.
	if len(d.HasPerm) != 1 || d.HasPerm[0] != "TASKS:VIEW" {
		t.Errorf("has = %v, want [TASKS:VIEW]", d.HasPerm)
	}
}

func TestRequireNamesRequiredPermissionToClient(t *testing.T) {
	svc := newTestService(t)

	req := sessionRequest(t, http.MethodGet, "/admin", User{Identity: "ada"})
	rec := serve(svc.Middleware()(svc.Require(P("tasks", "admin"))(okHandler("ok"))), req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	// Telling the caller which grant is missing is deliberate: they already
	// know who they are, and "forbidden" with no reason is a support ticket.
	if !strings.Contains(rec.Body.String(), "TASKS:ADMIN") {
		t.Fatalf("body = %q, want the required permission named", rec.Body.String())
	}
}

func TestRequireModuleAdminSatisfies(t *testing.T) {
	svc := newTestService(t)

	req := sessionRequest(t, http.MethodGet, "/admin", User{
		Identity: "ada",
		Groups:   map[string]struct{}{"TASKS:ADMIN": {}},
	})
	rec := serve(svc.Middleware()(svc.Require(P("tasks", "edit"))(okHandler("ok"))), req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestRequireCoreAdminSatisfiesEverything(t *testing.T) {
	svc := newTestService(t)

	req := sessionRequest(t, http.MethodGet, "/admin", User{
		Identity: "ada",
		Groups:   map[string]struct{}{CoreAdmin().String(): {}},
	})
	rec := serve(svc.Middleware()(svc.Require(P("billing", "delete"))(okHandler("ok"))), req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestEmptyPermBehavesLikeRequireLogin(t *testing.T) {
	// The zero Perm must degrade to the less privileged outcome: an
	// authorization bug that accidentally clears a grant field should lock a
	// route, not open it.
	svc := newTestService(t)

	anon := serve(svc.Middleware()(svc.Require(Perm{})(okHandler("ok"))),
		httptest.NewRequest(http.MethodGet, "/", nil))
	if anon.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", anon.Code)
	}

	req := sessionRequest(t, http.MethodGet, "/", User{Identity: "ada"})
	rec := serve(svc.Middleware()(svc.Require(Perm{})(okHandler("ok"))), req)
	if rec.Code != http.StatusOK {
		t.Fatalf("identified status = %d, want 200", rec.Code)
	}
}

func TestMiddlewareStampsUserOnContext(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)
	mustGrant(t, svc, "ada", "TASKS:EDIT")

	var seen User
	var ok bool
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, ok = UserFrom(r)
	})

	req := sessionRequest(t, http.MethodGet, "/", User{
		Identity: "ada",
		Groups:   map[string]struct{}{"TASKS:EDIT": {}},
	})
	serve(svc.Middleware()(probe), req)

	if !ok {
		t.Fatal("no user on the request context")
	}
	if seen.Identity != "ada" {
		t.Errorf("identity = %q", seen.Identity)
	}
	if !HasPerm(probeRequest(seen), P("tasks", "edit")) {
		t.Error("the stamped user should hold the session's groups")
	}
}

// probeRequest builds a request carrying a user the way the middleware does.
func probeRequest(u User) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	return r.WithContext(ContextWithUser(r.Context(), u))
}

func TestMiddlewareContinuesForAnonymous(t *testing.T) {
	svc := newTestService(t)

	// The gate is per route, so the base middleware must not refuse anyone:
	// a login page has to be reachable anonymously.
	rec := serve(svc.Middleware()(okHandler("ok")), httptest.NewRequest(http.MethodGet, "/login", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestOptionalIsMiddleware(t *testing.T) {
	svc := newTestService(t)

	req := sessionRequest(t, http.MethodGet, "/", User{Identity: "ada"})
	rec := serve(svc.Optional()(okHandler("ok")), req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestServiceUserAccessor(t *testing.T) {
	svc := newTestService(t)

	req := sessionRequest(t, http.MethodGet, "/", User{Identity: "ada"})
	var seen User
	var found bool
	serve(svc.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, found = svc.User(r)
	})), req)

	if !found || seen.Identity != "ada" {
		t.Fatalf("User = %+v, %v", seen, found)
	}
}

func TestInfoWorksWithoutMiddleware(t *testing.T) {
	svc := newTestService(t)

	// Info degrades to resolving the request itself rather than reporting
	// anonymous, so a handler that forgot the middleware behaves like one
	// that never forgot it.
	req := sessionRequest(t, http.MethodGet, "/", User{Identity: "ada"})
	info := svc.Info(req)
	if info.Via != ViaSession {
		t.Fatalf("via = %v, want session", info.Via)
	}
	if info.User.Identity != "ada" {
		t.Errorf("identity = %q", info.User.Identity)
	}
}

func TestInfoOnNilRequest(t *testing.T) {
	svc := newTestService(t)

	if info := svc.Info(nil); info.Via != ViaNone {
		t.Fatalf("via = %v, want none", info.Via)
	}
}

func TestDefaultDenialHandlerBody(t *testing.T) {
	cases := []struct {
		name       string
		denial     Denial
		wantStatus int
		wantKey    string
		wantAuth   bool
	}{
		{"no user", Denial{Kind: DenialNoUser}, http.StatusUnauthorized, "authentication required", true},
		{"forbidden", Denial{Kind: DenialForbidden, Perm: P("tasks", "edit")}, http.StatusForbidden, "TASKS:EDIT", false},
		{"csrf", Denial{Kind: DenialCSRF}, http.StatusForbidden, "forgery", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			defaultDenialHandler(rec, httptest.NewRequest(http.MethodGet, "/", nil), tc.denial)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not JSON: %q", rec.Body.String())
			}
			if !strings.Contains(body["error"], tc.wantKey) {
				t.Errorf("error = %q, want it to contain %q", body["error"], tc.wantKey)
			}
			if got := rec.Header().Get("WWW-Authenticate"); (got != "") != tc.wantAuth {
				t.Errorf("WWW-Authenticate = %q, want present: %v", got, tc.wantAuth)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}

func TestDenialKindStatuses(t *testing.T) {
	cases := map[DenialKind]int{
		DenialNoUser:    http.StatusUnauthorized,
		DenialForbidden: http.StatusForbidden,
		DenialCSRF:      http.StatusForbidden,
		DenialKind(99):  http.StatusForbidden,
	}
	for kind, want := range cases {
		if got := kind.Status(); got != want {
			t.Errorf("%v.Status() = %d, want %d", kind, got, want)
		}
	}
}

func TestDenialKindStrings(t *testing.T) {
	cases := map[DenialKind]string{
		DenialNoUser:    "no_identity",
		DenialForbidden: "forbidden",
		DenialCSRF:      "csrf",
		DenialKind(-1):  "denied",
	}
	for kind, want := range cases {
		if got := kind.String(); got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	}
}

func TestDenialErrorMessages(t *testing.T) {
	if got := (Denial{Kind: DenialNoUser}).Error(); got != "authentication required" {
		t.Errorf("got %q", got)
	}
	if got := (Denial{Kind: DenialCSRF}).Error(); !strings.Contains(got, "forgery") {
		t.Errorf("got %q", got)
	}
	plain := Denial{Kind: DenialForbidden}
	if got := plain.Error(); got != "permission denied" {
		t.Errorf("got %q", got)
	}
	named := Denial{Kind: DenialForbidden, Perm: P("tasks", "edit")}
	if got := named.Error(); !strings.Contains(got, "TASKS:EDIT") {
		t.Errorf("got %q", got)
	}
}

func TestGroupNamesAreSorted(t *testing.T) {
	groups := map[string]struct{}{
		"CORE:ADMIN": {},
		"TASKS:EDIT": {},
		"BLOG:VIEW":  {},
		"AAAA:FIRST": {},
	}
	got := groupNames(groups)
	want := []string{"AAAA:FIRST", "BLOG:VIEW", "CORE:ADMIN", "TASKS:EDIT"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if groupNames(nil) != nil {
		t.Error("no groups should produce no names")
	}
}

func TestJsonEscape(t *testing.T) {
	cases := map[string]string{
		`plain`:       `plain`,
		`say "hi"`:    `say \"hi\"`,
		"line\nbreak": `line\nbreak`,
		"tab\there":   `tab\there`,
		"return\r":    `return\r`,
		`back\slash`:  `back\\slash`,
		"bell\x07":    `bell\u001f`,
	}
	for input, want := range cases {
		if got := jsonEscape(input); got != want {
			t.Errorf("jsonEscape(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestDenialBase64CredentialRoundTrip(t *testing.T) {
	// Sanity on the test helper itself: a Basic header that the standard
	// library can decode.
	encoded := base64.StdEncoding.EncodeToString([]byte("ada:secret"))
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(decoded) != "ada:secret" {
		t.Fatalf("decoded %q", decoded)
	}
}

// TestDefaultDenialHandlerChallengeByAccept pins which clients are handed a
// challenge. A client that reads headers needs the 401 to say it may retry
// with a credential; a browser answered a challenge by painting its own
// dialog over the response body, so the one client that can render a page is
// the one client that must not get the header.
func TestDefaultDenialHandlerChallengeByAccept(t *testing.T) {
	cases := []struct {
		accept   string
		wantAuth bool
	}{
		{"", true},
		{"*/*", true},
		{"application/json", true},
		{"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", false},
		{"text/html", false},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("accept=%q", tc.accept), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/private", nil)
			if tc.accept != "" {
				req.Header.Set("Accept", tc.accept)
			}

			rec := httptest.NewRecorder()
			defaultDenialHandler(rec, req, Denial{Kind: DenialNoUser})

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if got := rec.Header().Get("WWW-Authenticate"); (got != "") != tc.wantAuth {
				t.Errorf("WWW-Authenticate = %q, want present: %v", got, tc.wantAuth)
			}
		})
	}
}
