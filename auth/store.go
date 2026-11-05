package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/the-maldridge/webapp/db"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// store is the persistence layer for the auth package.
//
// It is unexported on purpose. Everything an application can do to accounts,
// groups, tokens, and profiles goes through Service, which is where identity
// normalization, validation, and the deliberately-indistinct error codes
// live. A caller holding a *store could bypass all of that, and the
// normalizations are not conveniences: an identity written "Ada" by one path
// and looked up as "ada" by another is a lockout, and a raw error returned
// from here is an enumeration leak.
//
// Every method takes a context and attaches it to the query, so a cancelled
// request stops holding a connection from the pool.
type store struct {
	d *db.DB
}

// notFound maps GORM's missing-row error onto the package's own sentinel.
// Every other error passes through unchanged, because "the database is on
// fire" must not be reported as "no such account".
func notFound(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrUnknownAccount
	}
	return err
}

// createAccount inserts an account. A duplicate identity is reported as an
// error naming the identity rather than as a constraint violation, because
// the caller's next action is "pick another name" and not "inspect the
// schema".
func (s *store) createAccount(ctx context.Context, acct *Account) error {
	err := db.G[Account](s.d).Create(ctx, acct)
	if isUniqueViolation(err) {
		return fmt.Errorf("auth: account %q already exists", acct.Identity)
	}
	return err
}

// setPassword replaces a stored hash. It reports ErrUnknownAccount for an
// identity with no row rather than succeeding silently, which an Update
// matching zero rows otherwise would.
func (s *store) setPassword(ctx context.Context, identity, hash string) error {
	rows, err := db.G[Account](s.d).
		Where("identity = ?", identity).
		Update(ctx, "password_hash", hash)
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrUnknownAccount
	}
	return nil
}

// setDisabled flips the disabled flag on an account.
func (s *store) setDisabled(ctx context.Context, identity string, disabled bool) error {
	rows, err := db.G[Account](s.d).
		Where("identity = ?", identity).
		Update(ctx, "disabled", disabled)
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrUnknownAccount
	}
	return nil
}

// account returns one account by identity.
func (s *store) account(ctx context.Context, identity string) (*Account, error) {
	acct, err := db.G[Account](s.d).Where("identity = ?", identity).First(ctx)
	if err != nil {
		return nil, notFound(err)
	}
	return &acct, nil
}

// accounts lists accounts ordered by identity so paging is stable. Ordering
// by insertion time instead makes a page boundary move between two requests
// the same user makes a second apart, which reads as a row vanishing.
func (s *store) accounts(ctx context.Context, limit, offset int) ([]Account, error) {
	if limit <= 0 {
		limit = db.DefaultPageSize
	}
	if limit > db.MaxPageSize {
		limit = db.MaxPageSize
	}
	if offset < 0 {
		offset = 0
	}
	return db.G[Account](s.d).
		Order("identity ASC").
		Limit(limit).
		Offset(offset).
		Find(ctx)
}

