// Package auth provides authentication and authorization for a webapp
// application: accounts stored in the application's own database,
// stateless encrypted session cookies, CSRF protection, permissions, and
// the profile row that gives every authenticated identity a stable local
// key.
//
// It builds on the authware types — User, the Authenticator and
// TokenAuthenticator interfaces, and the AES-GCM cookie codec — without
// depending on authware's middleware, so that the framework can own cookie
// attributes, denial status codes, and content negotiation in one place.
//
// # Mechanisms
//
// A mechanism verifies a username and password and reports an identity's
// groups. The database-backed mechanism is installed by default, and any
// value satisfying authware.Authenticator can be added alongside it, which
// is how an application plugs in an htpasswd file, an LDAP directory, or
// something that does not exist yet:
//
//	svc, err := auth.New(d, auth.WithMechanism(backend))
//
// Mechanisms are consulted in the order given and the first to accept wins.
// A mechanism that errors is treated as not-applicable rather than as a
// rejection, so a chain of backends degrades to the next one instead of
// turning an unreachable directory into "wrong password".
//
// # Sessions
//
// Sessions are stateless: the user, including its groups, is encrypted into
// the cookie with AES-GCM and authenticated, so nothing is stored server
// side and a horizontal fleet needs no shared session store. The trade is
// that revoking a group membership waits for the session to expire, and
// group changes therefore take at most one session lifetime to apply.
// Logout is a short session rather than a server-side deletion, for the same
// reason.
//
// Because a session carries groups, the session key is a secret with root
// authority over the application: anyone holding it can mint a session for
// any identity with any groups. In production the framework refuses to
// start without one explicitly configured. A per-process random key, which
// is what an unstated key becomes, also invalidates every session on restart
// and makes each replica its own authority.
//
// # Permissions
//
// A permission is a module and a grant, written MODULE:GRANT, and an
// identity holds one by holding a group of that exact name. Satisfaction is
// deliberately narrow, see Perm.
//
// # What is not here
//
// There is no registration flow and no password reset email. Accounts are
// created by an administrator or by an operator command, and the reasoning
// is in Service.CreateAccount: an application that reaches the internet with
// open signups and a permission system it has not exercised in production is
// a security incident waiting for a patient attacker.
package auth
