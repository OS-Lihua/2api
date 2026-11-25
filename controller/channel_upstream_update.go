package controller

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/advancedcustom"
	"github.com/QuantumNous/new-api/relay/channel/gemini"
	"github.com/QuantumNous/new-api/relay/channel/ollama"
	"github.com/QuantumNous/new-api/service"

	"github.com/samber/lo"
)

func normalizeModelNames(models []string) []string {
	return lo.Uniq(lo.FilterMap(models, func(model string, _ int) (string, bool) {
		trimmed := strings.TrimSpace(model)
		return trimmed, trimmed != ""
	}))
}

func parseOpenAIModelIDs(body []byte) ([]string, error) {
	var result struct {
		Data *[]OpenAIModel `json:"data"`
	}
	if err := common.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("invalid OpenAI Models response: %w", err)
	}
	if result.Data == nil {
		return nil, fmt.Errorf("invalid OpenAI Models response: data is required")
	}
	ids := normalizeModelNames(lo.Map(*result.Data, func(item OpenAIModel, _ int) string {
		return item.ID
	}))
	if len(ids) == 0 {
		return nil, fmt.Errorf("OpenAI Models response contains no valid model IDs")
	}
	return ids, nil
}

func sanitizeFetchModelsError(err error, key string) error {
	if err == nil {
		return nil
	}

	// net/http includes the complete request URL in url.Error. Discovery routes
	// may put the API key in a custom query name or value, so never return that
	// wrapper to an API client.
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		err = urlErr.Err
	}

	message := err.Error()
	key = strings.TrimSpace(key)
	if key != "" {
		message = strings.ReplaceAll(message, key, "[REDACTED]")
		message = strings.ReplaceAll(message, url.QueryEscape(key), "[REDACTED]")
		message = strings.ReplaceAll(message, url.PathEscape(key), "[REDACTED]")
	}
	return errors.New(message)
}

func sanitizeAdvancedCustomRequestError(err error, key string, requestURL string) error {
	err = sanitizeFetchModelsError(err, key)
	if err == nil {
		return nil
	}
	parsedURL, parseErr := url.Parse(requestURL)
	if parseErr != nil {
		return err
	}
	message := err.Error()
	for _, value := range parsedURL.Query() {
		for _, secret := range value {
			if secret == "" {
				continue
			}
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
			message = strings.ReplaceAll(message, url.QueryEscape(secret), "[REDACTED]")
			message = strings.ReplaceAll(message, url.PathEscape(secret), "[REDACTED]")
		}
	}
	if key != "" {
		message = strings.ReplaceAll(message, key, "[REDACTED]")
		message = strings.ReplaceAll(message, url.QueryEscape(key), "[REDACTED]")
		message = strings.ReplaceAll(message, url.PathEscape(key), "[REDACTED]")
	}
	return errors.New(message)
}

func getFetchModelsResponseBody(method string, requestURL string, channel *model.Provider, headers http.Header) ([]byte, error) {
	request, err := http.NewRequest(method, requestURL, nil)
	if err != nil {
		return nil, err
	}
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
		if strings.EqualFold(name, "Host") {
			request.Host = headers.Get(name)
		}
	}
	setting := channel.GetSetting()
	client, err := service.GetHttpClientWithProxySettings(setting.Proxy, setting)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status code: %d", response.StatusCode)
	}
	return io.ReadAll(response.Body)
}

