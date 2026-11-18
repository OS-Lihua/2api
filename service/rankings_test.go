package service

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestRankingsUseHistoricalTokensAndIndependentPublisherMetadata(t *testing.T) {
	database := model.UseTestDatabases(t)
	require.NoError(t, database.AutoMigrate(&model.QuotaData{}, &model.Model{}, &model.Vendor{}))

	publisher := model.Vendor{Name: "Recorded publisher", Icon: "RecordedIcon"}
	prefixPublisher := model.Vendor{Name: "Prefix publisher", Icon: "PrefixIcon"}
	deletedPublisher := model.Vendor{Name: "Deleted publisher", Icon: "DeletedIcon"}
	for _, item := range []*model.Vendor{&publisher, &prefixPublisher, &deletedPublisher} {
		require.NoError(t, database.Create(item).Error)
	}
	metadata := []model.Model{
		{ModelName: "rank-exact", VendorID: publisher.Id, NameRule: model.NameRuleExact},
		{ModelName: "rank-", VendorID: prefixPublisher.Id, NameRule: model.NameRulePrefix},
		{ModelName: "retired-model", VendorID: publisher.Id},
		{ModelName: "deleted-model", VendorID: publisher.Id},
		{ModelName: "orphan-model", VendorID: deletedPublisher.Id},
		{ModelName: "metadata-only-private-name", VendorID: publisher.Id},
	}
	require.NoError(t, database.Create(&metadata).Error)
	require.NoError(t, database.Model(&metadata[2]).Update("status", 0).Error)
	require.NoError(t, database.Delete(&metadata[3]).Error)
	require.NoError(t, database.Delete(&deletedPublisher).Error)

	now := time.Now().UTC()
	current := now.Add(-time.Hour).Unix()
	previous := now.Add(-8 * 24 * time.Hour).Unix()
	rows := []model.QuotaData{
		{ModelName: "rank-exact", CreatedAt: current, TokenUsed: 150, Quota: 999999},
		{ModelName: "rank-exact", CreatedAt: current, TokenUsed: 50, Quota: 1},
		{ModelName: "rank-prefix", CreatedAt: current, TokenUsed: 100},
		{ModelName: "gpt-historical", CreatedAt: current, TokenUsed: 80},
		{ModelName: "retired-model", CreatedAt: current, TokenUsed: 40},
		{ModelName: "deleted-model", CreatedAt: current, TokenUsed: 30},
		{ModelName: "orphan-model", CreatedAt: current, TokenUsed: 20},
		{ModelName: "unknown-model", CreatedAt: current, TokenUsed: 10},
		{ModelName: "rank-exact", CreatedAt: previous, TokenUsed: 100},
		{ModelName: "rank-previous-only", CreatedAt: previous, TokenUsed: 80},
	}
	require.NoError(t, database.Create(&rows).Error)

	// These historical token rows remain readable after the old routing and
	// pricing tables are removed. Reading labels must not recreate either.
	var forbiddenReads []string
	require.NoError(t, database.Callback().Query().Before("gorm:query").Register("rankings:independent_metadata", func(tx *gorm.DB) {
		switch tx.Statement.Table {
		case "quota_data", "models", "vendors":
		default:
			forbiddenReads = append(forbiddenReads, tx.Statement.Table)
			tx.AddError(errors.New("rankings must not read routing or pricing"))
		}
	}))
	t.Cleanup(func() { require.NoError(t, database.Callback().Query().Remove("rankings:independent_metadata")) })
	require.NoError(t, database.Callback().Row().Before("gorm:row").Register("rankings:no_retired_sql", func(tx *gorm.DB) {
		statement := strings.ToLower(tx.Statement.SQL.String())
		for _, table := range []string{"abilities", "providers", "options", "price_versions"} {
			if strings.Contains(statement, table) {
				forbiddenReads = append(forbiddenReads, table)
				tx.AddError(errors.New("rankings must not read routing or pricing"))
				return
			}
		}
	}))
	t.Cleanup(func() { require.NoError(t, database.Callback().Row().Remove("rankings:no_retired_sql")) })

	config, err := rankingConfig("week")
	require.NoError(t, err)
	result, err := buildRankingsSnapshot(config, now)
	require.NoError(t, err)
	require.Len(t, result.Models, 7)
	assert.Equal(t, RankedModel{
		Rank: 1, PreviousRank: new(1), ModelName: "rank-exact", Vendor: "Recorded publisher", VendorIcon: "RecordedIcon",
		Category: "all", TotalTokens: 200, Share: 0.4167, GrowthPct: 100,
	}, result.Models[0])
	assert.Equal(t, "Prefix publisher", result.Models[1].Vendor)
	assert.Equal(t, "OpenAI", result.Models[2].Vendor)
	assert.Equal(t, "OpenAI", result.Models[2].VendorIcon)
	for _, row := range result.Models[3:] {
		assert.Equal(t, "Unknown", row.Vendor, row.ModelName)
		assert.Empty(t, row.VendorIcon, row.ModelName)
	}
	assert.Empty(t, forbiddenReads)
	require.Len(t, result.ModelsHistory.Points, 7)
	for _, row := range result.ModelsHistory.Points {
		if row.Model == "rank-exact" {
			assert.Equal(t, int64(200), row.Tokens)
			assert.Equal(t, "Recorded publisher", row.Vendor)
		}
		assert.NotEqual(t, "metadata-only-private-name", row.Model)
	}
	prefixResult := -1
	for i, row := range result.Vendors {
		if row.Vendor == "Prefix publisher" {
			prefixResult = i
		}
	}
	require.NotEqual(t, -1, prefixResult)
	assert.Equal(t, int64(100), result.Vendors[prefixResult].TotalTokens)
	assert.Equal(t, float64(25), result.Vendors[prefixResult].GrowthPct, "previous-only model contributes to its recorded publisher")
	var vendorCount, modelCount, historyCount int64
	require.NoError(t, database.Unscoped().Model(&model.Vendor{}).Count(&vendorCount).Error)
	require.NoError(t, database.Unscoped().Model(&model.Model{}).Count(&modelCount).Error)
	require.NoError(t, database.Model(&model.QuotaData{}).Count(&historyCount).Error)
	assert.Equal(t, int64(3), vendorCount, "built-in display publishers must not create database records")
	assert.Equal(t, int64(6), modelCount)
	assert.Equal(t, int64(len(rows)), historyCount)

	rankingCacheMu.Lock()
	previousCache := rankingCache
	rankingCacheMu.Unlock()
	t.Cleanup(func() {
		rankingCacheMu.Lock()
		rankingCache = previousCache
		rankingCacheMu.Unlock()
	})
	for _, table := range []string{"models", "vendors"} {
		t.Run(table+" read failure is retryable", func(t *testing.T) {
			rankingCacheMu.Lock()
			rankingCache = map[string]rankingCacheItem{}
			rankingCacheMu.Unlock()
			readFailure := errors.New("metadata read unavailable")
			require.NoError(t, database.Callback().Query().Before("gorm:query").Register("rankings:metadata_failure", func(tx *gorm.DB) {
				if tx.Statement.Table == table {
					tx.AddError(readFailure)
				}
			}))
			failed, err := GetRankingsSnapshot("week")
			assert.ErrorIs(t, err, readFailure)
			assert.Nil(t, failed, "a metadata failure must not be returned or cached as Unknown labels")
			require.NoError(t, database.Callback().Query().Remove("rankings:metadata_failure"))
			retried, err := GetRankingsSnapshot("week")
			require.NoError(t, err)
			require.Len(t, retried.Models, 7)
			assert.Equal(t, "Recorded publisher", retried.Models[0].Vendor)
		})
	}
}
