package auth

import (
	"context"

	authware "github.com/the-maldridge/authware"
)

// User is the authenticated identity carried by a request.
//
// It is an alias rather than a distinct type so that a session produced by
// this package and one produced by authware are the same value in the
// request context. Declaring a new struct here would look equivalent and
// would not be: context lookups compare concrete types, so an application
// that read the user through authware's helper after moving to this package
// would find nothing, and nothing is what an authorization check reads as
// "anonymous".
//
// Groups travel inside the session cookie, which is why revoking one takes
// effect at the next login rather than immediately; see the package comment
// and Service.HasPerm for the store-backed alternative.
type User = authware.User

// UserFromContext returns the user stored in a context by the
// authentication middleware. It is UserFrom with a context instead of a
// request.
func UserFromContext(ctx context.Context) (User, bool) {
	return authware.UserFromContext(ctx)
}

// ContextWithUser returns ctx carrying u. Applications rarely need it: the
// middleware stamps the request, and this exists for tests and for
// impersonation paths that have already decided they are allowed.
func ContextWithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, authware.UserKey{}, u)
}