func fetchChannelUpstreamModelIDs(channel *model.Provider) ([]string, error) {
	if channel.Type == constant.ChannelTypeTaskPlugin {
		plugin, ok := jsplugin.DefaultRegistry.Get(channel.GetSetting().TaskPluginKey)
		if !ok {
			return nil, fmt.Errorf("task plugin %q is not registered", channel.GetSetting().TaskPluginKey)
		}
		return normalizeModelNames(plugin.Meta.Models), nil
	}
	baseURL := constant.GetChannelBaseURL(channel.Type)
	if channel.GetBaseURL() != "" {
		baseURL = channel.GetBaseURL()
	}

	if channel.Type == constant.ChannelTypeOllama {
		key := strings.TrimSpace(strings.Split(channel.Key, "\n")[0])
		models, err := ollama.FetchOllamaModels(baseURL, key)
		if err != nil {
			return nil, err
		}
		return normalizeModelNames(lo.Map(models, func(item ollama.OllamaModel, _ int) string {
			return item.Name
		})), nil
	}

	if channel.Type == constant.ChannelTypeGemini {
		key, _, apiErr := channel.GetNextEnabledKey()
		if apiErr != nil {
			return nil, fmt.Errorf("failed to get channel key: %w", apiErr)
		}
		key = strings.TrimSpace(key)
		models, err := gemini.FetchGeminiModels(baseURL, key, channel.GetSetting())
		if err != nil {
			return nil, err
		}
		return normalizeModelNames(models), nil
	}

	if constant.IsAdvancedCustomChannel(channel.Type) {
		return fetchAdvancedCustomUpstreamModelIDs(channel, baseURL)
	}

	if channel.Type == constant.ChannelTypeCodex {
		return service.FetchCodexChannelModels(channel)
	}

	var url string
	switch channel.Type {
	case constant.ChannelTypeAli:
		url = fmt.Sprintf("%s/compatible-mode/v1/models", baseURL)
	case constant.ChannelTypeZhipu_v4:
		if plan, ok := constant.ChannelSpecialBases[baseURL]; ok && plan.OpenAIBaseURL != "" {
			url = fmt.Sprintf("%s/models", plan.OpenAIBaseURL)
		} else {
			url = fmt.Sprintf("%s/api/paas/v4/models", baseURL)
		}
	case constant.ChannelTypeVolcEngine:
		// 火山方舟 OpenAI 兼容 API 根路径是 /api/v3（chat/embeddings 均为
		// {base}/api/v3/...，见 relay/channel/volcengine 的 GetRequestURL），
		// 不存在 /v1/models 端点。原拼接会导致「获取模型列表」固定 404 失败。
		if plan, ok := constant.ChannelSpecialBases[baseURL]; ok && plan.OpenAIBaseURL != "" {
			url = fmt.Sprintf("%s/v1/models", plan.OpenAIBaseURL)
		} else {
			url = fmt.Sprintf("%s/api/v3/models", baseURL)
		}
	case constant.ChannelTypeMoonshot:
		if plan, ok := constant.ChannelSpecialBases[baseURL]; ok && plan.OpenAIBaseURL != "" {
			url = fmt.Sprintf("%s/models", plan.OpenAIBaseURL)
		} else {
			url = fmt.Sprintf("%s/v1/models", baseURL)
		}
	default:
		url = fmt.Sprintf("%s/v1/models", baseURL)
	}

	key, _, apiErr := channel.GetNextEnabledKey()
	if apiErr != nil {
		return nil, fmt.Errorf("failed to get channel key: %w", apiErr)
	}
	key = strings.TrimSpace(key)

	headers, err := buildFetchModelsHeaders(channel, key)
	if err != nil {
		return nil, sanitizeFetchModelsError(err, key)
	}

	body, err := getFetchModelsResponseBody(http.MethodGet, url, channel, headers)
	if err != nil {
		return nil, sanitizeAdvancedCustomRequestError(err, key, url)
	}

	var result OpenAIModelsResponse
	if err := common.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	ids := lo.Map(result.Data, func(item OpenAIModel, _ int) string {
		if channel.Type == constant.ChannelTypeGemini {
			return strings.TrimPrefix(item.ID, "models/")
		}
		return item.ID
	})
	return normalizeModelNames(ids), nil
}

func fetchAdvancedCustomUpstreamModelIDs(channel *model.Provider, baseURL string) ([]string, error) {
	key, _, apiErr := channel.GetNextEnabledKey()
	if apiErr != nil {
		return nil, fmt.Errorf("failed to get channel key: %w", apiErr)
	}
	key = strings.TrimSpace(key)

	metadata := &relaychannel.ProviderRequest{
		ChannelType:          channel.Type,
		ChannelBaseUrl:       baseURL,
		ApiKey:               key,
		ChannelOtherSettings: relaychannel.ProviderAccountSettings{AdvancedCustom: channel.GetOtherSettings().AdvancedCustom},
	}
	url, headers, err := (&advancedcustom.Provider{}).BuildModelListRequest(metadata)
	if err != nil {
		return nil, sanitizeFetchModelsError(err, key)
	}
	if err := applyFetchModelsHeaderOverrides(channel, key, headers); err != nil {
		return nil, sanitizeFetchModelsError(err, key)
	}

	body, err := getFetchModelsResponseBody(http.MethodGet, url, channel, headers)
	if err != nil {
		return nil, sanitizeFetchModelsError(err, key)
	}
	return parseOpenAIModelIDs(body)
}
