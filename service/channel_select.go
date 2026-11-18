package service

import (
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"

	"github.com/QuantumNous/new-api/pkg/jsplugin"

	"github.com/gin-gonic/gin"
)

func GetChannelConstraints(c *gin.Context) *dto.ProviderConstraints {
	if c == nil {
		return &dto.ProviderConstraints{}
	}
	if existing, ok := common.GetContextKeyType[*dto.ProviderConstraints](c, constant.ContextKeyChannelConstraints); ok && existing != nil {
		return existing
	}
	constraints := &dto.ProviderConstraints{}
	common.SetContextKey(c, constant.ContextKeyChannelConstraints, constraints)
	return constraints
}

func AppendTaskPluginIdentityFilter(c *gin.Context, pluginKey string) {
	if c == nil {
		return
	}
	channelTypes, pluginKeys := pinnedTaskPluginIdentities(c, pluginKey)
	GetChannelConstraints(c).AddFilter(dto.ProviderFilter{
		Kind:                   dto.FilterTaskPluginIdentity,
		TaskPluginKey:          pluginKey,
		TaskPluginChannelTypes: channelTypes,
		TaskPluginKeys:         pluginKeys,
	})
}

func pinnedTaskPluginIdentities(c *gin.Context, expected string) ([]int, []string) {
	if c == nil || expected == "" {
		return nil, nil
	}
	// A shared host endpoint and a native route several plugins declare both
	// admit a channel of any candidate.
	var generation *jsplugin.RoutingGeneration
	var candidates []*jsplugin.LoadedPlugin
	if value, exists := c.Get(jsplugin.ContextKeyPinnedEndpoint); exists {
		if pinned, ok := value.(jsplugin.PinnedEndpoint); ok && len(pinned.Candidates) > 1 {
			generation = pinned.Generation
			for _, candidate := range pinned.Candidates {
				candidates = append(candidates, candidate.Plugin)
			}
		}
	} else if value, exists := c.Get(jsplugin.ContextKeyPinnedRoute); exists {
		if pinned, ok := value.(jsplugin.PinnedRoute); ok && len(pinned.Candidates) > 1 {
			generation = pinned.Generation
			for _, candidate := range pinned.Candidates {
				candidates = append(candidates, candidate.Plugin)
			}
		}
	}
	if generation != nil {
		expectedFound := false
		channelTypes := make([]int, 0, len(candidates))
		pluginKeys := make([]string, 0, len(candidates))
		seen := make(map[int]struct{}, len(candidates))
		for _, candidate := range candidates {
			if candidate == nil {
				continue
			}
			if candidate.Meta.Key == expected {
				expectedFound = true
			}
			pluginKeys = append(pluginKeys, candidate.Meta.Key)
			for _, channelType := range candidate.Meta.ChannelTypes {
				if channelType == 0 || channelType == constant.ChannelTypeTaskPlugin {
					continue
				}
				if _, duplicate := seen[channelType]; duplicate {
					continue
				}
				if plugin, indexed := generation.GetByChannelType(channelType); indexed && plugin == candidate {
					seen[channelType] = struct{}{}
					channelTypes = append(channelTypes, channelType)
				}
			}
		}
		if expectedFound {
			return channelTypes, pluginKeys
		}
	}
	value, exists := c.Get(jsplugin.ContextKeyPinnedPlugin)
	pinned, ok := value.(jsplugin.PinnedPlugin)
	if !exists || !ok || pinned.Generation == nil || pinned.Plugin == nil || pinned.Plugin.Meta.Key != expected {
		return nil, nil
	}
	channelTypes := make([]int, 0, len(pinned.Plugin.Meta.ChannelTypes))
	for _, channelType := range pinned.Plugin.Meta.ChannelTypes {
		if channelType == 0 || channelType == constant.ChannelTypeTaskPlugin {
			continue
		}
		channelTypes = append(channelTypes, channelType)
	}
	return channelTypes, []string{expected}
}

// AppendUsedChannel records an attempted channel in the request's channel
// trail, which the retry log and the consume log's admin_info both read.
func AppendUsedChannel(c *gin.Context, channelID int) {
	c.Set("use_channel", append(c.GetStringSlice("use_channel"), fmt.Sprintf("%d", channelID)))
}
