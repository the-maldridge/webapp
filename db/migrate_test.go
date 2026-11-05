package db

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gorm.io/gorm"
)

// testDB opens a sqlite database in a temporary directory.
func testDB(t *testing.T, opts ...Option) *DB {
	t.Helper()
	base := []Option{
		WithEnvPrefix("TEST"),
		WithPath(filepath.Join(t.TempDir(), "test.db")),
	}
	d, err := New(append(base, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

type author struct {
	ID   uint
	Name string
}

type book struct {
	ID       uint
	Title    string
	AuthorID uint
	Author   author `gorm:"foreignKey:AuthorID"`
}

type tag struct {
	ID   uint
	Name string
}

type post struct {
	ID   uint
	Tags []tag `gorm:"many2many:post_tags"`
}

// orderModels must return a parent before its child even when the
// caller registered them in the wrong order, because PostgreSQL
// validates foreign keys at table creation.
func TestOrderModelsFixesChildFirstRegistration(t *testing.T) {
	out, err := orderModels([]any{&book{}, &author{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out[0].(*author); !ok {
		t.Fatalf("expected author first, got %T", out[0])
	}
	if _, ok := out[1].(*book); !ok {
		t.Fatalf("expected book second, got %T", out[1])
	}
}

// A correctly ordered list must be returned unchanged: sorting should
// never reorder work the developer already got right.
func TestOrderModelsPreservesCorrectOrder(t *testing.T) {
	out, err := orderModels([]any{&author{}, &book{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out[0].(*author); !ok {
		t.Fatalf("expected author first, got %T", out[0])
	}
}

// A has-many relation stores the key on the child, so it must not force
// a reordering when the parent is registered second.
func TestOrderModelsIgnoresReverseHasMany(t *testing.T) {
	type chapter struct {
		ID     uint
		Name   string
		BookID uint
	}
	out, err := orderModels([]any{&chapter{}, &author{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out[0].(*chapter); !ok {
		t.Fatalf("unrelated models should keep registration order, got %T first", out[0])
	}
}

func TestOrderModelsDetectsCycle(t *testing.T) {
	// A real Go type cannot reference itself through a non-pointer
	// struct field, so the cycle is fed to the sorter directly.
	_, err := topoSort(
		[]reflect.Type{reflect.TypeOf(author{}), reflect.TypeOf(book{})},
		[]map[int]struct{}{
			{1: {}},
			{0: {}},
		},
	)
	if err == nil {
		t.Fatal("expected a cycle error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error %q should mention a cycle", err)
	}
	// The message must name the stuck models so the developer knows
	// which association to break.
	for _, want := range []string{"author", "book"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should name %q", err, want)
		}
	}
}

func TestAutoMigrateAndVersionedMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migrate.db")
	var runs []string

	newHandle := func() *DB {
		d, err := New(
			WithEnvPrefix("TEST"),
			WithPath(path),
			WithAutoMigrate(&author{}, &book{}),
			WithMigrations(Migration{
				Version: "001-add-bio",
				Up: func(tx *gorm.DB) error {
					runs = append(runs, "001")
					return tx.Exec("ALTER TABLE authors ADD COLUMN bio TEXT").Error
				},
			}),
		)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return d
	}

	d := newHandle()
	if len(runs) != 1 {
		t.Fatalf("migration ran %d times, want 1", len(runs))
	}
	if !d.Raw().Migrator().HasColumn(&author{}, "bio") {
		t.Error("versioned migration did not add the column")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	// A second boot on the same file must not re-run the migration.
	d2 := newHandle()
	defer d2.Close()
	if len(runs) != 1 {
		t.Errorf("migration ran %d times across two boots, want 1", len(runs))
	}
}

func TestFailedMigrationIsRetriedAndNotRecorded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retry.db")
	boom := errors.New("boom")
	attempts := 0

	boot := func(fail bool) error {
		d, err := New(
			WithEnvPrefix("TEST"),
			WithPath(path),
			WithMigrations(Migration{
				Version: "001-fails",
				Up: func(tx *gorm.DB) error {
					attempts++
					if fail {
						return boom
					}
					return nil
				},
			}),
		)
		if d != nil {
			d.Close()
		}
		return err
	}

	if err := boot(true); err == nil {
		t.Fatal("expected New to fail when a migration fails")
	} else if !errors.Is(err, boom) {
		t.Errorf("error %q should wrap the migration error", err)
	}
	if attempts != 1 {
		t.Fatalf("migration ran %d times, want 1", attempts)
	}

	if err := boot(false); err != nil {
		t.Fatalf("second boot should succeed: %v", err)
	}
	if attempts != 2 {
		t.Errorf("failed migration should be retried, ran %d times total", attempts)
	}
}

func TestWithoutMigrationsSkipsEverything(t *testing.T) {
	d := testDB(t, WithAutoMigrate(&author{}), WithoutMigrations())
	if d.Raw().Migrator().HasTable(&author{}) {
		t.Error("WithoutMigrations still created tables")
	}
}

func TestOrderModelsRejectsNonStruct(t *testing.T) {
	_, err := orderModels([]any{42, &author{}})
	if err == nil {
		t.Fatal("expected an error for a non-struct model")
	}
	if !strings.Contains(err.Error(), "42") && !strings.Contains(err.Error(), "int") {
		t.Errorf("error %q should identify the bad model", err)
	}
}

func TestOrderModelsMany2ManyOrdering(t *testing.T) {
	// A many-to-many creates a join table referencing both sides, so
	// post must migrate after tag.
	out, err := orderModels([]any{&post{}, &tag{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out[0].(*tag); !ok {
		t.Fatalf("expected tag before post, got %T", out[0])
	}
}
