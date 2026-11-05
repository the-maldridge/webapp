package webapp

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/the-maldridge/webapp/db"
)

// BadInput reports that a request carried a value that could not be used
// as given. Handlers return it, and the error responder turns it into a
// 400 rather than a 500: a malformed URL is the client's mistake, and
// logging it as a server failure hides real ones.
type BadInput struct {
	// Name is the parameter the client got wrong.
	Name string

	// Value is what the client sent, included so the reason is
	// actionable. It is never rendered unescaped, and appears in detail
	// responses only in debug mode like any other error text.
	Value string

	// Reason explains what was expected.
	Reason string

	// Cause is the underlying parse error, when there was one.
	Cause error
}

func (b *BadInput) Error() string {
	if b.Cause != nil {
		return fmt.Sprintf("parameter %q: %s: %v", b.Name, b.Reason, b.Cause)
	}
	return fmt.Sprintf("parameter %q: %s", b.Name, b.Reason)
}

// Unwrap exposes the parse error for errors.Is and errors.As.
func (b *BadInput) Unwrap() error { return b.Cause }

// ErrBadInput reports whether an error is a client input problem, which
// is a 4xx rather than a 500.
func ErrBadInput(err error) bool {
	var bad *BadInput
	return errors.As(err, &bad)
}

// ParamUint reads a required path parameter as an unsigned integer.
//
// This is the helper that matters most. The idiomatic
// strconv.ParseUint(chi.URLParam(r, "id"), 10, 64) with the error
// discarded turns "abc" into 0, and 0 is a valid-looking ID: in a lookup
// it becomes a confusing 404, and in a create-with-parent it becomes a
// row attached to whatever happens to own key zero. Reporting the parse
// failure is the whole point.
func ParamUint(r *http.Request, name string) (uint64, error) {
	raw := URLParam(r, name)
	if raw == "" {
		return 0, &BadInput{Name: name, Reason: "is required"}
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, &BadInput{Name: name, Value: raw, Reason: "must be a non-negative integer", Cause: err}
	}
	return v, nil
}

// QueryUint reads an optional query parameter as an unsigned integer. An
// absent parameter yields zero and no error, which is the right default
// for filters; a present but malformed one is always an error, because
// silently ignoring a filter shows the user more rows than they asked
// for.
func QueryUint(r *http.Request, name string) (uint64, error) {
	return QueryUintDefault(r, name, 0)
}

// QueryUintDefault reads an optional query parameter as an unsigned
// integer with an explicit default for the absent case.
func QueryUintDefault(r *http.Request, name string, def uint64) (uint64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return def, &BadInput{Name: name, Value: raw, Reason: "must be a non-negative integer", Cause: err}
	}
	return v, nil
}

// QueryInt reads an optional query parameter as a signed integer.
func QueryInt(r *http.Request, name string) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, &BadInput{Name: name, Value: raw, Reason: "must be an integer", Cause: err}
	}
	return v, nil
}

// QueryBool reads an optional query parameter as a boolean.
//
// Absent means false. The string "false" is also false, which is why
// strconv.ParseBool is used rather than a non-empty check: the latter is
// how an application ends up treating <input type="hidden" value="false">
// as true.
func QueryBool(r *http.Request, name string) (bool, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return false, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, &BadInput{Name: name, Value: raw, Reason: "must be a boolean", Cause: err}
	}
	return v, nil
}

// QueryDate reads an optional query parameter as a date in ISO form.
//
// Layouts are tried in a fixed order and the first match wins, and the
// value must be the whole string: a prefix match would accept
// "2026-01-01T00:00:00Z" under a date-only layout in some engines and
// produce a silently different instant depending on which parse helper is
// used.
func QueryDate(r *http.Request, name string) (time.Time, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, nil
		}
	}
	return time.Time{}, &BadInput{
		Name:   name,
		Value:  raw,
		Reason: "must be an ISO date (YYYY-MM-DD) or an RFC 3339 timestamp",
	}
}

// Page reads the paging parameters from a request and clamps them with
// db.ClampPage, so that a request for page 999999999 or size 100000
// cannot ask the database for an offset past the end of a table or a
// result set sized to exhaust memory.
//
// Size is read from "size" and page from "page"; either may be absent.
func Page(r *http.Request) (page, size int, err error) {
	page, err = QueryInt(r, "page")
	if err != nil {
		return 1, db.DefaultPageSize, err
	}
	size, err = QueryInt(r, "size")
	if err != nil {
		return 1, db.DefaultPageSize, err
	}
	page, size = db.ClampPage(page, size)
	return page, size, nil
}

// FormString reads a submitted form value.
//
// The error is not optional in this API even though ParseForm's return is
// routinely discarded in calling code. A form that was never parsed reads
// as a form with no values, so a handler that ignores the failure reports
// every field as missing rather than reporting the real cause, which is
// usually an unreadable or oversized body.
func FormString(r *http.Request, name string) (string, error) {
	if err := parseForm(r); err != nil {
		return "", &BadInput{Name: name, Reason: "request body could not be parsed", Cause: err}
	}
	return r.PostFormValue(name), nil
}

// FormValues reads all submitted form values. The body size is already
// bounded by the framework; see DefaultMaxFormBytes.
func FormValues(r *http.Request) (url.Values, error) {
	if err := parseForm(r); err != nil {
		return nil, &BadInput{Reason: "request body could not be parsed", Cause: err}
	}
	return r.PostForm, nil
}

// parseForm parses the body once, relying on net/http's memoization so
// that calling it from several places in a request is free.
func parseForm(r *http.Request) error {
	if r.PostForm != nil {
		return nil
	}
	return r.ParseForm()
}
