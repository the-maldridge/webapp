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
// groups. Mechanism and TokenMechanism are aliases of authware's
// Authenticator and TokenAuthenticator rather than this package's own
// interfaces, so a backend written for authware — an htpasswd file, an LDAP
// directory, PAM, NetAuth, something that does not exist yet — goes into
// WithMechanism as it is, with no forwarding type in between. The database
// mechanism is installed by default:
//
//	svc, err := auth.New(d, auth.WithMechanism(backend))
//
// Mechanisms are consulted in the order given and the first to accept wins.
// A mechanism that reports a credential answer — wrong password, unknown
// identity, or disabled account, in authware's error types or this package's
// — is treated as not-applicable rather than as a rejection, so a chain of
// backends degrades to the next one instead of turning an unreachable
// directory into "wrong password". A failure that is not a credential answer
// is logged as a warning, because that is the difference between a user who
// mistyped and a directory that is down.
//
// # Tokens
//
// Bearer tokens are resolved by a second chain, installed the same way:
// WithTokenMechanism appends and WithOnlyTokenMechanisms replaces, with the
// database mechanism in the chain by default. The two chains are orthogonal
// deliberately, so passwords can come from a directory while machine
// credentials come from a token file or a remote validator, each credential
// type keeping the lifetime and revocation story that suits it. TokenMechanisms
// reports what is installed, alongside Mechanisms.
//
// A token mechanism decides what a token means: how long it lives, which
// groups it carries, and whether it can be revoked before expiry. This package
// guarantees those properties only for the mechanism it ships and makes no
// attempt to guarantee them for one it was handed, so NewToken, ListTokens, and
// RevokeToken administer the database rows and keep writing a table nothing
// reads once WithOnlyTokenMechanisms has dropped that mechanism. Choosing a
// backend, and knowing what it costs in revocation and audit, is the
// application's decision to make.
//
// What a token mechanism asserts is used in two different ways: the identity it
// names is normalized — trimmed and lower-cased — before becoming a local
// principal, because profile rows, permission checks, and ownership all key off
// one name, while the groups it reports are taken exactly as given.
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
