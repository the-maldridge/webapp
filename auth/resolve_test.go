package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestViaStrings(t *testing.T) {
	cases := map[Via]string{
		ViaNone:    "anonymous",
		ViaSession: "session",
		ViaBasic:   "basic",
		ViaToken:   "token",
		Via(42):    "anonymous",
	}
	for via, want := range cases {
		if got := via.String(); got != want {
			t.Errorf("Via(%d).String() = %q, want %q", via, got, want)
		}
	}
}

func TestResolveSession(t *testing.T) {
	svc := newTestService(t)

	req := sessionRequest(t, http.MethodGet, "/", User{
		Identity: "Ada@Example.com",
		Groups:   map[string]struct{}{"TASKS:EDIT": {}},
	})
	info := svc.resolve(req)

	if info.Via != ViaSession {
		t.Fatalf("via = %v", info.Via)
	}
	// The identity inside a cookie is normalized on the way out too: a
	// session minted before a normalization rule changed must not produce a
	// second principal for the same person.
	if info.User.Identity != "ada@example.com" {
		t.Errorf("identity = %q", info.User.Identity)
	}
	if info.Expires.IsZero() {
		t.Error("a session identity should report when it stops being valid")
	}
}

func TestResolveSessionNilGroupsBecomesEmptySet(t *testing.T) {
	svc := newTestService(t)

	req := sessionRequest(t, http.MethodGet, "/", User{Identity: "ada"})
	info := svc.resolve(req)
	if info.User.Groups == nil {
		t.Fatal("Groups = nil; a handler ranging over it should not have to check")
	}
}

func TestResolveRejectsExpiredSession(t *testing.T) {
	svc := newTestService(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{
		Name:  DefaultCookieName,
		Value: sealSession(t, testSessionKey, User{Identity: "ada"}, time.Now().Add(-time.Minute)),
	})

	info := svc.resolve(req)
	if info.Via != ViaNone {
		t.Fatalf("via = %v, want none for an expired session", info.Via)
	}
	// An expired cookie is the ordinary condition of a browser that sat
	// overnight. Recording it as a failure would put a line in the log for
	// every one of them.
	if info.Err != nil {
		t.Errorf("err = %v, want nil", info.Err)
	}
}

func TestResolveRejectsGarbageSession(t *testing.T) {
	svc := newTestService(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: DefaultCookieName, Value: "not-a-session"})

	info := svc.resolve(req)
	if info.Via != ViaNone || info.Err != nil {
		t.Fatalf("via = %v err = %v, want anonymous with no recorded failure", info.Via, info.Err)
	}
}

func TestResolveRejectsEmptyIdentitySession(t *testing.T) {
	svc := newTestService(t)

	// A cookie that opens correctly but names nobody must not be treated as
	// an identity: an empty principal that satisfies RequireLogin would be
	// the worst possible outcome of a bug elsewhere.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{
		Name:  DefaultCookieName,
		Value: sealSession(t, testSessionKey, User{Identity: "   "}, time.Now().Add(time.Hour)),
	})

	if info := svc.resolve(req); info.Via != ViaNone {
		t.Fatalf("via = %v, want none", info.Via)
	}
}

func TestResolveIgnoresForeignCookieName(t *testing.T) {
	svc := newTestService(t)

	value := sealSession(t, testSessionKey, User{Identity: "ada"}, time.Now().Add(time.Hour))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "someone_elses_session", Value: value})

	if info := svc.resolve(req); info.Via != ViaNone {
		t.Fatalf("via = %v, want none: the cookie name is part of the contract", info.Via)
	}
}

func TestResolvePolicyNameIsHonored(t *testing.T) {
	svc := newTestService(t)
	if err := svc.SetCookiePolicy(CookiePolicy{Name: "renamed_session"}); err != nil {
		t.Fatalf("SetCookiePolicy: %v", err)
	}

	old := httptest.NewRequest(http.MethodGet, "/", nil)
	old.AddCookie(&http.Cookie{
		Name:  DefaultCookieName,
		Value: sealSession(t, testSessionKey, User{Identity: "ada"}, time.Now().Add(time.Hour)),
	})
	if info := svc.resolve(old); info.Via != ViaNone {
		t.Fatal("the previous cookie name should stop working once the policy changes")
	}

	fresh := httptest.NewRequest(http.MethodGet, "/", nil)
	fresh.AddCookie(&http.Cookie{
		Name:  "renamed_session",
		Value: sealSession(t, testSessionKey, User{Identity: "ada"}, time.Now().Add(time.Hour)),
	})
	if info := svc.resolve(fresh); info.Via != ViaSession {
		t.Fatalf("via = %v, want session", info.Via)
	}
}

