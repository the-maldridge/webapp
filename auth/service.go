package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	authware "github.com/the-maldridge/authware"
	"github.com/the-maldridge/webapp/db"
	"golang.org/x/crypto/bcrypt"
)

// bcryptCost is the work factor for password hashes.
//
// The default of 10 has been comfortably crackable by determined attackers
// for years; 12 is roughly four times the work and still keeps a login
// attempt well under a tenth of a second on modest hardware, which matters
// because password hashing is the one CPU cost an unauthenticated caller can
// make the server pay on demand. Raising this is a forward-only change:
// existing hashes keep verifying, and each one is upgraded on the next
// successful login.
const bcryptCost = 12

// minPasswordLength is enforced on creation. It is not a strength
// requirement and no composition rules are enforced — length is what does
// the work, and requiring symbols pushes people toward writing the password
// on a sticky note.
const minPasswordLength = 12

// maxPasswordLength bounds bcrypt's input. bcrypt silently truncates at 72
// bytes, so without a limit a 100-character passphrase would be verified
// against its first 72 bytes, and a user would be silently protected by less
// than they thought. Passing the raw bytes to bcrypt also errors on some
// inputs, so the check happens first and is explicit.
const maxPasswordLength = 72

// tokenPrefixLen is how much of a token is kept for display.
const tokenPrefixLen = 12

// lastWriteInterval bounds how often a token's or profile's "last used"
// timestamp is written. Recording every use turns authentication into a
// write on every request.
const lastWriteInterval = time.Minute

// Errors that callers branch on. These are values rather than statuses so a
// login handler can tell "no such account" from "wrong password" internally
// while presenting one message outward.
var (
	// ErrUnknownAccount is returned when the identity does not exist.
	ErrUnknownAccount = errors.New("auth: no such account")

	// ErrBadCredential is returned when the secret does not match. It is
	// deliberately indistinguishable from a nonexistent account at the
	// response layer.
	ErrBadCredential = errors.New("auth: credential rejected")

	// ErrAccountDisabled is returned for an account that exists but has
	// been turned off.
	ErrAccountDisabled = errors.New("auth: account disabled")

	// ErrTokenInvalid is returned for an unknown, expired, or disabled
	// token. Like ErrBadCredential it is one answer to three questions.
	ErrTokenInvalid = errors.New("auth: token rejected")
)

// Service is the authentication and authorization store.
//
// One Service is constructed per application and shared. It holds no
// per-request state; everything request-scoped travels in the request
// context.
type Service struct {
	store *store

	// mechanisms verify username/password pairs, in order.
	mechanisms []Mechanism

	// tokenMechanisms resolve bearer tokens, in order.
	tokenMechanisms []TokenMechanism

	logger *slog.Logger

	// now is a seam so expiry can be tested without waiting.
	now func() time.Time

	// sessionKey is the root secret: it seals sessions and, through a
	// derived key, signs CSRF tokens.
	sessionKey string

	// keySetByOption records that the application supplied the key rather
	// than the environment or the generated fallback doing so.
	keySetByOption bool

	// lifetimeSet records that an option chose the session lifetime. The
	// flag is needed because the default is non-zero, so a zero field could
	// mean "1 hour" and could not also mean "nobody chose".
	lifetimeSet bool

	// policy is the cookie attribute set in effect.
	policy CookiePolicy

	// production turns a missing session key from a generated one into a
	// startup failure.
	production bool

	// codec seals and opens session payloads.
	codec *sessionCodec

	// csrfKey signs CSRF tokens. Derived, never equal to sessionKey.
	csrfKey []byte

	// deny renders refusals. The core replaces it so that denials get the
	// application's error pages and content negotiation.
	deny DenialHandler

	// env reads configuration; replaced in tests.
	env envLookup
}

// Mechanism verifies a password and reports an identity's groups.
//
// It is authware's Authenticator rather than a look-alike declared here, which
// is what lets the backends authware ships — an htpasswd file, an LDAP
// directory, PAM, NetAuth — go straight into WithMechanism, just as its token
// backends go into WithTokenMechanism. A distinct interface with the same three
// methods under different names would have looked equivalent and cost every
// application that wanted one of those backends a forwarding type with nothing
// in it but renames.
//
// A mechanism that does not know an identity reports it as an error rather than
// false-and-nil, so that "not mine" and "wrong password" stay distinguishable
// internally; see expectedRejection for how the two groups are told apart.
type Mechanism = authware.Authenticator

