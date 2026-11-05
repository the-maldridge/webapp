package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
)

// profileContextKey is the private key a profile ID travels under.
type profileContextKey struct{}

// ProfileID is the framework-owned row for an authenticated identity.
//
// It exists because an identity string is not a foreign key. It is whatever a
// backend produced — an email address, an LDAP uid, a name someone chose — and
// an application column referencing it breaks the day a directory is replaced
// or a person renames themselves. A numeric key issued by this application
// does not have that problem, which is why every application table that owns
// rows should reference this rather than the identity.
type ProfileID uint

// contextWithProfileID returns ctx carrying id.
func contextWithProfileID(ctx context.Context, id ProfileID) context.Context {
	return context.WithValue(ctx, profileContextKey{}, id)
}

// ProfileIDFrom returns the profile ID stamped by the profile middleware, and
// false when the request is anonymous or the middleware is not installed.
func ProfileIDFrom(r *http.Request) (ProfileID, bool) {
	if r == nil {
		return 0, false
	}
	id, ok := r.Context().Value(profileContextKey{}).(ProfileID)
	return id, ok && id != 0
}

// ProfileMiddleware provisions the profile row for each authenticated request
// and makes its ID available to handlers.
//
// The cost is one indexed read per authenticated request, plus a write on an
// identity's first sight and at most one per interval after that for the
// last-seen timestamp. That is the whole price of giving every application a
// stable key for "the current user" without a registration flow: there isn't
// one to write, because the first request provisions the row.
//
// Provisioning failure is not fatal. An application whose profile table is
// unreachable has a database problem that its next query will report
// accurately; refusing every request here would report it as a login problem,
// which is a worse lie.
func (s *Service) ProfileMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			info := s.Info(r)
			if info.Via == ViaNone || info.User.Identity == "" {
				next.ServeHTTP(w, r)
				return
			}

			profile, err := s.Profile(r.Context(), info.User.Identity, info.User.AuthedBy)
			if err != nil {
				s.logger.WarnContext(r.Context(), "auth: profile unavailable",
					"identity", info.User.Identity, "error", err)
				next.ServeHTTP(w, r)
				return
			}

			ctx := contextWithProfileID(r.Context(), ProfileID(profile.ID))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Ownership is the answer to "may this identity touch this row".
//
// A permission model cannot answer it: TASKS:EDIT says the caller may edit
// tasks, not that this task is theirs, and the two questions get conflated in
// exactly one endpoint per application, which is the bug and not the feature.
// So ownership is a separate helper, and every mutating handler that owns rows
// should use it rather than comparing an owner ID by hand.
type Ownership struct {
	// ProfileID is the acting identity's profile.
	ProfileID ProfileID

	// Identity is the acting identity string, for error messages and logs.
	Identity string
}

// ErrNotOwner reports that the authenticated identity does not own the row.
//
// It is one error for both "wrong owner" and "no identity". When a caller has
// been gated by RequireLogin, the second cannot happen; when it can, the
// correct response is the same as for a stranger, so the distinction has no
// use and reporting it would be a hint.
var ErrNotOwner = errors.New("auth: not the owner")

// RequireOwnership reports whether the request's identity owns a row owned by
// ownerProfileID. The denial is returned rather than rendered so a handler can
// choose between a 404 and a 403; whether an invisible resource and a refusal
// are the same answer is an application decision, and for anything whose
// existence is itself sensitive, 404 is the right one.
//
// The owner ID comes from the request context, never from the submitted body.
// That is the invariant the whole input-handling design rests on: identity is
// what the server established, so a form field cannot claim to be somebody
// else.
func RequireOwnership(r *http.Request, ownerProfileID uint) (Ownership, error) {
	identity, err := MustIdentity(r)
	if err != nil {
		return Ownership{}, ErrNotOwner
	}
	actual, ok := ProfileIDFrom(r)
	if !ok || uint(actual) != ownerProfileID {
		return Ownership{Identity: identity}, ErrNotOwner
	}
	return Ownership{ProfileID: actual, Identity: identity}, nil
}

// RequireOwnershipOf is RequireOwnership for callers that hold a ProfileID
// rather than a raw column value.
func RequireOwnershipOf(r *http.Request, owner ProfileID) (Ownership, error) {
	return RequireOwnership(r, uint(owner))
}

// MustProfileIdentity returns the profile ID of the authenticated caller or an
// error. It is the accessor the DESIGN calls for: checked, and never a panic,
// because a panic in a handler is a 500 with a stack in the log where a 401
// with one line was available.
func MustProfileIdentity(r *http.Request) (ProfileID, error) {
	id, ok := ProfileIDFrom(r)
	if !ok {
		return 0, ErrNoUser
	}
	return id, nil
}

// ParseProfileID reads a profile ID from user input, reporting malformed
// values instead of yielding zero. A zero that reaches a query is either a
// "not found" or, worse, an unintended create against a foreign key of zero.
func ParseProfileID(raw string) (ProfileID, error) {
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, &PermError{Text: raw, Reason: "not a profile ID"}
	}
	if value == 0 {
		return 0, &PermError{Text: raw, Reason: "profile IDs start at one"}
	}
	return ProfileID(value), nil
}
