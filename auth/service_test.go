package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/the-maldridge/authware"
	"golang.org/x/crypto/bcrypt"
)

// These assertions are the property this package promises about its mechanism
// interfaces: they are authware's, so a backend written against authware
// satisfies them with no forwarding type in between. Reintroducing a private
// interface with renamed methods fails compilation here rather than silently
// costing every consumer of those backends a shim.
var (
	_ authware.Authenticator      = Mechanism(nil)
	_ Mechanism                   = authware.Authenticator(nil)
	_ authware.TokenAuthenticator = TokenMechanism(nil)
	_ TokenMechanism              = authware.TokenAuthenticator(nil)
	_ PasswordUpgrader            = (*dbMechanism)(nil)
)

func TestCreateAccountNormalizesIdentity(t *testing.T) {
	svc := newTestService(t)

	acct := mustCreateAccount(t, svc, "  Ada@example.com  ", testPassword)
	if acct.Identity != "ada@example.com" {
		t.Fatalf("identity = %q, want lower-cased and trimmed", acct.Identity)
	}

	found, err := svc.Account(context.Background(), "ADA@EXAMPLE.COM")
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	if found.Identity != "ada@example.com" {
		t.Errorf("lookup by upper-case identity returned %q", found.Identity)
	}
}

func TestCreateAccountStoresNoPlaintext(t *testing.T) {
	svc := newTestService(t)
	acct := mustCreateAccount(t, svc, "ada", testPassword)

	if acct.PasswordHash == testPassword {
		t.Fatal("the password was stored verbatim")
	}
	if !strings.HasPrefix(acct.PasswordHash, "$2") {
		t.Errorf("hash does not look like bcrypt: %q", trunc(acct.PasswordHash, 12))
	}
}

func TestCreateAccountRejectsDuplicate(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)

	if _, err := svc.CreateAccount(context.Background(), "ADA", testPassword); err == nil {
		t.Fatal("a duplicate identity must be refused")
	}
}

func TestCreateAccountRejectsBadIdentities(t *testing.T) {
	svc := newTestService(t)
	for _, identity := range []string{"", "   ", "has space", strings.Repeat("x", 256)} {
		if _, err := svc.CreateAccount(context.Background(), identity, testPassword); err == nil {
			t.Errorf("CreateAccount(%q) should fail", trunc(identity, 20))
		}
	}
}

func TestPasswordLengthBoundaries(t *testing.T) {
	svc := newTestService(t)

	tooShort := strings.Repeat("a", minPasswordLength-1)
	if _, err := svc.CreateAccount(context.Background(), "short", tooShort); err == nil {
		t.Error("a password below the minimum must be refused")
	}

	exact := strings.Repeat("a", minPasswordLength)
	if _, err := svc.CreateAccount(context.Background(), "exact", exact); err != nil {
		t.Errorf("a password at exactly the minimum should be accepted: %v", err)
	}

	longest := strings.Repeat("a", maxPasswordLength)
	if _, err := svc.CreateAccount(context.Background(), "long", longest); err != nil {
		t.Errorf("a password at the bcrypt limit should be accepted: %v", err)
	}

	// bcrypt silently truncates at 72 bytes, so a longer passphrase would be
	// verified against less than the user thinks they chose.
	over := strings.Repeat("a", maxPasswordLength+1)
	if _, err := svc.CreateAccount(context.Background(), "over", over); err == nil {
		t.Error("a password past the bcrypt limit must be refused")
	}
}

func TestAuthenticateIndistinguishableFailures(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)
	mustCreateAccount(t, svc, "disabled", testPassword)
	if err := svc.SetDisabled(context.Background(), "disabled", true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}

	// Every one of these must produce the same outward error. A login form
	// that distinguishes them is an enumeration service.
	cases := []struct {
		name     string
		identity string
		password string
	}{
		{"unknown account", "nobody", testPassword},
		{"wrong password", "ada", "wrong wrong wrong"},
		{"disabled account", "disabled", testPassword},
		{"empty identity", "", testPassword},
		{"empty password", "ada", ""},
	}

	var first error
	for _, tc := range cases {
		_, err := svc.Authenticate(context.Background(), tc.identity, tc.password)
		if !errors.Is(err, ErrBadCredential) {
			t.Errorf("%s: err = %v, want ErrBadCredential", tc.name, err)
		}
		if first == nil {
			first = err
			continue
		}
		// Not merely equivalent under errors.Is: the same value. A wrapper
		// carrying a mechanism count would be a leak through anything that
		// logs or displays the error.
		if err != first {
			t.Errorf("%s: %v is not the same error as %v", tc.name, err, first)
		}
	}
}

