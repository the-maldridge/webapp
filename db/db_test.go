package db

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

// The pragmas are only useful if the driver actually applied them, so
// this asserts on what the server reports rather than on the DSN string.
func TestSqlitePragmasActuallyApplied(t *testing.T) {
	d := testDB(t)

	var journal string
	if err := d.Raw().Raw("PRAGMA journal_mode").Scan(&journal).Error; err != nil {
		t.Fatal(err)
	}
	if strings.ToLower(journal) != "wal" {
		t.Errorf("journal_mode = %q, want wal", journal)
	}

	var fks int
	if err := d.Raw().Raw("PRAGMA foreign_keys").Scan(&fks).Error; err != nil {
		t.Fatal(err)
	}
	if fks != 1 {
		t.Errorf("foreign_keys = %d, want 1", fks)
	}
}

// Foreign keys being enforced is the observable consequence of the
// pragma, and it is the behaviour applications silently lose when the
// DSN is assembled by hand.
func TestSqliteForeignKeysEnforced(t *testing.T) {
	d := testDB(t, WithAutoMigrate(&author{}, &book{}))

	err := G[book](d).Create(context.Background(), &book{Title: "orphan", AuthorID: 9999})
	if err == nil {
		t.Fatal("expected a foreign key violation inserting a book with no author")
	}
}

func TestPoolLimitsAreConfigured(t *testing.T) {
	d := testDB(t, WithPool(3, 1, time.Minute))
	sqlDB, err := d.SQL()
	if err != nil {
		t.Fatal(err)
	}
	stats := sqlDB.Stats()
	if stats.MaxOpenConnections != 3 {
		t.Errorf("MaxOpenConnections = %d, want 3", stats.MaxOpenConnections)
	}
}

func TestSQLOnNilHandleErrors(t *testing.T) {
	var d *DB
	if _, err := d.SQL(); !errors.Is(err, ErrNoDriver) {
		t.Errorf("SQL() on a nil handle returned %v, want ErrNoDriver", err)
	}
	if d.Raw() != nil {
		t.Error("Raw() on a nil handle should be nil")
	}
	if err := d.Close(); err != nil {
		t.Errorf("Close() on a nil handle should be nil, got %v", err)
	}
	if d.Driver() != "" {
		t.Error("Driver() on a nil handle should be empty")
	}
}

func TestDriverReportsSelection(t *testing.T) {
	d := testDB(t)
	if d.Driver() != DriverSQLite {
		t.Errorf("Driver() = %q, want sqlite", d.Driver())
	}
}

func TestOpenFailureReturnsErrorNotPanic(t *testing.T) {
	// A directory that does not exist cannot hold the database file.
	_, err := New(WithEnvPrefix("TEST"), WithPath(filepath.Join(t.TempDir(), "missing", "sub", "x.db")))
	if err == nil {
		t.Fatal("expected an error opening a database in a missing directory")
	}
	if strings.Contains(err.Error(), "panic") {
		t.Errorf("error should not mention a panic: %v", err)
	}
}

