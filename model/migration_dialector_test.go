package model

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type MigrationIdentityFields struct {
	ID        int    `gorm:"primaryKey"`
	Name      string `gorm:"size:64;unique"`
	Reference string `gorm:"size:64;uniqueIndex"`
	Provider  string `gorm:"size:32;uniqueIndex:,composite:provider_subject"`
	Subject   string `gorm:"size:64;uniqueIndex:,composite:provider_subject"`
}

type migrationIdentityV1 struct {
	MigrationIdentityFields
	Digest string `gorm:"type:char(32)"`
}

type migrationIdentityV2 struct {
	MigrationIdentityFields
	Digest string `gorm:"type:char(64)"`
	Note   string `gorm:"size:128"`
}

type migrationConstraintV1 struct {
	ID   int    `gorm:"primaryKey"`
	Name string `gorm:"size:64"`
}

type migrationConstraintV2 struct {
	ID   int    `gorm:"primaryKey"`
	Name string `gorm:"size:64;unique"`
}

func TestMigrationSchemaStability(t *testing.T) {
	recorder := &migrationSQLRecorder{}
	db := UseTestDatabases(t).Session(&gorm.Session{Logger: recorder})

	t.Run("identity_and_indexes", func(t *testing.T) {
		const table = "migration_identity_test"
		t.Cleanup(func() { _ = db.Migrator().DropTable(table) })
		require.NoError(t, db.Table(table).AutoMigrate(&migrationIdentityV1{}))
		row := migrationIdentityV1{
			MigrationIdentityFields: MigrationIdentityFields{ID: 1, Name: "root", Reference: "token-reference", Provider: "oidc", Subject: "subject"},
			Digest:                  "old-digest",
		}
		require.NoError(t, db.Table(table).Create(&row).Error)
		recorder.reset()
		require.NoError(t, db.Table(table).AutoMigrate(&migrationIdentityV1{}))
		assert.Empty(t, recorder.schemaMutations())

		require.NoError(t, db.Table(table).AutoMigrate(&migrationIdentityV2{}))
		columns, err := db.Table(table).Migrator().ColumnTypes(&migrationIdentityV2{})
		require.NoError(t, err)
		for _, column := range columns {
			if column.Name() == "digest" {
				length, ok := column.Length()
				require.True(t, ok)
				assert.EqualValues(t, 64, length)
			}
		}
		assert.True(t, db.Table(table).Migrator().HasColumn(&migrationIdentityV2{}, "note"))
		recorder.reset()
		require.NoError(t, db.Table(table).AutoMigrate(&migrationIdentityV2{}))
		assert.Empty(t, recorder.schemaMutations())
		var saved migrationIdentityV2
		require.NoError(t, db.Table(table).First(&saved, 1).Error)
		assert.Equal(t, row.MigrationIdentityFields, saved.MigrationIdentityFields)
		assert.Equal(t, row.Digest+strings.Repeat(" ", 64-len(row.Digest)), saved.Digest)
		for _, duplicate := range []migrationIdentityV2{
			{MigrationIdentityFields: MigrationIdentityFields{Name: "root", Reference: "other-1", Provider: "other", Subject: "1"}},
			{MigrationIdentityFields: MigrationIdentityFields{Name: "other-2", Reference: "token-reference", Provider: "other", Subject: "2"}},
			{MigrationIdentityFields: MigrationIdentityFields{Name: "other-3", Reference: "other-3", Provider: "oidc", Subject: "subject"}},
		} {
			assert.Error(t, db.Table(table).Create(&duplicate).Error)
		}
	})

	t.Run("unique_constraint_changes", func(t *testing.T) {
		const table = "migration_constraint_test"
		t.Cleanup(func() { _ = db.Migrator().DropTable(table) })
		require.NoError(t, db.Table(table).AutoMigrate(&migrationConstraintV1{}))
		require.NoError(t, db.Table(table).Create(&migrationConstraintV1{Name: "existing"}).Error)
		require.NoError(t, db.Table(table).AutoMigrate(&migrationConstraintV2{}))
		assert.Error(t, db.Table(table).Create(&migrationConstraintV2{Name: "existing"}).Error)
		recorder.reset()
		require.NoError(t, db.Table(table).AutoMigrate(&migrationConstraintV2{}))
		assert.Empty(t, recorder.schemaMutations())
		require.NoError(t, db.Table(table).AutoMigrate(&migrationConstraintV1{}))
		require.NoError(t, db.Table(table).Create(&migrationConstraintV1{Name: "existing"}).Error)
	})

	t.Run("renamed_unique_constraints", func(t *testing.T) {
		const table = "migration_renamed_unique"
		t.Cleanup(func() { _ = db.Migrator().DropTable(table) })
		tableDB := db.Table(table).Session(&gorm.Session{})
		require.NoError(t, tableDB.AutoMigrate(&migrationConstraintV1{}))
		original := migrationConstraintV1{Name: "existing"}
		require.NoError(t, tableDB.Create(&original).Error)
		for _, name := range []string{"models_model_name_key", `imported "model" name`} {
			require.NoError(t, db.Exec("ALTER TABLE ? ADD CONSTRAINT ? UNIQUE (name)", clause.Table{Name: table}, clause.Column{Name: name}).Error)
		}
		require.NoError(t, db.Exec("ALTER TABLE ? ADD CONSTRAINT keep_composite UNIQUE (id, name)", clause.Table{Name: table}).Error)
		require.NoError(t, db.Exec("CREATE INDEX keep_name_lookup ON ? (name)", clause.Table{Name: table}).Error)
		// A unique field must keep the old names and still reject duplicates.
		require.NoError(t, tableDB.AutoMigrate(&migrationConstraintV2{}))
		require.Error(t, tableDB.Create(&migrationConstraintV2{Name: "existing"}).Error)
		require.NoError(t, db.Exec("CREATE TABLE migration_unique_reference (name varchar(64) REFERENCES migration_renamed_unique(name))").Error)
		t.Cleanup(func() { _ = db.Migrator().DropTable("migration_unique_reference") })
		require.NoError(t, db.Exec("INSERT INTO migration_unique_reference (name) VALUES (?)", "existing").Error)
		require.Error(t, tableDB.AutoMigrate(&migrationConstraintV1{}), "dependent foreign keys must not be cascaded away")
		for _, name := range []string{"models_model_name_key", `imported "model" name`} {
			assert.True(t, tableDB.Migrator().HasConstraint(&migrationConstraintV1{}, name), "all old constraints must survive rollback")
		}
		var references int64
		require.NoError(t, db.Table("migration_unique_reference").Count(&references).Error)
		assert.EqualValues(t, 1, references)
		require.NoError(t, db.Migrator().DropTable("migration_unique_reference"))
		// Removing column uniqueness must resolve both actual constraint names.
		require.NoError(t, tableDB.AutoMigrate(&migrationConstraintV1{}))
		recorder.reset()
		require.NoError(t, tableDB.AutoMigrate(&migrationConstraintV1{}))
		assert.Empty(t, recorder.schemaMutations())
		require.NoError(t, tableDB.Create(&migrationConstraintV1{Name: "existing"}).Error)
		assert.True(t, tableDB.Migrator().HasConstraint(&migrationConstraintV1{}, "keep_composite"))
		assert.True(t, tableDB.Migrator().HasIndex(&migrationConstraintV1{}, "keep_name_lookup"))
		var rows []migrationConstraintV1
		require.NoError(t, tableDB.Order("id").Find(&rows).Error)
		require.Len(t, rows, 2)
		assert.Equal(t, original, rows[0])
	})
}
