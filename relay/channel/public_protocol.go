package channel

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/protocol"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/gin-gonic/gin"
)

// ResponseErrorCarrier preserves a transport adapter's failure while the
// protocol extracts received source usage before returning that failure.
type ResponseErrorCarrier interface {
	ResponseError() *types.NewAPIError
}

// GatewayResponseBufferLimit matches the default text scanner ceiling.
const GatewayResponseBufferLimit = 128 << 20

var errGatewayResponseLimit = errors.New("gateway response source exceeds capture limit")

type gatewayResponseBody struct {
	io.ReadCloser
	remaining int64
	exceeded  bool
}

func (b *gatewayResponseBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.exceeded {
		return 0, errGatewayResponseLimit
	}
	if b.remaining == 0 {
		var next [1]byte
		n, err := b.ReadCloser.Read(next[:])
		if n > 0 {
			b.exceeded = true
			_ = b.ReadCloser.Close()
			return 0, errGatewayResponseLimit
		}
		return 0, err
	}
	n, err := b.ReadCloser.Read(p[:min(len(p), int(b.remaining))])
	b.remaining -= int64(n)
	return n, err
}

func (b *gatewayResponseBody) UnwrapResponseBody() io.ReadCloser { return b.ReadCloser }

func (b *gatewayResponseBody) ResponseError() *types.NewAPIError {
	if source, ok := GatewayResponseBodySource(b.ReadCloser).(ResponseErrorCarrier); ok {
		return source.ResponseError()
	}
	return nil
}

// GatewayResponseSourceLimit applies the configured scanner ceiling to capture.
func GatewayResponseSourceLimit() int64 {
	limit := int64(GatewayResponseBufferLimit)
	if configured := constant.StreamScannerMaxBufferMB; configured > 0 {
		return int64(configured) << 20
	}
	return limit
}

// BoundGatewayResponseBody stops the source owner before cumulative parsing can
// retain an unbounded response. Complete received fields survive the read error.
func BoundGatewayResponseBody(body io.ReadCloser) io.ReadCloser {
	if body == nil {
		return http.NoBody
	}
	if _, bounded := body.(*gatewayResponseBody); bounded {
		return body
	}
	limit := GatewayResponseSourceLimit()
	if bounded, ok := body.(interface{ GatewayResponseSourceBound() int64 }); ok && bounded.GatewayResponseSourceBound() > 0 && bounded.GatewayResponseSourceBound() <= limit {
		return body
	}
	return &gatewayResponseBody{ReadCloser: body, remaining: limit}
}

// GatewayResponseBodySource exposes protocol metadata through the source bound.
func GatewayResponseBodySource(body io.ReadCloser) io.ReadCloser {
	for {
		bounded, ok := body.(interface{ UnwrapResponseBody() io.ReadCloser })
		if !ok {
			return body
		}
		body = bounded.UnwrapResponseBody()
	}
}

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
