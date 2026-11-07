package relay

import (
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/advancedcustom"
	"github.com/QuantumNous/new-api/relay/channel/ali"
	"github.com/QuantumNous/new-api/relay/channel/aws"
	"github.com/QuantumNous/new-api/relay/channel/baidu"
	"github.com/QuantumNous/new-api/relay/channel/baidu_v2"
	"github.com/QuantumNous/new-api/relay/channel/claude"
	"github.com/QuantumNous/new-api/relay/channel/cloudflare"
	"github.com/QuantumNous/new-api/relay/channel/codex"
	"github.com/QuantumNous/new-api/relay/channel/cohere"
	"github.com/QuantumNous/new-api/relay/channel/coze"
	"github.com/QuantumNous/new-api/relay/channel/deepseek"
	"github.com/QuantumNous/new-api/relay/channel/dify"
	"github.com/QuantumNous/new-api/relay/channel/gemini"
	"github.com/QuantumNous/new-api/relay/channel/jimeng"
	"github.com/QuantumNous/new-api/relay/channel/jina"
	"github.com/QuantumNous/new-api/relay/channel/minimax"
	"github.com/QuantumNous/new-api/relay/channel/mistral"
	"github.com/QuantumNous/new-api/relay/channel/mokaai"
	"github.com/QuantumNous/new-api/relay/channel/moonshot"
	"github.com/QuantumNous/new-api/relay/channel/newapi"
	"github.com/QuantumNous/new-api/relay/channel/ollama"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	"github.com/QuantumNous/new-api/relay/channel/palm"
	"github.com/QuantumNous/new-api/relay/channel/perplexity"
	"github.com/QuantumNous/new-api/relay/channel/replicate"
	"github.com/QuantumNous/new-api/relay/channel/siliconflow"
	"github.com/QuantumNous/new-api/relay/channel/submodel"
	"github.com/QuantumNous/new-api/relay/channel/tencent"
	"github.com/QuantumNous/new-api/relay/channel/vertex"
	"github.com/QuantumNous/new-api/relay/channel/volcengine"
	"github.com/QuantumNous/new-api/relay/channel/xai"
	"github.com/QuantumNous/new-api/relay/channel/xunfei"
	"github.com/QuantumNous/new-api/relay/channel/zhipu"
	"github.com/QuantumNous/new-api/relay/channel/zhipu_4v"
)

func providerNeedsPayload(provider channel.ProviderAdapter) bool {
	switch provider.(type) {
	case *tencent.Provider, *jimeng.Provider, *aws.Provider:
		return true
	}
	return false
}

// GetProviderAdapter selects addressing and authentication without selecting a
// wire protocol. Aliases sharing account behavior reuse the same implementation.
func GetProviderAdapter(apiType int) channel.ProviderAdapter {
	switch apiType {
	case constant.APITypeOpenAI, constant.APITypeOpenRouter, constant.APITypeXinference:
		return &openai.Provider{}
	case constant.APITypeAli:
		return &ali.Provider{}
	case constant.APITypeAnthropic:
		return &claude.Provider{}
	case constant.APITypeBaidu:
		return &baidu.Provider{}
	case constant.APITypeGemini:
		return &gemini.Provider{}
	case constant.APITypePaLM:
		return &palm.Provider{}
	case constant.APITypeTencent:
		return &tencent.Provider{}
	case constant.APITypeXunfei:
		return &xunfei.Provider{}
	case constant.APITypeZhipu:
		return &zhipu.Provider{}
	case constant.APITypeZhipuV4:
		return &zhipu_4v.Provider{}
	case constant.APITypeOllama:
		return &ollama.Provider{}
	case constant.APITypePerplexity:
		return &perplexity.Provider{}
	case constant.APITypeAws:
		return &aws.Provider{}
	case constant.APITypeCohere:
		return &cohere.Provider{}
	case constant.APITypeDify:
		return &dify.Provider{}
	case constant.APITypeJina:
		return &jina.Provider{}
	case constant.APITypeCloudflare:
		return &cloudflare.Provider{}
	case constant.APITypeSiliconFlow:
		return &siliconflow.Provider{}
	case constant.APITypeVertexAi:
		return &vertex.Provider{}
	case constant.APITypeMistral:
		return &mistral.Provider{}
	case constant.APITypeDeepSeek:
		return &deepseek.Provider{}
	case constant.APITypeMokaAI:
		return &mokaai.Provider{}
	case constant.APITypeVolcEngine:
		return &volcengine.Provider{}
	case constant.APITypeBaiduV2:
		return &baidu_v2.Provider{}
	case constant.APITypeXai:
		return &xai.Provider{}
	case constant.APITypeCoze:
		return &coze.Provider{}
	case constant.APITypeJimeng:
		return &jimeng.Provider{}
	case constant.APITypeMoonshot:
		return &moonshot.Provider{}
	case constant.APITypeSubmodel:
		return &submodel.Provider{}
	case constant.APITypeMiniMax:
		return &minimax.Provider{}
	case constant.APITypeReplicate:
		return &replicate.Provider{}
	case constant.APITypeCodex:
		return &codex.Provider{}
	case constant.APITypeAdvancedCustom:
		return &advancedcustom.Provider{}
	case constant.APITypeSub2API, constant.APITypeNewAPI:
		return &newapi.Provider{}
	}
	return nil
}
