package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// legacyProviderRow creates the table that releases before the Provider rename
// used: the same columns and GORM indexes, stored as "channels".
type legacyProviderRow struct {
	Provider
}

func (legacyProviderRow) TableName() string { return legacyProviderTable }

type providerSchemaObject struct {
	Name       string `gorm:"column:name"`
	Definition string `gorm:"column:definition"`
}

func providerSchemaObjects(t *testing.T, db *gorm.DB) []providerSchemaObject {
	t.Helper()
	var objects []providerSchemaObject
	require.NoError(t, db.Raw(`
SELECT indexname AS name, replace(indexdef, current_schema() || '.', '') AS definition
FROM pg_catalog.pg_indexes
WHERE schemaname = current_schema() AND tablename = ?
UNION ALL
SELECT conname AS name, pg_get_constraintdef(oid) AS definition
FROM pg_catalog.pg_constraint
WHERE conrelid = to_regclass(?)
ORDER BY name`, providerTable, providerTable).Scan(&objects).Error)
	return objects
}

func migrateProviderSchema(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, migrateProviderTable(db))
	require.NoError(t, db.AutoMigrate(&Provider{}))
}

func TestMigrateProviderTable(t *testing.T) {
	freshDB := UseTestDatabases(t)
	migrateProviderSchema(t, freshDB)
	freshObjects := providerSchemaObjects(t, freshDB)
	require.NotEmpty(t, freshObjects)

	db := UseTestDatabases(t)
	require.NoError(t, db.AutoMigrate(&legacyProviderRow{}))
	tag := "team-a"
	legacy := legacyProviderRow{Provider: Provider{
		Type:   1,
		Key:    "sk-legacy-1\nsk-legacy-2",
		Name:   "legacy account",
		Status: common.ChannelStatusEnabled,
		Tag:    &tag,
		ProviderInfo: ProviderInfo{
			IsMultiKey:         true,
			MultiKeySize:       2,
			MultiKeyStatusList: map[int]int{1: common.ChannelStatusAutoDisabled},
		},
	}}
	require.NoError(t, db.Create(&legacy).Error)

	for range 2 {
		migrateProviderSchema(t, db)
	}

	var legacyTable *string
	require.NoError(t, db.Raw("SELECT to_regclass(?)::text", legacyProviderTable).Scan(&legacyTable).Error)
	assert.Nil(t, legacyTable)

	var leftovers []string
	require.NoError(t, db.Raw(`
SELECT relname FROM pg_catalog.pg_class
WHERE relnamespace = current_schema()::regnamespace AND relname LIKE '%channels%'
UNION ALL
SELECT conname FROM pg_catalog.pg_constraint
WHERE connamespace = current_schema()::regnamespace AND conname LIKE '%channels%'`).Scan(&leftovers).Error)
	assert.Empty(t, leftovers)
	assert.Equal(t, freshObjects, providerSchemaObjects(t, db))

	var migrated Provider
	require.NoError(t, db.First(&migrated, legacy.Id).Error)
	assert.Equal(t, "legacy account", migrated.Name)
	assert.Equal(t, "sk-legacy-1\nsk-legacy-2", migrated.Key)
	assert.Equal(t, &tag, migrated.Tag)
	assert.Equal(t, map[int]int{1: common.ChannelStatusAutoDisabled}, migrated.ProviderInfo.MultiKeyStatusList)

	next := Provider{Type: 1, Key: "sk-next", Name: "next account"}
	require.NoError(t, db.Create(&next).Error)
	assert.Greater(t, next.Id, legacy.Id)
}

