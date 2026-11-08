package model

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupChannelStatusTest(t *testing.T) {
	t.Helper()
	truncateTables(t)
	require.NoError(t, DB.Exec("DELETE FROM abilities").Error)
	require.NoError(t, DB.Exec("DELETE FROM providers").Error)

	memoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		common.MemoryCacheEnabled = memoryCacheEnabled
	})
}

func TestUpdateChannelStatusPersistsMultiKeyState(t *testing.T) {
	setupChannelStatusTest(t)

	channel := Provider{
		Name:   "multi-key-status",
		Key:    "key-a\nkey-b",
		Status: common.ChannelStatusEnabled,
		ProviderInfo: ProviderInfo{
			IsMultiKey:           true,
			MultiKeySize:         2,
			MultiKeyMode:         constant.MultiKeyModePolling,
			MultiKeyPollingIndex: 1,
		},
	}
	require.NoError(t, DB.Create(&channel).Error)

	changed := UpdateProviderStatus(channel.Id, "key-a", common.ChannelStatusAutoDisabled, "provider rejected key")
	require.True(t, changed)

	var stored Provider
	require.NoError(t, DB.First(&stored, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored.ProviderInfo.MultiKeyStatusList[0])
	assert.Equal(t, "provider rejected key", stored.ProviderInfo.MultiKeyDisabledReason[0])
	assert.NotZero(t, stored.ProviderInfo.MultiKeyDisabledTime[0])
	assert.Equal(t, 1, stored.ProviderInfo.MultiKeyPollingIndex)
}

func TestUpdateProviderStatusPreservesConcurrentConfiguration(t *testing.T) {
	setupChannelStatusTest(t)
	clock.UseFake(t, time.Unix(1234, 0))

	channel := Provider{
		Name:         "single-key-status",
		Key:          "original-key",
		Status:       common.ChannelStatusEnabled,
		Models:       "original-model",
		Group:        "default",
		UsedQuota:    100,
		ProviderInfo: ProviderInfo{},
	}
	require.NoError(t, DB.Create(&channel).Error)

	stale, err := GetProviderById(channel.Id, true)
	require.NoError(t, err)

	concurrentChannelInfo := ProviderInfo{
		IsMultiKey:           true,
		MultiKeySize:         2,
		MultiKeyMode:         constant.MultiKeyModePolling,
		MultiKeyPollingIndex: 1,
	}
	require.NoError(t, DB.Model(&Provider{}).Where("id = ?", channel.Id).Updates(map[string]any{
		"key":          "rotated-key\nspare-key",
		"used_quota":   gorm.Expr("used_quota + ?", 250),
		"models":       "concurrent-model",
		"channel_info": concurrentChannelInfo,
	}).Error)

	assert.False(t, UpdateProviderStatus(stale.Id, stale.Key, common.ChannelStatusAutoDisabled, "old failure", 0))
	require.True(t, UpdateProviderStatus(channel.Id, "", common.ChannelStatusManuallyDisabled, "manual operation"))

	var stored Provider
	require.NoError(t, DB.First(&stored, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusManuallyDisabled, stored.Status)
	assert.Equal(t, "rotated-key\nspare-key", stored.Key)
	assert.Equal(t, int64(350), stored.UsedQuota)
	assert.Equal(t, "concurrent-model", stored.Models)
	assert.Equal(t, concurrentChannelInfo, stored.ProviderInfo)

	otherInfo := stored.GetOtherInfo()
	assert.Equal(t, "manual operation", otherInfo["status_reason"])
	assert.Equal(t, float64(1234), otherInfo["status_time"])
}
