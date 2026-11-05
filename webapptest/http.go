package webapptest

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// origin is the URL a test application pretends to live at.
//
// It exists because cookies are scoped to a URL and a handler driven through a
// RoundTripper has none: a request for "/ui/task/" carries no host, so the jar
// has nowhere to keep the session cookie it was issued, and the second request
// of a test arrives anonymous. That is a genuinely confusing failure, the code
// under test is correct and the identity has simply evaporated, so the origin
// is invented once here rather than left for each test to notice.
//
// It is https because the framework marks cookies Secure unless debug mode is
// on, and a jar asked to store a Secure cookie over http drops it silently.
const origin = "https://" + testHost

// testHost is the host portion of origin, named so that the transport and the
// jar agree on it without either repeating the literal.
const testHost = "webapp.test"

// handlerTransport drives an application's handler with no network in the way.
type handlerTransport struct{ handler http.Handler }

func (t handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "" {
		req.URL.Scheme = "https"
		req.URL.Host = testHost
	}
	if req.Host == "" {
		req.Host = req.URL.Host
	}

	rec := httptest.NewRecorder()
	t.handler.ServeHTTP(rec, req)

	res := rec.Result()
	// The jar takes the redirect target from the request when the response
	// records one, and a response without it has nowhere to send a relative
	// Location. Recording the request makes both work.
	res.Request = req
	return res, nil
}

