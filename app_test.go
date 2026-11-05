package webapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHeartbeatAnswersWithoutRouting(t *testing.T) {
	a := newTestApp(t)

	rec := a.get(t, HeartbeatPath)
	if rec.Code != http.StatusOK {
		t.Errorf("heartbeat status = %d, want 200", rec.Code)
	}
	// The heartbeat must not require a route, and must not be the page
	// that a not-found handler would have produced.
	if strings.Contains(rec.Body.String(), "404") {
		t.Errorf("heartbeat produced a not-found body: %q", rec.Body.String())
	}
}

func TestNotFoundIsARealFourOhFour(t *testing.T) {
	a := newTestApp(t)

	rec := a.get(t, "/nothing/here")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; an error page served with 200 tells every client the request succeeded", rec.Code)
	}
}

func TestMethodNotAllowedIsARealFourOhFive(t *testing.T) {
	a := newTestApp(t)
	a.Route("/thing", func(r Router) { r.Get("/", okHandler) })

	rec := a.postForm(t, "/thing", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestSecureHeadersPresent(t *testing.T) {
	a := newTestApp(t)
	a.Route("/page", func(r Router) { r.Get("/", okHandler) })

	rec := a.get(t, "/page")
	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "same-origin",
		"X-Frame-Options":        "SAMEORIGIN",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestRequestIDIsInHeaderAndStableWithinRequest(t *testing.T) {
	a := newTestApp(t)

	var seen string
	a.Route("/x", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) { seen = a.requestID(r) })
	})

	rec := a.get(t, "/x")
	header := rec.Header().Get(requestIDHeader)
	if header == "" {
		t.Fatal("no correlation ID in the response header")
	}
	// A header captured before the ID existed in the context is the
	// classic way to end up with a header that is always empty.
	if header != seen {
		t.Errorf("header ID %q differs from the ID the handler saw %q", header, seen)
	}
}

func TestPanicBecomesFiveHundredAndProcessSurvives(t *testing.T) {
	recs := &recorder{}
	a := newTestApp(t, WithLogger(slogWith(recs)))
	a.Route("/boom", func(r Router) {
		r.Get("/", func(http.ResponseWriter, *http.Request) { panic("kaboom") })
	})
	a.Route("/fine", func(r Router) { r.Get("/", okHandler) })

	rec := a.get(t, "/boom")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	// A recovered panic must be visible: a connection reset with no log
	// line is how these bugs hide for months.
	levels := recs.atLevels("panic handled by webapp")
	if len(levels) != 1 || levels[0] < slog.LevelError {
		t.Errorf("expected one error-level panic record, got %v", levels)
	}
	// The panic value is logged, never rendered.
	if strings.Contains(rec.Body.String(), "kaboom") {
		t.Error("the panic value reached the client")
	}

	// The same application must still serve afterwards.
	if rec := a.get(t, "/fine"); rec.Code != http.StatusOK {
		t.Errorf("after a panic, subsequent request returned %d", rec.Code)
	}
}

func TestAbortHandlerStillAborts(t *testing.T) {
	a := newTestApp(t)
	a.Route("/abort", func(r Router) {
		r.Get("/", func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
	})

	// http.ErrAbortHandler means "drop this connection". Converting it to
	// a 500 would turn a correct abandonment into a bogus response, so it
	// must reach net/http. httptest does not run the real recovery, so
	// the panic propagating out of ServeHTTP is the pass condition.
	defer func() {
		rec := recover()
		if rec != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler to propagate", rec)
		}
	}()
	a.send(t, httptest.NewRequest(http.MethodGet, "/abort", nil))
	t.Error("expected the abort sentinel to panic")
}

