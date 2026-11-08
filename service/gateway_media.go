package service

import (
	"slices"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/capability"
	"github.com/QuantumNous/new-api/pkg/protocol"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

// GatewayMediaCapabilitiesMatch is applied to both original and effective DTOs.
// Uploaded audio duration is measured before reservation; no request duration or
// output ceiling is accepted as actual usage.
func GatewayMediaCapabilitiesMatch(c *gin.Context, kind protocol.Type, request dto.Request, caps capability.Set, inputTokens int, facts *relaycommon.ResponseFacts) bool {
	if request.IsStream(c.Request) && !slices.Contains(caps.Features, capability.Streaming) {
		return false
	}
	input, output := capability.Text, capability.Image
	switch kind {
	case protocol.OpenAIImages:
		image, ok := request.(*dto.ImageRequest)
		if !ok {
			return false
		}
		count, err := image.ImageCount(false)
		if err != nil || caps.Limits.MaxImages != nil && int64(count) > *caps.Limits.MaxImages {
			return false
		}
		images := 0
		if form := c.Request.MultipartForm; form != nil {
			for field, files := range form.File {
				if field == "image" || strings.HasPrefix(field, "image[") {
					images += len(files)
				}
			}
		}
		for _, raw := range []common.RawMessage{image.Image, image.Images} {
			if len(raw) == 0 {
				continue
			}
			value := ojson.ParseBytes(raw)
			if value.IsArray() {
				images += len(value.Array())
			} else if value.Exists() {
				images++
			}
		}
		if images > dto.MaxImageN || images > 0 && !slices.Contains(caps.InputModalities, capability.Image) || caps.Limits.MaxImages != nil && int64(images) > *caps.Limits.MaxImages {
			return false
		}
	case protocol.OpenAIAudioSpeech:
		output = capability.Audio
	case protocol.OpenAIAudioTranscription:
		input, output = capability.Audio, capability.Text
	default:
		return false
	}
	if !slices.Contains(caps.InputModalities, input) || !slices.Contains(caps.OutputModalities, output) {
		return false
	}
	if caps.Limits.ContextLength != nil && int64(inputTokens) > *caps.Limits.ContextLength {
		return false
	}
	if facts != nil && facts.AudioInputSeconds != nil && caps.Limits.MaxDurationSeconds != nil {
		seconds, err := decimal.NewFromString(*facts.AudioInputSeconds)
		if err != nil || seconds.IsNegative() || seconds.GreaterThan(decimal.NewFromInt(*caps.Limits.MaxDurationSeconds)) {
			return false
		}
	}
	return true
}
