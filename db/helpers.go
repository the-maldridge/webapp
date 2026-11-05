package db

import (
	"context"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// G is an alias for gorm.G, so an application can import this package
// alone for every database operation.
//
// Note the session semantics: gorm.G begins a fresh session, so a
// filtered *gorm.DB cannot be passed in and have its conditions
// preserved. Build the entire query, including filters and limits, on
// the returned chain.
func G[T any](d *DB, opts ...clause.Expression) gorm.Interface[T] {
	return gorm.G[T](d.Raw(), opts...)
}

// InsertOrUpdate creates data, or updates every column of the existing
// row when the primary key already exists.
//
// This is what lets a single handler serve both create and edit: a
// zero primary key inserts, a non-zero one updates. The extra
// expressions are passed to gorm.G, so a caller can override the
// conflict target, for example to upsert on a unique column other than
// the primary key.
func InsertOrUpdate[T any](ctx context.Context, d *DB, data *T, opts ...clause.Expression) error {
	if len(opts) == 0 {
		return gorm.G[T](d.Raw(), clause.OnConflict{UpdateAll: true}).Create(ctx, data)
	}
	return gorm.G[T](d.Raw(), opts...).Create(ctx, data)
}

// OnConflictColumns returns the upsert expression that conflicts on the
// named columns instead of the primary key, updating all others.
func OnConflictColumns(columns ...string) clause.Expression {
	targets := make([]clause.Column, len(columns))
	for i, c := range columns {
		targets[i] = clause.Column{Name: c}
	}
	return clause.OnConflict{
		Columns:   targets,
		UpdateAll: true,
	}
}

// Transaction runs fn inside a transaction, rolling back if it returns
// an error. The context is attached to the transaction, so cancelling
// it cancels the in-flight statements.
//
// Multi-statement writes need this. Without it, a failure halfway
// through leaves the first statements committed, and the failure mode
// is quietly inconsistent data rather than an error.
func Transaction(ctx context.Context, d *DB, fn func(tx *gorm.DB) error) error {
	return d.WithContext(ctx).Transaction(fn)
}

// TransactionResult runs fn inside a transaction and returns the value
// it produced, rolling back if fn returns an error. It exists because
// the common shape is "write two rows, return the second one", and
// doing that with the error-only form requires declaring a zero value
// and assigning into it from the closure.
func TransactionResult[T any](ctx context.Context, d *DB, fn func(tx *gorm.DB) (T, error)) (T, error) {
	var out T
	err := d.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		out, err = fn(tx)
		return err
	})
	return out, err
}

// DefaultPageSize is the page size used when a caller asks for
// pagination without naming a size.
const DefaultPageSize = 25

// MaxPageSize bounds the page size a caller may request. Unbounded page
// sizes turn a list endpoint into a memory and latency problem, and any
// client can ask.
const MaxPageSize = 250

// ClampPage normalizes a requested page and size into usable values. A
// page below one becomes one; a size below one becomes DefaultPageSize
// and a size above MaxPageSize is reduced to it.
func ClampPage(page, size int) (int, int) {
	if page < 1 {
		page = 1
	}
	switch {
	case size < 1:
		size = DefaultPageSize
	case size > MaxPageSize:
		size = MaxPageSize
	}
	return page, size
}

// Offset returns the row offset for a page, clamping its inputs first.
func Offset(page, size int) int {
	page, size = ClampPage(page, size)
	return (page - 1) * size
}

// Paginate returns a scope for GORM's classic chainable API.
//
//	db.Scopes(db.Paginate(page, size)).Find(&tasks)
func Paginate(page, size int) func(*gorm.DB) *gorm.DB {
	page, size = ClampPage(page, size)
	offset := (page - 1) * size
	return func(d *gorm.DB) *gorm.DB {
		return d.Limit(size).Offset(offset)
	}
}

// PaginateScope returns the same pagination for GORM's generic chain.
//
//	db.G[Task](d).Scopes(db.PaginateScope(page, size)).Find(ctx)
//
// The two variants exist because the generic chain's Scopes takes
// func(*gorm.Statement) while the classic chain takes
// func(*gorm.DB) *gorm.DB. A single function cannot satisfy both
// signatures, and the framework targets the generic chain, so both are
// exported rather than forcing every caller to adapt by hand.
func PaginateScope(page, size int) func(*gorm.Statement) {
	page, size = ClampPage(page, size)
	limit := size
	offset := (page - 1) * size
	return func(stmt *gorm.Statement) {
		stmt.AddClause(clause.Limit{Limit: &limit, Offset: offset})
	}
}

// TotalPages returns how many pages a result count occupies at size.
// A count of zero yields one page, so a list with no rows still renders
// a pager showing the current page.
func TotalPages(count int64, size int) int {
	_, size = ClampPage(1, size)
	if count <= 0 {
		return 1
	}
	pages := int(count / int64(size))
	if count%int64(size) != 0 {
		pages++
	}
	return pages
}