func TestAuthenticateSuccess(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)
	mustGrant(t, svc, "ada", "TASKS:EDIT")

	user, err := svc.Authenticate(context.Background(), "ada", testPassword)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if user.Identity != "ada" {
		t.Errorf("identity = %q", user.Identity)
	}
	if user.AuthedBy != "database" {
		t.Errorf("AuthedBy = %q, want database", user.AuthedBy)
	}
	if _, held := user.Groups["TASKS:EDIT"]; !held {
		t.Errorf("groups = %v, want TASKS:EDIT", user.Groups)
	}
}

func TestAuthenticateIsCaseInsensitive(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)

	if _, err := svc.Authenticate(context.Background(), "ADA", testPassword); err != nil {
		t.Fatalf("upper-case login should succeed: %v", err)
	}
}

func TestNeedsUpgrade(t *testing.T) {
	// A hash recorded at a lower cost must be marked for replacement, or the
	// cost bump is a change that only applies to new accounts.
	weak := hashAtCost(t, "a password long enough", 10)
	if !needsUpgrade(weak) {
		t.Error("a cost-10 hash should need upgrading")
	}

	strong := hashAtCost(t, "a password long enough", bcryptCost)
	if needsUpgrade(strong) {
		t.Error("a hash at the current cost should not need upgrading")
	}

	if !needsUpgrade("not a bcrypt hash") {
		t.Error("an unreadable hash should be treated as needing replacement")
	}
}

func TestPasswordUpgradedOnLogin(t *testing.T) {
	svc := newTestServiceOn(t, newTestDB(t))

	const password = "a password long enough"
	mustCreateAccount(t, svc, "upgrade-me", password)

	weak := hashAtCost(t, password, 10)
	if err := svc.store.setPassword(context.Background(), "upgrade-me", weak); err != nil {
		t.Fatalf("setPassword: %v", err)
	}

	before, err := svc.Account(context.Background(), "upgrade-me")
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	if !needsUpgrade(before.PasswordHash) {
		t.Fatal("setup failed: stored hash is not weak")
	}

	if _, err := svc.Authenticate(context.Background(), "upgrade-me", password); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	after, err := svc.Account(context.Background(), "upgrade-me")
	if err != nil {
		t.Fatalf("Account after login: %v", err)
	}
	if needsUpgrade(after.PasswordHash) {
		t.Error("a successful login should have re-hashed at the current cost")
	}

	// And the password still works, which is the property that makes an
	// upgrade safe rather than a lockout.
	if _, err := svc.Authenticate(context.Background(), "upgrade-me", password); err != nil {
		t.Errorf("login after upgrade failed: %v", err)
	}
}

func TestGrantIsIdempotent(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)

	mustGrant(t, svc, "ada", "TASKS:EDIT")
	mustGrant(t, svc, "ada", "tasks:edit")

	groups, err := svc.Groups(context.Background(), "ada")
	if err != nil {
		t.Fatalf("Groups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %v, want exactly one membership", groups)
	}
}

func TestGrantUpperCases(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)
	mustGrant(t, svc, "ada", "tasks:edit")

	if _, held := mustGroups(t, svc, "ada")["TASKS:EDIT"]; !held {
		t.Fatal("group names must be stored upper-cased")
	}
}

func TestRevokeRemovesMembership(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)
	mustGrant(t, svc, "ada", "TASKS:EDIT")

	if err := svc.Revoke(context.Background(), "ada", "TASKS:EDIT"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if groups := mustGroups(t, svc, "ada"); len(groups) != 0 {
		t.Fatalf("groups after revoke = %v", groups)
	}

	// Revoking something not held is not an error: it is what already being
	// revoked means.
	if err := svc.Revoke(context.Background(), "ada", "TASKS:EDIT"); err != nil {
		t.Errorf("revoking an unheld group should be a no-op: %v", err)
	}
}