// TokenMechanism resolves a bearer token to an identity. Like Mechanism it is
// authware's interface, here TokenAuthenticator, so a token backend needs no
// adapter.
//
// Implementations must not set AuthedBy on the returned user; the caller stamps
// it with the mechanism name.
type TokenMechanism = authware.TokenAuthenticator

// expectedRejection reports whether a mechanism's failure is a credential answer
// rather than a backend going wrong. Wrong password, unknown identity, and
// disabled account belong to the first group, and authware's own backends report
// them with authware's error types instead of this package's sentinels.
//
// The distinction is what keeps the log legible: a credential answer is normal
// operation, and an attacker trying a thousand usernames must not turn the
// application's log into a thousand warnings. A backend failure still is one,
// because that is the difference between a locked-out user and a dead directory.
func expectedRejection(err error) bool {
	var unauthenticated *authware.ErrUnauthenticated
	var doesNotExist *authware.ErrDoesNotExist
	return errors.Is(err, ErrUnknownAccount) ||
		errors.Is(err, ErrBadCredential) ||
		errors.Is(err, ErrAccountDisabled) ||
		errors.As(err, &unauthenticated) ||
		errors.As(err, &doesNotExist)
}

// PasswordUpgrader is an optional interface a mechanism installed here can
// implement when it is able to store a stronger hash for an identity it just
// verified.
//
// It is not part of authware's Authenticator, and so not part of Mechanism,
// because most backends cannot honour one: an LDAP directory is written by the
// directory team, an htpasswd file is readable-but-not-writable by design, and
// a third-party identity provider has no interest in this application's work
// factor. A mechanism that can write declares this interface and gets called
// after a successful credential check, which is the only moment the plaintext
// exists to re-hash.
type PasswordUpgrader interface {
	// UpgradePassword stores a hash of plaintext for identity when the
	// stored one is weaker than the mechanism now prefers. It must be a
	// no-op when no upgrade is needed, since it runs on every successful
	// login.
	UpgradePassword(ctx context.Context, identity, plaintext string) error
}

// Option configures a Service.
type Option func(*Service) error

// WithMechanism appends a password-verification mechanism. The database
// mechanism stays in the chain unless WithOnlyMechanisms is used, so an
// application can add a directory without losing local emergency accounts.
//
// The parameter type is authware's Authenticator, which is what lets a backend
// from authware's own backend packages be handed over as it is:
//
//	backend, err := htpasswd.New()
//	svc, err := auth.New(d, auth.WithMechanism(backend))
func WithMechanism(m Mechanism) Option {
	return func(s *Service) error {
		if m == nil {
			return errors.New("auth: WithMechanism called with nil")
		}
		s.mechanisms = append(s.mechanisms, m)
		return nil
	}
}

// WithOnlyMechanisms replaces the password mechanism chain entirely, dropping
// the database backend. Use it when accounts live somewhere else and a local
// table is a liability rather than a fallback.
//
// It says nothing about bearer tokens, deliberately: the two chains are
// orthogonal, so directory-backed passwords and database-backed machine tokens
// is a configuration an application can have, each credential type with the
// revocation story that suits it. Use WithOnlyTokenMechanisms to change the
// token chain.
func WithOnlyMechanisms(ms ...Mechanism) Option {
	return func(s *Service) error {
		if len(ms) == 0 {
			return errors.New("auth: WithOnlyMechanisms needs at least one mechanism")
		}
		for _, m := range ms {
			if m == nil {
				return errors.New("auth: WithOnlyMechanisms called with a nil mechanism")
			}
		}
		s.mechanisms = append([]Mechanism(nil), ms...)
		return nil
	}
}

// WithTokenMechanism appends a bearer-token mechanism. The database mechanism
// stays in the chain unless WithOnlyTokenMechanisms is used, so an application
// can validate tokens against an external service while its own API tokens keep
// working and remain revocable from the admin screen.
//
// The parameter type is authware's TokenAuthenticator, so a backend from
// authware's own backend packages is handed over as it is:
//
//	backend, err := apitoken.New()
//	svc, err := auth.New(d, auth.WithTokenMechanism(backend))
//
// A mechanism defines what a token means: its lifetime, where its groups come
// from, and whether and how it can be revoked before it expires. The framework
// neither enforces nor inherits those properties, and it does not make a
// non-database token revocable through Service.RevokeToken, which operates on
// the rows the database mechanism reads. Whatever guarantee a token needs is
// the mechanism's business; what the framework does promise is to consult
// mechanisms in order, stamp the answering mechanism's name on the identity,
// and normalize it.
func WithTokenMechanism(m TokenMechanism) Option {
	return func(s *Service) error {
		if m == nil {
			return errors.New("auth: WithTokenMechanism called with nil")
		}
		s.tokenMechanisms = append(s.tokenMechanisms, m)
		return nil
	}
}