// deleteAccount removes an account together with its group memberships. The
// two deletes are one transaction: a membership row with no account behind
// it is a grant nobody can find through the account list, which is the worst
// possible thing for an audit to discover.
func (s *store) deleteAccount(ctx context.Context, identity string) error {
	return db.Transaction(ctx, s.d, func(tx *gorm.DB) error {
		if err := tx.WithContext(ctx).
			Where("identity = ?", identity).
			Delete(&Group{}).Error; err != nil {
			return fmt.Errorf("auth: could not remove memberships for %q: %w", identity, err)
		}
		if err := tx.WithContext(ctx).
			Where("identity = ?", identity).
			Delete(&APIToken{}).Error; err != nil {
			return fmt.Errorf("auth: could not remove tokens for %q: %w", identity, err)
		}
		res := tx.WithContext(ctx).
			Where("identity = ?", identity).
			Delete(&Account{})
		if res.Error != nil {
			return fmt.Errorf("auth: could not delete account %q: %w", identity, res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrUnknownAccount
		}
		return nil
	})
}

// grant adds a membership, idempotently.
//
// The conflict target is the composite unique index on (identity, name), so
// granting the same group twice is a no-op rather than an error. An admin
// form that errors on the second click is indistinguishable from one that
// silently dropped the grant, and operators cannot tell which they have.
func (s *store) grant(ctx context.Context, identity, name string) error {
	err := db.G[Group](s.d, clause.OnConflict{DoNothing: true}).Create(ctx, &Group{
		Identity: identity,
		Name:     name,
	})
	if isUniqueViolation(err) {
		// Drivers that do not support ON CONFLICT DO NOTHING surface the
		// collision as a plain unique violation. The intent is satisfied
		// either way, so it is not an error.
		return nil
	}
	return err
}

// revoke removes a membership. Revoking a group the identity does not hold
// is not an error; it is what already being revoked means.
func (s *store) revoke(ctx context.Context, identity, name string) error {
	_, err := db.G[Group](s.d).
		Where("identity = ? AND name = ?", identity, name).
		Delete(ctx)
	return err
}

// groups returns an identity's groups as a set, upper-cased.
//
// A set rather than a slice because the only question ever asked of it is
// membership, and because it is the same shape authware's User carries, which
// is what lets a mechanism return these directly.
func (s *store) groups(ctx context.Context, identity string) (map[string]struct{}, error) {
	rows, err := db.G[Group](s.d).Where("identity = ?", identity).Find(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(rows))
	for _, g := range rows {
		name := strings.ToUpper(g.Name)
		out[name] = struct{}{}
	}
	return out, nil
}

// createToken stores a token record. The caller has already digested the
// plaintext; this never sees it.
func (s *store) createToken(ctx context.Context, record *APIToken) error {
	err := db.G[APIToken](s.d).Create(ctx, record)
	if isUniqueViolation(err) {
		// A digest collision means the same token was created twice. With
		// 256 bits of CSPRNG output that is either a caller bug or a broken
		// random source, and both deserve a loud error.
		return fmt.Errorf("auth: token already exists (prefix %q)", record.Prefix)
	}
	return err
}

// revokeToken disables a token by database ID. The row is kept so that what
// was revoked, by whom it belonged, and when it was created remain
// answerable after the fact.
func (s *store) revokeToken(ctx context.Context, id uint) error {
	rows, err := db.G[APIToken](s.d).
		Where("id = ?", id).
		Update(ctx, "disabled", true)
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrTokenInvalid
	}
	return nil
}

// tokens lists an identity's tokens, newest first, which is what an admin
// screen wants: the one just created is the one being looked at.
func (s *store) tokens(ctx context.Context, identity string) ([]APIToken, error) {
	return db.G[APIToken](s.d).
		Where("identity = ?", identity).
		Order("id DESC").
		Find(ctx)
}

// tokenByDigest looks a token up by its hex digest. The unique index on the
// digest makes this the fast path for every authenticated API request.
func (s *store) tokenByDigest(ctx context.Context, digest string) (*APIToken, error) {
	record, err := db.G[APIToken](s.d).
		Where("digest = ?", strings.ToLower(digest)).
		First(ctx)
	if err != nil {
		return nil, notFound(err)
	}
	return &record, nil
}

// profile returns the profile row for an identity.
func (s *store) profile(ctx context.Context, identity string) (*Profile, error) {
	p, err := db.G[Profile](s.d).Where("identity = ?", identity).First(ctx)
	if err != nil {
		return nil, notFound(err)
	}
	return &p, nil
}

// createProfile inserts a profile row. A unique violation is returned as-is
// so Service.Profile can recognize the lost side of a create race and read
// the winning row back.
func (s *store) createProfile(ctx context.Context, p *Profile) error {
	return db.G[Profile](s.d).Create(ctx, p)
}

// touchProfile records that an identity was seen now.
func (s *store) touchProfile(ctx context.Context, id uint) error {
	now := time.Now().UTC()
	_, err := db.G[Profile](s.d).
		Where("id = ?", id).
		Update(ctx, "last_seen_at", now)
	return err
}

// isUniqueViolation reports whether an error is a constraint violation on a
// unique index.
//
// GORM does not normalize these across drivers, so the check has to be
// textual. Both drivers this package supports put recognizable tokens in the
// message: SQLite says "UNIQUE constraint failed", PostgreSQL says "duplicate
// key value violates unique constraint" with SQLSTATE 23505. Substring
// matching on driver-specific text is brittle in principle; the alternative
// is a pre-query existence check per write, which is both a race and slower,
// and correctness under the race matters more here than purity does.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint failed") ||
		strings.Contains(msg, "duplicate key value") ||
		strings.Contains(msg, "23505")
}
