package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
)

func seedFlowQuotaData(t *testing.T, quotaData QuotaData) {
	t.Helper()
	require.NoError(t, DB.Create(&quotaData).Error)
}

func seedFlowLookupData(t *testing.T) {
	t.Helper()
	require.NoError(t, DB.Create(&Provider{Id: 1, Name: "east"}).Error)
	require.NoError(t, DB.Create(&Provider{Id: 2, Name: "west"}).Error)
	require.NoError(t, DB.Create(&Token{Id: 11, UserId: 1, Key: "sk-primary", Name: "primary"}).Error)
	require.NoError(t, DB.Create(&Token{Id: 22, UserId: 2, Key: "sk-backup", Name: "backup"}).Error)
	require.NoError(t, DB.Delete(&Token{Id: 11}).Error)
}

func TestGetFlowQuotaDataUsesQuotaDataRoleSpecificDimensions(t *testing.T) {
	truncateTables(t)
	seedFlowLookupData(t)

	seedFlowQuotaData(t, QuotaData{
		UserID:    1,
		Username:  "alice",
		NodeName:  "node-a",
		TokenID:   11,
		UseGroup:  "vip",
		ModelName: "gpt-a",
		ChannelID: 1,
		CreatedAt: 1000,
		Count:     2,
		Quota:     100,
		TokenUsed: 40,
	})
	seedFlowQuotaData(t, QuotaData{
		UserID:    1,
		Username:  "alice",
		NodeName:  "node-a",
		TokenID:   11,
		UseGroup:  "vip",
		ModelName: "gpt-a",
		ChannelID: 1,
		CreatedAt: 1100,
		Count:     1,
		Quota:     50,
		TokenUsed: 20,
	})
	seedFlowQuotaData(t, QuotaData{
		UserID:    1,
		Username:  "alice",
		NodeName:  "node-a",
		TokenID:   11,
		UseGroup:  "vip",
		ModelName: "gpt-a",
		ChannelID: 2,
		CreatedAt: 1200,
		Count:     1,
		Quota:     25,
		TokenUsed: 10,
	})
	seedFlowQuotaData(t, QuotaData{
		UserID:    2,
		Username:  "bob",
		NodeName:  "node-b",
		TokenID:   22,
		UseGroup:  "default",
		ModelName: "gpt-b",
		ChannelID: 1,
		CreatedAt: 1300,
		Count:     3,
		Quota:     70,
		TokenUsed: 30,
	})
	seedFlowQuotaData(t, QuotaData{
		UserID:    1,
		Username:  "alice",
		ModelName: "legacy",
		CreatedAt: 1400,
		Count:     99,
		Quota:     999,
		TokenUsed: 999,
	})

	rootRows, err := GetFlowQuotaData(900, 2000, "", 0, common.RoleRootUser)
	require.NoError(t, err)
	require.Len(t, rootRows, 3)
	// Token 11 was soft-deleted, so its name is intentionally left empty for the
	// frontend to render a localized "deleted (id)" label instead.
	require.Equal(t, FlowQuotaData{
		UserID:      1,
		Username:    "alice",
		NodeName:    "node-a",
		TokenID:     11,
		TokenName:   "",
		UseGroup:    "vip",
		ChannelID:   1,
		ChannelName: "east",
		ModelName:   "gpt-a",
		TokenUsed:   60,
		Count:       3,
		Quota:       150,
	}, *rootRows[0])
	// A token that still exists resolves to its current name.
	require.Equal(t, 22, rootRows[1].TokenID)
	require.Equal(t, "backup", rootRows[1].TokenName)

	adminRows, err := GetFlowQuotaData(900, 2000, "alice", 0, common.RoleAdminUser)
	require.NoError(t, err)
	require.Len(t, adminRows, 2)
	require.Equal(t, 0, adminRows[0].TokenID)
	require.Empty(t, adminRows[0].TokenName)
	require.Empty(t, adminRows[0].NodeName)
	require.Equal(t, "alice", adminRows[0].Username)
	require.Equal(t, "vip", adminRows[0].UseGroup)
	require.Equal(t, "east", adminRows[0].ChannelName)
	require.Equal(t, 150, adminRows[0].Quota)

	selfRows, err := GetFlowQuotaData(900, 2000, "", 1, common.RoleCommonUser)
	require.NoError(t, err)
	require.Len(t, selfRows, 1)
	require.Empty(t, selfRows[0].Username)
	require.Equal(t, 0, selfRows[0].ChannelID)
	require.Empty(t, selfRows[0].ChannelName)
	require.Empty(t, selfRows[0].TokenName)
	require.Equal(t, "vip", selfRows[0].UseGroup)
	require.Equal(t, 175, selfRows[0].Quota)
}