func TestFailDisclosurePolicy(t *testing.T) {
	secret := errors.New("connection to postgres://user:hunter2@db.internal:5432 failed")

	t.Run("production hides detail", func(t *testing.T) {
		a := newTestApp(t)
		a.Route("/x", func(r Router) {
			r.Get("/", func(w http.ResponseWriter, r *http.Request) {
				a.Fail(w, r, http.StatusInternalServerError, secret)
			})
		})

		rec := a.get(t, "/x")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "hunter2") {
			t.Error("internal error text reached the client in production")
		}
		if !strings.Contains(rec.Body.String(), rec.Header().Get(requestIDHeader)) {
			t.Error("client got no reference matching a log line")
		}
	})

	t.Run("debug shows detail", func(t *testing.T) {
		a := newTestApp(t, WithDebug())
		a.Route("/x", func(r Router) {
			r.Get("/", func(w http.ResponseWriter, r *http.Request) {
				a.Fail(w, r, http.StatusInternalServerError, secret)
			})
		})

		rec := a.get(t, "/x")
		if !strings.Contains(rec.Body.String(), "hunter2") {
			t.Error("debug mode should surface the error for the developer")
		}
	})
}

func TestFailEscapesUntrustedText(t *testing.T) {
	a := newTestApp(t, WithDebug())
	xss := errors.New(`<script>alert("pwned")</script>`)
	a.Route("/x", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.Fail(w, r, http.StatusBadRequest, xss)
		})
	})

	rec := a.get(t, "/x")
	body := rec.Body.String()
	if strings.Contains(body, "<script>") {
		t.Error("error text was rendered unescaped; an error can quote a request body")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Error("expected the escaped form of the error text")
	}
}

func TestFailStatusIsNeverZero(t *testing.T) {
	a := newTestApp(t)
	a.Route("/x", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.Fail(w, r, 0, errors.New("boom"))
		})
	})

	if rec := a.get(t, "/x"); rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 for an unspecified status", rec.Code)
	}
}

func TestFailAfterBodyDoesNotWriteHeaders(t *testing.T) {
	a := newTestApp(t)
	a.Route("/partial", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "start of page")
			a.Fail(w, r, http.StatusInternalServerError, errors.New("late failure"))
		})
	})

	rec := a.get(t, "/partial")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want the already-written 200 to stand", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "Internal Server Error") {
		t.Error("an error page was appended to a response that had already begun")
	}
}

