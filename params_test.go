package webapp

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/the-maldridge/webapp/db"
)

// TestParamUintReportsParseFailure is the regression guard for the whole
// reason typed parameter helpers exist: "abc" must not become the
// plausible-looking ID zero.
func TestParamUintReportsParseFailure(t *testing.T) {
	a := newTestApp(t)

	var gotValue uint64
	var gotErr error

	a.Route("/things", func(r Router) {
		r.Get("/{id}", func(w http.ResponseWriter, r *http.Request) {
			gotValue, gotErr = ParamUint(r, "id")
			w.WriteHeader(http.StatusNoContent)
		})
	})

	cases := []struct {
		path string
	}{
		{"/things/abc"},
		{"/things/-1"},
		{"/things/1.5"},
		{"/things/"},
	}

	for _, tc := range cases {
		gotValue, gotErr = 0, nil
		rec := a.get(t, tc.path)
		if rec.Code != http.StatusNoContent {
			// The empty case is routed around by chi, which produces a
			// 404 rather than reaching the handler. That is an acceptable
			// outcome, but it must not be reported as a good one silently.
			if tc.path != "/things/" {
				t.Fatalf("%s: status = %d, want handler reached", tc.path, rec.Code)
			}
			continue
		}
		if gotErr == nil {
			t.Errorf("%s: ParamUint accepted %q and produced %d", tc.path, tc.path, gotValue)
			continue
		}
		if !ErrBadInput(gotErr) {
			t.Errorf("%s: error %v is not a *BadInput", tc.path, gotErr)
		}
		if gotValue != 0 {
			t.Errorf("%s: value = %d, want 0 alongside an error", tc.path, gotValue)
		}
	}

	// The positive control: a real number parses.
	rec := a.get(t, "/things/42")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if gotErr != nil || gotValue != 42 {
		t.Errorf("ParamUint = (%d, %v), want (42, nil)", gotValue, gotErr)
	}
}

// TestBadInputSurvivesWrapping covers handlers that decorate the error
// before returning it, which is the common case.
func TestBadInputSurvivesWrapping(t *testing.T) {
	inner := &BadInput{Name: "id", Reason: "must be a non-negative integer"}
	wrapped := fmt.Errorf("looking up task: %w", inner)

	if !ErrBadInput(wrapped) {
		t.Error("ErrBadInput did not see through fmt.Errorf wrapping")
	}

	var target *BadInput
	if !errors.As(wrapped, &target) {
		t.Fatal("errors.As could not extract the BadInput")
	}
	if target.Name != "id" {
		t.Errorf("Name = %q, want %q", target.Name, "id")
	}
	if inner.Unwrap() != nil {
		t.Error("a BadInput without a cause should unwrap to nil")
	}

	withCause := &BadInput{Name: "id", Reason: "boom", Cause: errors.New("parse failed")}
	if !errors.Is(withCause, withCause.Cause) {
		t.Error("BadInput.Unwrap must expose its cause to errors.Is")
	}
	if withCause.Error() == "" {
		t.Error("BadInput.Error must not be empty")
	}
}

// TestBadInputReachesBadRequest checks the responder's side of the
// contract: a malformed URL is the client's mistake.
func TestBadInputReachesBadRequest(t *testing.T) {
	a := newTestApp(t)

	a.Route("/widgets", func(r Router) {
		r.GetErr("/{id}", func(w http.ResponseWriter, r *http.Request) error {
			if _, err := ParamUint(r, "id"); err != nil {
				return fmt.Errorf("widget lookup: %w", err)
			}
			w.WriteHeader(http.StatusOK)
			return nil
		})
	})

	rec := a.get(t, "/widgets/not-a-number")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; a malformed path parameter is not a server failure", rec.Code)
	}
}

