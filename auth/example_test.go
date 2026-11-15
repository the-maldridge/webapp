// Examples for the auth package. The permission and redirect helpers are pure, so
// their examples run and assert output. Anything needing a database handle or a
// live request carries no Output comment, which makes "go test" compile it without
// running it: a documented call that no longer compiles is then a build failure
// rather than a plausible-looking paragraph.
package auth_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/the-maldridge/authware"

	"github.com/the-maldridge/webapp/auth"
	"github.com/the-maldridge/webapp/db"
)

// directoryMech is what an authentication mechanism looks like from the outside.
// The method set is authware's, so a value like this and one of authware's shipped
// backends are the same thing to the service: no adapter, no wrapper, no bridge
// type sitting in the application that only forwards calls.
type directoryMech struct{}

func (directoryMech) Name() string { return "directory" }

// AuthUserPassword returns nil to accept a password. To reject, return an error:
// auth.ErrBadCredential, authware's ErrUnauthenticated, ErrDoesNotExist for an
// unknown identity. Anything else is read as the backend being broken, logged as a
// warning, and the chain moves on.
func (directoryMech) AuthUserPassword(_ context.Context, identity, password string) error {
	if identity != "alice" || password != "correct horse" {
		return new(authware.ErrUnauthenticated)
	}
	return nil
}

// UserGroups returns the identity's group names, upper-cased, as a set.
func (directoryMech) UserGroups(_ context.Context, identity string) (map[string]struct{}, error) {
	if identity != "alice" {
		return nil, new(authware.ErrDoesNotExist)
	}
	return map[string]struct{}{"TASKS:EDIT": {}}, nil
}

// tokenMech resolves opaque bearer tokens. It owns its tokens entirely: what they
// look like, how long they last, and how one is revoked. The framework does not
// second-guess that.
type tokenMech struct{}

func (tokenMech) Name() string { return "issued-tokens" }

func (tokenMech) AuthToken(_ context.Context, token string) (authware.User, error) {
	if token != "s3cret" {
		return authware.User{}, auth.ErrTokenInvalid
	}
	return authware.User{
		Identity: "alice",
		Groups:   map[string]struct{}{"TASKS:VIEW": {}},
	}, nil
}

func ExampleP() {
	// Case in a literal cannot produce a permission nobody holds, because both
	// halves are upper-cased here.
	fmt.Println(auth.P("tasks", "edit"))
	fmt.Println(auth.P("TASKS", "EDIT"))

	// Output:
	// TASKS:EDIT
	// TASKS:EDIT
}

func ExampleCoreAdmin() {
	// The one spelling of the superuser grant.
	fmt.Println(auth.CoreAdmin())

	// Output:
	// CORE:ADMIN
}

func ExampleParsePerm() {
	p, err := auth.ParsePerm(" tasks : edit ")
	fmt.Println(p, err)

	// A malformed permission is an error rather than a permission that matches
	// nothing, because a typo in a config file should be reported.
	_, err = auth.ParsePerm("TASKS")
	fmt.Println(err)

	_, err = auth.ParsePerm("A:B:C")
	fmt.Println(err)

	// Output:
	// TASKS:EDIT <nil>
	// auth: invalid permission TASKS: expected MODULE:GRANT
	// auth: invalid permission A:B:C: may contain only one ':'
}

func ExampleSafeNext() {
	// Anything that could send the person somewhere else is replaced by the
	// fallback, including the percent-decoded form of the same trick.
	fmt.Println(auth.SafeNext("/ui/task/1", "/"))
	fmt.Println(auth.SafeNext("https://example.invalid", "/"))
	fmt.Println(auth.SafeNext("//example.invalid", "/"))
	fmt.Println(auth.SafeNext("/%2F%2Fexample.invalid", "/"))
	fmt.Println(auth.SafeNext("", "/ui"))

	// Output:
	// /ui/task/1
	// /
	// /
	// /
	// /ui
}

