package relay

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/gin-gonic/gin"
)

// PreparedMediaRequest binds the validated effective quantities to the exact
// transport prepared without I/O. Input is transient purchase-price context.
type PreparedMediaRequest struct {
	Prepared *PreparedChannelRequest
	Request  dto.Request
	Input    billingexpr.RequestInput
}

// PrepareMediaRequest uses the production Provider/protocol composition and
// validates the final body after parameter overrides, before any reservation
// or dispatch. It never calls the legacy quota chain.
func (a *ChannelAdaptor) PrepareMediaRequest(c *gin.Context, info *relaycommon.RelayInfo, parsed *helper.GatewayMediaRequest) (*PreparedMediaRequest, error) {
	if parsed.Protocol != a.kind {
		return nil, fmt.Errorf("media protocol mismatch")
	}
	if contentType := parsed.Input.Headers["content-type"]; contentType != "" {
		c.Request.Header.Set("Content-Type", contentType)
	}
	var request dto.Request
	switch original := parsed.Request.(type) {
	case *dto.ImageRequest:
		copy, err := common.DeepCopy(original)
		if err != nil {
			return nil, err
		}
		copy.Model = info.UpstreamModelName
		request = copy
	case *dto.AudioRequest:
		copy, err := common.DeepCopy(original)
		if err != nil {
			return nil, err
		}
		copy.Model = info.UpstreamModelName
		request = copy
	default:
		return nil, fmt.Errorf("invalid media request")
	}
	converted, err := a.ConvertRequest(c, info, request)
	if err != nil {
		return nil, err
	}
	var body []byte
	if reader, ok := converted.(io.Reader); ok {
		body, err = io.ReadAll(reader)
	} else {
		body, err = common.Marshal(converted)
	}
	if err != nil {
		return nil, err
	}
	if c.GetHeader("Content-Type") == "application/octet-stream" && a.kind == protocol.OpenAIAudioTranscription {
		if len(info.ParamOverride) > 0 {
			return nil, fmt.Errorf("binary transcription requests cannot apply JSON parameter overrides")
		}
		audio := request.(*dto.AudioRequest)
		if audio.ResponseFormat != "json" && audio.ResponseFormat != "text" {
			return nil, fmt.Errorf("binary transcription supports JSON or text responses")
		}
		prepared, err := a.PrepareRequest(c, info, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		input := billingexpr.RequestInput{Headers: make(map[string]string)}
		for key, values := range prepared.Headers() {
			input.Headers[strings.ToLower(key)] = strings.Join(values, ",")
		}
		info.Request = request
		info.IsStream = false
		return &PreparedMediaRequest{Prepared: prepared, Request: request, Input: input}, nil
	}
	var form *multipart.Form
	if !strings.HasPrefix(c.GetHeader("Content-Type"), "multipart/form-data") && (info.ChannelSetting.PassThroughBodyEnabled || model_setting.GetGlobalSettings().PassThroughRequestEnabled) {
		var fields map[string]common.RawMessage
		if err := common.Unmarshal(parsed.Input.Body, &fields); err != nil {
			return nil, err
		}
		fields["model"], err = common.Marshal(info.UpstreamModelName)
		if err != nil {
			return nil, err
		}
		body, err = common.Marshal(fields)
		if err != nil {
			return nil, err
		}
	}
	if strings.HasPrefix(c.GetHeader("Content-Type"), "multipart/form-data") {
		_, parameters, parseErr := mime.ParseMediaType(c.GetHeader("Content-Type"))
		if parseErr != nil {
			return nil, parseErr
		}
		form, err = multipart.NewReader(bytes.NewReader(body), parameters["boundary"]).ReadForm(32 << 20)
		if err != nil {
			return nil, err
		}
		defer form.RemoveAll()
		body, err = helper.GatewayMediaMultipartInput(form)
		if err != nil {
			return nil, err
		}
	}
	if len(info.ParamOverride) > 0 {
		body, err = relaycommon.ApplyParamOverrideWithRelayInfo(body, info)
		if err != nil {
			return nil, err
		}
	}
	if err := helper.ValidateGatewayMediaMultipliers(body); err != nil {
		return nil, err
	}
	result := &PreparedMediaRequest{Request: request, Input: billingexpr.RequestInput{Body: body}}
	if projector, ok := converted.(channel.ImageRequestProjector); a.kind == protocol.OpenAIImages && ok {
		effective, err := projector.ProjectImageRequest(body, *request.(*dto.ImageRequest))
		if err != nil {
			return nil, err
		}
		count, err := effective.ImageCount(false)
		if err != nil {
			return nil, err
		}
		result.Request, result.Input.ImageCount = effective, &count
	} else if a.kind == protocol.OpenAIImages {
		var effective dto.ImageRequest
		if err := common.Unmarshal(body, &effective); err != nil {
			return nil, err
		}
		if effective.Model != info.UpstreamModelName {
			return nil, fmt.Errorf("image model override is not allowed")
		}
		count, err := effective.ImageCount(false)
		if err != nil {
			return nil, err
		}
		effective.N = common.GetPointer(uint(count))
		var providerParameters struct {
			Parameters *dto.ImageBillingParameters `json:"parameters"`
		}
		if err := common.Unmarshal(body, &providerParameters); err != nil {
			return nil, err
		}
		if parameters := providerParameters.Parameters; parameters != nil && parameters.N != nil && *parameters.N != uint(count) {
			return nil, fmt.Errorf("provider image count differs from n")
		}
		// Preserve native/provider fields while normalizing an explicit zero.
		var fields map[string]common.RawMessage
		if err := common.Unmarshal(body, &fields); err != nil {
			return nil, err
		}
		fields["n"], err = common.Marshal(count)
		if err != nil {
			return nil, err
		}
		body, err = common.Marshal(fields)
		if err != nil {
			return nil, err
		}
		result.Request, result.Input.ImageCount = &effective, &count
	} else if projector, ok := converted.(channel.AudioRequestProjector); ok {
		effective, err := projector.ProjectAudioRequest(body, *request.(*dto.AudioRequest))
		if err != nil {
			return nil, err
		}
		result.Request = effective
	} else {
		var effective dto.AudioRequest
		if err := common.Unmarshal(body, &effective); err != nil {
			return nil, err
		}
		if effective.Model != info.UpstreamModelName {
			return nil, fmt.Errorf("audio model override is not allowed")
		}
		if a.kind == protocol.OpenAIAudioSpeech && (effective.Input == "" || effective.Voice == "") {
			return nil, fmt.Errorf("speech input and voice are required")
		}
		result.Request = &effective
	}
	if result.Request.IsStream(c.Request) != parsed.Request.IsStream(c.Request) {
		return nil, fmt.Errorf("media stream mode override is not allowed")
	}
	result.Input.Body = body
	if form != nil {
		var fields map[string]common.RawMessage
		if err := common.Unmarshal(body, &fields); err != nil {
			return nil, err
		}
		var encoded bytes.Buffer
		writer := multipart.NewWriter(&encoded)
		for name, raw := range fields {
			value := string(raw)
			if len(raw) > 0 && raw[0] == '"' {
				if err := common.Unmarshal(raw, &value); err != nil {
					return nil, err
				}
			}
			if err := writer.WriteField(name, value); err != nil {
				return nil, err
			}
		}
		for _, files := range form.File {
			for _, file := range files {
				part, err := writer.CreatePart(file.Header)
				if err != nil {
					return nil, err
				}
				reader, err := file.Open()
				if err != nil {
					return nil, err
				}
				_, copyErr := io.Copy(part, reader)
				closeErr := reader.Close()
				if copyErr != nil {
					return nil, copyErr
				}
				if closeErr != nil {
					return nil, closeErr
				}
			}
		}
		if err := writer.Close(); err != nil {
			return nil, err
		}
		body = encoded.Bytes()
		c.Request.Header.Set("Content-Type", writer.FormDataContentType())
	}
	result.Prepared, err = a.PrepareRequest(c, info, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	result.Input.Headers = make(map[string]string)
	for key, values := range result.Prepared.Headers() {
		result.Input.Headers[strings.ToLower(key)] = strings.Join(values, ",")
	}
	if result.Input.ImageCount != nil {
		info.ImageRequestCount = *result.Input.ImageCount
	}
	info.IsStream = parsed.Request.IsStream(c.Request)
	info.Request = result.Request
	return result, nil
}
