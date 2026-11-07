package zhipu_4v

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

func (p *Provider) RepairUsage(context *channel.ProtocolResponseContext, usage *dto.Usage, body []byte) {
	if usage == nil || usage.PromptTokensDetails.CachedTokens != 0 {
		return
	}
	if usage.InputTokensDetails != nil && usage.InputTokensDetails.CachedTokens > 0 {
		usage.PromptTokensDetails.CachedTokens = usage.InputTokensDetails.CachedTokens
		return
	}

	var report struct {
		Usage struct {
			PromptTokensDetails struct {
				CachedTokens *int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CachedTokens         *int `json:"cached_tokens"`
			PromptCacheHitTokens *int `json:"prompt_cache_hit_tokens"`
		} `json:"usage"`
	}
	if common.Unmarshal(body, &report) == nil {
		for _, count := range []*int{report.Usage.PromptTokensDetails.CachedTokens, report.Usage.CachedTokens, report.Usage.PromptCacheHitTokens} {
			if count != nil {
				usage.PromptTokensDetails.CachedTokens = *count
				return
			}
		}
	}
	if usage.PromptCacheHitTokens > 0 {
		usage.PromptTokensDetails.CachedTokens = usage.PromptCacheHitTokens
	}
}
