package controller

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
)

var channelId2Models map[int][]string

func init() {
	channelId2Models = make(map[int][]string)
	for i := 1; i <= constant.ChannelTypeDummy; i++ {
		apiType, success := common.ChannelType2APIType(i)
		if !success || apiType == constant.APITypeAIProxyLibrary {
			if plugin, ok := jsplugin.DefaultRegistry.GetByChannelType(i); ok {
				channelId2Models[i] = append([]string(nil), plugin.Meta.Models...)
			}
			continue
		}
		meta := &relaycommon.RelayInfo{ProviderMeta: &relaycommon.ProviderMeta{
			ChannelType: i,
		}}
		adaptor := relay.GetAdaptor(apiType)
		adaptor.Init(meta)
		channelId2Models[i] = adaptor.GetModelList()
		if len(channelId2Models[i]) == 0 {
			if plugin, ok := jsplugin.DefaultRegistry.GetByChannelType(i); ok {
				channelId2Models[i] = append([]string(nil), plugin.Meta.Models...)
			}
		}
	}
}

func DashboardListModels(c *gin.Context) {
	modelsByChannel := make(map[int][]string, len(channelId2Models))
	for channelType, models := range channelId2Models {
		modelsByChannel[channelType] = append([]string(nil), models...)
	}
	for channelType := 1; channelType <= constant.ChannelTypeDummy; channelType++ {
		if plugin, ok := jsplugin.DefaultRegistry.GetByChannelType(channelType); ok {
			modelsByChannel[channelType] = append([]string(nil), plugin.Meta.Models...)
		}
	}
	c.JSON(200, gin.H{
		"success": true,
		"data":    modelsByChannel,
	})
}
