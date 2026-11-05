package testas_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/the-maldridge/webapp/auth"
	"github.com/the-maldridge/webapp/testas"
)

func TestAsStampedAnIdentityAGuardWouldHonour(t *testing.T) {
	// The documented limitation, asserted rather than promised: the identity is
	// in the context, and a guard reading the context sees it.
	req := testas.As(httptest.NewRequest(http.MethodGet, "/", nil), "alice", "tasks:view")

	user, ok := auth.UserFrom(req)
	if !ok {
		t.Fatal("no user on the request")
	}
	if user.Identity != "alice" {
		t.Errorf("identity = %q, want alice", user.Identity)
	}
	if !auth.HasPerm(req, auth.P("tasks", "view")) {
		t.Errorf("groups = %v, want them to satisfy TASKS:VIEW", user.Groups)
	}
	if user.AuthedBy != "testas" {
		t.Errorf("AuthedBy = %q, want testas so a log line says where the identity came from", user.AuthedBy)
	}
}

func TestAsUpperCasesGroupNames(t *testing.T) {
	// Permission matching is upper-case, so a test writing a lower-case group has
	// to get a membership that matches, not one that never will.
	req := testas.As(httptest.NewRequest(http.MethodGet, "/", nil), "ada", "tasks:admin", "core:admin")

	for _, p := range []auth.Perm{auth.P("tasks", "delete"), auth.CoreAdmin()} {
		if !auth.HasPerm(req, p) {
			t.Errorf("holding TASKS:ADMIN and CORE:ADMIN should satisfy %s", p)
		}
	}
}

func TestAsLeavesTheOriginalRequestAlone(t *testing.T) {
	// A table-driven test builds one fixture and derives a viewer per case. If As
	// mutated its argument, every case after the first would see the previous
	// case's identity, and the failure would land in whichever case ran second.
	base := httptest.NewRequest(http.MethodGet, "/ui/task/", nil)

	viewer := testas.As(base, "alice", "tasks:view")
	admin := testas.As(base, "root", "core:admin")

	if _, ok := auth.UserFrom(base); ok {
		t.Error("the fixture picked up an identity")
	}
	if who, _ := auth.MustIdentity(viewer); who != "alice" {
		t.Errorf("viewer identity = %q, want alice", who)
	}
	if who, _ := auth.MustIdentity(admin); who != "root" {
		t.Errorf("admin identity = %q, want root", who)
	}
}

func TestAnonymousIsARealCase(t *testing.T) {
	// An empty identity must stay anonymous rather than becoming a user with no
	// name, which is the shape that satisfies a login gate.
	req := testas.As(httptest.NewRequest(http.MethodGet, "/", nil), "", "core:admin")

	if _, ok := auth.UserFrom(req); ok {
		t.Error("an empty identity produced an authenticated request")
	}
	if auth.HasPerm(req, auth.CoreAdmin()) {
		t.Error("an anonymous request holds a permission")
	}
	if _, err := auth.MustIdentity(req); err == nil {
		t.Error("MustIdentity accepted an anonymous request")
	}
}

func TestAsIgnoresBlankGroupNames(t *testing.T) {
	// A stray empty string in a table of groups should not create a membership
	// with an empty name, which no permission would ever ask for and which shows
	// up as a group in the denial log.
	req := testas.As(httptest.NewRequest(http.MethodGet, "/", nil), "alice", "", "   ", "tasks:view")

	user, ok := auth.UserFrom(req)
	if !ok {
		t.Fatal("no user on the request")
	}
	if len(user.Groups) != 1 {
		t.Errorf("groups = %v, want only TASKS:VIEW", groupNames(user.Groups))
	}
}

// groupNames sorts a group set for a readable failure message.
func groupNames(set map[string]struct{}) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	return names
}

func TestAsNilRequest(t *testing.T) {
	if got := testas.As(nil, "alice"); got != nil {
		t.Errorf("As(nil) = %v, want nil", got)
	}
}
