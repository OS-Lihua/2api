package model

import (
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	kitdto "github.com/QuantumNous/new-api/relaykit/dto"
)

var channelsIDM map[int]*Provider // Provider accounts, including disabled accounts
// channel2advancedCustomConfig caches parsed Advanced Custom (type 58) configs so
// path-aware selection avoids re-parsing JSON per request. Refreshed on full sync.
var channel2advancedCustomConfig map[int]*kitdto.AdvancedCustomConfig
var channelSyncLock sync.RWMutex

func InitProviderCache() {
	if !common.MemoryCacheEnabled {
		return
	}
	newChannelId2channel := make(map[int]*Provider)
	newChannel2advancedCustomConfig := make(map[int]*kitdto.AdvancedCustomConfig)
	var channels []*Provider
	DB.Find(&channels)
	for _, channel := range channels {
		newChannelId2channel[channel.Id] = channel
		if channel.Type == constant.ChannelTypeAdvancedCustom {
			if config := channel.GetOtherSettings().AdvancedCustom; config != nil {
				newChannel2advancedCustomConfig[channel.Id] = config
			}
		}
	}
	channelSyncLock.Lock()
	for i, channel := range newChannelId2channel {
		if channel.ProviderInfo.IsMultiKey {
			channel.Keys = channel.GetKeys()
			if channel.ProviderInfo.MultiKeyMode == constant.MultiKeyModePolling {
				if oldChannel, ok := channelsIDM[i]; ok {
					// 存在旧的渠道，如果是多key且轮询，保留轮询索引信息
					if oldChannel.ProviderInfo.IsMultiKey && oldChannel.ProviderInfo.MultiKeyMode == constant.MultiKeyModePolling {
						channel.ProviderInfo.MultiKeyPollingIndex = oldChannel.ProviderInfo.MultiKeyPollingIndex
					}
				}
			}
		}
	}
	channelsIDM = newChannelId2channel
	channel2advancedCustomConfig = newChannel2advancedCustomConfig
	channelSyncLock.Unlock()
	common.SysLog(common.LogText("channels synced from database"))
}

func SyncProviderCache(frequency int) {
	for {
		time.Sleep(time.Duration(frequency) * time.Second)
		common.SysLog(common.LogText("syncing channels from database"))
		InitProviderCache()
	}
}

func GetRandomSatisfiedProvider(
	group string,
	model string,
	retry int,
	filters []dto.ProviderFilter,
) (*Provider, error) {
	// Explicit historical helper only. Offering routes do not use this legacy
	// projection, and account-cache initialization must not build it.
	return GetProvider(group, model, retry, filters)
}

func CacheGetProvider(id int) (*Provider, error) {
	if !common.MemoryCacheEnabled {
		return GetProviderById(id, true)
	}
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()

	c, ok := channelsIDM[id]
	if !ok {
		return nil, fmt.Errorf("channel #%d no longer exists", id)
	}
	return c, nil
}

func CacheGetProviderInfo(id int) (*ProviderInfo, error) {
	if !common.MemoryCacheEnabled {
		channel, err := GetProviderById(id, true)
		if err != nil {
			return nil, err
		}
		return &channel.ProviderInfo, nil
	}
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()

	c, ok := channelsIDM[id]
	if !ok {
		return nil, fmt.Errorf("channel #%d no longer exists", id)
	}
	return &c.ProviderInfo, nil
}

func CacheUpdateProviderStatus(id int, status int) {
	if !common.MemoryCacheEnabled {
		return
	}
	channelSyncLock.Lock()
	defer channelSyncLock.Unlock()
	cacheUpdateProviderStatus(id, status)
}

// cacheProviderStatusTransition publishes committed state. If this node's
// snapshot differs, read the current Provider rather than guessing whether the
// cache is older or newer. A refresh failure evicts only this Provider.
func cacheProviderStatusTransition(before, after *Provider) {
	channelSyncLock.Lock()
	defer channelSyncLock.Unlock()
	cached, ok := channelsIDM[after.Id]
	var updated Provider
	if !ok || cached.Key != before.Key || cached.Status != before.Status || cached.OtherInfo != before.OtherInfo ||
		!maps.Equal(cached.ProviderInfo.MultiKeyStatusList, before.ProviderInfo.MultiKeyStatusList) {
		current, err := GetProviderById(after.Id, true)
		if err != nil {
			common.SysError(common.LogText("failed to refresh provider status cache: provider_id=%d, error=%v", after.Id, err))
			delete(channelsIDM, after.Id)
			delete(channel2advancedCustomConfig, after.Id)
			return
		}
		updated = *current
		if ok && cached.Key == current.Key && cached.ProviderInfo.MultiKeyMode == current.ProviderInfo.MultiKeyMode {
			updated.ProviderInfo.MultiKeyPollingIndex = cached.ProviderInfo.MultiKeyPollingIndex
		}
	} else {
		updated = *cached
		updated.Status = after.Status
		updated.OtherInfo = after.OtherInfo
		updated.ProviderInfo.MultiKeyStatusList = after.ProviderInfo.MultiKeyStatusList
		updated.ProviderInfo.MultiKeyDisabledReason = after.ProviderInfo.MultiKeyDisabledReason
		updated.ProviderInfo.MultiKeyDisabledTime = after.ProviderInfo.MultiKeyDisabledTime
	}
	if channelsIDM == nil {
		channelsIDM = make(map[int]*Provider)
	}
	channelsIDM[after.Id] = &updated
	if channel2advancedCustomConfig == nil {
		channel2advancedCustomConfig = make(map[int]*kitdto.AdvancedCustomConfig)
	}
	delete(channel2advancedCustomConfig, after.Id)
	if updated.Type == constant.ChannelTypeAdvancedCustom {
		if config := updated.GetOtherSettings().AdvancedCustom; config != nil {
			channel2advancedCustomConfig[after.Id] = config
		}
	}
	cacheUpdateProviderStatus(after.Id, updated.Status)
}

// Caller holds channelSyncLock.
func cacheUpdateProviderStatus(id int, status int) {
	channel, ok := channelsIDM[id]
	if ok {
		channel.Status = status
	}
}

func CacheUpdateProvider(channel *Provider) {
	if !common.MemoryCacheEnabled {
		return
	}
	channelSyncLock.Lock()
	if channel == nil {
		channelSyncLock.Unlock()
		return
	}

	if channelsIDM == nil {
		channelsIDM = make(map[int]*Provider)
	}
	if oldChannel, ok := channelsIDM[channel.Id]; ok {
		logger.LogDebug(nil, "CacheUpdateProvider before: id=%d, name=%s, status=%d, polling_index=%d", channel.Id, channel.Name, channel.Status, oldChannel.ProviderInfo.MultiKeyPollingIndex)
	}
	channelsIDM[channel.Id] = channel
	if channel2advancedCustomConfig == nil {
		channel2advancedCustomConfig = make(map[int]*kitdto.AdvancedCustomConfig)
	}
	delete(channel2advancedCustomConfig, channel.Id)
	if channel.Type == constant.ChannelTypeAdvancedCustom {
		if config := channel.GetOtherSettings().AdvancedCustom; config != nil {
			channel2advancedCustomConfig[channel.Id] = config
		}
	}
	logger.LogDebug(nil, "CacheUpdateProvider after: id=%d, name=%s, status=%d, polling_index=%d", channel.Id, channel.Name, channel.Status, channel.ProviderInfo.MultiKeyPollingIndex)
	channelSyncLock.Unlock()
}