// WithOnlyTokenMechanisms replaces the bearer-token chain entirely, dropping the
// database backend. Use it when tokens are validated somewhere else — a service
// that knows whether it revoked one, a file the platform owns — and a local
// token table would be a credential store nobody administers rather than a
// fallback.
//
// The application owns the consequences, the largest being that this package's
// token administration — NewToken, ListTokens, RevokeToken — writes rows no
// mechanism reads. With the database chain dropped, those calls keep succeeding
// and stop authenticating anything, which is why replacing the chain is a
// separate option from appending to it.
func WithOnlyTokenMechanisms(ms ...TokenMechanism) Option {
	return func(s *Service) error {
		if len(ms) == 0 {
			return errors.New("auth: WithOnlyTokenMechanisms needs at least one mechanism")
		}
		for _, m := range ms {
			if m == nil {
				return errors.New("auth: WithOnlyTokenMechanisms called with a nil mechanism")
			}
		}
		s.tokenMechanisms = append([]TokenMechanism(nil), ms...)
		return nil
	}
}

// WithLogger sends the package's log lines somewhere specific. Without it
// they go to slog's default logger, which is almost always right.
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) error {
		if l == nil {
			return errors.New("auth: WithLogger called with nil")
		}
		s.logger = l
		return nil
	}
}

// New builds a Service over a database. The accounts, groups, token, and
// profile tables are created by the caller passing Models() to the
// application, so that the whole application's schema is decided before the
// database opens.
func New(d *db.DB, opts ...Option) (*Service, error) {
	if d == nil {
		return nil, errors.New("auth: New requires a database")
	}

	s := &Service{
		store:  &store{d: d},
		logger: slog.Default(),
		now:    time.Now,
	}

	// The database mechanism goes first so that a local admin account can
	// always log in even when a directory is down, which is precisely when
	// an emergency account matters.
	// The closure rather than s.now itself so that a test replacing the
	// Service's clock moves token expiry along with everything else.
	local := &dbMechanism{store: s.store, clock: func() time.Time { return s.now() }}
	s.mechanisms = []Mechanism{local}
	s.tokenMechanisms = []TokenMechanism{local}

	for _, opt := range opts {
		if err := opt(s); err != nil {
			return nil, err
		}
	}
	if err := s.applySettings(); err != nil {
		return nil, err
	}
	return s, nil
}

// Models returns the models the Service needs, for the application to
// migrate before serving.
func (s *Service) Models() []any {
	return []any{Account{}, Group{}, APIToken{}, Profile{}}
}

// Mechanisms returns the installed password mechanisms, for an application
// that wants to display or assert on the configuration.
func (s *Service) Mechanisms() []string {
	names := make([]string, 0, len(s.mechanisms))
	for _, m := range s.mechanisms {
		names = append(names, m.Name())
	}
	return names
}

// TokenMechanisms returns the installed bearer-token mechanisms, for the same
// display-and-assert purpose as Mechanisms. The two lists are separate because
// the chains are: a deployment that authenticates people against a directory and
// machines against a token file has one name in the first list and a different
// one in the second, and an admin screen that showed only one of the two would
// be a screen that lies about who can get in.
func (s *Service) TokenMechanisms() []string {
	names := make([]string, 0, len(s.tokenMechanisms))
	for _, m := range s.tokenMechanisms {
		names = append(names, m.Name())
	}
	return names
}