func TestMigrateProviderTableKeepsSingleModelChannels(t *testing.T) {
	db := UseTestDatabases(t)
	require.NoError(t, db.AutoMigrate(&Provider{}))
	provider := Provider{Type: 1, Key: "test-key", Name: "existing provider"}
	require.NoError(t, db.Create(&provider).Error)
	// The single-model Channel introduced after the Provider rename reuses
	// the channels table name. Startup must keep both tables and their rows.
	type singleModelChannel struct {
		Id         int `gorm:"primaryKey"`
		ProviderId int
		ModelId    int
	}
	require.NoError(t, db.Table("channels").AutoMigrate(&singleModelChannel{}))
	channel := singleModelChannel{ProviderId: provider.Id, ModelId: 7}
	require.NoError(t, db.Table("channels").Create(&channel).Error)

	for range 2 {
		migrateProviderSchema(t, db)
	}

	var keptProvider Provider
	require.NoError(t, db.First(&keptProvider, provider.Id).Error)
	assert.Equal(t, provider.Name, keptProvider.Name)
	var keptChannel singleModelChannel
	require.NoError(t, db.Table("channels").First(&keptChannel, channel.Id).Error)
	assert.Equal(t, channel, keptChannel)
}

func TestRetireLegacyProviderRoutingColumns(t *testing.T) {
	db := UseTestDatabases(t)
	require.NoError(t, db.AutoMigrate(&Provider{}))
	provider := Provider{Type: 1, Key: "provider-retirement-fixture", Name: "existing provider"}
	require.NoError(t, db.Create(&provider).Error)
	require.NoError(t, db.Exec(`ALTER TABLE "providers"
		ADD COLUMN IF NOT EXISTS "models" text, ADD COLUMN IF NOT EXISTS "model_mapping" text,
		ADD COLUMN IF NOT EXISTS "group" text, ADD COLUMN IF NOT EXISTS "priority" bigint,
		ADD COLUMN IF NOT EXISTS "weight" bigint`).Error)
	require.NoError(t, db.Exec(`UPDATE "providers" SET "models" = ?, "priority" = ?, "weight" = ? WHERE "id" = ?`, "source-model", 2, 3, provider.Id).Error)
	require.NoError(t, db.Exec(`CREATE TABLE "channels" ("provider_id" bigint REFERENCES "providers"("id"), "weight" bigint NOT NULL)`).Error)
	require.NoError(t, db.Exec(`INSERT INTO "channels" ("provider_id", "weight") VALUES (?, ?)`, provider.Id, 17).Error)
	require.NoError(t, db.Exec(`CREATE VIEW "legacy_provider_weight_view" AS SELECT "weight" FROM "providers"`).Error)

	retire := func(want int64) error {
		return db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec(`SELECT pg_advisory_xact_lock(510050)`).Error; err != nil {
				return err
			}
			count, err := retireLegacyProviderRoutingTx(tx)
			if err == nil {
				assert.Equal(t, want, count)
			}
			return err
		})
	}
	require.ErrorContains(t, retire(0), "SQLSTATE 2BP01")
	columns, err := inspectLegacyProviderRoutingColumns(db)
	require.NoError(t, err)
	assert.EqualValues(t, 5, columns)
	var sourceModel string
	require.NoError(t, db.Model(&Provider{}).Where("id = ?", provider.Id).Pluck("models", &sourceModel).Error)
	assert.Equal(t, "source-model", sourceModel)
	require.NoError(t, db.Exec(`DROP VIEW "legacy_provider_weight_view"`).Error)

	require.NoError(t, retire(5))
	require.NoError(t, retire(0))
	columns, err = inspectLegacyProviderRoutingColumns(db)
	require.NoError(t, err)
	assert.Zero(t, columns)
	var saved Provider
	require.NoError(t, db.First(&saved, provider.Id).Error)
	assert.Equal(t, provider.Name, saved.Name)
	assert.Equal(t, provider.Key, saved.Key)
	var weight int64
	require.NoError(t, db.Table("channels").Where("provider_id = ?", provider.Id).Pluck("weight", &weight).Error)
	assert.EqualValues(t, 17, weight)
}

func TestMigrateProviderTableRejectsBothTables(t *testing.T) {
	db := UseTestDatabases(t)
	require.NoError(t, db.AutoMigrate(&legacyProviderRow{}, &Provider{}))

	err := migrateProviderTable(db)
	require.EqualError(t, err, `both "channels" and "providers" tables exist; move the rows into "providers" and drop "channels" before starting`)
}