func TestGrantRejectsBadGroupNames(t *testing.T) {
	svc := newTestService(t)
	for _, group := range []string{"", "   ", "has space", strings.Repeat("g", 256)} {
		if err := svc.Grant(context.Background(), "ada", group); err == nil {
			t.Errorf("Grant(%q) should fail", trunc(group, 20))
		}
	}
}

func TestHasPermConsultsStore(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)

	granted, err := svc.HasPerm(context.Background(), "ada", P("tasks", "edit"))
	if err != nil {
		t.Fatalf("HasPerm: %v", err)
	}
	if granted {
		t.Fatal("a fresh account holds nothing")
	}

	mustGrant(t, svc, "ada", "TASKS:EDIT")
	granted, err = svc.HasPerm(context.Background(), "ADA", P("tasks", "edit"))
	if err != nil {
		t.Fatalf("HasPerm: %v", err)
	}
	if !granted {
		t.Fatal("HasPerm should see the grant without a session")
	}
}

func TestDisabledAccountCannotAuthenticate(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)

	if err := svc.SetDisabled(context.Background(), "ada", true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	if _, err := svc.Authenticate(context.Background(), "ada", testPassword); !errors.Is(err, ErrBadCredential) {
		t.Fatalf("err = %v, want ErrBadCredential", err)
	}

	if err := svc.SetDisabled(context.Background(), "ada", false); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	if _, err := svc.Authenticate(context.Background(), "ada", testPassword); err != nil {
		t.Fatalf("re-enabled account should authenticate: %v", err)
	}
}

func TestSetDisabledUnknownAccount(t *testing.T) {
	svc := newTestService(t)
	if err := svc.SetDisabled(context.Background(), "nobody", true); !errors.Is(err, ErrUnknownAccount) {
		t.Fatalf("err = %v, want ErrUnknownAccount", err)
	}
}

func TestDeleteAccountRemovesMemberships(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)
	mustGrant(t, svc, "ada", "TASKS:EDIT")

	if err := svc.DeleteAccount(context.Background(), "ada"); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if _, err := svc.Account(context.Background(), "ada"); !errors.Is(err, ErrUnknownAccount) {
		t.Fatalf("err = %v, want ErrUnknownAccount", err)
	}
	// The membership has to go with it: a grant with no account behind it is
	// invisible to an audit that starts from the account list.
	if groups := mustGroups(t, svc, "ada"); len(groups) != 0 {
		t.Fatalf("memberships survived deletion: %v", groups)
	}
}

func TestAccountsPagination(t *testing.T) {
	svc := newTestService(t)
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		mustCreateAccount(t, svc, name, testPassword)
	}

	page, err := svc.Accounts(context.Background(), 2, 0)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("len = %d, want 2", len(page))
	}
	if page[0].Identity != "a" || page[1].Identity != "b" {
		t.Errorf("first page = %q, %q; want a, b", page[0].Identity, page[1].Identity)
	}

	second, err := svc.Accounts(context.Background(), 2, 2)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if second[0].Identity != "c" {
		t.Errorf("second page starts %q, want c", second[0].Identity)
	}
}

func TestSetPasswordReplacesCredential(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)

	next := "an entirely new password"
	if err := svc.SetPassword(context.Background(), "ada", next); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if _, err := svc.Authenticate(context.Background(), "ada", testPassword); err == nil {
		t.Error("the old password must stop working")
	}
	if _, err := svc.Authenticate(context.Background(), "ada", next); err != nil {
		t.Errorf("the new password should work: %v", err)
	}
}

func TestNewTokenShowsPlaintextOnce(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)

	created, err := svc.NewToken(context.Background(), "ada", "ci", time.Hour)
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if created.Token == "" {
		t.Fatal("no plaintext token returned")
	}
	if created.Record.Digest != "" {
		t.Error("the stored digest must not be handed to the caller")
	}
	if created.Record.Prefix == "" {
		t.Error("a display prefix should be kept so tokens can be told apart")
	}
	if !strings.HasPrefix(created.Token, created.Record.Prefix) {
		t.Error("the prefix should be the token's leading characters")
	}
}

