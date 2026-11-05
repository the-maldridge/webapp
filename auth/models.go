package auth

import "time"

// Account is a local account: an identity plus the secret that proves it.
//
// It is deliberately thin. Display name, preferences, theme, and anything
// else an application wants about a person belong to Profile or to the
// application's own model joined to it, because those change with the
// product while this table is the security boundary. Keeping them apart is
// what makes an audit of "who can do what" a two-table question.
type Account struct {
	ID       uint   `gorm:"primaryKey"`
	Identity string `gorm:"size:255;uniqueIndex;not null"`

	// PasswordHash is a bcrypt sum, never the password. A zero value means
	// the account cannot authenticate by password at all, which is the
	// correct shape for an account that exists only to own API tokens.
	PasswordHash string `gorm:"size:255;column:password_hash"`

	CreatedAt time.Time
	UpdatedAt time.Time

	// Disabled accounts exist but cannot sign in. Deleting an account that
	// owns rows would either orphan them or cascade into data loss, so
	// revocation is a flag and deletion is a migration the operator runs
	// deliberately.
	Disabled bool `gorm:"not null;default:false"`
}

// TableName pins the table name. GORM's pluralization of "Account" happens
// to be stable, but a name that moves when a library's inflection rules
// change is a name that should be written down.
func (Account) TableName() string { return "auth_accounts" }

// Group is one membership edge: an identity holds this group.
//
// Groups are rows rather than a serialized column so that membership is
// queryable and auditable ("who else has CORE:ADMIN?") and so two
// administrators changing different groups for the same user cannot
// overwrite each other, which a blob cannot guarantee.
type Group struct {
	ID       uint   `gorm:"primaryKey"`
	Identity string `gorm:"size:255;not null;uniqueIndex:auth_group_membership"`
	Name     string `gorm:"size:255;not null;uniqueIndex:auth_group_membership"`
}

// TableName pins the membership table name.
func (Group) TableName() string { return "auth_groups" }

// APIToken is a bearer credential.
//
// Only the SHA-256 digest is stored. A digest is not reversible, so a
// database dump does not leak usable credentials, and verification is a
// digest-and-compare rather than a decrypt. The plaintext is returned once,
// at creation, and never again: that is a property of the storage, not a
// policy the code could relax later.
//
// The digest is sufficient here because the token itself is 256 bits of
// CSPRNG output. Hashing a low-entropy secret with a fast hash would be
// worse than useless, since the digest becomes an offline cracking target;
// bcrypt belongs on passwords, not on high-entropy randoms.
type APIToken struct {
	ID       uint   `gorm:"primaryKey"`
	Identity string `gorm:"size:255;not null;index"`

	// Digest is the hex-encoded SHA-256 of the token.
	Digest string `gorm:"size:64;not null;uniqueIndex"`

	// Prefix is the first few characters of the plaintext, kept so a list
	// of tokens can be told apart. It is not secret and is not used for
	// lookup.
	Prefix string `gorm:"size:16;not null"`

	// Name is what the creator called it, for the same reason.
	Name string `gorm:"size:255"`

	CreatedAt time.Time

	// LastUsedAt is updated at most once per minute per token. Recording
	// every use turns authentication into a write on every request, which
	// makes a token the cheapest thing in the system to use for
	// database-level denial of service.
	LastUsedAt *time.Time `gorm:"index"`

	// ExpiresAt nil means the token never expires. Because a token that is
	// created without one stays valid until someone revokes it, NewToken
	// requires an explicit choice rather than defaulting it.
	ExpiresAt *time.Time `gorm:"index"`

	// Disabled stops a token without deleting it, so an audit trail of
	// what was revoked and when survives the revocation.
	Disabled bool `gorm:"not null;default:false"`
}

// TableName pins the token table name.
func (APIToken) TableName() string { return "auth_api_tokens" }

// Profile is the framework's row for an authenticated identity, created on
// first sight by the profile middleware.
//
// Its purpose is to hand applications a stable numeric key for "the current
// user" that does not depend on the identity string. Identity is whatever a
// backend produced — an email address, an LDAP uid, a name a user chose —
// and a foreign key onto it breaks the day someone renames themselves or a
// directory is replaced. Applications that need more hang their own model
// off Profile.ID.
type Profile struct {
	ID       uint   `gorm:"primaryKey"`
	Identity string `gorm:"size:255;uniqueIndex;not null"`

	// AuthedBy records which mechanism provisioned the row, kept as
	// provenance rather than as an authority: the session decides who is
	// making a request, this field only says where the row came from.
	AuthedBy string `gorm:"size:255"`

	CreatedAt time.Time
	UpdatedAt time.Time

	// LastSeenAt is updated on a coarse interval, for the same write-
	// amplification reason as APIToken.LastUsedAt.
	LastSeenAt *time.Time `gorm:"index"`
}

// TableName pins the profile table name.
func (Profile) TableName() string { return "auth_profiles" }
