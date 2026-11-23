package model

import (
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	providerTable       = "providers"
	legacyProviderTable = "channels"
)

var ErrLegacyProviderRouting = errors.New("legacy Provider routing requires offline migration; stop all nodes and run migrate-legacy")

func inspectLegacyProviderRoutingColumns(db *gorm.DB) (int64, error) {
	var count int64
	err := db.Raw(`SELECT COUNT(*) FROM pg_catalog.pg_attribute
		WHERE attrelid = to_regclass('providers') AND attnum > 0 AND NOT attisdropped
		AND attname IN ('models', 'model_mapping', 'group', 'priority', 'weight')`).Scan(&count).Error
	return count, err
}

// The caller holds the schema migration lock and has either initialized a
// native catalog or finished converting the legacy Provider declarations.
// Only providers is altered; channels.weight still controls weighted routing.
func retireLegacyProviderRoutingTx(tx *gorm.DB) (int64, error) {
	count, err := inspectLegacyProviderRoutingColumns(tx)
	if err != nil || count == 0 {
		return 0, err
	}
	// No CASCADE: a dependant outside the old schema must block retirement.
	if err := tx.Exec(`ALTER TABLE "providers"
		DROP COLUMN IF EXISTS "models", DROP COLUMN IF EXISTS "model_mapping",
		DROP COLUMN IF EXISTS "group", DROP COLUMN IF EXISTS "priority",
		DROP COLUMN IF EXISTS "weight"`).Error; err != nil {
		return 0, err
	}
	return count, nil
}

type providerSchemaRename struct {
	// Kind is a pg_class relkind ("i" index, "S" sequence) or "c" for a
	// constraint without an index.
	Kind string `gorm:"column:kind"`
	Name string `gorm:"column:name"`
}

// migrateProviderTable renames the table that stored Providers before the
// rename, together with its sequence, indexes, and constraints, so the result
// matches a fresh schema. Column names are unchanged. A channels table with
// provider_id belongs to the new single-model Channel and is left unchanged.
func migrateProviderTable(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var tables struct {
			Legacy  bool `gorm:"column:legacy"`
			Current bool `gorm:"column:current"`
		}
		if err := tx.Raw(
			"SELECT to_regclass(?) IS NOT NULL AS legacy, to_regclass(?) IS NOT NULL AS current",
			legacyProviderTable, providerTable,
		).Scan(&tables).Error; err != nil {
			return fmt.Errorf("inspect provider tables: %w", err)
		}
		if !tables.Legacy {
			return nil
		}
		// The new single-model Channel table has provider_id and no account
		// credentials. It must not be mistaken for the old Provider table.
		if tx.Migrator().HasColumn(legacyProviderTable, "provider_id") &&
			tx.Migrator().HasColumn(legacyProviderTable, "model_id") &&
			!tx.Migrator().HasColumn(legacyProviderTable, "key") {
			return nil
		}
		if tables.Current {
			return fmt.Errorf("both %q and %q tables exist; move the rows into %q and drop %q before starting",
				legacyProviderTable, providerTable, providerTable, legacyProviderTable)
		}
		if err := tx.Exec("ALTER TABLE ? RENAME TO ?",
			clause.Table{Name: legacyProviderTable}, clause.Table{Name: providerTable}).Error; err != nil {
			return fmt.Errorf("rename %q to %q: %w", legacyProviderTable, providerTable, err)
		}

		// Renaming an index also renames the constraint that owns it, so only
		// constraints without an index are renamed directly.
		var renames []providerSchemaRename
		if err := tx.Raw(`
SELECT class_meta.relkind::text AS kind, class_meta.relname AS name
FROM pg_catalog.pg_class AS class_meta
WHERE class_meta.relnamespace = current_schema()::regnamespace
  AND class_meta.relkind IN ('i', 'S')
  AND class_meta.relname LIKE '%channels%'
  AND (
      EXISTS (SELECT 1 FROM pg_catalog.pg_index AS index_meta
              WHERE index_meta.indexrelid = class_meta.oid AND index_meta.indrelid = to_regclass(?))
      OR EXISTS (SELECT 1 FROM pg_catalog.pg_depend AS depend_meta
                 WHERE depend_meta.objid = class_meta.oid AND depend_meta.refobjid = to_regclass(?)
                   AND depend_meta.deptype IN ('a', 'i'))
  )
UNION ALL
SELECT 'c' AS kind, constraint_meta.conname AS name
FROM pg_catalog.pg_constraint AS constraint_meta
WHERE constraint_meta.conrelid = to_regclass(?)
  AND constraint_meta.conindid = 0
  AND constraint_meta.conname LIKE '%channels%'
ORDER BY kind, name`, providerTable, providerTable, providerTable).Scan(&renames).Error; err != nil {
			return fmt.Errorf("inspect %q indexes, sequences, and constraints: %w", providerTable, err)
		}
		for _, rename := range renames {
			target := strings.Replace(rename.Name, legacyProviderTable, providerTable, 1)
			statement := "ALTER TABLE ? RENAME CONSTRAINT ? TO ?"
			args := []any{clause.Table{Name: providerTable}, clause.Column{Name: rename.Name}, clause.Column{Name: target}}
			switch rename.Kind {
			case "i":
				statement = "ALTER INDEX ? RENAME TO ?"
				args = []any{clause.Table{Name: rename.Name}, clause.Table{Name: target}}
			case "S":
				statement = "ALTER SEQUENCE ? RENAME TO ?"
				args = []any{clause.Table{Name: rename.Name}, clause.Table{Name: target}}
			}
			if err := tx.Exec(statement, args...).Error; err != nil {
				return fmt.Errorf("rename %q to %q: %w", rename.Name, target, err)
			}
		}
		return nil
	})
}
