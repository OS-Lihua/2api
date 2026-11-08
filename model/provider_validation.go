package model

import (
	"fmt"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"strings"
)

// maxTaskExtendPluginKeys bounds the plugins one New API channel can be
// extended with; it comfortably covers every built-in and installed plugin.
const maxTaskExtendPluginKeys = 32

// validateChannel 通用的渠道校验函数
func ValidateProvider(channel *Provider, isAdd bool) error {
	if channel == nil {
		return fmt.Errorf("channel cannot be empty")
	}
	if _, ok := constant.ChannelTypeNames[channel.Type]; (isAdd || channel.Type != 0) && (!ok || channel.Type == constant.ChannelTypeUnknown) {
		return common.NewMessage("Invalid provider type")
	}

	if channel.RouteLimits != nil {
		if !channel.RouteLimits.Valid() {
			return common.NewMessage("Invalid parameters")
		}
		if isAdd {
			channel.ProviderInfo.RouteLimits = *channel.RouteLimits
		}
	}
	// 校验 channel settings
	if err := channel.ValidateSettings(); err != nil {
		return common.NewMessage("Invalid channel settings format: {{error}}", map[string]any{"error": err.Error()})
	}
	if channel.Type == constant.ChannelTypeTaskPlugin {
		pluginKey := strings.TrimSpace(channel.GetSetting().TaskPluginKey)
		if pluginKey == "" {
			return fmt.Errorf("task plugin key is required")
		}
		if len(pluginKey) > 30 {
			return fmt.Errorf("task plugin key must not exceed 30 characters")
		}
		plugin, ok := jsplugin.DefaultRegistry.Get(pluginKey)
		if !ok {
			return fmt.Errorf("task plugin %q is not registered", pluginKey)
		}
		if channel.BaseURL == nil || strings.TrimSpace(*channel.BaseURL) == "" {
			// The plugin default is persisted onto the channel instead of being
			// resolved per request, so the destination host stays an auditable
			// channel property that only an administrator edit can change.
			if plugin.Meta.BaseURL == "" {
				return fmt.Errorf("base URL is required for task plugin channels")
			}
			defaultBaseURL := plugin.Meta.BaseURL
			channel.BaseURL = &defaultBaseURL
		}
	}

	setting := channel.GetSetting()
	if channel.Type != constant.ChannelTypeNewAPI && len(setting.TaskExtendPluginKeys) > 0 {
		return fmt.Errorf("task_extend_plugin_keys is only supported on New API channels")
	}
	if channel.Type == constant.ChannelTypeNewAPI {
		if len(setting.TaskExtendPluginKeys) > maxTaskExtendPluginKeys {
			return fmt.Errorf("task_extend_plugin_keys must not exceed %d plugins", maxTaskExtendPluginKeys)
		}
		// The single key stays valid on a New API channel, so both bindings are
		// checked as one set.
		keys := setting.TaskExtendPluginKeys
		if setting.TaskPluginKey != "" {
			keys = append([]string{setting.TaskPluginKey}, keys...)
		}
		bound := make(map[string]struct{}, len(keys))
		for _, key := range keys {
			if key == "" || key != strings.TrimSpace(key) {
				return fmt.Errorf("task plugin key %q is invalid", key)
			}
			if len(key) > 30 {
				return fmt.Errorf("task plugin key must not exceed 30 characters")
			}
			if _, duplicate := bound[key]; duplicate {
				return fmt.Errorf("task plugin %q is bound more than once", key)
			}
			plugin, ok := jsplugin.DefaultRegistry.Get(key)
			if !ok {
				return fmt.Errorf("task plugin %q is not registered", key)
			}
			if !plugin.Meta.SupportsUpstream(jsplugin.UpstreamKindNewAPI) {
				return fmt.Errorf("task plugin %q does not support a New API upstream and cannot be bound to a New API channel", key)
			}
			bound[key] = struct{}{}
		}
	}

	if channel.Type == constant.ChannelTypeNewAPI && strings.TrimSpace(channel.GetBaseURL()) == "" {
		return fmt.Errorf("New API channel base URL cannot be empty")
	}
	if channel.Type == constant.ChannelTypeVLLM && strings.TrimSpace(channel.GetBaseURL()) == "" {
		return fmt.Errorf("vLLM channel base URL cannot be empty")
	}
	if channel.Type == constant.ChannelTypeSGLang && strings.TrimSpace(channel.GetBaseURL()) == "" {
		return fmt.Errorf("SGLang channel base URL cannot be empty")
	}

	// 如果是添加操作，检查 channel 和 key 是否为空
	if isAdd {
		if channel.Key == "" {
			return fmt.Errorf("channel cannot be empty")
		}

		// 检查模型名称长度是否超过 255
		for _, m := range channel.GetModels() {
			if len(m) > 255 {
				return common.NewMessage("Model name is too long: {{model}}", map[string]any{"model": m})
			}
		}
	}

	// VertexAI 特殊校验
	if channel.Type == constant.ChannelTypeVertexAi {
		if channel.Other == "" {
			return common.NewMessage("Deployment region cannot be empty")
		}

		regionMap, err := common.StrToMap(channel.Other)
		if err != nil {
			return common.NewMessage("Deployment region must be valid JSON, for example {{example}}", map[string]any{"example": `{"default": "us-central1", "region2": "us-east1"}`})
		}

		if regionMap["default"] == nil {
			return common.NewMessage("Deployment region must include a default field")
		}

		switch channel.GetOtherSettings().VertexAPIVersion {
		case "", "v1", "v1beta1":
		default:
			return common.NewMessage("Vertex AI API version must be v1 or v1beta1")
		}
	}

	// Codex OAuth key validation (optional, only when JSON object is provided)
	if channel.Type == constant.ChannelTypeCodex {
		trimmedKey := strings.TrimSpace(channel.Key)
		if isAdd || trimmedKey != "" {
			if !strings.HasPrefix(trimmedKey, "{") {
				return fmt.Errorf("Codex key must be a valid JSON object")
			}
			var keyMap map[string]any
			if err := common.Unmarshal([]byte(trimmedKey), &keyMap); err != nil {
				return fmt.Errorf("Codex key must be a valid JSON object")
			}
			if v, ok := keyMap["access_token"]; !ok || v == nil || strings.TrimSpace(fmt.Sprintf("%v", v)) == "" {
				return fmt.Errorf("Codex key JSON must include access_token")
			}
			if v, ok := keyMap["account_id"]; !ok || v == nil || strings.TrimSpace(fmt.Sprintf("%v", v)) == "" {
				return fmt.Errorf("Codex key JSON must include account_id")
			}
		}
	}

	return nil
}