// Authenticate checks a username and password against every mechanism in
// order and returns the authenticated user.
//
// Failures are aggregated internally and reported as one error outward. A
// login form that distinguishes "no such user" from "wrong password" is an
// account enumeration primitive, and every framework-level login endpoint
// that has ever leaked the difference has done it by being helpful.
//
// A mechanism erroring is recorded and skipped rather than aborting the
// chain: an unreachable directory should not report "wrong password" to a
// user whose local account would have worked, and it should not report
// "directory unreachable" to them either. It is logged, which is where that
// belongs.
func (s *Service) Authenticate(ctx context.Context, identity, password string) (user User, err error) {
	identity = normalizeIdentity(identity)
	if identity == "" || password == "" {
		// Same answer as a wrong password, and taken before any backend is
		// consulted, so an empty submission costs the same as a wrong one.
		return User{}, ErrBadCredential
	}

	var errs []error
	for _, m := range s.mechanisms {
		switch verifyErr := m.AuthUserPassword(ctx, identity, password); {
		case verifyErr == nil:
			// Re-hash if this mechanism's stored hash used a weaker cost than
			// the package now wants. The upgrade is offered the plaintext it
			// needs and only this function has it, which is why the interface
			// is asserted here rather than being driven from the mechanism.
			if upgrader, ok := m.(PasswordUpgrader); ok {
				if err := upgrader.UpgradePassword(ctx, identity, password); err != nil {
					s.logger.WarnContext(ctx, "auth: password hash upgrade failed",
						"identity", identity, "mechanism", m.Name(), "error", err)
				}
			}
			groups, groupErr := m.UserGroups(ctx, identity)
			if groupErr != nil {
				// The identity is known but groups are unavailable.
				// Continuing with no groups would silently downgrade the
				// user, and downgrades are invisible to whoever is locked
				// out; refusing is the legible outcome.
				s.logger.ErrorContext(ctx, "auth: groups unavailable for a verified identity",
					"identity", identity, "mechanism", m.Name(), "error", groupErr)
				return User{}, fmt.Errorf("auth: groups unavailable for %q: %w", identity, groupErr)
			}
			return User{Identity: identity, Groups: groups, AuthedBy: m.Name()}, nil

		case expectedRejection(verifyErr):
			// Expected outcomes; recorded but never surfaced as themselves.
			errs = append(errs, fmt.Errorf("%s: %w", m.Name(), verifyErr))
		default:
			errs = append(errs, fmt.Errorf("%s: %w", m.Name(), verifyErr))
			s.logger.WarnContext(ctx, "auth: mechanism failed",
				"mechanism", m.Name(), "error", verifyErr)
		}
	}

	// One error value for every way this can fail, so that no caller — not
	// even one reading the error string rather than the response body — can
	// tell an unknown account from a wrong password. What actually happened
	// is recorded here, which is where diagnostics belong.
	s.logger.DebugContext(ctx, "auth: login rejected",
		"identity", identity, "mechanisms", len(errs), "reasons", errors.Join(errs...))
	return User{}, ErrBadCredential
}

// AuthenticateToken resolves a bearer token against the installed token
// mechanisms in order, the first to accept winning.
//
// What a mechanism reports is not taken at face value in one respect: the
// identity is trimmed and lower-cased, the normalization an account name also
// gets, because the local principal is a single namespace and profile rows,
// permission checks, and ownership all key off it. Two of a foreign backend's
// names that differ only in case therefore become one local identity, and
// namespacing them is the job of the application that adopted that backend; the
// alternative, two identity spaces decided by which credential happened to
// arrive, is worse.
//
// Groups are exactly what the mechanism reported. This function consults no
// store of its own, which is what lets an external token source be authoritative
// about its own groups; the database mechanism reads memberships itself, so the
// local path is unchanged.
func (s *Service) AuthenticateToken(ctx context.Context, token string) (User, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return User{}, ErrTokenInvalid
	}

	for _, m := range s.tokenMechanisms {
		resolved, err := m.AuthToken(ctx, token)
		if err != nil {
			continue
		}
		// Normalized before the emptiness test: a mechanism asserting blank
		// space has no better claim to be a principal than one asserting nothing.
		identity := normalizeIdentity(resolved.Identity)
		if identity == "" {
			continue
		}
		return User{
			Identity: identity,
			Groups:   resolved.Groups,
			AuthedBy: m.Name(),
		}, nil
	}
	return User{}, ErrTokenInvalid
}

// CreateAccount stores a new local account with a bcrypt-hashed password.
//
// There is deliberately no exported path from an HTTP request to this
// function without an authenticated caller: the handler is the application's,
// and it should require CORE:ADMIN or equivalent. Registration is a product
// decision, and an application that ships open signups alongside a permission
// system has chosen to be owned by the first person who reads the docs.
func (s *Service) CreateAccount(ctx context.Context, identity, password string) (*Account, error) {
	identity = normalizeIdentity(identity)
	if err := validIdentity(identity); err != nil {
		return nil, err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return nil, err
	}

	acct := &Account{Identity: identity, PasswordHash: hash}
	if err := s.store.createAccount(ctx, acct); err != nil {
		return nil, err
	}
	return acct, nil
}

