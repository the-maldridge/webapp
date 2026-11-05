package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSessionCodecRoundTrip(t *testing.T) {
	codec := codecFor(t, testSessionKey)

	want := Session{
		Expires: time.Now().Add(time.Hour).UTC().Truncate(time.Second),
		User:    User{Identity: "ada", Groups: map[string]struct{}{"TASKS:EDIT": {}}},
	}
	value, err := codec.seal(want)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	got, ok := codec.open(value)
	if !ok {
		t.Fatal("open rejected a value it sealed")
	}
	if got.User.Identity != want.User.Identity {
		t.Errorf("identity = %q, want %q", got.User.Identity, want.User.Identity)
	}
	if _, held := got.User.Groups["TASKS:EDIT"]; !held {
		t.Errorf("groups = %v, want TASKS:EDIT", got.User.Groups)
	}
	if !got.Expires.Equal(want.Expires) {
		t.Errorf("expires = %v, want %v", got.Expires, want.Expires)
	}
}

func TestSessionCodecRejectsWrongKey(t *testing.T) {
	value, err := codecFor(t, testSessionKey).seal(Session{
		Expires: time.Now().Add(time.Hour),
		User:    User{Identity: "ada"},
	})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	other := codecFor(t, "ffffeeeeddddccccbbbbaaaa9999888877776666555544443333222211110000")
	if got, ok := other.open(value); ok {
		t.Fatalf("a different key opened the session: %+v", got)
	}
}

func TestSessionCodecRejectsTampering(t *testing.T) {
	value, err := codecFor(t, testSessionKey).seal(Session{
		Expires: time.Now().Add(time.Hour),
		User:    User{Identity: "ada"},
	})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	// Flip one character. AES-GCM authentication must notice; a codec that
	// decrypts to garbage and reports success is worse than one that fails.
	mid := len(value) / 2
	tampered := value[:mid] + string(flipByte(value[mid])) + value[mid+1:]

	if got, ok := codecFor(t, testSessionKey).open(tampered); ok {
		t.Fatalf("tampered value accepted: %+v", got)
	}
}

func TestSessionCodecRejectsMalformed(t *testing.T) {
	codec := codecFor(t, testSessionKey)
	for _, v := range []string{"", "not-base64-$$$", "AAAAAAAA"} {
		if _, ok := codec.open(v); ok {
			t.Errorf("open(%q) accepted malformed input", v)
		}
	}
}

// flipByte changes a character within the base64url alphabet so the tampered
// value still decodes as base64 and reaches the authentication layer.
func flipByte(b byte) byte {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	for _, c := range []byte(alphabet) {
		if c != b {
			return c
		}
	}
	return 'A'
}

func TestNewSessionCodecRejectsShortKey(t *testing.T) {
	// The underlying library accepts a key of any length, which means a weak
	// key is silently weak. This is the only place that says otherwise.
	_, err := newSessionCodec("hunter2")
	if err == nil {
		t.Fatal("a 7-byte key must be refused")
	}
	if !strings.Contains(err.Error(), "openssl rand -hex 32") {
		t.Errorf("error should say how to fix it, got: %v", err)
	}
}

func TestNewSessionCodecAcceptsExactlyMinimum(t *testing.T) {
	if _, err := newSessionCodec(strings.Repeat("a", sessionKeySize)); err != nil {
		t.Fatalf("a minimum-length key should be accepted: %v", err)
	}
}

func TestReadSessionValueRejectsDuplicates(t *testing.T) {
	// Two cookies of the same name are legal to send, and net/http hands back
	// the first match. For an authorization decision that is unacceptable in
	// either direction, so a request carrying two is treated as carrying none.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: "first"})
	req.AddCookie(&http.Cookie{Name: "session", Value: "second"})

	if _, ok := readSessionValue(req, "session"); ok {
		t.Fatal("a request with two session cookies must yield nothing")
	}
}

func TestReadSessionValueSingle(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: "only"})

	value, ok := readSessionValue(req, "session")
	if !ok || value != "only" {
		t.Fatalf("readSessionValue = %q, %v", value, ok)
	}
}

