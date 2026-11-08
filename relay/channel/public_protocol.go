package channel

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/pkg/protocol"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/gin-gonic/gin"
)

// PublicProtocol applies the shared normalization for the selected public DTO.
// Explicit scalar zero values remain present on the new protocol path.
type PublicProtocol struct {
	Kind    protocol.Type
	Handler ProtocolHandler
}

func (p *PublicProtocol) ConvertRequest(c *gin.Context, info *relaycommon.RelayInfo, request any) (any, error) {
	request, err := p.Handler.ConvertRequest(c, info, request)
	if err != nil {
		return nil, err
	}
	switch p.Kind {
	case protocol.AnthropicMessages:
		req, ok := request.(*dto.ClaudeRequest)
		if !ok || req == nil {
			return nil, fmt.Errorf("expected Anthropic Messages DTO, got %T", request)
		}
		if err := relayconvert.ApplyClaudeThinkingModel(req, info); err != nil {
			return nil, err
		}
		if original, ok := info.Request.(*dto.ClaudeRequest); ok && original.Thinking != nil && req.Thinking != nil && req.Thinking.Type == "adaptive" && len(req.Thinking.BlockBinding) == 0 {
			req.Thinking.BlockBinding = original.Thinking.BlockBinding
		}
		if req.MaxTokens == nil {
			n := uint(model_setting.GetClaudeSettings().GetDefaultMaxTokens(req.Model))
			req.MaxTokens = &n
		}
	case protocol.GeminiGenerate:
		req, ok := request.(*dto.GeminiChatRequest)
		if !ok || req == nil {
			return nil, fmt.Errorf("expected Gemini generateContent DTO, got %T", request)
		}
		if err := relayconvert.ApplyGeminiThinkingConfigChecked(req, info); err != nil {
			return nil, err
		}
		if len(req.Contents) > 0 && req.Contents[0].Role == "" {
			req.Contents[0].Role = "user"
		}
		for _, content := range req.Contents {
			for _, part := range content.Parts {
				if part.FileData != nil && part.FileData.MimeType == "" && strings.Contains(part.FileData.FileUri, "www.youtube.com") {
					part.FileData.MimeType = "video/webm"
				}
			}
		}
	}
	return request, nil
}
func (p *PublicProtocol) DoNativeResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo, wire ProtocolHandler) (any, *types.NewAPIError) {
	gateway, ok := p.Handler.(NativeResponseGateway)
	if !ok {
		return nil, types.NewError(fmt.Errorf("public protocol cannot present a native response"), types.ErrorCodeBadResponse)
	}
	if info.ChannelSetting.ThinkingToContent {
		info.ThinkingContentInfo = relaycommon.ThinkingContentInfo{IsFirstThinkingContent: true}
	}
	return gateway.DoNativeResponse(c, resp, info, wire)
}

func (p *PublicProtocol) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	if info.ChannelSetting.ThinkingToContent {
		info.ThinkingContentInfo = relaycommon.ThinkingContentInfo{IsFirstThinkingContent: true}
	}
	return p.Handler.DoResponse(c, resp, info)
}