// SetPassword replaces an account's password.
func (s *Service) SetPassword(ctx context.Context, identity, password string) error {
	identity = normalizeIdentity(identity)
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	return s.store.setPassword(ctx, identity, hash)
}

// SetDisabled turns an account on or off. Disabled accounts cannot
// authenticate; sessions already issued to them remain valid until they
// expire, which is the cost of stateless sessions and is documented in the
// package comment rather than hidden.
func (s *Service) SetDisabled(ctx context.Context, identity string, disabled bool) error {
	return s.store.setDisabled(ctx, normalizeIdentity(identity), disabled)
}

// Account returns one account.
func (s *Service) Account(ctx context.Context, identity string) (*Account, error) {
	return s.store.account(ctx, normalizeIdentity(identity))
}

// Accounts lists accounts. Pagination is the caller's, and required: an
// unbounded list of every identity in a system is a enumeration primitive
// that an admin screen should have to ask for.
func (s *Service) Accounts(ctx context.Context, limit, offset int) ([]Account, error) {
	return s.store.accounts(ctx, limit, offset)
}

// DeleteAccount removes an account and its memberships. It does not touch
// rows the account owns in application tables, which is why disabling is
// usually what is wanted.
func (s *Service) DeleteAccount(ctx context.Context, identity string) error {
	return s.store.deleteAccount(ctx, normalizeIdentity(identity))
}

// Grant adds a group membership, idempotently.
func (s *Service) Grant(ctx context.Context, identity, group string) error {
	name, err := normalizeGroup(group)
	if err != nil {
		return err
	}
	return s.store.grant(ctx, normalizeIdentity(identity), name)
}

// Revoke removes a group membership.
func (s *Service) Revoke(ctx context.Context, identity, group string) error {
	name, err := normalizeGroup(group)
	if err != nil {
		return err
	}
	return s.store.revoke(ctx, normalizeIdentity(identity), name)
}

// Groups returns an identity's groups.
func (s *Service) Groups(ctx context.Context, identity string) (map[string]struct{}, error) {
	return s.store.groups(ctx, normalizeIdentity(identity))
}

// HasPerm reports whether an identity holds a permission, consulting the
// store. The request-scoped HasPerm is cheaper and reads the session; this
// is for authorization decisions that must not wait for a session to expire,
// which is the only way in which group revocation can take effect before
// then.
func (s *Service) HasPerm(ctx context.Context, identity string, p Perm) (bool, error) {
	groups, err := s.store.groups(ctx, normalizeIdentity(identity))
	if err != nil {
		return false, err
	}
	return holds(groups, p), nil
}

// NewTokenResult carries a created token.
type NewTokenResult struct {
	// Token is the plaintext. It exists only here and only now; the
	// database holds a digest and no method can recover it.
	Token string

	// Record is the stored token without its digest.
	Record APIToken
}

// NewToken creates an API token for an identity.
//
// expiresIn may be zero, meaning the token never expires. That is the
// caller's explicit choice rather than a default: the parameter is required,
// so "no expiry" is a decision visible at the call site.
func (s *Service) NewToken(ctx context.Context, identity, name string, expiresIn time.Duration) (*NewTokenResult, error) {
	identity = normalizeIdentity(identity)
	plaintext, err := newToken()
	if err != nil {
		return nil, err
	}

	record := APIToken{
		Identity: identity,
		Digest:   digestToken(plaintext),
		Prefix:   tokenDisplayPrefix(plaintext),
		Name:     strings.TrimSpace(name),
	}
	if expiresIn > 0 {
		expiry := s.now().Add(expiresIn)
		record.ExpiresAt = &expiry
	}

	if err := s.store.createToken(ctx, &record); err != nil {
		return nil, err
	}
	// The stored digest is not returned to the caller of this package in a
	// struct field they might log; Record keeps it, so clear it.
	record.Digest = ""
	return &NewTokenResult{Token: plaintext, Record: record}, nil
}

// RevokeToken disables a token by its database ID.
func (s *Service) RevokeToken(ctx context.Context, id uint) error {
	return s.store.revokeToken(ctx, id)
}