func TestResolveBasic(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)
	mustGrant(t, svc, "ada", "TASKS:EDIT")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("ada", testPassword)

	info := svc.resolve(req)
	if info.Via != ViaBasic {
		t.Fatalf("via = %v, want basic", info.Via)
	}
	if info.User.Identity != "ada" {
		t.Errorf("identity = %q", info.User.Identity)
	}
	if _, held := info.User.Groups["TASKS:EDIT"]; !held {
		t.Errorf("groups = %v, want TASKS:EDIT", info.User.Groups)
	}
	// Header credentials are re-verified every request, so there is no
	// expiry to report.
	if !info.Expires.IsZero() {
		t.Error("a header-authenticated identity should report no expiry")
	}
}

func TestResolveBasicDisabledAccount(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)
	if err := svc.SetDisabled(t.Context(), "ada", true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("ada", testPassword)

	info := svc.resolve(req)
	if info.Via != ViaNone {
		t.Fatalf("via = %v, want none", info.Via)
	}
	// The failure is recorded internally for logs and never changes the
	// response, which is one indistinguishable 401 either way.
	if info.Err == nil {
		t.Error("a rejected credential should be recorded for logs")
	}
}

func TestResolveBasicMalformedHeaderIsIgnored(t *testing.T) {
	svc := newTestService(t)

	// net/http reports a malformed Basic header as "no credentials", and
	// answering it would be a way for a client to learn something.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Basic !!!notbase64!!!")

	info := svc.resolve(req)
	if info.Via != ViaNone {
		t.Fatalf("via = %v, want none", info.Via)
	}
}

func TestResolveBasicEmptyHalves(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)

	cases := [][2]string{{"", ""}, {"ada", ""}, {"", testPassword}}
	for _, pair := range cases {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.SetBasicAuth(pair[0], pair[1])

		if info := svc.resolve(req); info.Via != ViaNone {
			t.Errorf("identity %q password %q: via = %v, want none", pair[0], pair[1], info.Via)
		}
	}
}

func TestResolveBearer(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)
	mustGrant(t, svc, "ada", "TASKS:EDIT")

	created, err := svc.NewToken(t.Context(), "ada", "resolve-test", 0)
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+created.Token)

	info := svc.resolve(req)
	if info.Via != ViaToken {
		t.Fatalf("via = %v, want token", info.Via)
	}
	if info.User.Identity != "ada" {
		t.Errorf("identity = %q", info.User.Identity)
	}
}

func TestBearerTokenParsing(t *testing.T) {
	cases := map[string]bool{
		"Bearer tok":       true,
		"bearer tok":       true,
		"BEARER tok":       true,
		"Bearer":           false,
		"Basic tok":        false,
		"tok":              false,
		"Bearer    ":       false,
		"Bearer tok extra": true,
	}
	for value, want := range cases {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", value)

		token, ok := bearerToken(req)
		if ok != want {
			t.Errorf("bearerToken(%q) = %q, %v; want found: %v", value, token, ok, want)
		}
		if ok && strings.TrimSpace(token) == "" {
			t.Errorf("bearerToken(%q) returned an empty token", value)
		}
	}
}

func TestResolvePrefersSessionOverHeaders(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)
	mustCreateAccount(t, svc, "bob", testPassword)

	// The cookie is ambient, the header is deliberate — but the cookie is
	// checked first, and this pins that order down so it is a decision
	// rather than an accident. A change here needs a reason.
	req := sessionRequest(t, http.MethodGet, "/", User{Identity: "ada"})
	req.SetBasicAuth("bob", testPassword)

	info := svc.resolve(req)
	if info.Via != ViaSession {
		t.Fatalf("via = %v, want session", info.Via)
	}
	if info.User.Identity != "ada" {
		t.Errorf("identity = %q, want the session's ada", info.User.Identity)
	}
}

func TestResolveFallsThroughToHeaderWhenSessionIsStale(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)

	created, err := svc.NewToken(t.Context(), "ada", "rotation-test", 0)
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}

	// The shape of a session-key rotation: every browser holds a cookie that
	// can no longer open, and the API client in that same browser has a valid
	// token. Refusing the request because the ambient credential was stale
	// would make the rotation impossible to survive.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: DefaultCookieName, Value: "unreadable-after-rotation"})
	req.Header.Set("Authorization", "Bearer "+created.Token)

	info := svc.resolve(req)
	if info.Via != ViaToken {
		t.Fatalf("via = %v, want token", info.Via)
	}
	if info.User.Identity != "ada" {
		t.Errorf("identity = %q", info.User.Identity)
	}
}

