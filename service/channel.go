package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/operation_setting"
)

func formatNotifyType(channelId int, status int) string {
	return fmt.Sprintf("%s_%d_%d", dto.NotifyTypeChannelUpdate, channelId, status)
}

func shouldCloseActiveWebSocketsAfterDisable(channelId int) bool {
	channel, err := model.GetProviderById(channelId, true)
	if err != nil {
		common.SysLog(common.LogText("failed to check channel status before closing active websockets: channel_id=%d, error=%v", channelId, err))
		return true
	}
	return channel.Status != common.ChannelStatusEnabled
}

// disable & notify
func DisableChannel(channelError types.ProviderError, reason string, selectedIndex ...int) bool {
	return disableProvider(channelError, reason, false, false, selectedIndex...)
}

// DisableProvider handles account-wide failures while retaining the credential
// that produced the failure as a precondition of the transition.
func DisableProvider(channelError types.ProviderError, reason string, selectedIndex ...int) bool {
	return disableProvider(channelError, reason, true, false, selectedIndex...)
}

var gatewayDisableNoticeSlots = make(chan struct{}, 64)

// DisableGatewayProvider commits the status transition before returning, while
// delivering its best-effort notice independently of request failover.
func DisableGatewayProvider(channelError types.ProviderError, reason string, providerWide bool, selectedIndex ...int) bool {
	return disableProvider(channelError, reason, providerWide, true, selectedIndex...)
}

func disableProvider(channelError types.ProviderError, reason string, providerWide, asyncNotice bool, selectedIndex ...int) bool {
	common.SysLog(common.LogText("channel %q (#%d) failed, disabling it, reason: %s", channelError.ChannelName, channelError.ChannelId, common.LocalLogPreview(reason)))

	// 检查是否启用自动禁用功能
	if !channelError.AutoBan {
		common.SysLog(common.LogText("channel %q (#%d) has automatic disabling turned off, skipping", channelError.ChannelName, channelError.ChannelId))
		return false
	}

	change := model.ProviderStatusChange{Status: common.ChannelStatusAutoDisabled, Reason: reason,
		UsingKey: channelError.UsingKey, ProviderWide: providerWide}
	if len(selectedIndex) > 0 {
		change.KeyIndex = &selectedIndex[0]
	}
	success, _ := model.ApplyProviderStatus(channelError.ChannelId, change)
	if success {
		if shouldCloseActiveWebSocketsAfterDisable(channelError.ChannelId) {
			CloseActiveWebSocketsForChannel(channelError.ChannelId, ChannelDisabledCloseReason)
		}
		params := map[string]any{"Name": channelError.ChannelName, "Id": channelError.ChannelId, "Reason": reason}
		notifyType := formatNotifyType(channelError.ChannelId, common.ChannelStatusAutoDisabled)
		notify := func() {
			NotifyChannelWatchers(notifyType, func(lang string) (string, string) {
				return i18n.Translate(lang, i18n.MsgChannelNotifyDisabledSubject, params), i18n.Translate(lang, i18n.MsgChannelNotifyDisabledContent, params)
			})
		}
		if !asyncNotice {
			notify()
			return success
		}
		// Bound queued and running notices together. The job retains only safe
		// notice fields, never the ProviderError containing the failed Key.
		select {
		case gatewayDisableNoticeSlots <- struct{}{}:
			common.RelayCtxGo(context.Background(), func() {
				defer func() { <-gatewayDisableNoticeSlots }()
				notify()
			})
		default:
			common.SysLog(common.LogText("gateway Provider disable notice skipped: notification queue is full, provider_id=%d", channelError.ChannelId))
		}
	}
	return success
}

func EnableChannel(channelId int, usingKey string, channelName string, selectedIndex ...int) bool {
	change := model.ProviderStatusChange{Status: common.ChannelStatusEnabled, UsingKey: usingKey}
	if len(selectedIndex) > 0 {
		change.KeyIndex = &selectedIndex[0]
	}
	success, _ := model.ApplyProviderStatus(channelId, change)
	if success {
		params := map[string]any{"Name": channelName, "Id": channelId}
		NotifyChannelWatchers(formatNotifyType(channelId, common.ChannelStatusEnabled), func(lang string) (string, string) {
			return i18n.Translate(lang, i18n.MsgChannelNotifyEnabledSubject, params), i18n.Translate(lang, i18n.MsgChannelNotifyEnabledContent, params)
		})
	}
	return success
}

func ShouldDisableChannel(err *types.NewAPIError) bool {
	if !common.AutomaticDisableChannelEnabled {
		return false
	}
	if err == nil {
		return false
	}
	if types.IsChannelError(err) {
		return true
	}
	if types.IsSkipRetryError(err) {
		return false
	}
	if operation_setting.ShouldDisableByStatusCode(err.StatusCode) {
		return true
	}

	lowerMessage := strings.ToLower(err.Error())
	search, _ := AcSearch(lowerMessage, operation_setting.AutomaticDisableKeywords, true)
	return search
}

func ShouldEnableChannel(newAPIError *types.NewAPIError, status int) bool {
	if !common.AutomaticEnableChannelEnabled {
		return false
	}
	if newAPIError != nil {
		return false
	}
	if status != common.ChannelStatusAutoDisabled {
		return false
	}
	return true
}
