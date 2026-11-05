package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSafeNextAcceptsSameSitePaths(t *testing.T) {
	const fallback = "/home"

	for _, target := range []string{
		"/",
		"/tasks",
		"/tasks/42",
		"/tasks?filter=open",
		"/tasks#top",
		"/a/b/c/d",
		"/with%20space",
	} {
		if got := SafeNext(target, fallback); got != target {
			t.Errorf("SafeNext(%q) = %q, want it accepted", target, got)
		}
	}
}

func TestSafeNextRejectsOpenRedirects(t *testing.T) {
	const fallback = "/home"

	cases := map[string]string{
		"empty":                      "",
		"scheme-relative":            "//evil.com",
		"scheme-relative upper":      "//Evil.COM/path",
		"backslash host":             `/\\evil.com`,
		"backslash single":           `/\evil.com`,
		"absolute http":              "http://evil.com",
		"absolute https":             "https://evil.com/path",
		"protocol relative name":     "evil.com",
		"encoded scheme relative":    "/%2F%2Fevil.com",
		"encoded scheme relative up": "/%2f%2fevil.com",
		"encoded colon slashes":      "/http://evil.com",
		"header injection cr":        "/x\r\nSet-Cookie: a=b",
		"header injection lf":        "/x\nSet-Cookie: a=b",
		"NUL":                        "/x\x00y",
		"tab":                        "/x\ty",
		"bare backslash":             `\`,
		"trailing backslash":         "/x\\",
		"percent malformed":          "/%zz",
		"userinfo trick":             "/\\evil.com",
	}
	for name, target := range cases {
		if got := SafeNext(target, fallback); got != fallback {
			t.Errorf("%s: SafeNext(%q) = %q, want the fallback", name, target, got)
		}
	}
}

func TestSafeNextEmptyRawReturnsFallback(t *testing.T) {
	if got := SafeNext("", ""); got != "" {
		t.Errorf("SafeNext(\"\", \"\") = %q, want empty", got)
	}
}

func TestSafeNextKeepsTheRawForm(t *testing.T) {
	// The accepted value is the raw one, not the decoded one: returning a
	// decoded path would hand back something that was never checked.
	target := "/search?q=%2F%2Fsafe-inside-a-query"
	if got := SafeNext(target, "/"); got != target {
		t.Errorf("SafeNext = %q, want the raw input", got)
	}
}

func loginForm(t *testing.T, fields map[string]string) *http.Request {
	t.Helper()

	values := url.Values{}
	for k, v := range fields {
		values.Set(k, v)
	}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func TestLoginHandlerIssuesSessionOnSuccess(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)
	mustGrant(t, svc, "ada", "TASKS:EDIT")

	handler := svc.LoginHandler(LoginOptions{Fallback: "/home"})
	rec := serve(handler, loginForm(t, map[string]string{
		"username": "ada",
		"password": testPassword,
	}))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/home" {
		t.Errorf("Location = %q, want the fallback", got)
	}

	cookie := responseCookie(t, rec, DefaultCookieName)
	if !cookie.HttpOnly {
		t.Error("the session cookie must not be readable by scripts")
	}
	if cookie.Path != "/" {
		t.Errorf("Path = %q, want /", cookie.Path)
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax rather than a browser default", cookie.SameSite)
	}
	if cookie.MaxAge <= 0 {
		t.Errorf("MaxAge = %d, want a positive lifetime", cookie.MaxAge)
	}
	if cookie.Value == "" {
		t.Fatal("no session value")
	}

	// The cookie must actually open, and name the right principal with the
	// right groups: the whole point of the login is the session it produces.
	session, ok := codecFor(t, testSessionKey).open(cookie.Value)
	if !ok {
		t.Fatal("the issued cookie does not open")
	}
	if session.User.Identity != "ada" {
		t.Errorf("identity = %q", session.User.Identity)
	}
	if _, held := session.User.Groups["TASKS:EDIT"]; !held {
		t.Errorf("groups = %v, want TASKS:EDIT", session.User.Groups)
	}
	if session.Expires.Before(time.Now()) {
		t.Error("the session was issued already expired")
	}
}

func TestLoginHandlerHonorsNext(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)

	rec := serve(svc.LoginHandler(LoginOptions{Fallback: "/home"}), loginForm(t, map[string]string{
		"username": "ada",
		"password": testPassword,
		"next":     "/tasks/42",
	}))

	if got := rec.Header().Get("Location"); got != "/tasks/42" {
		t.Errorf("Location = %q, want /tasks/42", got)
	}
}

func TestLoginHandlerRefusesOpenRedirectToNext(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)

	rec := serve(svc.LoginHandler(LoginOptions{Fallback: "/home"}), loginForm(t, map[string]string{
		"username": "ada",
		"password": testPassword,
		"next":     "//evil.com",
	}))

	// The login still succeeds: the destination was bad, not the credential.
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/home" {
		t.Fatalf("Location = %q, want the fallback rather than the attacker's host", got)
	}
}

func TestLoginHandlerFailuresAreIndistinguishable(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)
	mustCreateAccount(t, svc, "disabled", testPassword)
	if err := svc.SetDisabled(t.Context(), "disabled", true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}

	cases := map[string]map[string]string{
		"unknown identity": {"username": "nobody", "password": testPassword},
		"wrong password":   {"username": "ada", "password": "wrong wrong wrong wrong"},
		"disabled account": {"username": "disabled", "password": testPassword},
		"empty everything": {"username": "", "password": ""},
		"no identity":      {"password": testPassword},
		"no password":      {"username": "ada"},
	}

	var status int
	var body string
	var cookies []string
	for name, fields := range cases {
		rec := serve(svc.LoginHandler(LoginOptions{Fallback: "/home"}), loginForm(t, fields))

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401", name, rec.Code)
		}
		if status == 0 {
			status = rec.Code
			body = rec.Body.String()
			cookies = rec.Result().Header["Set-Cookie"]
			continue
		}
		// Identical status, identical body, and no cookie. A response that
		// differs for any of these six inputs is an account enumeration
		// endpoint that costs nothing to query.
		if rec.Code != status || rec.Body.String() != body {
			t.Errorf("%s: response differs from the reference failure: %d %q vs %d %q",
				name, rec.Code, rec.Body.String(), status, body)
		}
		if got := rec.Result().Header["Set-Cookie"]; len(got) != len(cookies) {
			t.Errorf("%s: Set-Cookie differs: %v vs %v", name, got, cookies)
		}
	}
}

func TestLoginHandlerRejectsNonPOST(t *testing.T) {
	svc := newTestService(t)

	// A login form over GET puts the credential in a URL, and URLs reach
	// access logs, history, and Referer headers.
	rec := serve(svc.LoginHandler(LoginOptions{}), httptest.NewRequest(http.MethodGet, "/login?username=ada&password=x", nil))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow = %q, want POST", got)
	}
}

func TestLoginHandlerOnFailureHook(t *testing.T) {
	svc := newTestService(t)

	var gotErr error
	var gotNext string
	handler := svc.LoginHandler(LoginOptions{
		Fallback: "/home",
		OnFailure: func(w http.ResponseWriter, r *http.Request, next string, err error) {
			gotErr = err
			gotNext = next
			w.WriteHeader(http.StatusTeapot)
		},
	})

	rec := serve(handler, loginForm(t, map[string]string{
		"username": "nobody",
		"password": "nope nope nope nope",
		"next":     "/wanted",
	}))

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want the hook's response", rec.Code)
	}
	// The hook always receives the one value. Whatever it renders, it cannot
	// render "no such account" because it was never told.
	if !errors.Is(gotErr, ErrLoginRejected) {
		t.Errorf("err = %v, want ErrLoginRejected", gotErr)
	}
	if gotNext != "/wanted" {
		t.Errorf("next = %q", gotNext)
	}
}

func TestLoginHandlerOnSuccessHookReplacesDefault(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)

	var seenUser User
	handler := svc.LoginHandler(LoginOptions{
		OnSuccess: func(w http.ResponseWriter, r *http.Request, u User, next string) {
			seenUser = u
			w.WriteHeader(http.StatusNoContent)
		},
	})

	rec := serve(handler, loginForm(t, map[string]string{
		"username": "ada",
		"password": testPassword,
	}))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
	if seenUser.Identity != "ada" {
		t.Errorf("identity = %q", seenUser.Identity)
	}
	// The documented contract: OnSuccess replaces issuing a cookie, so an
	// application that wants a session has to call Issue itself.
	if got := rec.Result().Header["Set-Cookie"]; len(got) != 0 {
		t.Errorf("Set-Cookie = %v, want none: the hook replaced the default", got)
	}
}

func TestLoginHandlerCustomFieldNames(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)

	handler := svc.LoginHandler(LoginOptions{
		UsernameField: "email",
		PasswordField: "secret",
		NextField:     "redirect",
		Fallback:      "/",
	})

	rec := serve(handler, loginForm(t, map[string]string{
		"email":    "ada",
		"secret":   testPassword,
		"redirect": "/dashboard",
	}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/dashboard" {
		t.Errorf("Location = %q", got)
	}
}

func TestLoginHandlerIgnoresBodyWithUnparseableForm(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)

	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("username=ada&password=%zz"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := serve(svc.LoginHandler(LoginOptions{}), req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: nothing usable was submitted", rec.Code)
	}
}

func TestIssueRefusesEmptyIdentity(t *testing.T) {
	svc := newTestService(t)

	// An empty principal that reaches a cookie would be an anonymous request
	// that satisfies RequireLogin. Refusing at the source is cheaper than
	// trusting every caller.
	rec := httptest.NewRecorder()
	if err := svc.Issue(rec, User{}); err == nil {
		t.Fatal("Issue with no identity should fail")
	}
	if got := rec.Result().Header["Set-Cookie"]; len(got) != 0 {
		t.Errorf("Set-Cookie = %v, want nothing written", got)
	}
}

func TestIssueNormalizesIdentity(t *testing.T) {
	svc := newTestService(t)

	rec := httptest.NewRecorder()
	if err := svc.Issue(rec, User{Identity: "  Ada@Example.COM  "}); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	session, ok := codecFor(t, testSessionKey).open(responseCookie(t, rec, DefaultCookieName).Value)
	if !ok {
		t.Fatal("issued cookie does not open")
	}
	if session.User.Identity != "ada@example.com" {
		t.Errorf("identity = %q", session.User.Identity)
	}
	if session.User.Groups == nil {
		t.Error("nil groups should become an empty set rather than a nil map")
	}
}

func TestIssueHonorsCookiePolicy(t *testing.T) {
	svc := newTestService(t)
	if err := svc.SetCookiePolicy(CookiePolicy{
		Name:     "myapp_session",
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		Lifetime: 15 * time.Minute,
	}); err != nil {
		t.Fatalf("SetCookiePolicy: %v", err)
	}

	rec := httptest.NewRecorder()
	if err := svc.Issue(rec, User{Identity: "ada"}); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	cookie := responseCookie(t, rec, "myapp_session")
	if !cookie.Secure {
		t.Error("Secure should follow the policy")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite = %v", cookie.SameSite)
	}
	if want := int((15 * time.Minute).Seconds()); cookie.MaxAge > want+1 || cookie.MaxAge < want-60 {
		t.Errorf("MaxAge = %d, want about %d", cookie.MaxAge, want)
	}
}

func TestClearSessionExpiresBothCookies(t *testing.T) {
	svc := newTestService(t)

	rec := httptest.NewRecorder()
	svc.ClearSession(rec)

	names := map[string]*http.Cookie{}
	for _, c := range rec.Result().Cookies() {
		names[c.Name] = c
	}

	session, ok := names[DefaultCookieName]
	if !ok {
		t.Fatalf("no session cookie cleared; got %v", rec.Result().Header["Set-Cookie"])
	}
	// A cleared cookie is empty and dated into the past. Max-Age comes back
	// as zero rather than negative because that is how net/http parses an
	// expired cookie; both spellings ask the browser to drop it.
	if session.Value != "" {
		t.Errorf("session cookie still has a value: %+v", session)
	}
	if session.Expires.IsZero() || session.Expires.After(time.Now()) {
		t.Errorf("session cookie = %+v, want an expiry in the past", session)
	}
	if session.MaxAge > 0 {
		t.Errorf("session cookie MaxAge = %d, want it to expire", session.MaxAge)
	}

	csrf, ok := names[csrfCookieName]
	if !ok {
		t.Fatal("the CSRF cookie should be cleared too")
	}
	if csrf.Value != "" || csrf.MaxAge > 0 {
		t.Errorf("CSRF cookie = %+v, want it emptied and expiring", csrf)
	}
	if csrf.Expires.IsZero() || csrf.Expires.After(time.Now()) {
		t.Errorf("CSRF cookie = %+v, want an expiry in the past", csrf)
	}
}

func TestLogoutHandlerClearsAndRedirects(t *testing.T) {
	svc := newTestService(t)

	rec := serve(svc.LogoutHandler("/bye"), httptest.NewRequest(http.MethodPost, "/logout", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/bye" {
		t.Errorf("Location = %q", got)
	}

	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == DefaultCookieName && c.Value == "" && !c.Expires.After(time.Now()) {
			cleared = true
		}
	}
	if !cleared {
		t.Error("the session cookie was not expired")
	}
}

func TestLogoutHandlerRejectsGET(t *testing.T) {
	// A logout reachable by GET is triggered by an <img> tag. The attack is a
	// nuisance; the reason it is refused here is that leaving it in means
	// every CSRF discussion has to include "except logout".
	svc := newTestService(t)

	rec := serve(svc.LogoutHandler("/"), httptest.NewRequest(http.MethodGet, "/logout", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow = %q, want POST", got)
	}
	if got := rec.Result().Header["Set-Cookie"]; len(got) != 0 {
		t.Errorf("Set-Cookie = %v, want nothing set by a refused request", got)
	}
}

func TestLogoutHandlerSanitizesRedirect(t *testing.T) {
	svc := newTestService(t)

	rec := serve(svc.LogoutHandler("//evil.com"), httptest.NewRequest(http.MethodPost, "/logout", nil))
	if got := rec.Header().Get("Location"); got != "/" {
		t.Fatalf("Location = %q, want / rather than an off-site target", got)
	}
}

func TestRedirectSeeOtherShape(t *testing.T) {
	rec := httptest.NewRecorder()
	redirectSeeOther(rec, "/somewhere")

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/somewhere" {
		t.Errorf("Location = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	// A 303 with no body should not claim a content type: the header is what
	// a client reads, and a stale one misleads proxies.
	if got := rec.Header().Get("Content-Type"); got != "" {
		t.Errorf("Content-Type = %q, want it removed", got)
	}
}

func TestOrDefault(t *testing.T) {
	if got := orDefault("", "fallback"); got != "fallback" {
		t.Errorf("got %q", got)
	}
	if got := orDefault("value", "fallback"); got != "value" {
		t.Errorf("got %q", got)
	}
}

func TestLoginHandlerReadsFieldConstants(t *testing.T) {
	// The field names are exported because the framework renders the form.
	// Renaming one here without renaming it there is a login form that does
	// nothing, so the constants are pinned.
	if UsernameField != "username" || PasswordField != "password" || NextField != "next" {
		t.Errorf("field names = %q/%q/%q", UsernameField, PasswordField, NextField)
	}
}