// ListTokens returns an identity's tokens with digests removed.
func (s *Service) ListTokens(ctx context.Context, identity string) ([]APIToken, error) {
	tokens, err := s.store.tokens(ctx, normalizeIdentity(identity))
	if err != nil {
		return nil, err
	}
	for i := range tokens {
		tokens[i].Digest = ""
	}
	return tokens, nil
}

// Profile returns the profile row for an identity, creating it on first
// sight. Called from the request path, so it is written to be cheap: one
// indexed read on the common path.
func (s *Service) Profile(ctx context.Context, identity, authedBy string) (*Profile, error) {
	identity = normalizeIdentity(identity)
	if identity == "" {
		return nil, ErrUnknownAccount
	}
	if p, err := s.store.profile(ctx, identity); err == nil {
		s.touchProfile(ctx, p)
		return p, nil
	} else if !errors.Is(err, ErrUnknownAccount) {
		return nil, err
	}

	p := &Profile{Identity: identity, AuthedBy: authedBy}
	if err := s.store.createProfile(ctx, p); err != nil {
		// A unique-violation means a concurrent request won the race. The
		// row now exists, so read it back rather than failing the request.
		if existing, lookupErr := s.store.profile(ctx, identity); lookupErr == nil {
			return existing, nil
		}
		return nil, err
	}
	return p, nil
}

// touchProfile updates LastSeenAt at most once per interval.
func (s *Service) touchProfile(ctx context.Context, p *Profile) {
	if p.LastSeenAt != nil && s.now().Sub(*p.LastSeenAt) < lastWriteInterval {
		return
	}
	if err := s.store.touchProfile(ctx, p.ID); err != nil {
		s.logger.DebugContext(ctx, "auth: profile touch failed", "profile", p.ID, "error", err)
	}
}

// hashPassword returns a bcrypt sum for a plaintext password.
func hashPassword(password string) (string, error) {
	if len(password) < minPasswordLength {
		return "", fmt.Errorf("auth: password must be at least %d characters", minPasswordLength)
	}
	if len(password) > maxPasswordLength {
		return "", fmt.Errorf("auth: password must be at most %d characters; bcrypt ignores anything past 72 bytes", maxPasswordLength)
	}
	sum, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("auth: could not hash password: %w", err)
	}
	return string(sum), nil
}

// verifyBcrypt reports whether a password matches a sum.
func verifyBcrypt(sum, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(sum), []byte(password)) == nil
}

// needsUpgrade reports whether a stored hash was made with a cost lower than
// the package now uses, which is the signal to re-hash on a successful login.
func needsUpgrade(sum string) bool {
	cost, err := bcrypt.Cost([]byte(sum))
	if err != nil {
		// Unreadable cost: treat it as needing replacement rather than
		// trusting a hash whose parameters are unknown.
		return true
	}
	return cost < bcryptCost
}

// digestToken returns the hex SHA-256 of a token.
func digestToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// tokenDisplayPrefix is the leading characters of a token, kept so a list of
// tokens can be told apart.
func tokenDisplayPrefix(token string) string {
	if len(token) <= tokenPrefixLen {
		return token
	}
	return token[:tokenPrefixLen]
}

// digestMatches compares a presented token against a stored digest in
// constant time. The comparison is over fixed-length hex, so length is not
// itself a leak; the constant-time compare is here so the property does not
// depend on that reasoning holding in the future.
func digestMatches(digest, token string) bool {
	return subtle.ConstantTimeCompare([]byte(strings.ToLower(digest)), []byte(digestToken(token))) == 1
}

// normalizeIdentity lower-cases and trims an identity.
//
// Case-insensitive usernames are near-universal expectation, and the one
// place that matters is that lookup and storage agree. A store containing
// both "Ada" and "ada" is two accounts for the same person with the same
// password, which is an account the owner of the second one cannot
// reliably reach.
func normalizeIdentity(identity string) string {
	return strings.ToLower(strings.TrimSpace(identity))
}

// validIdentity rejects identities that are empty or structurally confusing.
func validIdentity(identity string) error {
	switch {
	case identity == "":
		return errors.New("auth: identity may not be empty")
	case len(identity) > 255:
		return errors.New("auth: identity is longer than 255 characters")
	case strings.ContainsAny(identity, " \t\r\n"):
		return errors.New("auth: identity may not contain whitespace")
	}
	return nil
}

