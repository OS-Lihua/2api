package model

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	providerTable       = "providers"
	legacyProviderTable = "channels"
)

type providerSchemaRename struct {
	// Kind is a pg_class relkind ("i" index, "S" sequence) or "c" for a
	// constraint without an index.
	Kind string `gorm:"column:kind"`
	Name string `gorm:"column:name"`
}

// migrateProviderTable renames the table that stored Providers before the
// rename, together with its sequence, indexes, and constraints, so the result
// matches a fresh schema. Column names are unchanged. It does nothing once the
// "channels" table is gone, so it is safe to run on every start.
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