// Client returns an HTTP client wired to the application's handler.
//
// Requests go directly to the handler: no listener, no port, no sleep, and no
// test that fails because 127.0.0.1 was busy. Cookies are kept across requests,
// so a sign-in followed by a page load behaves like a browser.
//
// Redirects are not followed. A successful write answers 303, and a client that
// silently followed it would turn a test of the redirect into a test of the page
// it landed on; assert the Location, or use Follow.
//
// The returned client is shared, so its cookie jar is the application's
// browser state: SignIn, SignOut, and requests through this client all affect
// one another, which is the point.
func (a *App) Client() *http.Client {
	a.T.Helper()

	if a.client == nil {
		a.client = &http.Client{
			Transport: handlerTransport{handler: a.Handler},
			Jar:       a.jar(),
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return a.client
}

// jar returns the cookie jar backing this application, creating it on first
// use.
func (a *App) jar() *cookiejar.Jar {
	a.T.Helper()

	if a.cookies == nil {
		a.cookies = newJar(a.T)
	}
	return a.cookies
}

// newJar builds an empty cookie jar.
func newJar(t *testing.T) *cookiejar.Jar {
	t.Helper()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	return jar
}

// Get issues a GET.
func (a *App) Get(path string) *http.Response {
	a.T.Helper()
	return a.do(http.MethodGet, path, "", nil)
}

// Head issues a HEAD.
func (a *App) Head(path string) *http.Response {
	a.T.Helper()
	return a.do(http.MethodHead, path, "", nil)
}

// Delete issues a DELETE.
func (a *App) Delete(path string) *http.Response {
	a.T.Helper()
	return a.do(http.MethodDelete, path, "", nil)
}

// PostForm sends an urlencoded body, adding the CSRF token the application
// expects for the current identity.
//
// Adding it is the default because a test that has to think about it every time
// it submits a form either omits it and reads a 403 it does not care about, or
// copy-pastes the incantation. A test that is about CSRF should say so by
// calling PostFormNoCSRF.
func (a *App) PostForm(path, body string) *http.Response {
	a.T.Helper()
	return a.post(path, body, true)
}

// PostFormValues sends urlencoded values, adding the CSRF token as for
// PostForm.
func (a *App) PostFormValues(path string, values url.Values) *http.Response {
	a.T.Helper()
	return a.PostForm(path, values.Encode())
}

// PostFormNoCSRF sends an urlencoded body with no CSRF token, which is the shape
// of a cross-site request. The expected answer from an application with
// authentication installed is 403.
func (a *App) PostFormNoCSRF(path, body string) *http.Response {
	a.T.Helper()
	return a.post(path, body, false)
}

// Post sends a body with an explicit content type and no CSRF handling, which is
// what a JSON API call wants. A caller that needs the token reads it from
// CSRFToken and sets the X-CSRF-Token header itself.
func (a *App) Post(path, contentType string, body io.Reader) *http.Response {
	a.T.Helper()
	return a.do(http.MethodPost, path, contentType, body)
}

// Do issues a request the caller built. The URL may be a bare path, in which
// case the test origin is supplied so that cookies travel with it.
//
// A request built by httptest.NewRequest works here too, with one adjustment
// made automatically: such a request carries RequestURI, the field marking it as
// already received by a server, and an HTTP client refuses to send one. Clearing
// it is correct because the request is about to be sent, and it keeps the
// familiar one-line fixture working.
func (a *App) Do(req *http.Request) *http.Response {
	a.T.Helper()

	if req.RequestURI != "" {
		req = req.Clone(req.Context())
		req.RequestURI = ""
	}
	if req.URL.Host == "" {
		req.URL.Scheme = "https"
		req.URL.Host = testHost
	}
	if req.Host == "" {
		req.Host = req.URL.Host
	}

	resp, err := a.Client().Do(req)
	if err != nil {
		a.T.Fatalf("%s %s: %v", req.Method, req.URL.RequestURI(), err)
	}
	return resp
}

// post assembles the body, optionally with a CSRF field, and sends it.
func (a *App) post(path, body string, withCSRF bool) *http.Response {
	a.T.Helper()

	if withCSRF {
		if token := a.CSRFToken(); token != "" && !strings.Contains(body, "csrf_token=") {
			if body != "" {
				body += "&"
			}
			body += "csrf_token=" + url.QueryEscape(token)
		}
	}
	return a.do(http.MethodPost, path, "application/x-www-form-urlencoded", strings.NewReader(body))
}

// do builds and issues a request, failing the test on transport errors. A
// transport error against an in-process handler means the handler panicked
// without the recovery middleware catching it, or the method was invalid; both
// are worth the test stopping for.
func (a *App) do(method, path, contentType string, body io.Reader) *http.Response {
	a.T.Helper()

	req, err := http.NewRequest(method, urlFor(path), body)
	if err != nil {
		a.T.Fatalf("%s %s: %v", method, path, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return a.Do(req)
}

// CSRFToken returns the CSRF token the client currently holds, or the empty
// string when it holds none.
//
// A token appears when a response has offered one, which the framework does on
// any page containing a form. A test that needs one before its first request,
// to submit to a handler that answers no page, should issue a GET first.
func (a *App) CSRFToken() string {
	a.T.Helper()

	for _, c := range a.jar().Cookies(baseURL("/")) {
		if c.Name == "csrf_token" {
			return c.Value
		}
	}
	return ""
}

// Follow follows the Location of a redirect response with GET, repeatedly, until
// a response is not a redirect. It fails the test when given a response that has
// no Location to follow.
//
// Following is explicit rather than automatic so that the redirect stays under
// test: the caller has already seen the 303 and its target.
func (a *App) Follow(resp *http.Response) *http.Response {
	a.T.Helper()

	current := resp
	for hops := 0; ; hops++ {
		target := current.Header.Get("Location")
		if target == "" {
			a.T.Fatalf("response %d has no Location to follow", current.StatusCode)
		}
		if hops == 10 {
			a.T.Fatal("following redirects reached ten hops; stopping")
		}

		next, err := url.Parse(target)
		if err != nil {
			a.T.Fatalf("parse redirect target %q: %v", target, err)
		}
		if next.Host != "" && next.Host != testHost {
			// The framework only emits same-site targets, so one pointing
			// elsewhere is the open-redirect bug this is here to catch. It
			// should fail rather than make a request to somebody else's server.
			a.T.Fatalf("redirect left the application: %s", next.String())
		}

		path := next.RequestURI()
		if path == "" {
			path = "/"
		}
		current = a.Get(path)
		if current.StatusCode < 300 || current.StatusCode >= 400 {
			return current
		}
	}
}

// urlFor qualifies a path with the test origin.
func urlFor(path string) string {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	if !strings.HasPrefix(path, "/") {
		return origin + "/" + path
	}
	return origin + path
}

// baseURL parses the test origin into a URL for the cookie jar, which keys by
// URL rather than by string.
func baseURL(path string) *url.URL {
	return &url.URL{Scheme: "https", Host: testHost, Path: orSlash(path)}
}

func orSlash(path string) string {
	if path == "" {
		return "/"
	}
	return path
}

// Body reads a response body and closes it, returning the text.
//
// It takes the testing.T rather than living as a method because the response is
// the framework's type and the reading is the test's business; a test that wants
// bytes reads them itself.
func Body(t *testing.T, resp *http.Response) string {
	t.Helper()

	if resp.Body == nil {
		return ""
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return string(data)
}
