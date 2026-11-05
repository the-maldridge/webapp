package auth

import "testing"

func TestHoldsExactGrant(t *testing.T) {
	groups := map[string]struct{}{"TASKS:EDIT": {}}
	if !holds(groups, P("tasks", "edit")) {
		t.Fatal("exact grant should satisfy")
	}
}

func TestHoldsModuleAdmin(t *testing.T) {
	groups := map[string]struct{}{"TASKS:ADMIN": {}}
	for _, grant := range []string{"EDIT", "VIEW", "COMPLETE"} {
		if !holds(groups, P("tasks", grant)) {
			t.Errorf("TASKS:ADMIN should satisfy TASKS:%s", grant)
		}
	}
}

func TestHoldsCoreAdmin(t *testing.T) {
	groups := map[string]struct{}{"CORE:ADMIN": {}}
	for _, p := range []Perm{P("tasks", "edit"), P("blog", "view"), CoreAdmin()} {
		if !holds(groups, p) {
			t.Errorf("CORE:ADMIN should satisfy %s", p)
		}
	}
}

func TestModuleAdminDoesNotSatisfyAnotherModule(t *testing.T) {
	groups := map[string]struct{}{"TASKS:ADMIN": {}}
	if holds(groups, P("blog", "edit")) {
		t.Fatal("TASKS:ADMIN must not satisfy BLOG:EDIT")
	}
}

func TestAdminIsNotSatisfiedByLess(t *testing.T) {
	// The reverse direction matters as much: holding a lesser grant must not
	// be able to reach ADMIN.
	groups := map[string]struct{}{"TASKS:EDIT": {}}
	if holds(groups, P("tasks", "admin")) {
		t.Fatal("TASKS:EDIT must not satisfy TASKS:ADMIN")
	}
}

func TestNoInheritanceFromLesserGrant(t *testing.T) {
	// An application where viewing is the sensitive thing depends on this.
	groups := map[string]struct{}{"TASKS:EDIT": {}}
	if holds(groups, P("tasks", "view")) {
		t.Fatal("TASKS:EDIT must not imply TASKS:VIEW")
	}
}

func TestPrefixIsNotMatch(t *testing.T) {
	// CORE:ADMINFOO shares a string prefix with CORE:ADMIN; matching must be
	// on whole values, not prefixes.
	groups := map[string]struct{}{
		"CORE:ADMINFOO": {},
		"TASKS:ADMIN2":  {},
		"TASKS:ADMINX":  {},
	}
	if holds(groups, CoreAdmin()) {
		t.Fatal("CORE:ADMINFOO must not satisfy CORE:ADMIN")
	}
	if holds(groups, P("tasks", "admin")) {
		t.Fatal("TASKS:ADMIN2 must not satisfy TASKS:ADMIN")
	}
	if holds(groups, P("tasks", "edit")) {
		t.Fatal("TASKS:ADMINX must not satisfy TASKS:EDIT")
	}
}

func TestWildcardDoesNotMatch(t *testing.T) {
	groups := map[string]struct{}{"TASKS:*": {}, "*": {}, "CORE:*": {}}
	for _, p := range []Perm{P("tasks", "edit"), CoreAdmin()} {
		if holds(groups, p) {
			t.Fatalf("wildcard group must not satisfy %s", p)
		}
	}
}

func TestEmptyGroupsHoldNothing(t *testing.T) {
	if holds(nil, CoreAdmin()) {
		t.Fatal("nil groups must hold nothing")
	}
	if holds(map[string]struct{}{}, CoreAdmin()) {
		t.Fatal("empty groups must hold nothing")
	}
}

func TestCaseInsensitiveConstruction(t *testing.T) {
	// A typo in the case of a literal must not silently produce a permission
	// nobody holds.
	if P("tasks", "admin") != P("TASKS", "ADMIN") {
		t.Fatal("P must upper-case both halves")
	}

	groups := map[string]struct{}{"TASKS:EDIT": {}}
	if !holds(groups, P("TaSkS", "EdIt")) {
		t.Fatal("holds should match across literal case")
	}
}

func TestParsePermValid(t *testing.T) {
	for _, tc := range []struct {
		in     string
		module string
		grant  string
	}{
		{"TASKS:EDIT", "TASKS", "EDIT"},
		{"tasks:edit", "TASKS", "EDIT"},
		{" TASKS : EDIT ", "TASKS", "EDIT"},
		{"CORE:ADMIN", "CORE", "ADMIN"},
	} {
		got, err := ParsePerm(tc.in)
		if err != nil {
			t.Errorf("ParsePerm(%q): %v", tc.in, err)
			continue
		}
		if got.Module != tc.module || got.Grant != tc.grant {
			t.Errorf("ParsePerm(%q) = %+v, want %s:%s", tc.in, got, tc.module, tc.grant)
		}
	}
}

func TestParsePermRejectsMalformed(t *testing.T) {
	// Every rejected spelling must be an error. An accepted-but-degenerate
	// permission is worse than a refusal: it either matches nothing, which is
	// a silent lockout, or matches too much, which is a silent escalation.
	for _, in := range []string{
		"",
		"FOO",
		"FOO:",
		":BAR",
		":",
		"FOO:BAR:BAZ",
		"FOO:BA R",
		"FO O:BAR",
	} {
		if got, err := ParsePerm(in); err == nil {
			t.Errorf("ParsePerm(%q) = %+v, want error", in, got)
		}
	}
}

func TestParsePermSatisfiedAfterParse(t *testing.T) {
	// A parsed permission must behave like a constructed one, otherwise an
	// admin form that accepts text cannot express the grants the code checks.
	p, err := ParsePerm("tasks:edit")
	if err != nil {
		t.Fatalf("ParsePerm: %v", err)
	}
	groups := map[string]struct{}{"TASKS:EDIT": {}}
	if !holds(groups, p) {
		t.Fatal("parsed permission should match the constructed one")
	}
}

func TestParsePermMultipleColonsRejected(t *testing.T) {
	// "A:B:C" cuts at the first colon, producing grant "B:C". A group name
	// containing a colon is a typo for a second module, and accepting it would
	// create a permission that matches nothing while looking valid.
	if _, err := ParsePerm("A:B:C"); err == nil {
		t.Fatal("multi-colon permission should be rejected")
	}
}

func TestPermString(t *testing.T) {
	for _, tc := range []struct {
		p    Perm
		want string
	}{
		{P("tasks", "edit"), "TASKS:EDIT"},
		{CoreAdmin(), "CORE:ADMIN"},
		{Perm{Module: "TASKS"}, "TASKS"},
		{Perm{Grant: "EDIT"}, "EDIT"},
	} {
		if got := tc.p.String(); got != tc.want {
			t.Errorf("%+v.String() = %q, want %q", tc.p, got, tc.want)
		}
	}
}

func TestCoreAdminIsASingleSpelling(t *testing.T) {
	if CoreAdmin() != (Perm{Module: "CORE", Grant: "ADMIN"}) {
		t.Fatal("CoreAdmin must be CORE:ADMIN")
	}
	if CoreModule != "CORE" {
		t.Fatalf("CoreModule = %q", CoreModule)
	}
}