func TestTokenAuthenticates(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)
	mustGrant(t, svc, "ada", "TASKS:EDIT")

	created, err := svc.NewToken(context.Background(), "ada", "ci", time.Hour)
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}

	user, err := svc.AuthenticateToken(context.Background(), created.Token)
	if err != nil {
		t.Fatalf("AuthenticateToken: %v", err)
	}
	if user.Identity != "ada" {
		t.Errorf("identity = %q", user.Identity)
	}
	if _, held := user.Groups["TASKS:EDIT"]; !held {
		t.Errorf("groups = %v", user.Groups)
	}
}

func TestTokenUnknownOrTampered(t *testing.T) {
	svc := newTestService(t)

	// Unknown, empty, and wrong-length all give the same answer.
	for _, token := range []string{"", "   ", strings.Repeat("z", 64)} {
		if _, err := svc.AuthenticateToken(context.Background(), token); !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("AuthenticateToken(%q) err = %v, want ErrTokenInvalid", trunc(token, 12), err)
		}
	}
}

func TestTokenRevocation(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)

	created, err := svc.NewToken(context.Background(), "ada", "ci", 0)
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}

	if err := svc.RevokeToken(context.Background(), created.Record.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if _, err := svc.AuthenticateToken(context.Background(), created.Token); !errors.Is(err, ErrTokenInvalid) {
		t.Fatal("a revoked token must stop working")
	}

	// The row survives so the audit question "when was this revoked" has an
	// answer.
	list, err := svc.ListTokens(context.Background(), "ada")
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(list) != 1 || !list[0].Disabled {
		t.Fatalf("tokens = %+v, want one disabled record", list)
	}
}

func TestTokenExpiry(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)

	created, err := svc.NewToken(context.Background(), "ada", "short", time.Millisecond)
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}

	// Time is moved rather than slept on: the service has a clock seam exactly
	// so that expiry is testable without waiting.
	svc.now = func() time.Time { return time.Now().Add(time.Minute) }

	if _, err := svc.AuthenticateToken(context.Background(), created.Token); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("expired token err = %v, want ErrTokenInvalid", err)
	}
}

func TestListTokensNeverReturnsDigests(t *testing.T) {
	svc := newTestService(t)
	mustCreateAccount(t, svc, "ada", testPassword)

	for _, name := range []string{"one", "two"} {
		if _, err := svc.NewToken(context.Background(), "ada", name, 0); err != nil {
			t.Fatalf("NewToken: %v", err)
		}
	}

	list, err := svc.ListTokens(context.Background(), "ada")
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len = %d, want 2", len(list))
	}
	for _, tok := range list {
		if tok.Digest != "" {
			t.Errorf("digest leaked for token %q", tok.Name)
		}
	}
}

func TestProfileCreatedOnce(t *testing.T) {
	svc := newTestService(t)

	first, err := svc.Profile(context.Background(), "ada", "database")
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if first.ID == 0 {
		t.Fatal("profile has no ID")
	}

	second, err := svc.Profile(context.Background(), "ada", "database")
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("second call created a new row: %d vs %d", second.ID, first.ID)
	}
}

func TestProfileConcurrentFirstSight(t *testing.T) {
	// Two requests arriving for a brand-new identity race on the unique index.
	// Both must return a profile rather than one reporting a conflict.
	svc := newTestService(t)

	const workers = 8
	type outcome struct {
		id  uint
		err error
	}
	results := make(chan outcome, workers)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := svc.Profile(context.Background(), "racer", "database")
			if err != nil {
				results <- outcome{err: err}
				return
			}
			results <- outcome{id: p.ID}
		}()
	}
	wg.Wait()
	close(results)

	seen := map[uint]bool{}
	for o := range results {
		if o.err != nil {
			t.Fatalf("concurrent Profile: %v", o.err)
		}
		seen[o.id] = true
	}
	if len(seen) != 1 {
		t.Fatalf("racers produced %d distinct profiles", len(seen))
	}
}