func TestJSONNegotiation(t *testing.T) {
	a := newTestApp(t, WithDebug())
	a.Route("/api", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.Fail(w, r, http.StatusNotFound, ErrNotFound)
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/api", nil)
	req.Header.Set("Accept", "application/json")
	rec := a.send(t, req)

	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"status":404`) {
		t.Errorf("body = %q, want a status field", body)
	}
	if !strings.Contains(body, `"requestId"`) {
		t.Errorf("body = %q, want a correlation ID", body)
	}
}

func TestBrowserAcceptsHTMLNotJSON(t *testing.T) {
	a := newTestApp(t)
	a.Route("/x", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			a.Fail(w, r, http.StatusNotFound, ErrNotFound)
		})
	})

	// A browser's Accept header mentions json. Returning JSON to a
	// browser looks like a broken page, so HTML has to win.
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	rec := a.send(t, req)

	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q, want HTML for a browser", got)
	}
}

func TestHandlerErrRouting(t *testing.T) {
	a := newTestApp(t)

	a.Route("/things", func(r Router) {
		r.GetErr("/missing", func(w http.ResponseWriter, r *http.Request) error {
			return fmt.Errorf("%w: thing 3", ErrNotFound)
		})
		r.GetErr("/broken", func(w http.ResponseWriter, r *http.Request) error {
			return errors.New("database on fire")
		})
		r.GetErr("/badid", func(w http.ResponseWriter, r *http.Request) error {
			_, err := ParamUint(r, "id")
			return err
		})
		r.GetErr("/ok", func(w http.ResponseWriter, r *http.Request) error {
			w.WriteHeader(http.StatusTeapot)
			return nil
		})
	})

	tests := []struct {
		path string
		want int
	}{
		{"/things/missing", http.StatusNotFound},
		{"/things/broken", http.StatusInternalServerError},
		{"/things/badid", http.StatusBadRequest},
		{"/things/ok", http.StatusTeapot},
	}
	for _, tc := range tests {
		if rec := a.get(t, tc.path); rec.Code != tc.want {
			t.Errorf("%s status = %d, want %d", tc.path, rec.Code, tc.want)
		}
	}
}

func TestRejectedReRendersWithValuesAndStatus(t *testing.T) {
	a := newTestApp(t)
	a.Renderer = &stubRenderer{}

	a.Route("/tasks", func(r Router) {
		r.PostErr("/new", func(w http.ResponseWriter, r *http.Request) error {
			return &Rejected{
				Template: "task_form.p2",
				Values:   Context{"title": "buy milk"},
				Reasons:  []string{"points must be at least 0"},
			}
		})
	})

	rec := a.postForm(t, "/tasks/new", "title=buy+milk&points=-5")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; a re-rendered form is not a success", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "rendered:task_form.p2") {
		t.Errorf("body = %q, want the form re-rendered", rec.Body.String())
	}
}

func TestSeeOther(t *testing.T) {
	a := newTestApp(t)
	a.Route("/done", func(r Router) {
		r.Post("/", func(w http.ResponseWriter, r *http.Request) {
			a.SeeOther(w, r, "/tasks/")
		})
	})

	rec := a.postForm(t, "/done", "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/tasks/" {
		t.Errorf("Location = %q", got)
	}
}

func TestServeListenerBindsAndShutsDownIdempotently(t *testing.T) {
	a := newTestApp(t)
	a.Route("/", func(r Router) { r.Get("/", okHandler) })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- a.ServeListener(ln) }()

	// Every default deadline must be non-zero; an unset read timeout is a
	// slow-loris primitive.
	waitForAddr(t, ln.Addr().String())
	resp, err := http.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// Shutdown is documented as safe to call repeatedly; a second signal
	// must not turn an orderly stop into a confusing error.
	if err := a.Shutdown(ctx); err != nil {
		t.Errorf("second Shutdown: %v", err)
	}

	if err := <-serveErr; err != nil {
		t.Errorf("ServeListener returned %v, want nil for an orderly shutdown", err)
	}
}

func TestServerTimeoutsAreApplied(t *testing.T) {
	a := newTestApp(t)
	a.Route("/", func(r Router) { r.Get("/", okHandler) })

	// The server is inspected where it is built. Reaching for a.server
	// after dialing a listener would race the goroutine that installs it.
	srv := a.newServer()
	if srv.ReadTimeout <= 0 || srv.ReadHeaderTimeout <= 0 || srv.WriteTimeout <= 0 || srv.IdleTimeout <= 0 {
		t.Errorf("http.Server timeouts must be non-zero, got %+v", srv)
	}
	if srv.ErrorLog == nil {
		t.Error("http.Server must bridge net/http errors into slog")
	}
}

func TestShutdownBeforeServeIsHarmless(t *testing.T) {
	a := newTestApp(t)
	if err := a.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown before Serve: %v", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	a := newTestApp(t)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Errorf("second Close should be a no-op, got %v", err)
	}
}

func TestCompressionLeavesUncodedResponsesUsable(t *testing.T) {
	a := newTestApp(t)
	body := strings.Repeat("compressible text ", 100)
	a.Route("/big", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprint(w, body)
		})
	})

	rec := a.get(t, "/big")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := strings.Repeat("compressible text ", 100); rec.Body.String() != got {
		t.Errorf("body length = %d, want %d", rec.Body.Len(), len(got))
	}
}

// okHandler is a handler that does nothing interesting.
func okHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// waitForAddr polls until a listener accepts a TCP connection, replacing
// a sleep in tests that bind an ephemeral port.
func waitForAddr(t *testing.T, addr string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("nothing accepting connections on %s", addr)
}
