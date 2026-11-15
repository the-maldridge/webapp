// Examples for the query helpers. The ones that print are pure arithmetic and
// run as part of the suite; the ones that need a database handle carry no Output
// comment, which makes "go test" compile them without running them. That is the
// point: a documented call that does not compile is worse than no documentation,
// and only the compiler notices.
package db_test

import (
	"context"
	"fmt"
	"log"

	"gorm.io/gorm"

	"github.com/the-maldridge/webapp/db"
)

// task is the model the examples below query.
type task struct {
	ID      uint
	Title   string
	OwnerID uint
}

func ExampleClampPage() {
	// A size outside the allowed range is pulled back inside it rather than
	// rejected, so a hand-edited query string cannot ask for ten million rows
	// on one page and a blank one gets the default.
	page, size := db.ClampPage(0, 100_000)
	fmt.Println(page, size)

	page, size = db.ClampPage(3, 0)
	fmt.Println(page, size)

	// Output:
	// 1 250
	// 3 25
}

func ExampleOffset() {
	fmt.Println(db.Offset(1, 25))
	fmt.Println(db.Offset(4, 10))

	// The page is clamped before it is multiplied, so a negative page is the
	// first page rather than a negative offset.
	fmt.Println(db.Offset(-2, 10))

	// Output:
	// 0
	// 30
	// 0
}

func ExampleTotalPages() {
	fmt.Println(db.TotalPages(0, 25))
	fmt.Println(db.TotalPages(25, 25))
	fmt.Println(db.TotalPages(26, 25))

	// Output:
	// 1
	// 1
	// 2
}

// ExampleG shows the generic query entry point. It is compiled, not run.
func ExampleG() {
	d, err := db.New(
		db.WithDriver(db.DriverSQLite),
		db.WithPath("tasks.db"),
		db.WithAutoMigrate(task{}),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer d.Close()

	ctx := context.Background()

	// Build the entire query on the returned chain. gorm's generic entry point
	// starts a fresh session, so conditions applied to a *gorm.DB beforehand
	// are dropped rather than added to.
	tasks, err := db.G[task](d).
		Where("owner_id = ?", 1).
		Order("id ASC").
		Scopes(db.PaginateScope(1, 20)).
		Find(ctx)
	if err != nil {
		log.Fatal(err)
	}
	for _, t := range tasks {
		fmt.Println(t.Title)
	}
}

// ExampleG_recordNotFound shows the one case where gorm reports "not found", and
// why a handler that checks err after Find never sees it.
func ExampleG_recordNotFound() {
	d, err := db.New(db.WithDriver(db.DriverSQLite), db.WithPath("tasks.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer d.Close()

	// First, Last, and Take return gorm.ErrRecordNotFound. Find does not: an
	// empty slice is a successful query, and a handler that checks err after a
	// Find answers 200 with an empty page forever.
	one, err := db.G[task](d).Where("id = ?", 7).First(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	_ = one
}

// ExampleInsertOrUpdate upserts a row, which is the difference between "save"
// and "insert or update" without reading the row first.
func ExampleInsertOrUpdate() {
	d, err := db.New(db.WithDriver(db.DriverSQLite), db.WithPath("tasks.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer d.Close()

	row := &task{ID: 1, Title: "write it down", OwnerID: 4}
	if err := db.InsertOrUpdate(context.Background(), d, row,
		db.OnConflictColumns("id")); err != nil {
		log.Fatal(err)
	}
}

// ExampleTransactionResult runs a unit of work that produces a value, and rolls
// it back if anything returns an error.
func ExampleTransactionResult() {
	d, err := db.New(db.WithDriver(db.DriverSQLite), db.WithPath("tasks.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer d.Close()

	created, err := db.TransactionResult[int](context.Background(), d,
		func(tx *gorm.DB) (int, error) {
			var before, after int64
			if err := tx.Model(&task{}).Count(&before).Error; err != nil {
				return 0, err
			}
			if err := tx.Create(&task{Title: "first", OwnerID: 4}).Error; err != nil {
				return 0, err
			}
			if err := tx.Model(&task{}).Count(&after).Error; err != nil {
				return 0, err
			}
			return int(after - before), nil
		})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("rows created:", created)
}

// ExampleWithMigrations registers a versioned schema step that runs once, in
// version order, after the models have been synchronized.
func ExampleWithMigrations() {
	d, err := db.New(
		db.WithDriver(db.DriverPostgres),
		db.WithDSN("host=localhost dbname=tasks sslmode=require"),
		db.WithMigrations(db.Migration{
			Version: "2026-11-14-task-owner",
			Up: func(tx *gorm.DB) error {
				return tx.Exec(
					"CREATE INDEX IF NOT EXISTS task_owner ON task (owner_id)",
				).Error
			},
		}),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer d.Close()
}
