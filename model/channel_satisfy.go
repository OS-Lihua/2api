package model

import (
	"github.com/QuantumNous/new-api/setting/ratio_setting"
)

func IsProviderEnabledForGroupModel(group string, modelName string, channelID int) bool {
	if group == "" || modelName == "" || channelID <= 0 {
		return false
	}
	// Historical group/model checks use their explicit database projection;
	// Provider account caching is independent of legacy routing metadata.
	return isChannelEnabledForGroupModelDB(group, modelName, channelID)
}

func IsProviderEnabledForAnyGroupModel(groups []string, modelName string, channelID int) bool {
	if len(groups) == 0 {
		return false
	}
	for _, g := range groups {
		if IsProviderEnabledForGroupModel(g, modelName, channelID) {
			return true
		}
	}
	return false
}

func isChannelEnabledForGroupModelDB(group string, modelName string, channelID int) bool {
	var count int64
	err := DB.Model(&Ability{}).
		Where(`"group" = ? and model = ? and channel_id = ? and enabled = ?`, group, modelName, channelID, true).
		Count(&count).Error
	if err == nil && count > 0 {
		return true
	}
	normalized := ratio_setting.RoutingMatchModelName(modelName)
	if normalized == "" || normalized == modelName {
		return false
	}
	count = 0
	err = DB.Model(&Ability{}).
		Where(`"group" = ? and model = ? and channel_id = ? and enabled = ?`, group, normalized, channelID, true).
		Count(&count).Error
	return err == nil && count > 0
}
