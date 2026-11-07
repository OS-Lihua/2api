package controller

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"mime/multipart"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/protocol"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/tidwall/gjson"
)

// These small synthetic assets contain no user data and require no remote fetch.
//
//go:embed probe-fixtures/red.png
var probeRedImage []byte

//go:embed probe-fixtures/blue.png
var probeBlueImage []byte

//go:embed probe-fixtures/red.mp4
var probeRedVideo []byte

//go:embed probe-fixtures/seven.wav
var probeSevenAudio []byte

//go:embed probe-fixtures/code.pdf
var probeCodePDF []byte

type channelProbeRecipe struct {
	Request      dto.Request
	Format       types.RelayFormat
	Mode         int
	ContentType  string
	Body         []byte
	ExpectedText string
	TestedValue  int64
	// SampledLimit prevents a small bounded sample from verifying a larger declaration.
	SampledLimit bool
}

func channelProbeRecipeFor(ctx context.Context, record model.Channel, feature string, chat *dto.GeneralOpenAIRequest) (channelProbeRecipe, error) {
	recipe := channelProbeRecipe{Request: chat, Format: types.RelayFormatOpenAI, ContentType: "application/json"}
	var parent model.Model
	if err := model.DB.WithContext(ctx).First(&parent, record.ModelID).Error; err != nil {
		return recipe, err
	}
	effective, err := record.EffectiveCapabilities(parent.Capabilities)
	if err != nil {
		return recipe, err
	}
	switch record.Protocol {
	case protocol.OpenAIChat, protocol.OpenAIResponses, protocol.AnthropicMessages, protocol.GeminiGenerate:
		switch feature {
		case "input_text", "output_text":
			recipe.ExpectedText = "ok"
		case "input_image", "input_video", "input_audio", "input_pdf", "max_images", "max_duration_seconds":
			part := dto.MediaContent{}
			text := ""
			switch feature {
			case "input_image", "max_images":
				part = dto.MediaContent{Type: "image_url", ImageUrl: map[string]any{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(probeRedImage)}}
				text = "What color is the square? Reply with only the color name."
				recipe.ExpectedText = "red"
			case "input_video", "max_duration_seconds":
				part = dto.MediaContent{Type: "video_url", VideoUrl: map[string]any{"url": "data:video/mp4;base64," + base64.StdEncoding.EncodeToString(probeRedVideo)}}
				text = "What color fills the video? Reply with only the color name."
				recipe.ExpectedText = "red"
			case "input_audio":
				part = dto.MediaContent{Type: "input_audio", InputAudio: map[string]any{"data": base64.StdEncoding.EncodeToString(probeSevenAudio), "format": "wav"}}
				text = "Transcribe the spoken word. Reply with only the word."
				recipe.ExpectedText = "seven"
			case "input_pdf":
				part = dto.MediaContent{Type: "file", File: map[string]any{"filename": "probe.pdf", "file_data": "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(probeCodePDF)}}
				text = "Read the code in the document. Reply with only that code."
				recipe.ExpectedText = "GWP4831"
			}
			parts := []dto.MediaContent{{Type: "text", Text: text}, part}
			if feature == "max_images" {
				recipe.TestedValue = 1
				if effective.Limits.MaxImages != nil {
					recipe.TestedValue = min(*effective.Limits.MaxImages, 2)
					recipe.SampledLimit = *effective.Limits.MaxImages > 2
				}
				if recipe.TestedValue == 2 {
					parts[0].Text = "List the colors of every image in order. Reply with only comma-separated color names."
					recipe.ExpectedText = "red,blue"
					parts = append(parts, dto.MediaContent{Type: "image_url", ImageUrl: map[string]any{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(probeBlueImage)}})
				}
			}
			if feature == "max_duration_seconds" {
				recipe.TestedValue = 1
				recipe.SampledLimit = effective.Limits.MaxDurationSeconds != nil && *effective.Limits.MaxDurationSeconds != 1
			}
			chat.Messages[0].Content = parts
		case "context_length":
			recipe.TestedValue = 128
			if effective.Limits.ContextLength != nil {
				recipe.TestedValue = min(*effective.Limits.ContextLength, 4096)
			}
			// Tokenization differs by Provider. Verify the retained sample against
			// reported input/output tokens before claiming the tested boundary.
			recipe.SampledLimit = effective.Limits.ContextLength != nil && *effective.Limits.ContextLength > 4096
			chat.Messages[0].Content = "Remember code GWP4831. " + strings.Repeat("padding ", int(recipe.TestedValue)) + "What was the code at the beginning? Reply with only the code."
			recipe.ExpectedText = "GWP4831"
		case "max_output_tokens":
			recipe.TestedValue = 64
			if effective.Limits.MaxOutputTokens != nil {
				recipe.TestedValue = min(*effective.Limits.MaxOutputTokens, 2048)
				recipe.SampledLimit = *effective.Limits.MaxOutputTokens > 2048
			}
			chat.MaxTokens = common.GetPointer(uint(recipe.TestedValue))
			chat.Messages[0].Content = "Continue writing the word probe separated by spaces until you reach the output limit. Do not stop early."
		case "output_image":
			if record.Protocol != protocol.GeminiGenerate {
				return recipe, common.NewMessage("Invalid parameters")
			}
			chat.Messages[0].Content = "Generate a small solid red square image."
			chat.Modalities = []byte(`["text","image"]`)
		case "output_audio":
			if record.Protocol != protocol.OpenAIChat {
				return recipe, common.NewMessage("Invalid parameters")
			}
			chat.Messages[0].Content = "Say seven."
			chat.Modalities = []byte(`["text","audio"]`)
			chat.Audio = []byte(`{"voice":"alloy","format":"wav"}`)
		case "output_video", "output_pdf", "embedding_dimensions":
			return recipe, common.NewMessage("Invalid parameters")
		}
		return recipe, nil
	case protocol.OpenAIEmbeddings, protocol.GeminiEmbed:
		if feature != "input_text" && feature != "embedding_dimensions" {
			return recipe, common.NewMessage("Invalid parameters")
		}
		input := &dto.EmbeddingRequest{Model: record.UpstreamModelName, Input: "gateway capability probe", EncodingFormat: "float"}
		if feature == "embedding_dimensions" {
			recipe.TestedValue = 2
			if effective.Limits.EmbeddingDimensions != nil {
				recipe.TestedValue = min(*effective.Limits.EmbeddingDimensions, 4096)
				recipe.SampledLimit = *effective.Limits.EmbeddingDimensions > 4096
			}
			input.Dimensions = common.GetPointer(int(recipe.TestedValue))
		}
		recipe.Request = input
		recipe.Format = types.RelayFormatEmbedding
		recipe.Mode = relayconstant.RelayModeEmbeddings
	case protocol.OpenAIImages:
		if feature != "input_text" && feature != "input_image" && feature != "output_image" && feature != "max_images" {
			return recipe, common.NewMessage("Invalid parameters")
		}
		count := uint(1)
		if feature == "max_images" {
			recipe.TestedValue = 1
			if effective.Limits.MaxImages != nil {
				recipe.TestedValue = min(*effective.Limits.MaxImages, 2)
				recipe.SampledLimit = *effective.Limits.MaxImages > 2
			}
			count = uint(recipe.TestedValue)
		}
		recipe.Request = &dto.ImageRequest{Model: record.UpstreamModelName, Prompt: "A small solid red square", N: &count, ResponseFormat: "b64_json"}
		recipe.Format = types.RelayFormatOpenAIImage
		recipe.Mode = relayconstant.RelayModeImagesGenerations
		if feature == "input_image" {
			imageRequest := recipe.Request.(*dto.ImageRequest)
			imageRequest.Prompt = "Turn the supplied red square entirely blue. Return only the edited image."
			imageRequest.Images, _ = common.Marshal([]map[string]string{{"image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(probeRedImage)}})
			recipe.ExpectedText = "blue"
			recipe.Mode = relayconstant.RelayModeImagesEdits
		}
	case protocol.OpenAIAudioSpeech:
		if feature != "input_text" && feature != "output_audio" && feature != "max_duration_seconds" {
			return recipe, common.NewMessage("Invalid parameters")
		}
		recipe.Request = &dto.AudioRequest{Model: record.UpstreamModelName, Input: "seven", Voice: "alloy", ResponseFormat: "wav"}
		recipe.Format = types.RelayFormatOpenAIAudio
		recipe.Mode = relayconstant.RelayModeAudioSpeech
		if feature == "max_duration_seconds" {
			recipe.TestedValue = 1
			recipe.SampledLimit = effective.Limits.MaxDurationSeconds != nil && *effective.Limits.MaxDurationSeconds != 1
		}
	case protocol.OpenAIAudioTranscription:
		if feature != "input_audio" && feature != "output_text" && feature != "max_duration_seconds" {
			return recipe, common.NewMessage("Invalid parameters")
		}
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		part, err := writer.CreateFormFile("file", "seven.wav")
		if err != nil {
			return recipe, err
		}
		if _, err = part.Write(probeSevenAudio); err != nil {
			return recipe, err
		}
		if err = writer.WriteField("model", record.UpstreamModelName); err != nil {
			return recipe, err
		}
		if err = writer.WriteField("response_format", "json"); err != nil {
			return recipe, err
		}
		if err = writer.Close(); err != nil {
			return recipe, err
		}
		recipe.Request = &dto.AudioRequest{Model: record.UpstreamModelName, ResponseFormat: "json"}
		recipe.Format = types.RelayFormatOpenAIAudio
		recipe.Mode = relayconstant.RelayModeAudioTranscription
		recipe.Body = body.Bytes()
		recipe.ContentType = writer.FormDataContentType()
		recipe.ExpectedText = "seven"
		if feature == "max_duration_seconds" {
			recipe.TestedValue = 1
			recipe.SampledLimit = effective.Limits.MaxDurationSeconds != nil && *effective.Limits.MaxDurationSeconds != 1
		}
	case protocol.Rerank:
		if feature != "input_text" {
			return recipe, common.NewMessage("Invalid parameters")
		}
		recipe.Request = &dto.RerankRequest{Model: record.UpstreamModelName, Query: "red square", Documents: []any{"A red square"}, TopN: common.GetPointer(1)}
		recipe.Format = types.RelayFormatRerank
		recipe.Mode = relayconstant.RelayModeRerank
	default:
		return recipe, common.NewMessage("Invalid parameters")
	}
	return recipe, nil
}

func observeChannelProbe(recipe channelProbeRecipe, kind protocol.Type, feature string, body []byte) bool {
	root := gjson.ParseBytes(body)
	switch kind {
	case protocol.OpenAIEmbeddings, protocol.GeminiEmbed:
		path := "data.0.embedding"
		if kind == protocol.GeminiEmbed {
			path = "embedding.values"
			if !root.Get(path).Exists() {
				path = "embeddings.0.values"
			}
		}
		vector := root.Get(path)
		if !vector.IsArray() || len(vector.Array()) == 0 {
			return false
		}
		for _, value := range vector.Array() {
			if value.Type != gjson.Number || math.IsInf(value.Float(), 0) || math.IsNaN(value.Float()) {
				return false
			}
		}
		return feature != "embedding_dimensions" || int64(len(vector.Array())) == recipe.TestedValue
	case protocol.OpenAIImages:
		entries := root.Get("data")
		if entries.IsObject() {
			entries = gjson.Parse("[" + entries.Raw + "]")
		}
		count := int64(0)
		for _, entry := range entries.Array() {
			if validProbeImage(entry.Get("b64_json").String()) && (feature != "input_image" || probeImageColor(entry.Get("b64_json").String(), "blue")) {
				count++
			}
		}
		return count > 0 && (feature != "max_images" || count == recipe.TestedValue)
	case protocol.OpenAIAudioSpeech:
		seconds, valid := probeWaveDuration(body)
		return valid && (feature != "max_duration_seconds" || seconds >= recipe.TestedValue)
	case protocol.OpenAIAudioTranscription:
		return strings.EqualFold(strings.Trim(root.Get("text").String(), " .!\n\r"), recipe.ExpectedText)
	case protocol.Rerank:
		return root.Get("results.0.index").Exists() && root.Get("results.0.index").Int() == 0 && root.Get("results.0.relevance_score").Type == gjson.Number
	}
	if feature == "output_image" && kind == protocol.GeminiGenerate {
		for _, part := range root.Get("candidates.0.content.parts").Array() {
			if validProbeImage(part.Get("inlineData.data").String()) {
				return true
			}
		}
		return false
	}
	if feature == "output_audio" {
		data, err := base64.StdEncoding.DecodeString(root.Get("choices.0.message.audio.data").String())
		_, valid := probeWaveDuration(data)
		return err == nil && valid
	}
	if feature == "max_output_tokens" {
		actual, finish := root.Get("usage.completion_tokens"), root.Get("choices.0.finish_reason").String()
		switch kind {
		case protocol.OpenAIResponses:
			actual = root.Get("usage.output_tokens")
			finish = root.Get("incomplete_details.reason").String()
		case protocol.AnthropicMessages:
			actual = root.Get("usage.output_tokens")
			finish = root.Get("stop_reason").String()
		case protocol.GeminiGenerate:
			actual = root.Get("usageMetadata.candidatesTokenCount")
			finish = root.Get("candidates.0.finishReason").String()
		}
		count, valid := probeReportedTokenCount(actual)
		if kind == protocol.GeminiGenerate && root.Get("usageMetadata.thoughtsTokenCount").Exists() {
			thoughts, ok := probeReportedTokenCount(root.Get("usageMetadata.thoughtsTokenCount"))
			valid = valid && ok
			count += thoughts
		}
		return valid && count == recipe.TestedValue && (finish == "length" || finish == "max_output_tokens" || finish == "max_tokens" || finish == "MAX_TOKENS")
	}
	if recipe.ExpectedText != "" {
		text := ""
		switch kind {
		case protocol.OpenAIChat:
			text = root.Get("choices.0.message.content").String()
		case protocol.OpenAIResponses:
			for _, output := range root.Get("output").Array() {
				for _, part := range output.Get("content").Array() {
					if part.Get("type").String() == "output_text" {
						text += part.Get("text").String()
					}
				}
			}
		case protocol.AnthropicMessages:
			for _, part := range root.Get("content").Array() {
				if part.Get("type").String() == "text" {
					text += part.Get("text").String()
				}
			}
		case protocol.GeminiGenerate:
			for _, part := range root.Get("candidates.0.content.parts").Array() {
				if !part.Get("thought").Bool() {
					text += part.Get("text").String()
				}
			}
		}
		if feature == "max_images" {
			colors := strings.Split(text, ",")
			expected := strings.Split(recipe.ExpectedText, ",")
			if len(colors) != len(expected) {
				return false
			}
			for i := range expected {
				if !strings.EqualFold(strings.TrimSpace(colors[i]), expected[i]) {
					return false
				}
			}
		} else if !strings.EqualFold(strings.Trim(text, " .!\n\r"), recipe.ExpectedText) {
			return false
		}
		if feature == "context_length" {
			fields := []string{"usage.prompt_tokens", "usage.completion_tokens"}
			switch kind {
			case protocol.OpenAIResponses:
				fields = []string{"usage.input_tokens", "usage.output_tokens"}
			case protocol.AnthropicMessages:
				fields = []string{"usage.input_tokens", "usage.cache_creation_input_tokens", "usage.cache_read_input_tokens", "usage.output_tokens"}
			case protocol.GeminiGenerate:
				fields = []string{"usageMetadata.promptTokenCount", "usageMetadata.candidatesTokenCount", "usageMetadata.thoughtsTokenCount"}
			}
			if !root.Get(fields[0]).Exists() {
				return false
			}
			var observed int64
			for _, field := range fields {
				value := root.Get(field)
				if !value.Exists() {
					continue
				}
				count, valid := probeReportedTokenCount(value)
				if !valid {
					return false
				}
				observed += count
			}
			return observed >= recipe.TestedValue
		}
		return true
	}
	return observeProbeJSON(kind, feature, body)
}

// probeReportedTokenCount accepts evidence from the upstream response only.
// Counts outside this small probe's possible budget cannot verify a boundary.
func probeReportedTokenCount(value gjson.Result) (int64, bool) {
	if value.Type != gjson.Number || value.Float() < 0 || value.Float() > 16384 {
		return 0, false
	}
	count := value.Int()
	return count, value.Float() == float64(count)
}

// probeImageColor checks the requested edit or reference color in decoded pixels.
func probeImageColor(encoded, color string) bool {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return false
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 2048 || config.Height > 2048 {
		return false
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return false
	}
	matched, total := 0, 0
	for y := decoded.Bounds().Min.Y; y < decoded.Bounds().Max.Y; y++ {
		for x := decoded.Bounds().Min.X; x < decoded.Bounds().Max.X; x++ {
			r, g, b, a := decoded.At(x, y).RGBA()
			total++
			if a > 0 && (color == "blue" && b > 2*r && b > 2*g || color == "red" && r > 2*g && r > 2*b) {
				matched++
			}
		}
	}
	return matched*4 >= total*3
}

func validProbeImage(encoded string) bool {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) == 0 {
		return false
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 2048 || config.Height > 2048 {
		return false
	}
	_, _, err = image.Decode(bytes.NewReader(data))
	return err == nil
}

// probeWaveDuration validates PCM framing before observing an audio payload.
// The integer is the sample's complete seconds, never billable usage.
func probeWaveDuration(data []byte) (int64, bool) {
	if len(data) < 44 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return 0, false
	}
	if int64(binary.LittleEndian.Uint32(data[4:8]))+8 != int64(len(data)) {
		return 0, false
	}
	var rate, align uint32
	var payload uint32
	for offset := 12; offset+8 <= len(data); {
		size := binary.LittleEndian.Uint32(data[offset+4 : offset+8])
		start := offset + 8
		if int64(size) > int64(len(data)-start) {
			return 0, false
		}
		switch string(data[offset : offset+4]) {
		case "fmt ":
			if size < 16 || binary.LittleEndian.Uint16(data[start:start+2]) != 1 {
				return 0, false
			}
			channels := uint32(binary.LittleEndian.Uint16(data[start+2 : start+4]))
			rate = binary.LittleEndian.Uint32(data[start+4 : start+8])
			align = uint32(binary.LittleEndian.Uint16(data[start+12 : start+14]))
			bits := uint32(binary.LittleEndian.Uint16(data[start+14 : start+16]))
			if channels < 1 || channels > 8 || bits != 8 && bits != 16 && bits != 24 && bits != 32 || align != channels*bits/8 || rate < 8000 || rate > 192000 {
				return 0, false
			}
		case "data":
			payload = size
		}
		offset = start + int(size) + (int(size) % 2)
	}
	if rate == 0 || align == 0 || payload == 0 || payload%align != 0 {
		return 0, false
	}
	samples := int64(payload / align)
	return samples / int64(rate), true
}