// ExampleNew builds a service over a database handle. It is compiled, not run.
func ExampleNew() {
	d, err := db.New(db.WithDriver(db.DriverSQLite), db.WithPath("tasks.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer d.Close()

	svc, err := auth.New(d,
		// Root authority for session cookies. In production this is required,
		// and a missing one is a startup failure rather than a per-process
		// secret that logs everyone out on restart.
		auth.WithSessionKey("set-me-from-a-secret-manager"),
		auth.WithProduction(true),
	)
	if err != nil {
		log.Fatal(err)
	}

	// What the middleware decided, for a request that already went through it.
	info := svc.Info(httptest.NewRequest(http.MethodGet, "/", nil))
	fmt.Println(info.Via, info.User.Identity)
}

// ExampleWithMechanism attaches a mechanism that verifies passwords somewhere
// other than this application's own database.
func ExampleWithMechanism() {
	d, err := db.New(db.WithDriver(db.DriverSQLite), db.WithPath("tasks.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer d.Close()

	// WithMechanism appends. WithOnlyMechanisms replaces the chain, which drops
	// the local password table from the credential path: accounts you did not
	// create become unable to sign in, and a forgotten administrator account is
	// not recoverable from inside the application. Bearer tokens are unaffected
	// either way; that chain is separate.
	svc, err := auth.New(d,
		auth.WithSessionKey("set-me-from-a-secret-manager"),
		auth.WithOnlyMechanisms(directoryMech{}),
	)
	if err != nil {
		log.Fatal(err)
	}

	user, err := svc.Authenticate(context.Background(), "alice", "correct horse")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(user.Identity, user.AuthedBy)

	// A wrong password is one error whatever the reason was: unknown account,
	// disabled account, and rejected credential are not distinguishable to the
	// person typing, which is the point.
	if _, err := svc.Authenticate(context.Background(), "alice", "wrong"); errors.Is(err, auth.ErrBadCredential) {
		fmt.Println("rejected")
	}
}

// ExampleWithTokenMechanism attaches a bearer-token chain of the application's own
// making, alongside a password chain it did not make.
func ExampleWithTokenMechanism() {
	d, err := db.New(db.WithDriver(db.DriverSQLite), db.WithPath("tasks.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer d.Close()

	// The two chains are independent, which is the feature: passwords come from
	// a directory, machine tokens stay rows in a database where someone can
	// revoke one. WithOnlyTokenMechanisms replaces the token chain, and note that
	// the built-in NewToken, ListTokens, and RevokeToken then write rows this
	// chain never reads, because the mechanism owns token lifetime.
	svc, err := auth.New(d,
		auth.WithSessionKey("set-me-from-a-secret-manager"),
		auth.WithMechanism(directoryMech{}),
		auth.WithTokenMechanism(tokenMech{}),
	)
	if err != nil {
		log.Fatal(err)
	}

	user, err := svc.AuthenticateToken(context.Background(), "s3cret")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(user.Identity)
}

// ExampleService_LoginHandler serves the login form endpoint.
func ExampleService_LoginHandler() {
	d, err := db.New(db.WithDriver(db.DriverSQLite), db.WithPath("tasks.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer d.Close()

	svc, err := auth.New(d, auth.WithSessionKey("set-me-from-a-secret-manager"))
	if err != nil {
		log.Fatal(err)
	}

	// POST only. The failure is one message whatever went wrong, the password is
	// never echoed back, and ?next= goes through SafeNext so a login form cannot
	// be turned into an open redirect.
	http.Handle("POST /login", svc.LoginHandler(auth.LoginOptions{
		Fallback: "/",
		OnSuccess: func(w http.ResponseWriter, r *http.Request, u auth.User, next string) {
			http.Redirect(w, r, next, http.StatusSeeOther)
		},
	}))
	http.Handle("POST /logout", svc.LogoutHandler("/"))
}

// ExampleService_NewToken issues a machine credential.
func ExampleService_NewToken() {
	d, err := db.New(db.WithDriver(db.DriverSQLite), db.WithPath("tasks.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer d.Close()

	svc, err := auth.New(d, auth.WithSessionKey("set-me-from-a-secret-manager"))
	if err != nil {
		log.Fatal(err)
	}

	// The plaintext token is returned once and never stored; the row holds a
	// digest and a prefix short enough to show in a list without being useful.
	result, err := svc.NewToken(context.Background(), "alice", "ci", 90*24*time.Hour)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(result.Record.Prefix)

	if err := svc.RevokeToken(context.Background(), result.Record.ID); err != nil {
		log.Fatal(err)
	}
}

// ExampleRequireOwnership shows the profile check for a row someone claims.
func ExampleRequireOwnership() {
	// In a real request the profile middleware has already stamped the numeric
	// profile of the authenticated identity; testas is the same stamp without a
	// login. A row stores the profile of whoever created it, because comparing
	// identity strings breaks the moment two mechanisms assert the same person
	// differently.
	r := httptest.NewRequest(http.MethodGet, "/ui/task/42", nil)
	own, err := auth.RequireOwnership(r, 42)
	if err != nil {
		// auth.ErrNotOwner, which is a 403 rather than a 404: the row exists and
		// this person may not see it.
		log.Fatal(err)
	}
	fmt.Println(own.Identity, own.ProfileID)
}
