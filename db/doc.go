// Package db provides the database layer for webapp applications.
//
// A DB is a thin wrapper around *gorm.DB that applies, by default, the
// configuration that hand-wired applications reliably forget: SQLite
// pragmas (WAL, a busy timeout, and enforced foreign keys), statement
// preparation, bounded connection pools, and a slow-query threshold
// that is reported through log/slog rather than GORM's own logger.
//
// Driver selection is by option first, environment variable second,
// and default third. With an application prefix of "TASKS" the
// following variables are consulted:
//
//	TASKS_DB_DRIVER  "sqlite" (default) or "postgres"
//	TASKS_DB         path to the SQLite database file
//	TASKS_DB_DSN     PostgreSQL DSN, required when the driver is postgres
//
// # Migrations
//
// Two layers of migration run at construction time. AutoMigrate is
// applied on every boot for the registered models and is idempotent,
// so a fresh deployment needs no operator step and a crash during
// startup is safe to retry. Versioned migrations then run in
// lexicographic order, each inside its own transaction and tracked in
// a schema_migrations table so that each one is applied exactly once.
// Versioned migrations are for the changes AutoMigrate cannot express:
// renames, data backfills, and index creation after a de-duplication
// pass.
//
// Models are migrated in registration order. PostgreSQL validates
// foreign keys when a table is created, so a model that references
// another model must be registered after it.
//
// # Query layer
//
// Applications should use the generic API, either through GORM
// directly or through the G[T] alias exported here:
//
//	tasks, err := db.G[Task](d).
//		Where("owner_id = ?", profileID).
//		Order("created_at desc").
//		Find(ctx)
//
// Four behaviours of that API are worth knowing before debugging a
// query that "ignores" a condition:
//
//   - gorm.G[T]() begins a fresh session with NewDB: true. Conditions
//     applied to the *gorm.DB before the call are silently dropped;
//     filters, limits, and offsets must be part of the generic chain.
//   - Find never reports a missing row. Only First, Last, and Take
//     return ErrRecordNotFound, so treating a Find error as "no rows"
//     hides real database failures.
//   - Zero values in a struct condition are omitted from the WHERE
//     clause. Querying for a zero-valued column requires an explicit
//     Where("column = ?", 0).
//   - PostgreSQL enforces foreign keys at DDL time and reserves some
//     words that SQLite accepts as identifiers. Column names should
//     avoid SQL reserved words, and parent models must migrate before
//     their children.
package db