// normalizeGroup upper-cases and validates a group name.
func normalizeGroup(group string) (string, error) {
	name := strings.ToUpper(strings.TrimSpace(group))
	if name == "" {
		return "", errors.New("auth: group name may not be empty")
	}
	if len(name) > 255 {
		return "", errors.New("auth: group name is longer than 255 characters")
	}
	if strings.ContainsAny(name, " \t\r\n") {
		return "", errors.New("auth: group name may not contain whitespace")
	}
	return name, nil
}

// dbMechanism is the default Mechanism and TokenMechanism, backed by the
// accounts table.
type dbMechanism struct {
	store *store

	// clock is the Service's clock, shared so that moving time in a test
	// moves token expiry along with everything else.
	clock func() time.Time
}

// Name identifies the mechanism as the local database.
func (m *dbMechanism) Name() string { return "database" }

// AuthUserPassword checks a password against the stored bcrypt sum.
//
// An unknown identity and a wrong password both return ErrBadCredential
// after doing the same amount of work. The second part matters: if a
// nonexistent account returns immediately while an existing one burns a
// bcrypt round, timing alone answers "does this user exist", which is the
// enumeration the error message was careful not to provide.
func (m *dbMechanism) AuthUserPassword(ctx context.Context, identity, password string) error {
	acct, err := m.store.account(ctx, identity)
	if err != nil {
		// Spend the same time as a real verification would.
		burnBcrypt(password)
		if errors.Is(err, ErrUnknownAccount) {
			return ErrBadCredential
		}
		return err
	}
	if acct.Disabled {
		burnBcrypt(password)
		return ErrAccountDisabled
	}
	if acct.PasswordHash == "" {
		// A token-only account. Same answer as a wrong password.
		burnBcrypt(password)
		return ErrBadCredential
	}
	if !verifyBcrypt(acct.PasswordHash, password) {
		return ErrBadCredential
	}
	return nil
}

// UserGroups returns the identity's groups.
func (m *dbMechanism) UserGroups(ctx context.Context, identity string) (map[string]struct{}, error) {
	return m.store.groups(ctx, identity)
}

// UpgradePassword re-hashes a stored password when its cost is below the
// current one. It requires the plaintext, so it can only run during a
// successful login — which is the only moment it ever can.
func (m *dbMechanism) UpgradePassword(ctx context.Context, identity, password string) error {
	acct, err := m.store.account(ctx, identity)
	if err != nil {
		return err
	}
	if !needsUpgrade(acct.PasswordHash) {
		return nil
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	return m.store.setPassword(ctx, identity, hash)
}

// burnBcrypt spends roughly one verification's worth of CPU on an input that
// is not checked against anything, so that the cost of probing an unknown
// identity matches the cost of probing a known one.
func burnBcrypt(password string) {
	// A hash of the empty string is as good as any: the point is the
	// constant-time work, not the value.
	_ = bcrypt.CompareHashAndPassword(defaultBurnHash, []byte(password))
}

// defaultBurnHash is a valid bcrypt sum generated once at package
// initialization, used only by burnBcrypt.
var defaultBurnHash = func() []byte {
	sum, err := bcrypt.GenerateFromPassword([]byte("webapp-auth-timing-pad"), bcryptCost)
	if err != nil {
		// bcrypt only fails here on a cost outside its range or on
		// randomness failure, neither of which leaves a usable fallback.
		// Panicking at init is honest: the timing property cannot be
		// provided, and silently dropping it would be worse.
		panic("auth: could not initialize password verification: " + err.Error())
	}
	return sum
}()

// AuthToken looks a token up by digest and resolves it to an identity and its
// groups. AuthedBy is left unset: the Service stamps it, so that the value
// reports which mechanism in the chain was consulted rather than whatever the
// backend happened to guess.
func (m *dbMechanism) AuthToken(ctx context.Context, token string) (authware.User, error) {
	record, err := m.store.tokenByDigest(ctx, digestToken(token))
	if err != nil {
		return authware.User{}, ErrTokenInvalid
	}
	if record.Disabled {
		return authware.User{}, ErrTokenInvalid
	}
	if record.ExpiresAt != nil && m.clock().After(*record.ExpiresAt) {
		return authware.User{}, ErrTokenInvalid
	}

	groups, err := m.store.groups(ctx, record.Identity)
	if err != nil {
		return authware.User{}, err
	}
	return authware.User{Identity: record.Identity, Groups: groups}, nil
}