func TestQueryHelpers(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		target string
		check  func(t *testing.T, r *http.Request)
	}{
		{
			name:   "bool false literal",
			target: "/x?flag=false",
			check: func(t *testing.T, r *http.Request) {
				v, err := QueryBool(r, "flag")
				if err != nil || v {
					t.Errorf("QueryBool = (%v, %v), want (false, nil); a hidden input with value \"false\" is not true", v, err)
				}
			},
		},
		{
			name:   "bool absent",
			target: "/x",
			check: func(t *testing.T, r *http.Request) {
				v, err := QueryBool(r, "flag")
				if err != nil || v {
					t.Errorf("QueryBool absent = (%v, %v), want (false, nil)", v, err)
				}
			},
		},
		{
			name:   "bool junk",
			target: "/x?flag=maybe",
			check: func(t *testing.T, r *http.Request) {
				if _, err := QueryBool(r, "flag"); !ErrBadInput(err) {
					t.Errorf("QueryBool junk error = %v, want *BadInput", err)
				}
			},
		},
		{
			name:   "query uint absent is zero",
			target: "/x",
			check: func(t *testing.T, r *http.Request) {
				v, err := QueryUint(r, "owner")
				if err != nil || v != 0 {
					t.Errorf("QueryUint absent = (%d, %v), want (0, nil)", v, err)
				}
			},
		},
		{
			name:   "query uint junk errors",
			target: "/x?owner=abc",
			check: func(t *testing.T, r *http.Request) {
				if _, err := QueryUint(r, "owner"); !ErrBadInput(err) {
					t.Errorf("QueryUint junk error = %v, want *BadInput", err)
				}
			},
		},
		{
			name:   "query uint default",
			target: "/x",
			check: func(t *testing.T, r *http.Request) {
				v, err := QueryUintDefault(r, "owner", 7)
				if err != nil || v != 7 {
					t.Errorf("QueryUintDefault absent = (%d, %v), want (7, nil)", v, err)
				}
			},
		},
		{
			name:   "query int negative",
			target: "/x?delta=-3",
			check: func(t *testing.T, r *http.Request) {
				v, err := QueryInt(r, "delta")
				if err != nil || v != -3 {
					t.Errorf("QueryInt = (%d, %v), want (-3, nil)", v, err)
				}
			},
		},
		{
			name:   "date only",
			target: "/x?day=2026-01-02",
			check: func(t *testing.T, r *http.Request) {
				v, err := QueryDate(r, "day")
				if err != nil {
					t.Fatalf("QueryDate: %v", err)
				}
				if want := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC); !v.Equal(want) {
					t.Errorf("QueryDate = %v, want %v", v, want)
				}
			},
		},
		{
			name:   "rfc3339",
			target: "/x?day=2026-01-02T03:04:05Z",
			check: func(t *testing.T, r *http.Request) {
				v, err := QueryDate(r, "day")
				if err != nil {
					t.Fatalf("QueryDate: %v", err)
				}
				if want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC); !v.Equal(want) {
					t.Errorf("QueryDate = %v, want %v", v, want)
				}
			},
		},
		{
			name:   "date junk",
			target: "/x?day=yesterday",
			check: func(t *testing.T, r *http.Request) {
				if _, err := QueryDate(r, "day"); !ErrBadInput(err) {
					t.Errorf("QueryDate junk error = %v, want *BadInput", err)
				}
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.check(t, httptest.NewRequest(http.MethodGet, tc.target, nil))
		})
	}
}

func TestPageClamps(t *testing.T) {
	t.Parallel()

	cases := []struct {
		target   string
		wantPage int
		wantSize int
	}{
		{"/x", 1, db.DefaultPageSize},
		{"/x?page=3&size=10", 3, 10},
		{"/x?page=0", 1, db.DefaultPageSize},
		{"/x?page=-5", 1, db.DefaultPageSize},
		{"/x?size=0", 1, db.DefaultPageSize},
		{"/x?size=100000", 1, db.MaxPageSize},
		{"/x?page=999999999", 999999999, db.DefaultPageSize},
	}

	for _, tc := range cases {
		page, size, err := Page(httptest.NewRequest(http.MethodGet, tc.target, nil))
		if err != nil {
			t.Errorf("%s: Page: %v", tc.target, err)
			continue
		}
		if page != tc.wantPage || size != tc.wantSize {
			t.Errorf("%s: Page = (%d, %d), want (%d, %d)", tc.target, page, size, tc.wantPage, tc.wantSize)
		}
	}

	// A malformed page must be reported, not silently read as page one.
	if _, _, err := Page(httptest.NewRequest(http.MethodGet, "/x?page=one", nil)); !ErrBadInput(err) {
		t.Errorf("malformed page error = %v, want *BadInput", err)
	}
}

func TestFormHelpers(t *testing.T) {
	a := newTestApp(t)

	var gotValue string
	var gotErr error
	var gotCount int

	a.Route("/form", func(r Router) {
		r.Post("/", func(w http.ResponseWriter, r *http.Request) {
			gotValue, gotErr = FormString(r, "title")
			values, err := FormValues(r)
			if err == nil {
				gotCount = len(values)
			}
			w.WriteHeader(http.StatusNoContent)
		})
	})

	rec := a.postForm(t, "/form", "title=hello&tag=a&tag=b")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if gotErr != nil || gotValue != "hello" {
		t.Errorf("FormString = (%q, %v), want (%q, nil)", gotValue, gotErr, "hello")
	}
	if gotCount != 2 {
		t.Errorf("FormValues returned %d fields, want 2", gotCount)
	}
}
