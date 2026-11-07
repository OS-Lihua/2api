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

func TestMigrateProviderTableRejectsBothTables(t *testing.T) {
	db := UseTestDatabases(t)
	require.NoError(t, db.AutoMigrate(&legacyProviderRow{}, &Provider{}))

	err := migrateProviderTable(db)
	require.EqualError(t, err, `both "channels" and "providers" tables exist; move the rows into "providers" and drop "channels" before starting`)
}
