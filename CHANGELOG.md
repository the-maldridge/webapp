# Changelog

A list of the changes worth knowing about before upgrading. The format follows
[Keep a Changelog](https://keepachangelog.com/) and this project follows
[Semantic Versioning](https://semver.org/), with the caveat that anything before
`v1.0.0` may change without a major version bump.

## Unreleased

### Changed

- **Breaking (for implementors):** `auth.Mechanism` interfaces are now aliases of
  the interfaces in `github.com/the-maldridge/authware`, which the framework
  already depended on. `Mechanism` is `authware.Authenticator` and
  `TokenMechanism` is `authware.TokenAuthenticator`.

  Nothing changes for an application that only *uses* authentication. It changes
  any type that *implemented* one of these interfaces, because the method names
  differ:

  | Implemented in `v0.0.1` | Now required |
  | --- | --- |
  | `Verify(ctx, identity, password) error` | `AuthUserPassword(ctx, identity, password) error` |
  | `Groups(ctx, identity) (map[string]struct{}, error)` | `UserGroups(ctx, identity) (map[string]struct{}, error)` |
  | `ResolveToken(ctx, token) (string, map[string]struct{}, error)` | `AuthToken(ctx, token) (authware.User, error)` |

  `Name()` is unchanged. `AuthToken` returns a single `authware.User` (identity,
  groups, and optionally `AuthedBy`) instead of an identity and a group set.

  The reason: the two interfaces had the same method sets under different names,
  which is invisible to a reader and fatal to a consumer. Every application that
  wanted one of authware's shipped backends (`htpasswd`, `ldap`, `pam`,
  `apitoken`, `netauth`) had to write a three-method forwarding shim first, and
  the shim had no behavior in it. With the aliases, a backend value goes straight
  into `auth.WithMechanism`.

- `Service.AuthenticateToken` normalizes the identity a token mechanism asserts,
  the same lower-casing and trimming every other identity gets, and skips a
  mechanism that resolves to a blank identity rather than authenticating the
  request as the empty user.

- A mechanism's error answering "is this credential correct" is no longer logged
  at warning level. Wrong passwords, unknown accounts, disabled accounts, and a
  token-mechanism's own not-authenticated and does-not-exist errors are skipped
  quietly, so a brute-force attempt does not fill the log. Anything else, a dead
  directory for example, still warns, because that is an outage rather than a
  login screen.

### Added

- `auth.WithTokenMechanism` appends a token mechanism to the bearer-token chain,
  and `auth.WithOnlyTokenMechanisms` replaces it. These are the token-chain
  counterparts of `WithMechanism` and `WithOnlyMechanisms`. The two chains are
  independent by design: an application can verify passwords against a directory
  while machine tokens stay revocable rows in its own database. Note that with
  `WithOnlyTokenMechanisms` the built-in `NewToken`, `ListTokens`, and
  `RevokeToken` still write token rows that nothing reads; a token mechanism owns
  token lifetime and revocation.
- `auth.Service.TokenMechanisms` reports the names of the configured token
  mechanisms, mirroring `Mechanisms`.

### Fixed

- The package documentation in `auth/doc.go` described attaching an authware
  backend in a form that did not compile. It is corrected, and the claim is now
  covered by `TestAuthwareBackendNeedsNoBridge`.

## v0.0.1 - 2026-11-10

First tagged release. The framework as a whole: the `App` surface and construction
sequence, namespaced templates and the theme contract, the database handle with
SQLite and PostgreSQL support, sessions, permissions, guards, CSRF, profiles,
modules, input handling, `webapptest`, `testas`, and the example application.
