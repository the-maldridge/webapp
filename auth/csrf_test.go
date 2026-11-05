package auth

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// csrfSessionRequest builds a request carrying a session cookie for an
// identity, the ordinary shape of an authenticated browser request.
func csrfSessionRequest(t *testing.T, method, target, identity string) *http.Request {
	t.Helper()

	cookie := &http.Cookie{
		Name:  DefaultCookieName,
		Value: sealSession(t, testSessionKey, User{Identity: identity}, time.Now().Add(time.Hour)),
	}
	req := httptest.NewRequest(method, target, nil)
	req.AddCookie(cookie)
	return req
}

// withFormBody replaces a request's body with a string.
func withFormBody(req *http.Request, body string) *http.Request {
	req.Body = io.NopCloser(strings.NewReader(body))
	req.ContentLength = int64(len(body))
	return req
}

// basicCredential renders an Authorization: Basic value.
func basicCredential(identity, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(identity + ":" + password))
}

// csrfCookieValue returns the token the middleware stored in a response's CSRF
// cookie.
func csrfCookieValue(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()

	for _, c := range rec.Result().Cookies() {
		if c.Name == csrfCookieName {
			return c.Value
		}
	}
	return ""
}

func TestSafeMethodMintsCSRFCookie(t *testing.T) {
	svc := newTestService(t)

	rec := serve(svc.CSRFMiddleware()(okHandler("ok")), httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if csrfCookieValue(t, rec) == "" {
		t.Fatal("a safe request should be given a CSRF token to echo later")
	}
}

func TestUnsafeWithoutCookieOrTokenPasses(t *testing.T) {
	// Anonymous API clients that never saw a page must keep working: the
	// browser-is-present signal is the cookie, and without one there is no
	// cross-site request to defend against.
	svc := newTestService(t)

	rec := serve(svc.CSRFMiddleware()(okHandler("ok")), httptest.NewRequest(http.MethodPost, "/api/things", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestUnsafeWithCookieButNoTokenIsForbidden(t *testing.T) {
	svc := newTestService(t)

	req := csrfSessionRequest(t, http.MethodPost, "/things", "ada")
	rec := serve(svc.CSRFMiddleware()(okHandler("ok")), req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestUnsafeWithGarbageTokenIsForbidden(t *testing.T) {
	svc := newTestService(t)

	req := csrfSessionRequest(t, http.MethodPost, "/things", "ada")
	req.Header.Set(csrfHeader, "not-even-close")
	rec := serve(svc.CSRFMiddleware()(okHandler("ok")), req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestUnsafeWithHeaderTokenSucceeds(t *testing.T) {
	svc := newTestService(t)

	req := csrfSessionRequest(t, http.MethodPost, "/things", "ada")
	req.Header.Set(csrfHeader, newCSRFFor(t, svc, "ada"))
	rec := serve(svc.CSRFMiddleware()(okHandler("ok")), req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestUnsafeWithFormFieldTokenSucceeds(t *testing.T) {
	svc := newTestService(t)

	req := csrfSessionRequest(t, http.MethodPost, "/things", "ada")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = withFormBody(req, "name=thing&csrf_token="+url.QueryEscape(newCSRFFor(t, svc, "ada")))

	rec := serve(svc.CSRFMiddleware()(okHandler("ok")), req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestTokenBoundToAnotherIdentityIsForbidden(t *testing.T) {
	svc := newTestService(t)

	// A token minted for ada must not satisfy bob's request. This is the
	// property that turns a session-fixation attempt into a failure instead
	// of a successful cross-account action.
	req := csrfSessionRequest(t, http.MethodPost, "/things", "bob")
	req.Header.Set(csrfHeader, newCSRFFor(t, svc, "ada"))

	rec := serve(svc.CSRFMiddleware()(okHandler("ok")), req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestExpiredTokenIsForbidden(t *testing.T) {
	svc := newTestService(t)

	expired, err := newCSRFToken(svc.csrfKey, "ada", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("newCSRFToken: %v", err)
	}

	req := csrfSessionRequest(t, http.MethodPost, "/things", "ada")
	req.Header.Set(csrfHeader, expired)

	rec := serve(svc.CSRFMiddleware()(okHandler("ok")), req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestTokenSignedWithAnotherKeyIsRejected(t *testing.T) {
	svc := newTestService(t)

	other, err := deriveCSRFKey("a-completely-different-session-key-of-sufficient-length")
	if err != nil {
		t.Fatalf("deriveCSRFKey: %v", err)
	}
	foreign, err := newCSRFToken(other, "ada", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("newCSRFToken: %v", err)
	}

	if csrfTokenValid(svc.csrfKey, foreign, "ada", time.Now()) {
		t.Fatal("a token signed with a different key must not verify")
	}
	if !csrfTokenValid(svc.csrfKey, newCSRFFor(t, svc, "ada"), "ada", time.Now()) {
		t.Fatal("the service's own token should verify")
	}
}

func TestBasicAndBearerRequestsSkipCSRF(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)
	created, err := svc.NewToken(t.Context(), "ada", "csrf-test", 0)
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}

	cases := []struct {
		name  string
		value string
	}{
		{"basic", "Basic " + basicCredential("ada", testPassword)},
		{"bearer", "Bearer " + created.Token},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// No CSRF cookie was ever issued to these clients, and no token
			// is presented; a browser cannot set this header cross-site, so
			// there is nothing left to check.
			req := httptest.NewRequest(http.MethodPost, "/api/things", nil)
			req.Header.Set("Authorization", tc.value)

			rec := serve(svc.CSRFMiddleware()(okHandler("ok")), req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
		})
	}
}

func TestUnknownBearerCredentialStillNeedsToken(t *testing.T) {
	// An Authorization header that authenticates nothing proves nothing, so
	// the CSRF exemption must not apply to it.
	svc := newTestService(t)

	req := httptest.NewRequest(http.MethodPost, "/api/things", nil)
	req.Header.Set("Authorization", "Bearer nonsense")
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "garbage"})

	rec := serve(svc.CSRFMiddleware()(okHandler("ok")), req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestAnonymousUnsafeWithCookieIsChecked(t *testing.T) {
	svc := newTestService(t)

	// The login form's protection: the browser that was served the page has
	// the cookie, an attacker's cross-site form cannot read it.
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "forged"})

	rec := serve(svc.CSRFMiddleware()(okHandler("ok")), req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestLoginFormTokenIsAcceptedAnonymously(t *testing.T) {
	svc := newTestService(t)

	// Get a real token the way a browser does: load a page, keep the cookie,
	// submit the form with the token from it.
	minted := serve(svc.CSRFMiddleware()(okHandler("ok")), httptest.NewRequest(http.MethodGet, "/login", nil))
	token := csrfCookieValue(t, minted)
	if token == "" {
		t.Fatal("no token minted")
	}

	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = withFormBody(req, "username=ada&csrf_token="+url.QueryEscape(token))
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: token})

	rec := serve(svc.CSRFMiddleware()(okHandler("ok")), req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestCSRFCookieAttributes(t *testing.T) {
	svc := newTestService(t)
	if err := svc.SetCookiePolicy(CookiePolicy{Name: DefaultCookieName, Secure: true}); err != nil {
		t.Fatalf("SetCookiePolicy: %v", err)
	}

	rec := serve(svc.CSRFMiddleware()(okHandler("ok")), httptest.NewRequest(http.MethodGet, "/", nil))
	cookie := responseCookie(t, rec, csrfCookieName)

	if cookie.HttpOnly {
		t.Error("the CSRF cookie must be readable by scripts on purpose")
	}
	if cookie.Path != "/" {
		t.Errorf("Path = %q, want /", cookie.Path)
	}
	if !cookie.Secure {
		t.Error("Secure should follow the policy")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.Expires.Before(time.Now()) {
		t.Error("the CSRF cookie was issued already expired")
	}
}

func TestIdentityChangeRemintsCSRFCookie(t *testing.T) {
	svc := newTestService(t)

	// The token the browser holds was minted for nobody. The authenticated
	// request must be handed one bound to its own identity, or every form
	// after login would fail.
	anonymous, err := newCSRFToken(svc.csrfKey, "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("newCSRFToken: %v", err)
	}

	req := csrfSessionRequest(t, http.MethodGet, "/", "ada")
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: anonymous})

	rec := serve(svc.CSRFMiddleware()(okHandler("ok")), req)
	fresh := csrfCookieValue(t, rec)
	if fresh == "" {
		t.Fatal("no replacement token issued")
	}
	if fresh == anonymous {
		t.Fatal("the stale token was kept; it is bound to the wrong identity")
	}
	if !csrfTokenValid(svc.csrfKey, fresh, "ada", time.Now()) {
		t.Fatal("the replacement should be bound to the request's identity")
	}
}

func TestValidCSRFCookieIsNotReplaced(t *testing.T) {
	svc := newTestService(t)

	token := newCSRFFor(t, svc, "ada")
	req := csrfSessionRequest(t, http.MethodGet, "/", "ada")
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: token})

	rec := serve(svc.CSRFMiddleware()(okHandler("ok")), req)
	if got := csrfCookieValue(t, rec); got != "" {
		t.Errorf("a valid cookie should be left alone, got a replacement: %q", trunc(got, 20))
	}
}

func TestUnparseableBodyIsTreatedAsNoToken(t *testing.T) {
	svc := newTestService(t)

	req := csrfSessionRequest(t, http.MethodPost, "/things", "ada")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// A percent-escape that cannot be decoded: ParseForm fails, so there is
	// no token, so the request is refused rather than waved through.
	req = withFormBody(req, "csrf_token=%zz")

	rec := serve(svc.CSRFMiddleware()(okHandler("ok")), req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestCSRFInputEscapesToken(t *testing.T) {
	svc := newTestService(t)
	token := newCSRFFor(t, svc, "ada")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: token})

	input := CSRFInput(req)
	if !strings.Contains(input, `name="csrf_token"`) {
		t.Fatalf("CSRFInput = %q", input)
	}
	if !strings.Contains(input, `value="`+token+`"`) {
		t.Fatalf("CSRFInput = %q, want it to carry the token", input)
	}
}

func TestCSRFInputEmptyWithoutToken(t *testing.T) {
	input := CSRFInput(httptest.NewRequest(http.MethodGet, "/", nil))
	if input != "" {
		t.Fatalf("CSRFInput = %q, want empty", input)
	}
}

// TestRenderedPageSeesMintedToken is the regression that makes the login form
// usable: a first-time visitor has no CSRF cookie, so the token signed for the
// response has to be readable by the page being rendered, otherwise every form
// on the first page renders empty and fails on submit.
func TestRenderedPageSeesMintedToken(t *testing.T) {
	svc := newTestService(t)

	var rendered string
	handler := svc.CSRFMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rendered = CSRFInput(r)
	}))

	rec := serve(handler, httptest.NewRequest(http.MethodGet, "/login", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	token := csrfCookieValue(t, rec)
	if token == "" {
		t.Fatal("the safe request should have been given a token")
	}
	if !strings.Contains(rendered, `value="`+token+`"`) {
		t.Errorf("rendered form = %q, want it to carry the minted token %q", rendered, token)
	}

	// The echoed pair verifies, which is the only thing that matters: a token
	// a page cannot copy is a token nobody can submit.
	submit := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("csrf_token="+url.QueryEscape(token)))
	submit.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	submit.AddCookie(&http.Cookie{Name: csrfCookieName, Value: token})
	if got := serve(svc.CSRFMiddleware()(okHandler("ok")), submit); got.Code != http.StatusOK {
		t.Fatalf("submitting the rendered token = %d, want 200", got.Code)
	}
}

// TestMintedTokenDoesNotSatisfyVerification states the boundary the render path
// must not leak across: the minted value lets a page draw a form, and never
// lets a request skip presenting one of its own.
func TestMintedTokenDoesNotSatisfyVerification(t *testing.T) {
	svc := newTestService(t)

	// A browser-shaped request: it carries the CSRF cookie, so a token must be
	// presented, but presents none of its own. The only token available is the
	// one this server put in the request context for rendering, and that must
	// not be treated as client-supplied.
	req := httptest.NewRequest(http.MethodPost, "/things", nil)
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: newCSRFFor(t, svc, "")})
	req = req.WithContext(withRenderedCSRF(req.Context(), newCSRFFor(t, svc, "")))

	if rec := serve(svc.CSRFMiddleware()(okHandler("ok")), req); rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: a value only this server can read is not evidence the client sent it", rec.Code)
	}
}

func TestHeaderWinsOverFormField(t *testing.T) {
	svc := newTestService(t)

	req := csrfSessionRequest(t, http.MethodPost, "/things", "ada")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = withFormBody(req, "csrf_token=stale-value")
	req.Header.Set(csrfHeader, newCSRFFor(t, svc, "ada"))

	rec := serve(svc.CSRFMiddleware()(okHandler("ok")), req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: the header should win", rec.Code)
	}
}

func TestUnsafeMethodsAreChecked(t *testing.T) {
	svc := newTestService(t)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		req := csrfSessionRequest(t, method, "/things", "ada")
		rec := serve(svc.CSRFMiddleware()(okHandler("ok")), req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", method, rec.Code)
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		req := csrfSessionRequest(t, method, "/things", "ada")
		rec := serve(svc.CSRFMiddleware()(okHandler("ok")), req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", method, rec.Code)
		}
	}
}

func TestCSRFTokenValidRejectsMalformed(t *testing.T) {
	key, err := deriveCSRFKey(testSessionKey)
	if err != nil {
		t.Fatalf("deriveCSRFKey: %v", err)
	}
	now := time.Now()

	valid, err := newCSRFToken(key, "ada", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("newCSRFToken: %v", err)
	}
	if !csrfTokenValid(key, valid, "ada", now) {
		t.Fatal("setup: token should be valid")
	}

	payload, sig, _ := strings.Cut(valid, csrfSignatureSeparator)
	cases := map[string]string{
		"empty":            "",
		"no separator":     payload,
		"empty signature":  payload + csrfSignatureSeparator,
		"empty payload":    csrfSignatureSeparator + sig,
		"dropped field":    strings.Join(strings.Split(payload, csrfPayloadSep)[1:], csrfPayloadSep) + csrfSignatureSeparator + sig,
		"wrong version":    strings.Replace(valid, csrfTokenPrefix+csrfPayloadSep, "v0"+csrfPayloadSep, 1),
		"trailing garbage": valid + "x",
	}
	for name, token := range cases {
		if csrfTokenValid(key, token, "ada", now) {
			t.Errorf("%s: accepted a malformed token", name)
		}
	}
}

func TestCSRFTokenExpiryGrace(t *testing.T) {
	key, err := deriveCSRFKey(testSessionKey)
	if err != nil {
		t.Fatalf("deriveCSRFKey: %v", err)
	}

	// Issued to expire thirty seconds ago: inside the one minute of grace, a
	// form the user submitted on time still works.
	justExpired, err := newCSRFToken(key, "ada", time.Now().Add(-30*time.Second))
	if err != nil {
		t.Fatalf("newCSRFToken: %v", err)
	}
	if !csrfTokenValid(key, justExpired, "ada", time.Now()) {
		t.Error("a token that expired moments ago should still be honored")
	}

	longExpired, err := newCSRFToken(key, "ada", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("newCSRFToken: %v", err)
	}
	if csrfTokenValid(key, longExpired, "ada", time.Now()) {
		t.Error("an hour past expiry is expired")
	}
}

func TestCookieNamedRejectsDuplicates(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "first"})
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "second"})

	if value, ok := cookieNamed(req, csrfCookieName); ok {
		t.Fatalf("cookieNamed = %q, want not-found for a duplicate pair", value)
	}
	if !cookiePresent(req, csrfCookieName) {
		t.Error("cookiePresent answers \"is a browser in play\", and the answer is yes")
	}
}

func TestCookieNamedReturnsSingleValue(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "only"})

	value, ok := cookieNamed(req, csrfCookieName)
	if !ok || value != "only" {
		t.Fatalf("cookieNamed = %q, %v", value, ok)
	}
	if _, ok := cookieNamed(req, "absent"); ok {
		t.Error("an absent cookie is not found")
	}
}

func TestTokenSurvivesCookieRoundTrip(t *testing.T) {
	// A CSRF token is stored in a cookie, and net/http silently deletes
	// bytes a cookie value cannot carry. A separator that gets eaten on the
	// way out produces a token that verified when it was minted and never
	// again, which is a bug that looks like a browser problem.
	svc := newTestService(t)

	token := newCSRFFor(t, svc, "ada")
	rec := httptest.NewRecorder()
	http.SetCookie(rec, &http.Cookie{Name: csrfCookieName, Value: token})

	parsed := responseCookie(t, rec, csrfCookieName)
	if parsed.Value != token {
		t.Fatalf("cookie round trip changed the token:\n got %q\nwant %q", parsed.Value, token)
	}
	if !csrfTokenValid(svc.csrfKey, parsed.Value, "ada", time.Now()) {
		t.Fatal("the token is unverifiable after a cookie round trip")
	}
}

func TestCSRFMiddlewareLeavesOtherRequestsAlone(t *testing.T) {
	svc := newTestService(t)

	// Downstream content must survive untouched: the middleware either calls
	// next or denies, and never rewrites a response on its way past.
	rec := serve(svc.CSRFMiddleware()(okHandler("payload")), httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Body.String() != "payload" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}