func TestModelsArePinned(t *testing.T) {
	// Table names that move when an inflection rule changes are table names
	// that should be written down; the auth tables are security-relevant.
	want := []struct {
		model interface{ TableName() string }
		table string
	}{
		{Account{}, "auth_accounts"},
		{Group{}, "auth_groups"},
		{APIToken{}, "auth_api_tokens"},
		{Profile{}, "auth_profiles"},
	}
	for _, tc := range want {
		if got := tc.model.TableName(); got != tc.table {
			t.Errorf("%T.TableName() = %q, want %q", tc.model, got, tc.table)
		}
	}
}

func TestServiceModelsIncludesAllTables(t *testing.T) {
	svc := newTestService(t)
	models := svc.Models()
	if len(models) != 4 {
		t.Fatalf("Models() returned %d entries, want 4", len(models))
	}
}

func TestMechanismsReportNames(t *testing.T) {
	svc := newTestService(t)
	names := svc.Mechanisms()
	if len(names) != 1 || names[0] != "database" {
		t.Fatalf("mechanisms = %v, want [database]", names)
	}
}

func TestWithMechanismAppends(t *testing.T) {
	svc := newTestService(t, WithMechanism(stubMechanism{name: "stub"}))

	names := svc.Mechanisms()
	if len(names) != 2 || names[1] != "stub" {
		t.Fatalf("mechanisms = %v, want the local backend to stay first", names)
	}
}

func TestWithOnlyMechanismsDropsDatabase(t *testing.T) {
	svc := newTestService(t, WithOnlyMechanisms(stubMechanism{name: "stub"}))

	names := svc.Mechanisms()
	if len(names) != 1 || names[0] != "stub" {
		t.Fatalf("mechanisms = %v, want [stub]", names)
	}
	// The local table is gone, so a database account must not authenticate.
	if _, err := svc.Authenticate(context.Background(), "ada", testPassword); !errors.Is(err, ErrBadCredential) {
		t.Fatalf("err = %v", err)
	}
}

func TestOptionValidation(t *testing.T) {
	svc := newTestService(t)
	if err := WithMechanism(nil)(svc); err == nil {
		t.Error("WithMechanism(nil) should fail")
	}
	if err := WithOnlyMechanisms()(svc); err == nil {
		t.Error("WithOnlyMechanisms() with none should fail")
	}
	if err := WithTokenMechanism(nil)(svc); err == nil {
		t.Error("WithTokenMechanism(nil) should fail")
	}
	if err := WithOnlyTokenMechanisms()(svc); err == nil {
		t.Error("WithOnlyTokenMechanisms() with none should fail")
	}
	if err := WithOnlyTokenMechanisms(nil)(svc); err == nil {
		t.Error("WithOnlyTokenMechanisms(nil) should fail")
	}
	if err := WithLogger(nil)(svc); err == nil {
		t.Error("WithLogger(nil) should fail")
	}
}

func TestNewRequiresDatabase(t *testing.T) {
	if _, err := New(nil, WithSessionKey(testSessionKey)); err == nil {
		t.Fatal("New(nil) must fail")
	}
}

// stubMechanism is a Mechanism that accepts one fixed credential.
type stubMechanism struct{ name string }

func (m stubMechanism) Name() string { return m.name }

func (m stubMechanism) AuthUserPassword(_ context.Context, identity, password string) error {
	if identity == "stub" && password == "stub password" {
		return nil
	}
	return ErrUnknownAccount
}

func (m stubMechanism) UserGroups(context.Context, string) (map[string]struct{}, error) {
	return map[string]struct{}{"STUB:VIEW": {}}, nil
}

// authwareStyleMechanism rejects with authware's own error type rather than this
// package's sentinels, which is what a backend from authware's backend packages
// does. It authenticates no one.
type authwareStyleMechanism struct{}

func (authwareStyleMechanism) Name() string { return "authware-style" }

func (authwareStyleMechanism) AuthUserPassword(context.Context, string, string) error {
	return new(authware.ErrUnauthenticated)
}

func (authwareStyleMechanism) UserGroups(context.Context, string) (map[string]struct{}, error) {
	return nil, new(authware.ErrUnauthenticated)
}