func TestHistoricalQuotaDataPreservesDimensionsAndCacheTotals(t *testing.T) {
	truncateTables(t)
	seedFlowLookupData(t)
	rows := []QuotaData{
		{UserID: 1, Username: "alice", ModelName: "gpt-a", CreatedAt: 3600, UseGroup: "vip", TokenID: 11, ChannelID: 1, NodeName: "node-a", Quota: 150, TokenUsed: 60, Count: 2, CacheReadTokens: 12, CacheCreationTokens: 30},
		{UserID: 1, Username: "alice", ModelName: "gpt-a", CreatedAt: 3600, UseGroup: "default", TokenID: 11, ChannelID: 1, NodeName: "node-a", Quota: 25, TokenUsed: 10, Count: 1, CacheReadTokens: 2, CacheCreationTokens: 5},
		{UserID: 1, Username: "alice", ModelName: "gpt-a", CreatedAt: 3600, UseGroup: "vip", TokenID: 12, ChannelID: 1, NodeName: "node-a", Quota: 10, TokenUsed: 1, Count: 1, CacheCreationTokens: 1},
		{UserID: 1, Username: "alice", ModelName: "gpt-a", CreatedAt: 3600, UseGroup: "vip", TokenID: 11, ChannelID: 2, NodeName: "node-a", Quota: 11, TokenUsed: 2, Count: 1, CacheCreationTokens: 2},
		{UserID: 1, Username: "alice", ModelName: "gpt-a", CreatedAt: 3600, UseGroup: "vip", TokenID: 11, ChannelID: 1, NodeName: "node-b", Quota: 12, TokenUsed: 3, Count: 1, CacheCreationTokens: 3},
	}
	require.NoError(t, DB.Create(&rows).Error)
	flow, err := GetFlowQuotaData(3600, 3600, "alice", 0, common.RoleRootUser)
	require.NoError(t, err)
	require.Len(t, flow, len(rows))
	for _, source := range rows {
		matched := false
		for _, view := range flow {
			if view.UseGroup == source.UseGroup && view.TokenID == source.TokenID && view.ChannelID == source.ChannelID && view.NodeName == source.NodeName {
				matched = true
				require.Equal(t, source.Quota, view.Quota)
				require.Equal(t, source.TokenUsed, view.TokenUsed)
				require.Equal(t, source.Count, view.Count)
			}
		}
		require.True(t, matched, "historical group/token/provider/node dimensions must not be collapsed")
	}
	for _, read := range []func() ([]*QuotaData, error){
		func() ([]*QuotaData, error) { return GetQuotaDataByUserId(1, 3600, 3600) },
		func() ([]*QuotaData, error) { return GetQuotaDataByUsername("alice", 3600, 3600) },
		func() ([]*QuotaData, error) { return GetAllQuotaDates(3600, 3600, "") },
		func() ([]*QuotaData, error) { return GetQuotaDataGroupByUser(3600, 3600) },
	} {
		history, err := read()
		require.NoError(t, err)
		require.Len(t, history, 1)
		require.Equal(t, 208, history[0].Quota)
		require.Equal(t, 76, history[0].TokenUsed)
		require.Equal(t, 6, history[0].Count)
		require.EqualValues(t, 14, history[0].CacheReadTokens)
		require.EqualValues(t, 41, history[0].CacheCreationTokens)
	}
	none, err := GetQuotaDataByUserId(1, 3601, 7199)
	require.NoError(t, err)
	require.Empty(t, none)
	var retained []QuotaData
	require.NoError(t, DB.Order("id").Find(&retained).Error)
	require.Equal(t, rows, retained, "all historical views are read-only")
}
