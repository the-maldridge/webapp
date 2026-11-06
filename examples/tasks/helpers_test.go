package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/the-maldridge/webapp"
	"github.com/the-maldridge/webapp/auth"
	"github.com/the-maldridge/webapp/db"
	"github.com/the-maldridge/webapp/webapptest"
	"gorm.io/gorm"
)

// The helpers here are the ones that need the database behind the application
// rather than only its handler. A test asserting what a person saw reads the
// response; a test asserting what actually happened reads the row, because a
// page that says "saved" over a write that did not happen is exactly the bug
// worth catching.

// signInFor gives the client a session and then the CSRF token that goes with
// it.
//
// The order is the whole point. A token is bound to the identity it was minted
// for, so signing in and then submitting a form with the token from before the
// sign-in is a token bound to nobody, and the answer is a 403 that has nothing
// to do with what the test is about. Any page will do to mint one, and the sign
// in page answers whether or not a session is already present.
func signInFor(t *testing.T, a *webapptest.App, identity string, groups ...string) {
	t.Helper()

	a.SignIn(identity, groups...)
	if resp := a.Get("/login"); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /login to mint a CSRF token = %d", resp.StatusCode)
	}
}

// insertTask writes a row directly.
//
// Going through the form would make every pagination test also a test of form
// validation, and the rows are not what those tests are about.
func insertTask(t *testing.T, a *webapptest.App, identity, title string) *Task {
	t.Helper()

	task := &Task{ProfileID: profileIDFor(t, a, identity), Title: title}
	if err := db.G[Task](a.DB).Create(context.Background(), task); err != nil {
		t.Fatalf("insert task %q for %s: %v", title, identity, err)
	}
	return task
}

// taskID returns the address of the most recently created task belonging to an
// identity.
//
// The newest row is the one a test just created, which makes this the shortest
// path from "a handler wrote something" to "act on that thing" without the test
// hard-coding an identifier that only holds while the suite runs in one order.
func taskID(t *testing.T, a *webapptest.App, identity string) string {
	t.Helper()

	rows, err := db.G[Task](a.DB).
		Where("profile_id = ?", profileIDFor(t, a, identity)).
		Order("id desc").
		Find(context.Background())
	if err != nil {
		t.Fatalf("find tasks for %s: %v", identity, err)
	}
	if len(rows) == 0 {
		t.Fatalf("%s owns no task; the write this test is checking did not happen", identity)
	}
	return "/ui/task/" + strconv.FormatUint(uint64(rows[0].ID), 10)
}

// taskOwner returns the identity that owns the task at an address, or the empty
// string when there is no task there.
//
// It answers "is the row still in the database" separately from "what did the
// response say", which is the only way to tell a refusal that spared the row
// from a refusal that also deleted it.
func taskOwner(t *testing.T, a *webapptest.App, path string) string {
	t.Helper()

	id, err := strconv.ParseUint(idFromPath(path), 10, 64)
	if err != nil {
		t.Fatalf("task id in %q: %v", path, err)
	}

	task, err := db.G[Task](a.DB).Where("id = ?", id).First(context.Background())
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ""
		}
		t.Fatalf("load task %d: %v", id, err)
	}

	profile, err := db.G[auth.Profile](a.DB).Where("id = ?", task.ProfileID).First(context.Background())
	if err != nil {
		t.Fatalf("load profile %d for task %d: %v", task.ProfileID, id, err)
	}
	return profile.Identity
}

// taskCount counts the rows an identity owns, zero when the identity has no
// profile yet, which is the same answer as owning nothing.
func taskCount(t *testing.T, a *webapptest.App, identity string) int {
	t.Helper()

	profile, found := findProfile(t, a, identity)
	if !found {
		return 0
	}
	total, err := db.G[Task](a.DB).Where("profile_id = ?", profile.ID).Count(context.Background(), "id")
	if err != nil {
		t.Fatalf("count tasks for %s: %v", identity, err)
	}
	return int(total)
}

// profileIDFor returns the profile a signed-in identity was given.
//
// The row is created by the profile middleware on a request, so this needs the
// test to have made one as that identity first. A missing row says so in those
// words, because "profile not found" on its own reads like a bug in the
// framework rather than a missing line in the test.
func profileIDFor(t *testing.T, a *webapptest.App, identity string) uint {
	t.Helper()

	profile, found := findProfile(t, a, identity)
	if !found {
		t.Fatalf("no profile for %q: sign that identity in and make one request before using its rows", identity)
	}
	return profile.ID
}

// findProfile reads the profile row the authentication layer provisioned.
func findProfile(t *testing.T, a *webapptest.App, identity string) (auth.Profile, bool) {
	t.Helper()

	profile, err := db.G[auth.Profile](a.DB).
		Where("identity = ?", strings.ToLower(strings.TrimSpace(identity))).
		First(context.Background())
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return auth.Profile{}, false
		}
		t.Fatalf("load profile for %s: %v", identity, err)
	}
	return profile, true
}

// idFromPath pulls the identifier out of a task address, tolerating the action
// segment a caller may have appended.
func idFromPath(path string) string {
	part := strings.Trim(strings.TrimPrefix(path, "/ui/task/"), "/")
	if i := strings.IndexByte(part, '/'); i >= 0 {
		part = part[:i]
	}
	return part
}

// urlEscape encodes a form value. It exists so that a test submitting a title
// of eight hundred characters does not also have to think about whether the
// plus sign in it means a space.
func urlEscape(s string) string { return url.QueryEscape(s) }

// primaryNav returns the navigation region of a page.
//
// Searching the whole body for a link would pass on a page whose navigation was
// empty, because the page's own heading says the same words the menu does. The
// assertions here are about the menu, so only the menu is searched.
func primaryNav(t *testing.T, body string) string {
	t.Helper()

	start := strings.Index(body, `class="site-nav"`)
	if start < 0 {
		t.Fatalf("the page has no primary navigation:\n%s", tail(body))
	}
	end := strings.Index(body[start:], "</nav>")
	if end < 0 {
		t.Fatalf("the primary navigation is unterminated:\n%s", tail(body))
	}
	return body[start : start+end]
}

// itoa is strconv.Itoa under the name a test reads better.
func itoa(i int) string { return strconv.Itoa(i) }

// tail returns the end of a document, for failure messages about pages worth
// of HTML where the interesting line is the last one.
func tail(s string) string {
	const keep = 1500
	if len(s) <= keep {
		return s
	}
	return "[...] " + s[len(s)-keep:]
}

// spareModule is a module with no capabilities at all.
//
// It is here so that a whitelist can name something else, which is the only way
// to ask the registry to leave this example out: an empty list means "no
// whitelist" rather than "nothing", and a binary that links one module cannot
// select none by naming them.
type spareModule struct{}

// spareInfo describes that module.
var spareInfo = webapp.ModuleInfo{
	Name:    "spare",
	Factory: func(s *webapp.Scope) (any, error) { return &spareModule{}, nil },
}