func TestInsertOrUpdate(t *testing.T) {
	d := testDB(t, WithAutoMigrate(&author{}))
	ctx := context.Background()

	if err := InsertOrUpdate(ctx, d, &author{ID: 1, Name: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := InsertOrUpdate(ctx, d, &author{ID: 1, Name: "second"}); err != nil {
		t.Fatal(err)
	}

	got, err := G[author](d).Where("id = ?", 1).First(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "second" {
		t.Errorf("Name = %q, want the update to win", got.Name)
	}

	count, err := G[author](d).Count(ctx, "*")
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("row count = %d, want 1", count)
	}
}

func TestInsertOrUpdateOnColumn(t *testing.T) {
	d := testDB(t, WithAutoMigrate(&author{}))
	ctx := context.Background()

	// Name carries a unique index, so a second insert with the same
	// name must update rather than conflict.
	if err := d.Raw().Exec("CREATE UNIQUE INDEX idx_authors_name ON authors (name)").Error; err != nil {
		t.Fatal(err)
	}
	first := &author{Name: "unique"}
	if err := InsertOrUpdate(ctx, d, first); err != nil {
		t.Fatal(err)
	}
	second := &author{Name: "unique"}
	if err := InsertOrUpdate(ctx, d, second, OnConflictColumns("name")); err != nil {
		t.Fatalf("upsert on a unique column should not conflict: %v", err)
	}
}

func TestTransactionRollsBack(t *testing.T) {
	d := testDB(t, WithAutoMigrate(&author{}))
	ctx := context.Background()
	boom := errors.New("nope")

	err := Transaction(ctx, d, func(tx *gorm.DB) error {
		if err := tx.Create(&author{Name: "rolled back"}).Error; err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Transaction returned %v, want the inner error", err)
	}

	count, err := G[author](d).Count(ctx, "*")
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("row count = %d after rollback, want 0", count)
	}
}

func TestTransactionResult(t *testing.T) {
	d := testDB(t, WithAutoMigrate(&author{}))
	ctx := context.Background()

	created, err := TransactionResult(ctx, d, func(tx *gorm.DB) (author, error) {
		a := author{Name: "kept"}
		if err := tx.Create(&a).Error; err != nil {
			return author{}, err
		}
		return a, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == 0 || created.Name != "kept" {
		t.Errorf("TransactionResult returned %+v, want a persisted author", created)
	}

	boom := errors.New("boom")
	_, err = TransactionResult(ctx, d, func(tx *gorm.DB) (author, error) {
		return author{Name: "discarded"}, boom
	})
	if !errors.Is(err, boom) {
		t.Errorf("TransactionResult returned %v, want the inner error", err)
	}
	count, _ := G[author](d).Count(ctx, "*")
	if count != 1 {
		t.Errorf("row count = %d, want only the successful transaction's row", count)
	}
}

func TestClampPage(t *testing.T) {
	cases := []struct {
		name             string
		page, size       int
		wantPage, wantSz int
	}{
		{"pass through", 3, 50, 3, 50},
		{"zero page", 0, 10, 1, 10},
		{"negative page", -5, 10, 1, 10},
		{"zero size", 1, 0, 1, DefaultPageSize},
		{"negative size", 1, -2, 1, DefaultPageSize},
		{"oversized size", 1, 100000, 1, MaxPageSize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotPage, gotSize := ClampPage(tc.page, tc.size)
			if gotPage != tc.wantPage || gotSize != tc.wantSz {
				t.Errorf("ClampPage(%d, %d) = (%d, %d), want (%d, %d)",
					tc.page, tc.size, gotPage, gotSize, tc.wantPage, tc.wantSz)
			}
		})
	}
}

func TestOffset(t *testing.T) {
	cases := []struct{ page, size, want int }{
		{1, 10, 0},
		{2, 10, 10},
		{4, 25, 75},
		{0, 10, 0},
	}
	for _, tc := range cases {
		if got := Offset(tc.page, tc.size); got != tc.want {
			t.Errorf("Offset(%d, %d) = %d, want %d", tc.page, tc.size, got, tc.want)
		}
	}
}

// The classic and generic pagination variants must produce the same
// window; the whole reason two exist is a signature mismatch, not a
// behavioural one.
func TestPaginateVariantsAgree(t *testing.T) {
	d := testDB(t, WithAutoMigrate(&author{}))
	ctx := context.Background()

	for i := range 10 {
		if err := d.Raw().Create(&author{Name: string(rune('a' + i))}).Error; err != nil {
			t.Fatal(err)
		}
	}

	var classic []author
	if err := d.WithContext(ctx).Scopes(Paginate(2, 3)).Order("id asc").Find(&classic).Error; err != nil {
		t.Fatal(err)
	}

	generic, err := G[author](d).Scopes(PaginateScope(2, 3)).Order("id asc").Find(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(classic) != 3 || len(generic) != 3 {
		t.Fatalf("classic returned %d rows, generic returned %d, want 3 each", len(classic), len(generic))
	}
	for i := range classic {
		if classic[i].ID != generic[i].ID {
			t.Errorf("position %d: classic id %d, generic id %d", i, classic[i].ID, generic[i].ID)
		}
	}
}

func TestTotalPages(t *testing.T) {
	cases := []struct {
		count int64
		size  int
		want  int
	}{
		{0, 10, 1},
		{1, 10, 1},
		{10, 10, 1},
		{11, 10, 2},
		{20, 10, 2},
		{21, 10, 3},
		{-5, 10, 1},
	}
	for _, tc := range cases {
		if got := TotalPages(tc.count, tc.size); got != tc.want {
			t.Errorf("TotalPages(%d, %d) = %d, want %d", tc.count, tc.size, got, tc.want)
		}
	}
}