func TestReadSessionValueIgnoresOtherNames(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "other", Value: "x"})

	if _, ok := readSessionValue(req, "session"); ok {
		t.Fatal("a differently-named cookie must not satisfy the lookup")
	}
}

func TestDeriveCSRFKeyDiffersFromSessionKey(t *testing.T) {
	// The two must never be the same bytes: one signature usable for both
	// purposes turns a CSRF oracle into a session forgery oracle.
	derived, err := deriveCSRFKey(testSessionKey)
	if err != nil {
		t.Fatalf("deriveCSRFKey: %v", err)
	}
	if string(derived) == testSessionKey {
		t.Fatal("CSRF key must not equal the session key")
	}
	if len(derived) != 32 {
		t.Errorf("CSRF key length = %d, want 32", len(derived))
	}
}

func TestDeriveCSRFKeyDeterministic(t *testing.T) {
	a, _ := deriveCSRFKey(testSessionKey)
	b, _ := deriveCSRFKey(testSessionKey)
	if string(a) != string(b) {
		t.Fatal("the same session key must derive the same CSRF key")
	}

	other, _ := deriveCSRFKey(strings.ToUpper(testSessionKey))
	if string(a) == string(other) {
		t.Fatal("different session keys must derive different CSRF keys")
	}
}

func TestDeriveCSRFKeyRejectsEmpty(t *testing.T) {
	if _, err := deriveCSRFKey(""); err == nil {
		t.Fatal("an empty session key must not derive a usable CSRF key")
	}
}

func TestMustIdentity(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, err := MustIdentity(req); !errors.Is(err, ErrNoUser) {
		t.Fatalf("anonymous MustIdentity err = %v, want ErrNoUser", err)
	}

	ctx := ContextWithUser(req.Context(), User{Identity: "ada"})
	if got, err := MustIdentity(req.WithContext(ctx)); err != nil || got != "ada" {
		t.Fatalf("MustIdentity = %q, %v", got, err)
	}
}

func TestMustIdentityRejectsEmptyIdentity(t *testing.T) {
	// A user present in the context with no identity is the shape a bug
	// produces, and treating it as authenticated is the worst outcome.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := ContextWithUser(req.Context(), User{})
	if _, err := MustIdentity(req.WithContext(ctx)); !errors.Is(err, ErrNoUser) {
		t.Fatalf("err = %v, want ErrNoUser", err)
	}
}

func TestHasPermOnRequest(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if HasPerm(req, CoreAdmin()) {
		t.Fatal("anonymous request must hold nothing")
	}

	ctx := ContextWithUser(req.Context(), User{Identity: "ada", Groups: map[string]struct{}{"CORE:ADMIN": {}}})
	if !HasPerm(req.WithContext(ctx), P("tasks", "view")) {
		t.Fatal("CORE:ADMIN should satisfy TASKS:VIEW")
	}
}

func TestHasGroupIsCaseInsensitive(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := ContextWithUser(req.Context(), User{Groups: map[string]struct{}{"TASKS:ADMIN": {}}})
	if !HasGroup(req.WithContext(ctx), "tasks:admin") {
		t.Fatal("HasGroup should ignore case")
	}
	if HasGroup(req.WithContext(ctx), "core:admin") {
		t.Fatal("unrelated group must not match")
	}
}

func TestUserAliasKeepsContextCompatible(t *testing.T) {
	// User is an alias for authware's type, which is the property that lets a
	// session issued here be read by an application using authware's helper.
	var u authwareUser
	u.Identity = "ada"

	ctx := ContextWithUser(req().Context(), u)
	got, ok := UserFromContext(ctx)
	if !ok || got.Identity != "ada" {
		t.Fatalf("UserFromContext = %+v, %v", got, ok)
	}

	// The request-level accessor must agree with the context-level one.
	if fromReq, ok := UserFrom(req().WithContext(ctx)); !ok || fromReq.Identity != got.Identity {
		t.Fatalf("UserFrom = %+v, %v; want %+v", fromReq, ok, got)
	}
}

// authwareUser is written through the alias so the test fails to compile if
// User ever stops being an alias.
type authwareUser = User

// req returns a fresh request; a helper keeps the assertions above readable.
func req() *http.Request { return httptest.NewRequest(http.MethodGet, "/", nil) }
