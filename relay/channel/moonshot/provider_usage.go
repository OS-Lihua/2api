package moonshot

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
	var choices struct {
		Choices []struct {
			Usage struct {
				CachedTokens *int `json:"cached_tokens"`
			} `json:"usage"`
		} `json:"choices"`
	}
	if common.Unmarshal(body, &choices) == nil {
		for _, choice := range choices.Choices {
			if choice.Usage.CachedTokens != nil && *choice.Usage.CachedTokens > 0 {
				usage.PromptTokensDetails.CachedTokens = *choice.Usage.CachedTokens
				return
			}
		}
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