// TestAuthwareBackendNeedsNoBridge hands an authware-shaped mechanism to
// WithMechanism as it is. A rejection has to read as "not mine", so a mechanism
// later in the chain still gets its turn, and the rejection must not be
// reported as anything but the one indistinguishable failure.
func TestAuthwareBackendNeedsNoBridge(t *testing.T) {
	svc := newTestService(t, WithMechanism(authwareStyleMechanism{}))

	if _, err := svc.Authenticate(context.Background(), "ada", testPassword); !errors.Is(err, ErrBadCredential) {
		t.Fatalf("err = %v, want ErrBadCredential", err)
	}

	svc = newTestService(t, WithOnlyMechanisms(
		authwareStyleMechanism{},
		stubMechanism{name: "stub"},
	))
	user, err := svc.Authenticate(context.Background(), "stub", "stub password")
	if err != nil {
		t.Fatalf("Authenticate past an authware-style rejection: %v", err)
	}
	if user.AuthedBy != "stub" {
		t.Errorf("AuthedBy = %q, want stub", user.AuthedBy)
	}
	if _, held := user.Groups["STUB:VIEW"]; !held {
		t.Errorf("groups = %v, want STUB:VIEW", user.Groups)
	}
}

func TestExpectedRejection(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"local unknown account", ErrUnknownAccount, true},
		{"local bad credential", ErrBadCredential, true},
		{"local disabled", ErrAccountDisabled, true},
		{"authware unauthenticated", new(authware.ErrUnauthenticated), true},
		{"authware does not exist", new(authware.ErrDoesNotExist), true},
		{"wrapped authware", errors.Join(errors.New("backend"), new(authware.ErrUnauthenticated)), true},
		{"authware backend internal", new(authware.ErrBackendInternal), false},
		{"unrelated failure", errors.New("connection refused"), false},
	} {
		if got := expectedRejection(tc.err); got != tc.want {
			t.Errorf("expectedRejection(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// stubTokenMechanism is a TokenMechanism that resolves one fixed token and
// reports whatever identity and groups it was built with, so that the rules
// about what a mechanism is allowed to assert are visible in one place.
type stubTokenMechanism struct {
	name     string
	token    string
	identity string
	groups   map[string]struct{}
}

func (m stubTokenMechanism) Name() string { return m.name }

func (m stubTokenMechanism) AuthToken(_ context.Context, token string) (authware.User, error) {
	if token != m.token {
		return authware.User{}, new(authware.ErrUnauthenticated)
	}
	return authware.User{Identity: m.identity, Groups: m.groups}, nil
}

func TestWithTokenMechanismAppends(t *testing.T) {
	svc := newTestService(t, WithTokenMechanism(stubTokenMechanism{
		name:     "external",
		token:    "external token",
		identity: "ci@example.com",
		groups:   map[string]struct{}{"TASKS:RUN": {}},
	}))

	names := svc.TokenMechanisms()
	if len(names) != 2 || names[0] != "database" || names[1] != "external" {
		t.Fatalf("token mechanisms = %v, want [database external]", names)
	}

	// Appending leaves the local path working, which is the reason the option
	// appends: machine credentials an operator can still revoke.
	mustCreateAccount(t, svc, "ada", testPassword)
	created, err := svc.NewToken(context.Background(), "ada", "ci", time.Hour)
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}

	local, err := svc.AuthenticateToken(context.Background(), created.Token)
	if err != nil {
		t.Fatalf("AuthenticateToken(local): %v", err)
	}
	if local.AuthedBy != "database" {
		t.Errorf("AuthedBy = %q, want database", local.AuthedBy)
	}

	foreign, err := svc.AuthenticateToken(context.Background(), "external token")
	if err != nil {
		t.Fatalf("AuthenticateToken(external): %v", err)
	}
	if foreign.AuthedBy != "external" {
		t.Errorf("AuthedBy = %q, want external", foreign.AuthedBy)
	}
}

func TestWithOnlyTokenMechanismsDropsDatabase(t *testing.T) {
	handle := newTestDB(t)

	// Issue a local token while the database chain is installed.
	withLocal := newTestServiceOn(t, handle)
	mustCreateAccount(t, withLocal, "ada", testPassword)
	created, err := withLocal.NewToken(context.Background(), "ada", "ci", time.Hour)
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}

	svc := newTestServiceOn(t, handle, WithOnlyTokenMechanisms(stubTokenMechanism{
		name:     "external",
		token:    "external token",
		identity: "ci@example.com",
	}))

	if names := svc.TokenMechanisms(); len(names) != 1 || names[0] != "external" {
		t.Fatalf("token mechanisms = %v, want [external]", names)
	}

	// The row still exists and nothing reads it: that is the documented
	// consequence of replacing the chain, not a leak.
	if _, err := svc.AuthenticateToken(context.Background(), created.Token); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("dropped database token err = %v, want ErrTokenInvalid", err)
	}
	if _, err := svc.AuthenticateToken(context.Background(), "external token"); err != nil {
		t.Errorf("external token: %v", err)
	}
}

