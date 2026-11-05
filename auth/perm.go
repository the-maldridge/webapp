package auth

import "strings"

// Perm is a single authorization grant in two parts: the module that owns
// it and the action it allows. The canonical text form is MODULE:GRANT, as
// in TASKS:ADMIN.
//
// Two parts rather than one flat name, because a single namespace produces
// either a permission per screen — TASKS_VIEW, TASKS_EDIT, TASKS_DELETE,
// TASKS_VIEW_ARCHIVED — or one name so broad it means nothing. The module
// half makes module administrators expressible and makes "what can this
// user do" readable.
type Perm struct {
	Module string
	Grant  string
}

// P builds a permission. Upper-casing happens here, so a typo in the case
// of a literal cannot silently produce a permission nobody holds.
func P(module, grant string) Perm {
	return Perm{Module: strings.ToUpper(module), Grant: strings.ToUpper(grant)}
}

// Well-known grants. ADMIN within a module is meaningful; the CORE module
// is the one with reach across all of them.
const (
	GrantAdmin    = "ADMIN"
	GrantEdit     = "EDIT"
	GrantView     = "VIEW"
	GrantComplete = "COMPLETE"

	// CoreModule names the module whose ADMIN grant means "everything".
	CoreModule = "CORE"
)

// CoreAdmin is the superuser grant. It exists so there is exactly one
// spelling of it.
func CoreAdmin() Perm { return P(CoreModule, GrantAdmin) }

// String renders the canonical MODULE:GRANT form. A permission with an
// empty grant is malformed rather than "all grants in the module", because
// a bug that widens authority is much worse than a bug that denies it, and
// an accidentally-empty Grant field is a plausible bug.
func (p Perm) String() string {
	if p.Module == "" {
		return p.Grant
	}
	if p.Grant == "" {
		return p.Module
	}
	return p.Module + ":" + p.Grant
}

// ParsePerm reads the MODULE:GRANT form. It is strict: an empty module or
// an empty grant is an error, so a configuration typo is reported instead of
// becoming a permission that silently matches nothing, or worse, everything.
func ParsePerm(s string) (Perm, error) {
	module, grant, found := strings.Cut(s, ":")
	if !found {
		return Perm{}, &PermError{Text: s, Reason: "expected MODULE:GRANT"}
	}
	module = strings.ToUpper(strings.TrimSpace(module))
	grant = strings.ToUpper(strings.TrimSpace(grant))
	if module == "" || grant == "" {
		return Perm{}, &PermError{Text: s, Reason: "module and grant may not be empty"}
	}
	if strings.ContainsAny(module+grant, " \t") {
		return Perm{}, &PermError{Text: s, Reason: "may not contain whitespace"}
	}
	if strings.Contains(grant, ":") {
		// "A:B:C" cuts at the first colon and leaves grant "B:C". A second
		// colon is a typo for another module, and accepting it produces a
		// permission that can never be held while looking valid enough to
		// configure.
		return Perm{}, &PermError{Text: s, Reason: "may contain only one ':'"}
	}
	return Perm{Module: module, Grant: grant}, nil
}

// PermError reports a permission that could not be parsed.
type PermError struct {
	Text   string
	Reason string
}

func (e *PermError) Error() string {
	return "auth: invalid permission " + e.Text + ": " + e.Reason
}

// holds reports whether a group set satisfies p.
//
// Three group names satisfy any permission, and that is the whole rule:
//
//  1. MODULE:GRANT, the exact grant.
//  2. MODULE:ADMIN, the module's administrator.
//  3. CORE:ADMIN, the superuser.
//
// Prefix matching over the module half is what makes this short and it is
// also what makes it dangerous in the naive form, so matching is on whole
// segments only. A group named CORE:ADMINFOO must not satisfy CORE:ADMIN
// because one is a prefix of the other in string terms, and a group named
// TASKS:ADMIN must not satisfy TASKS:DELETE. There is no wildcard grant, no
// MODULE:* , and no implicit inheritance from a group simply existing: a
// user holding TASKS:EDIT is not granted TASKS:VIEW, because the
// applications this targets have screens where viewing is the sensitive
// thing and an inheritance rule quietly opens them.
func holds(groups map[string]struct{}, p Perm) bool {
	if len(groups) == 0 {
		return false
	}
	if _, ok := groups[p.String()]; ok {
		return true
	}
	if p.Grant != GrantAdmin {
		if _, ok := groups[P(p.Module, GrantAdmin).String()]; ok {
			return true
		}
	}
	// CORE:ADMIN satisfies CORE:ADMIN by the exact match above, so this is
	// strictly about the superuser satisfying something else.
	if _, ok := groups[CoreAdmin().String()]; ok {
		return true
	}
	return false
}
