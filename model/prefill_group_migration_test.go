package model

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestMigratePrefillGroupUniqueness(t *testing.T) {
	db := UseTestDatabases(t)

	var version string
	require.NoError(t, db.Raw("SELECT version()").Scan(&version).Error)
	t.Log(version)

	type indexDefinition struct {
		Name       string `gorm:"column:indexname"`
		Definition string `gorm:"column:indexdef"`
	}

	tests := []struct {
		name                string
		constraints         []string
		indexes             []string
		replacePartialIndex bool
		withoutDeletedAt    bool
		prepareOld          func(*testing.T, *gorm.DB)
		wantError           string
	}{
		{name: "fresh"},
		{
			name:        "legacy_constraint",
			constraints: []string{"idx_prefill_groups_name"},
		},
		{
			name:                "legacy_standalone_index",
			indexes:             []string{"idx_prefill_groups_name"},
			replacePartialIndex: true,
		},
		{
			name:        "renamed_constraints_and_indexes",
			constraints: []string{"idx_prefill_groups_name", "prefill_groups_name_key"},
			indexes:     []string{"idx_37606_uk_prefill_name", `custom "name" index`},
		},
		{
			name:                "imported_index_without_soft_delete_column",
			indexes:             []string{"idx_37606_uk_prefill_name"},
			replacePartialIndex: true,
			withoutDeletedAt:    true,
		},
		{
			name:                "global_index_uses_target_name",
			indexes:             []string{prefillGroupNameIndex},
			replacePartialIndex: true,
		},
		{
			name:                "global_constraint_uses_target_name",
			constraints:         []string{prefillGroupNameIndex},
			replacePartialIndex: true,
		},
		{
			name:        "non_conflicting_indexes_are_preserved",
			constraints: []string{"prefill_groups_name_key"},
			prepareOld: func(t *testing.T, tx *gorm.DB) {
				t.Helper()
				require.NoError(t, tx.Exec(
					"CREATE INDEX ? ON ? (?)",
					clause.Column{Name: "keep_prefill_name"},
					clause.Table{Name: "prefill_groups"},
					clause.Column{Name: "name"},
				).Error)
				require.NoError(t, tx.Exec(
					"CREATE UNIQUE INDEX ? ON ? (?, ?)",
					clause.Column{Name: "keep_prefill_name_deleted_at"},
					clause.Table{Name: "prefill_groups"},
					clause.Column{Name: "name"},
					clause.Column{Name: "deleted_at"},
				).Error)
				require.NoError(t, tx.Exec(
					"CREATE UNIQUE INDEX ? ON ? (lower(?)) WHERE deleted_at IS NULL",
					clause.Column{Name: "keep_prefill_lower_name"},
					clause.Table{Name: "prefill_groups"},
					clause.Column{Name: "name"},
				).Error)
				require.NoError(t, tx.Exec(
					"CREATE UNIQUE INDEX ? ON ? (?) WHERE deleted_at IS NOT NULL",
					clause.Column{Name: "keep_prefill_deleted_name"},
					clause.Table{Name: "prefill_groups"},
					clause.Column{Name: "name"},
				).Error)
			},
		},
		{
			name:                "unexpected_target_definition_rolls_back",
			constraints:         []string{"prefill_groups_name_key"},
			indexes:             []string{"idx_37606_uk_prefill_name"},
			replacePartialIndex: true,
			prepareOld: func(t *testing.T, tx *gorm.DB) {
				t.Helper()
				require.NoError(t, tx.Exec(
					"CREATE INDEX ? ON ? (?)",
					clause.Column{Name: prefillGroupNameIndex},
					clause.Table{Name: "prefill_groups"},
					clause.Column{Name: "type"},
				).Error)
			},
			wantError: "unexpected definition",
		},
		{
			name:                "target_name_on_other_table_rolls_back",
			indexes:             []string{"idx_37606_uk_prefill_name"},
			replacePartialIndex: true,
			withoutDeletedAt:    true,
			prepareOld: func(t *testing.T, tx *gorm.DB) {
				t.Helper()
				require.NoError(t, tx.Exec("CREATE TABLE other_groups (name varchar(64))").Error)
				require.NoError(t, tx.Exec(
					"CREATE INDEX ? ON other_groups (name)",
					clause.Column{Name: prefillGroupNameIndex},
				).Error)
			},
			wantError: "unexpected definition",
		},
		{
			name:                "foreign_key_dependency_rolls_back",
			constraints:         []string{"prefill_groups_name_key"},
			replacePartialIndex: true,
			withoutDeletedAt:    true,
			prepareOld: func(t *testing.T, tx *gorm.DB) {
				t.Helper()
				require.NoError(t, tx.Exec("CREATE TABLE referenced_groups (name varchar(64) REFERENCES prefill_groups(name))").Error)
				require.NoError(t, tx.Exec("INSERT INTO referenced_groups (name) VALUES (?)", "shared-name").Error)
			},
			wantError: "drop conflicting prefill group constraint",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tx := db.Begin()
			require.NoError(t, tx.Error)
			t.Cleanup(func() { _ = tx.Rollback().Error })

			schemaName := fmt.Sprintf("prefill_group_migration_%d", time.Now().UnixNano())
			require.NoError(t, tx.Exec(
				"CREATE SCHEMA ?",
				clause.Table{Name: schemaName},
			).Error)
			require.NoError(t, tx.Exec(
				"SET LOCAL search_path TO ?",
				clause.Table{Name: schemaName},
			).Error)

			require.NoError(t, migratePrefillGroupUniqueness(tx))
			require.NoError(t, tx.AutoMigrate(&PrefillGroup{}))
			original := PrefillGroup{
				Name:        "shared-name",
				Type:        "model",
				Items:       JSONValue(`["gpt-test"]`),
				Description: "preserve me",
			}
			require.NoError(t, tx.Create(&original).Error)
			if test.replacePartialIndex {
				require.NoError(t, tx.Migrator().DropIndex(&PrefillGroup{}, prefillGroupNameIndex))
			}
			if test.withoutDeletedAt {
				require.NoError(t, tx.Migrator().DropColumn(&PrefillGroup{}, "DeletedAt"))
			}
			for _, constraintName := range test.constraints {
				require.NoError(t, tx.Exec(
					"ALTER TABLE ? ADD CONSTRAINT ? UNIQUE (?)",
					clause.Table{Name: "prefill_groups"},
					clause.Column{Name: constraintName},
					clause.Column{Name: "name"},
				).Error)
			}
			for _, indexName := range test.indexes {
				require.NoError(t, tx.Exec(
					"CREATE UNIQUE INDEX ? ON ? (?)",
					clause.Column{Name: indexName},
					clause.Table{Name: "prefill_groups"},
					clause.Column{Name: "name"},
				).Error)
			}
			if test.prepareOld != nil {
				test.prepareOld(t, tx)
			}
			var oldIndexes []indexDefinition
			require.NoError(t, tx.Raw(`
SELECT indexname, indexdef FROM pg_catalog.pg_indexes
WHERE schemaname = current_schema() AND tablename = 'prefill_groups'
ORDER BY indexname`).Scan(&oldIndexes).Error)
			if test.wantError != "" {
				err := migratePrefillGroupUniqueness(tx)
				require.ErrorContains(t, err, test.wantError)
				for _, constraintName := range test.constraints {
					assert.True(t, tx.Migrator().HasConstraint(&PrefillGroup{}, constraintName))
				}
				var restoredIndexes []indexDefinition
				require.NoError(t, tx.Raw(`
SELECT indexname, indexdef FROM pg_catalog.pg_indexes
WHERE schemaname = current_schema() AND tablename = 'prefill_groups'
ORDER BY indexname`).Scan(&restoredIndexes).Error)
				assert.Equal(t, oldIndexes, restoredIndexes)
				assert.Equal(t, !test.withoutDeletedAt, tx.Migrator().HasColumn(&PrefillGroup{}, "DeletedAt"))
				var preserved PrefillGroup
				require.NoError(t, tx.Unscoped().First(&preserved, original.Id).Error)
				assert.Equal(t, original, preserved)
				return
			}

			recorder := &migrationSQLRecorder{}
			migrationDB := tx.Session(&gorm.Session{Logger: recorder})
			for pass := range 2 {
				recorder.reset()
				require.NoError(t, migratePrefillGroupUniqueness(migrationDB))
				require.NoError(t, migrationDB.AutoMigrate(&PrefillGroup{}))
				if pass == 1 {
					assert.Empty(t, recorder.schemaMutations(), "repeated startup must not change the schema")
				}
			}
			for _, oldIndex := range oldIndexes {
				if oldIndex.Name == prefillGroupNameIndex || slices.Contains(test.constraints, oldIndex.Name) || slices.Contains(test.indexes, oldIndex.Name) {
					continue
				}
				var definition string
				require.NoError(t, tx.Raw(`
SELECT indexdef FROM pg_catalog.pg_indexes
WHERE schemaname = current_schema() AND indexname = ?`, oldIndex.Name).Scan(&definition).Error)
				assert.Equal(t, oldIndex.Definition, definition)
			}

			var preserved PrefillGroup
			require.NoError(t, tx.First(&preserved, original.Id).Error)
			assert.Equal(t, original, preserved)

			var targetIndexDefinition string
			require.NoError(t, tx.Raw(`
SELECT indexdef
FROM pg_catalog.pg_indexes
WHERE schemaname = current_schema()
  AND tablename = 'prefill_groups'
  AND indexname = ?`, prefillGroupNameIndex).Scan(&targetIndexDefinition).Error)
			assert.Contains(t, strings.ToLower(targetIndexDefinition), "unique index")
			assert.Contains(t, strings.ToLower(targetIndexDefinition), "where (deleted_at is null)")

			duplicateError := tx.Transaction(func(duplicateTx *gorm.DB) error {
				return duplicateTx.Create(&PrefillGroup{
					Name:  original.Name,
					Type:  "model",
					Items: JSONValue(`[]`),
				}).Error
			})
			require.Error(t, duplicateError)

			require.NoError(t, tx.Delete(&original).Error)
			require.NoError(t, tx.Create(&PrefillGroup{
				Name:  original.Name,
				Type:  "model",
				Items: JSONValue(`[]`),
			}).Error)

			var totalRows int64
			require.NoError(t, tx.Unscoped().Model(&PrefillGroup{}).Count(&totalRows).Error)
			assert.EqualValues(t, 2, totalRows)
		})
	}
}
