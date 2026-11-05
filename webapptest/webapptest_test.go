package webapptest_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/the-maldridge/webapp"
	"github.com/the-maldridge/webapp/auth"
	"github.com/the-maldridge/webapp/webapptest"
)

// hello is the handler a test mounts when it only needs to know it arrived.
func hello(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("hello"))
}

func TestNewAppServesThroughTheWholeChain(t *testing.T) {
	app := webapptest.NewApp(t)
	app.Route("/probe", func(r webapp.Router) {
		r.Get("/", hello)
	})

	resp := app.Get("/probe/")
	body := webapptest.Body(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", resp.StatusCode, body)
	}
	if body != "hello" {
		t.Errorf("body = %q, want hello", body)
	}

	// The point of driving the real handler is that the real middleware ran.
	// These come from the framework's own chain, so a missing one means the
	// helper bypassed it.
	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "SAMEORIGIN",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if resp.Header.Get("X-Request-Id") == "" {
		t.Error("no correlation ID on the response; the request never entered the stack")
	}
}

func TestNewAppUsesATemporaryDatabase(t *testing.T) {
	app := webapptest.NewApp(t)

	// A relative path would put webapp.db in the source tree and share it
	// between every test in the package, which is how a suite starts depending
	// on the order it ran in.
	var no int
	var name, file string
	err := app.DB.Raw().Raw("PRAGMA database_list").Row().Scan(&no, &name, &file)
	if err != nil {
		t.Skipf("backend does not report a file path: %v", err)
	}
	if !strings.Contains(file, "test.db") {
		t.Errorf("database file = %q, want one in the test's temporary directory", file)
	}
}

func TestNoStrayDatabaseFile(t *testing.T) {
	app := webapptest.NewApp(t)
	if app.DB == nil {
		t.Fatal("application has no database handle")
	}

	// The suite's own guard: a helper defaulting to a relative path would leave
	// this file behind in the source tree.
	if _, err := os.Stat("./webapp.db"); err == nil {
		t.Error("./webapp.db exists; the test database escaped the temporary directory")
	}
}

func TestSignInSatisfiesARouteGuard(t *testing.T) {
	app := webapptest.NewApp(t)
	app.InstallAuth()
	app.Route("/guarded", func(r webapp.Router) {
		r.Use(app.RequireLogin())
		r.Get("/", func(w http.ResponseWriter, req *http.Request) {
			who, err := auth.MustIdentity(req)
			if err != nil {
				t.Errorf("MustIdentity: %v", err)
			}
			_, _ = w.Write([]byte(who))
		})
	})

	anonymous := app.Get("/guarded/")
	if anonymous.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", anonymous.StatusCode)
	}
	webapptest.Body(t, anonymous)

	app.SignIn("alice")
	signed := app.Get("/guarded/")
	body := webapptest.Body(t, signed)

	if signed.StatusCode != http.StatusOK {
		t.Fatalf("signed-in status = %d, want 200", signed.StatusCode)
	}
	if body != "alice" {
		t.Errorf("identity read by the handler = %q, want alice", body)
	}
}

func TestSignInCarriesGroupsToAGuard(t *testing.T) {
	app := webapptest.NewApp(t)
	app.InstallAuth()
	app.Route("/edit", func(r webapp.Router) {
		r.Get("/", r.Guard(auth.P("tasks", "edit"), hello))
	})

	app.SignIn("alice", "tasks:view")
	viewer := app.Get("/edit/")
	if viewer.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer status = %d, want 403", viewer.StatusCode)
	}
	webapptest.Body(t, viewer)

	// A second identity, because groups travel in the session: the same client
	// holding a different session must not inherit the first one's answer.
	app.SignIn("root", "core:admin")
	admin := app.Get("/edit/")
	if admin.StatusCode != http.StatusOK {
		t.Fatalf("admin status = %d, want 200, body %q",
			admin.StatusCode, webapptest.Body(t, admin))
	}
	webapptest.Body(t, admin)
}

func TestSignOutEndsTheSession(t *testing.T) {
	app := webapptest.NewApp(t)
	app.InstallAuth()
	app.Route("/guarded", func(r webapp.Router) {
		r.Use(app.RequireLogin())
		r.Get("/", hello)
	})

	app.SignIn("alice")
	resp := app.Get("/guarded/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("signed-in status = %d, want 200", resp.StatusCode)
	}
	webapptest.Body(t, resp)

	app.SignOut()
	after := app.Get("/guarded/")
	if after.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after SignOut status = %d, want 401", after.StatusCode)
	}
	webapptest.Body(t, after)
}

