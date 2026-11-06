package main

import (
	"time"
)

// Task is one row of the example's only table.
//
// ProfileID is the owner. It is the profile the authentication layer
// provisioned for the identity that created the row, which is what makes
// ownership unspoofable: it is never read from a form field.
type Task struct {
	ID uint `gorm:"primaryKey"`

	// ProfileID is who the row belongs to. It is not a foreign key: the
	// profile row is provisioned by the authentication layer, and an example
	// that added a constraint across two packages' tables would be teaching
	// a coupling the framework does not require.
	ProfileID uint `gorm:"not null"`

	Title string `gorm:"size:200;not null"`
	Notes string `gorm:"size:4000"`

	Done bool `gorm:"not null;default:false"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName pins the table name rather than letting GORM pluralise the type, so
// that renaming the Go type later is a rename and not a data migration.
func (Task) TableName() string { return "tasks" }

// The two limits the form enforces. They match the column sizes: a form that
// accepts more than the column holds turns the user's own typing into a database
// error, which is the server's fault dressed up as the user's.
const (
	maxTitleLength = 200
	maxNotesLength = 4000
)