// TestTokenMechanismChainsAreOrthogonal covers the configuration the two chains
// exist to allow: people against a non-database password mechanism, machines
// against the database, one credential type not implying the other.
func TestTokenMechanismChainsAreOrthogonal(t *testing.T) {
	svc := newTestService(t,
		WithOnlyMechanisms(stubMechanism{name: "directory"}),
		WithTokenMechanism(stubTokenMechanism{
			name:     "external",
			token:    "external token",
			identity: "ci@example.com",
		}),
	)

	if names := svc.Mechanisms(); len(names) != 1 || names[0] != "directory" {
		t.Errorf("password mechanisms = %v, want [directory]", names)
	}
	if names := svc.TokenMechanisms(); len(names) != 2 || names[0] != "database" {
		t.Errorf("token mechanisms = %v, want the database chain kept", names)
	}

	// Local passwords are gone, and bearer tokens over the same table are not.
	if _, err := svc.Authenticate(context.Background(), "ada", testPassword); !errors.Is(err, ErrBadCredential) {
		t.Errorf("password through dropped chain err = %v, want ErrBadCredential", err)
	}
	if _, err := svc.AuthenticateToken(context.Background(), "external token"); err != nil {
		t.Errorf("external token: %v", err)
	}
}

func TestTokenMechanismIdentityAndGroups(t *testing.T) {
	svc := newTestService(t, WithTokenMechanism(stubTokenMechanism{
		name:     "external",
		token:    "external token",
		identity: "  CI@Example.COM  ",
		groups:   map[string]struct{}{"foreign:group": {}, "TASKS:RUN": {}},
	}))

	user, err := svc.AuthenticateToken(context.Background(), "external token")
	if err != nil {
		t.Fatalf("AuthenticateToken: %v", err)
	}
	// Identity is normalized into the local namespace; groups are exactly what
	// the mechanism reported, case included.
	if user.Identity != "ci@example.com" {
		t.Errorf("identity = %q, want ci@example.com", user.Identity)
	}
	if _, held := user.Groups["foreign:group"]; !held {
		t.Errorf("groups = %v, want the mechanism's groups passed through", user.Groups)
	}
}

func TestTokenMechanismWithBlankIdentityIsSkipped(t *testing.T) {
	svc := newTestService(t, WithTokenMechanism(stubTokenMechanism{
		name:     "blank",
		token:    "external token",
		identity: "   ",
	}))

	// A mechanism asserting whitespace has no more claim to be a principal than
	// one asserting nothing, and an empty identity must not reach a handler.
	user, err := svc.AuthenticateToken(context.Background(), "external token")
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
	if user.Identity != "" {
		t.Errorf("identity = %q, want empty", user.Identity)
	}
}

// mustGroups reads an identity's groups, failing the test on error.
func mustGroups(t *testing.T, svc *Service, identity string) map[string]struct{} {
	t.Helper()

	groups, err := svc.Groups(context.Background(), identity)
	if err != nil {
		t.Fatalf("Groups(%q): %v", identity, err)
	}
	return groups
}

// hashAtCost produces a bcrypt sum at an explicit cost, for the upgrade tests.
func hashAtCost(t *testing.T, password string, cost int) string {
	t.Helper()

	sum, err := bcrypt.GenerateFromPassword([]byte(password), cost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	return string(sum)
}

// trunc shortens a string for a failure message.
func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