func TestPostFormCarriesTheCSRFToken(t *testing.T) {
	app := webapptest.NewApp(t)
	app.InstallAuth()
	app.Route("/write", func(r webapp.Router) {
		r.Use(app.RequireLogin())
		r.Get("/", hello)
		r.Post("/", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("written"))
		})
	})

	app.SignIn("alice", "tasks:edit")

	// The token arrives on a page response, exactly as it does for a browser.
	page := app.Get("/write/")
	if page.StatusCode != http.StatusOK {
		t.Fatalf("page status = %d, want 200", page.StatusCode)
	}
	webapptest.Body(t, page)
	if app.CSRFToken() == "" {
		t.Fatal("no CSRF token after a page load; a submitted form cannot pass the check")
	}

	noToken := app.PostFormNoCSRF("/write/", "title=x")
	if noToken.StatusCode != http.StatusForbidden {
		t.Fatalf("without a token status = %d, want 403", noToken.StatusCode)
	}
	webapptest.Body(t, noToken)

	withToken := app.PostForm("/write/", "title=x")
	body := webapptest.Body(t, withToken)
	if withToken.StatusCode != http.StatusOK {
		t.Fatalf("with a token status = %d, want 200, body %q", withToken.StatusCode, body)
	}
	if body != "written" {
		t.Errorf("body = %q, want written", body)
	}
}

func TestAccountReachesAWorkingLogin(t *testing.T) {
	app := webapptest.NewApp(t)
	svc := app.InstallAuth()
	app.Account("alice", "correct horse battery staple", "tasks:edit")

	app.Route("/login", func(r webapp.Router) {
		r.Handle("/", svc.LoginHandler(auth.LoginOptions{Fallback: "/probe/"}))
	})
	app.Route("/probe", func(r webapp.Router) {
		r.Get("/", hello)
	})
	app.Route("/edit", func(r webapp.Router) {
		r.Get("/", r.Guard(auth.P("tasks", "edit"), hello))
	})

	// The password is checked with bcrypt, so this is the slow test here. It is
	// the assertion that matters: an account the helper created has to be an
	// account that can actually sign in, or every test built on it is exercising
	// a state production cannot produce.
	resp := app.PostForm("/login", "username=alice&password=correct+horse+battery+staple")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303, body %q", resp.StatusCode, webapptest.Body(t, resp))
	}
	webapptest.Body(t, resp)

	if loc := resp.Header.Get("Location"); loc != "/probe/" {
		t.Errorf("Location = %q, want /probe/", loc)
	}

	// The session the login issued carries the granted group, which is what
	// makes the account useful rather than merely valid.
	guarded := app.Get("/edit/")
	if guarded.StatusCode != http.StatusOK {
		t.Fatalf("granted permission status = %d, want 200, body %q",
			guarded.StatusCode, webapptest.Body(t, guarded))
	}
	webapptest.Body(t, guarded)
}

func TestFollowLandsOnTheTarget(t *testing.T) {
	app := webapptest.NewApp(t)
	app.Route("/send", func(r webapp.Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) { app.SeeOther(w, r, "/arrive/") })
	})
	app.Route("/arrive", func(r webapp.Router) {
		r.Get("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("arrived")) })
	})

	resp := app.Get("/send/")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	webapptest.Body(t, resp)

	landed := app.Follow(resp)
	body := webapptest.Body(t, landed)
	if landed.StatusCode != http.StatusOK {
		t.Fatalf("followed status = %d, want 200", landed.StatusCode)
	}
	if body != "arrived" {
		t.Errorf("body = %q, want arrived", body)
	}
}

func TestClientNeedsNoListener(t *testing.T) {
	app := webapptest.NewApp(t)
	app.Route("/", func(r webapp.Router) {
		r.Get("/probe", hello)
	})

	// The configured address has no port and is never bound. A request that
	// succeeded only because something was listening would mean the helper goes
	// over the network, which is the thing it exists to avoid.
	if app.Config.Addr != "127.0.0.1:0" {
		t.Errorf("Config.Addr = %q, want the unbound test address", app.Config.Addr)
	}

	resp := app.Get("/probe")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	webapptest.Body(t, resp)
}

func TestDoAcceptsABuiltRequest(t *testing.T) {
	app := webapptest.NewApp(t)
	app.Route("/probe", func(r webapp.Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("accept=" + r.Header.Get("Accept")))
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/probe/", nil)
	req.Header.Set("Accept", "application/json")

	body := webapptest.Body(t, app.Do(req))
	if !strings.Contains(body, "accept=application/json") {
		t.Errorf("body = %q, want the header to reach the handler", body)
	}
}

func TestBodyReadsAnEmptyResponse(t *testing.T) {
	app := webapptest.NewApp(t)
	app.Route("/empty", func(r webapp.Router) {
		r.Get("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	})

	if body := webapptest.Body(t, app.Get("/empty/")); body != "" {
		t.Errorf("body = %q, want empty", body)
	}
}

func TestRequestIDIsQuotable(t *testing.T) {
	app := webapptest.NewApp(t)
	app.Route("/probe", func(r webapp.Router) {
		r.Get("/", hello)
	})

	resp := app.Get("/probe/")
	webapptest.Body(t, resp)

	id := resp.Header.Get("X-Request-Id")
	if id == "" {
		t.Fatal("no correlation ID")
	}
	if strings.ContainsAny(id, " \t\r\n") {
		t.Errorf("X-Request-Id = %q, want a value with no whitespace so a user can quote it", id)
	}
}