func TestResolveRecognizesEachSchemeAlone(t *testing.T) {
	// Basic and Bearer share the one Authorization header, so a request never
	// carries both and no precedence between them is exercised in practice.
	// What is worth pinning down is that each scheme is recognized only under
	// its own name, and that neither is quietly reinterpreted as the other.
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)

	created, err := svc.NewToken(t.Context(), "ada", "scheme-test", 0)
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}

	bearer := httptest.NewRequest(http.MethodGet, "/", nil)
	bearer.Header.Set("Authorization", "Bearer "+created.Token)
	if info := svc.resolve(bearer); info.Via != ViaToken {
		t.Errorf("bearer request resolved as %v, want token", info.Via)
	}

	basic := httptest.NewRequest(http.MethodGet, "/", nil)
	basic.Header.Set("Authorization", "Basic "+basicCredential("ada", testPassword))
	if info := svc.resolve(basic); info.Via != ViaBasic {
		t.Errorf("basic request resolved as %v, want basic", info.Via)
	}

	wrongSlot := httptest.NewRequest(http.MethodGet, "/", nil)
	wrongSlot.Header.Set("Authorization", "Basic "+created.Token)
	if info := svc.resolve(wrongSlot); info.Via != ViaNone {
		t.Errorf("a bearer token in the Basic slot resolved as %v, want none", info.Via)
	}
}

func TestResolveAnonymousRecordsNothing(t *testing.T) {
	svc := newTestService(t)

	info := svc.resolve(httptest.NewRequest(http.MethodGet, "/", nil))
	if info.Via != ViaNone || info.Err != nil || info.User.Identity != "" {
		t.Fatalf("info = %+v, want a bare anonymous outcome", info)
	}
}

func TestResolveRecordsCredentialFailures(t *testing.T) {
	svc := newTestService(t)

	// A rejected credential is recorded on the info rather than reported to
	// the client: whoever reads the log needs to know a credential was
	// presented and failed, and whoever presented it gets one anonymous
	// outcome either way.
	bearer := httptest.NewRequest(http.MethodGet, "/", nil)
	bearer.Header.Set("Authorization", "Bearer not-a-real-token")

	basic := httptest.NewRequest(http.MethodGet, "/", nil)
	basic.Header.Set("Authorization", "Basic "+basicCredential("nobody", "not-a-password"))

	for name, req := range map[string]*http.Request{"bearer": bearer, "basic": basic} {
		info := svc.resolve(req)
		if info.Via != ViaNone {
			t.Errorf("%s: via = %v, want none", name, info.Via)
		}
		if info.Err == nil {
			t.Errorf("%s: a rejected credential should be recorded for logs", name)
		}
	}
}

func TestContextWithInfoRoundTrip(t *testing.T) {
	svc := newTestService(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	stamped := contextWithInfo(req.Context(), AuthInfo{
		User: User{Identity: "ada"},
		Via:  ViaToken,
	})
	req = req.WithContext(stamped)

	// A stamped outcome wins over re-resolving the request: the middleware's
	// decision is the authority for the request, not a second opinion.
	info := svc.Info(req)
	if info.Via != ViaToken || info.User.Identity != "ada" {
		t.Fatalf("info = %+v", info)
	}
}

func TestInfoFromContextDistinguishesUnstamped(t *testing.T) {
	if _, ok := infoFromContext(httptest.NewRequest(http.MethodGet, "/", nil).Context()); ok {
		t.Fatal("an unstamped context should report false")
	}
}

func TestProfileMiddlewareProvisions(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)

	var got ProfileID
	var found bool
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, found = ProfileIDFrom(r)
	})

	req := sessionRequest(t, http.MethodGet, "/", User{Identity: "ada", AuthedBy: "database"})
	rec := serve(svc.Middleware()(svc.ProfileMiddleware()(probe)), req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !found || got == 0 {
		t.Fatalf("ProfileIDFrom = %d, %v; want a provisioned ID", got, found)
	}

	profile, err := svc.Profile(t.Context(), "ada", "database")
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if uint(profile.ID) != uint(got) {
		t.Fatalf("middleware handed out %d, store says %d", got, profile.ID)
	}
}

