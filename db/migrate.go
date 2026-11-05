package db

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// Migration is a one-time, ordered schema or data change that runs
// after AutoMigrate. Version identifies the migration and determines
// the order in which migrations run; it sorts lexicographically, so a
// date prefix such as "2026-11-04-backfill-owners" gives chronological
// ordering for free.
//
// Up runs inside a transaction. If it returns an error the transaction
// is rolled back, the version is not recorded, and the next boot
// retries it. There is intentionally no down migration: a change that
// cannot be expressed as a forward step is usually better expressed as
// two forward steps, one to move and one to drop.
type Migration struct {
	Version string
	Up      func(tx *gorm.DB) error
}

// migrationsTable records which versioned migrations have already run.
const migrationsTable = "schema_migrations"

// createMigrationsTableDDL creates the bookkeeping table. Column types
// are deliberately restricted to ones every supported backend has: the
// applied timestamp is stored as Unix seconds rather than in a
// timestamp column, so no dialect-specific type is required.
const createMigrationsTableDDL = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version TEXT NOT NULL PRIMARY KEY,
	applied_at BIGINT NOT NULL
)`

// applyMigrations runs AutoMigrate for the registered models and then
// any pending versioned migrations.
func (d *DB) applyMigrations(ctx context.Context) error {
	if d.config.SkipMigrations {
		return nil
	}

	models, err := orderModels(d.config.Models)
	if err != nil {
		return err
	}

	if len(models) > 0 {
		if err := d.WithContext(ctx).AutoMigrate(models...); err != nil {
			return fmt.Errorf("db: auto-migration failed: %w", err)
		}
	}

	return d.applyVersionedMigrations(ctx)
}

// applyVersionedMigrations applies, in lexicographic order, every
// registered migration not yet recorded as applied.
func (d *DB) applyVersionedMigrations(ctx context.Context) error {
	migrations := make([]Migration, len(d.config.Migrations))
	copy(migrations, d.config.Migrations)
	sort.SliceStable(migrations, func(i, j int) bool {
		return migrations[i].Version < migrations[j].Version
	})
	if len(migrations) == 0 {
		return nil
	}

	db := d.WithContext(ctx)
	if err := db.Exec(createMigrationsTableDDL).Error; err != nil {
		return fmt.Errorf("db: could not create %s: %w", migrationsTable, err)
	}

	for _, m := range migrations {
		var applied int64
		if err := db.Table(migrationsTable).Where("version = ?", m.Version).Count(&applied).Error; err != nil {
			return fmt.Errorf("db: could not read %s: %w", migrationsTable, err)
		}
		if applied > 0 {
			continue
		}

		err := d.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := m.Up(tx); err != nil {
				return err
			}
			return tx.Exec(
				"INSERT INTO "+migrationsTable+" (version, applied_at) VALUES (?, ?)",
				m.Version,
				time.Now().Unix(),
			).Error
		})
		if err != nil {
			return fmt.Errorf("db: migration %q failed: %w", m.Version, err)
		}
	}

	return nil
}

// orderModels returns models in an order that satisfies foreign-key
// dependencies: a model holding a foreign key is migrated after the
// model it points at. Registration order is preserved wherever it does
// not conflict, so a developer's ordering survives and the result is
// deterministic.
//
// PostgreSQL validates foreign keys when a table is created, so a
// child created before its parent fails the migration outright. SQLite
// accepts either order, which means the mistake stays invisible until
// the application is pointed at PostgreSQL.
func orderModels(models []any) ([]any, error) {
	if len(models) < 2 {
		return models, nil
	}

	var store sync.Map
	types := make([]reflect.Type, len(models))
	index := make(map[reflect.Type]int, len(models))

	for i, m := range models {
		s, err := schema.Parse(m, &store, schema.NamingStrategy{})
		if err != nil {
			return nil, fmt.Errorf("db: could not parse model %T for dependency ordering: %w", m, err)
		}
		types[i] = s.ModelType
		index[s.ModelType] = i
	}

	// deps[i] holds the indexes of the models model i depends on.
	deps := make([]map[int]struct{}, len(models))
	for i, m := range models {
		s, err := schema.Parse(m, &store, schema.NamingStrategy{})
		if err != nil {
			return nil, fmt.Errorf("db: could not parse model %T for dependency ordering: %w", m, err)
		}
		deps[i] = dependencyIndexes(s, index)
	}

	return topoSort(types, deps)
}

// dependencyIndexes returns the registered indexes of the models that s
// must be migrated after.
//
// A belongs-to relation stores the foreign key on this model's table,
// so this model depends on the target. A many-to-many relation creates
// a join table referencing both sides, so this model depends on the
// target too. A has-many or has-one relation stores the key on the
// other side and is therefore not an ordering constraint in this
// direction.
func dependencyIndexes(s *schema.Schema, index map[reflect.Type]int) map[int]struct{} {
	out := make(map[int]struct{})
	add := func(other *schema.Schema) {
		if other == nil || other.ModelType == s.ModelType {
			return
		}
		if i, ok := index[other.ModelType]; ok {
			out[i] = struct{}{}
		}
	}

	for _, rel := range s.Relationships.BelongsTo {
		add(rel.FieldSchema)
	}
	for _, rel := range s.Relationships.Many2Many {
		add(rel.FieldSchema)
	}
	return out
}

// topoSort orders models so that every model follows its dependencies.
// Ties break on registration index, so an already-correct list is
// returned unchanged.
func topoSort(types []reflect.Type, deps []map[int]struct{}) ([]any, error) {
	n := len(types)
	outstanding := make([]map[int]struct{}, n)
	counts := make([]int, n)
	for i := range deps {
		outstanding[i] = make(map[int]struct{}, len(deps[i]))
		for dep := range deps[i] {
			outstanding[i][dep] = struct{}{}
		}
		counts[i] = len(outstanding[i])
	}

	out := make([]any, 0, n)
	emitted := make([]bool, n)

	for len(out) < n {
		progress := false
		for i := range types {
			if emitted[i] || counts[i] != 0 {
				continue
			}
			out = append(out, reflect.New(types[i]).Interface())
			emitted[i] = true
			progress = true

			for j := range types {
				if _, blocked := outstanding[j][i]; blocked {
					delete(outstanding[j], i)
					counts[j]--
				}
			}
		}
		if !progress {
			return nil, cycleError(types, emitted)
		}
	}

	return out, nil
}

// cycleError names the models participating in a dependency cycle.
func cycleError(types []reflect.Type, emitted []bool) error {
	var names []string
	for i := range types {
		if !emitted[i] {
			names = append(names, types[i].String())
		}
	}
	return fmt.Errorf("db: model dependency cycle among %v; break it by storing the foreign key without the association field", names)
}