func TestProfileMiddlewareSkipsAnonymous(t *testing.T) {
	svc := newTestService(t)

	var found bool
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, found = ProfileIDFrom(r)
	})

	rec := serve(svc.Middleware()(svc.ProfileMiddleware()(probe)),
		httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous requests must reach the handler: %d", rec.Code)
	}
	if found {
		t.Error("an anonymous request has no profile")
	}
}

func TestProfileIDFromNilRequest(t *testing.T) {
	if _, ok := ProfileIDFrom(nil); ok {
		t.Error("a nil request has no profile")
	}
}

func TestRequireOwnership(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)

	var handlerReq *http.Request
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerReq = r
	})

	req := sessionRequest(t, http.MethodGet, "/", User{Identity: "ada", AuthedBy: "database"})
	serve(svc.Middleware()(svc.ProfileMiddleware()(probe)), req)

	_, owned := ProfileIDFrom(handlerReq)
	if !owned {
		t.Fatal("setup: no profile on the request")
	}

	if _, err := RequireOwnership(handlerReq, uint(mustProfileID(t, handlerReq))); err != nil {
		t.Fatalf("owner refused: %v", err)
	}
	if _, err := RequireOwnership(handlerReq, uint(mustProfileID(t, handlerReq))+1); err != ErrNotOwner {
		t.Fatalf("stranger accepted: %v", err)
	}
}

func TestRequireOwnershipAnonymous(t *testing.T) {
	// No identity, so no owner. The error is the same one a stranger gets:
	// the distinction has no use, and reporting it would be a hint.
	if _, err := RequireOwnership(httptest.NewRequest(http.MethodGet, "/", nil), 1); err != ErrNotOwner {
		t.Fatalf("err = %v, want ErrNotOwner", err)
	}
}

func TestRequireOwnershipOfWrapsRequireOwnership(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)

	var handlerReq *http.Request
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerReq = r
	})
	req := sessionRequest(t, http.MethodGet, "/", User{Identity: "ada", AuthedBy: "database"})
	serve(svc.Middleware()(svc.ProfileMiddleware()(probe)), req)

	id, err := MustProfileIdentity(handlerReq)
	if err != nil {
		t.Fatalf("MustProfileIdentity: %v", err)
	}
	if _, err := RequireOwnershipOf(handlerReq, id); err != nil {
		t.Fatalf("owner refused: %v", err)
	}
	if _, err := RequireOwnershipOf(handlerReq, id+1); err != ErrNotOwner {
		t.Fatalf("stranger accepted: %v", err)
	}
}

func TestMustProfileIdentityAnonymous(t *testing.T) {
	if _, err := MustProfileIdentity(httptest.NewRequest(http.MethodGet, "/", nil)); err != ErrNoUser {
		t.Fatalf("err = %v, want ErrNoUser", err)
	}
}

func TestParseProfileID(t *testing.T) {
	for _, good := range []string{"1", "42", "18446744073709551615"} {
		id, err := ParseProfileID(good)
		if err != nil {
			t.Errorf("ParseProfileID(%q): %v", good, err)
			continue
		}
		if good == "1" && id != 1 {
			t.Errorf("ParseProfileID(%q) = %d", good, id)
		}
	}
	// Zero is a real number and a nonsense profile ID: letting it through
	// turns a missing value into either a not-found or a create against a
	// foreign key of zero.
	for _, bad := range []string{"0", "", "-1", "abc", "1.5", " 7"} {
		if id, err := ParseProfileID(bad); err == nil {
			t.Errorf("ParseProfileID(%q) = %d, want an error", bad, id)
		}
	}
}

func TestCurrentProfileIdentity(t *testing.T) {
	svc := newTestService(t)

	var handlerReq *http.Request
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerReq = r
	})
	req := sessionRequest(t, http.MethodGet, "/", User{Identity: "ada"})
	serve(svc.Middleware()(probe), req)

	identity, err := CurrentProfileIdentity(handlerReq)
	if err != nil {
		t.Fatalf("CurrentProfileIdentity: %v", err)
	}
	if identity != "ada" {
		t.Errorf("identity = %q", identity)
	}

	if _, err := CurrentProfileIdentity(httptest.NewRequest(http.MethodGet, "/", nil)); err != ErrNoUser {
		t.Fatalf("err = %v, want ErrNoUser", err)
	}
}

// mustProfileID returns a request's profile ID, failing the test when absent.
func mustProfileID(t *testing.T, r *http.Request) ProfileID {
	t.Helper()

	id, ok := ProfileIDFrom(r)
	if !ok {
		t.Fatal("no profile ID on the request")
	}
	return id
}
