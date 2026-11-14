package relay

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/protocol"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	kitreasoning "github.com/QuantumNous/new-api/relaykit/relayconvert/reasoning"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type imageReservation struct {
	held, limit int
}

func (s *imageReservation) Reserve(quota int) error {
	if quota > s.limit {
		return errors.New("insufficient image quota")
	}
	s.held = max(s.held, quota)
	return nil
}
func (s *imageReservation) GetPreConsumedQuota() int { return s.held }
func (*imageReservation) Settle(int) error           { return nil }
func (*imageReservation) Refund(*gin.Context)        {}
func (*imageReservation) NeedsRefund() bool          { return false }

func (s *imageReservation) ReserveBeforeSubmit(quota int) error { return s.Reserve(quota) }

func TestMediaPreparationValidatesAndSendsFinalQuantity(t *testing.T) {
	service.InitHttpClient()
	for _, tc := range []struct {
		name, body string
		override   any
		count      int
		invalid    bool
	}{
		{"zero normalized", `{"model":"image","prompt":"draw","n":0}`, nil, 1, false},
		{"provider override", `{"model":"image","prompt":"draw","n":1}`, 4, 4, false},
		{"oversized override", `{"model":"image","prompt":"draw","n":1}`, 129, 0, true},
		{"nested bypass", `{"model":"image","prompt":"draw","parameters":{"n":129}}`, nil, 0, true},
		{"passthrough", `{"model":"image","prompt":"draw","n":2,"parameters":{"n":2},"provider_extension":true}`, nil, 2, false},
		{"conflicting passthrough", `{"model":"image","prompt":"draw","n":2,"parameters":{"n":4}}`, nil, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sent []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var err error
				sent, err = io.ReadAll(r.Body)
				require.NoError(t, err)
				_, _ = io.WriteString(w, `{"data":{"url":"image"}}`)
			}))
			t.Cleanup(upstream.Close)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			parsed, err := helper.ParseGatewayMediaRequest(c)
			if tc.name == "nested bypass" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			info := &relaycommon.RelayInfo{ProviderMeta: &relaycommon.ProviderMeta{ChannelType: constant.ChannelTypeOpenAI, ChannelBaseUrl: upstream.URL, UpstreamModelName: "wire-image"}, RelayMode: relayconstant.RelayModeImagesGenerations, RelayFormat: types.RelayFormatOpenAIImage}
			if strings.Contains(tc.name, "passthrough") {
				info.ChannelSetting.PassThroughBodyEnabled = true
			}
			if tc.override != nil {
				info.ParamOverride = map[string]any{"operations": []any{map[string]any{"path": "n", "mode": "set", "value": tc.override}}}
			}
			adaptor, err := GetChannelAdaptor(constant.APITypeOpenAI, protocol.OpenAIImages)
			require.NoError(t, err)
			prepared, err := adaptor.PrepareMediaRequest(c, info, parsed)
			if tc.invalid {
				require.Error(t, err)
				assert.Empty(t, sent)
				return
			}
			require.NoError(t, err)
			assert.Empty(t, sent, "preparation must not dispatch")
			assert.Equal(t, tc.count, *prepared.Input.ImageCount)
			assert.Equal(t, tc.count, info.RequestedImageCount(), "abort protection uses the actual submitted quantity")
			response, err := adaptor.DoPreparedRequest(c, info, prepared.Prepared)
			require.NoError(t, err)
			require.NoError(t, response.(*http.Response).Body.Close())
			assert.Equal(t, int64(tc.count), ojson.GetBytes(sent, "n").Int())
			assert.Equal(t, "wire-image", ojson.GetBytes(sent, "model").String())
			if tc.name == "passthrough" {
				assert.True(t, ojson.GetBytes(sent, "provider_extension").Bool())
			}
		})
	}
}

func TestMediaMultipartPreparationUsesOverriddenQuantity(t *testing.T) {
	service.InitHttpClient()
	var received string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseMultipartForm(1<<20))
		received = r.FormValue("n")
		assert.Equal(t, "wire-image", r.FormValue("model"))
		assert.Equal(t, "false", r.FormValue("stream"))
		assert.Equal(t, "false", r.FormValue("watermark"))
		file, _, err := r.FormFile("image")
		require.NoError(t, err)
		image, err := io.ReadAll(file)
		require.NoError(t, err)
		require.NoError(t, file.Close())
		assert.Equal(t, "image bytes", string(image))
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(upstream.Close)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("model", "image"))
	require.NoError(t, writer.WriteField("prompt", "edit"))
	require.NoError(t, writer.WriteField("stream", "false"))
	require.NoError(t, writer.WriteField("watermark", "false"))
	require.NoError(t, writer.WriteField("n", "1"))
	part, err := writer.CreateFormFile("image", "image.png")
	require.NoError(t, err)
	_, err = part.Write([]byte("image bytes"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())
	parsed, err := helper.ParseGatewayMediaRequest(c)
	require.NoError(t, err)
	info := &relaycommon.RelayInfo{ProviderMeta: &relaycommon.ProviderMeta{ChannelType: constant.ChannelTypeOpenAI, ChannelBaseUrl: upstream.URL, UpstreamModelName: "wire-image", ParamOverride: map[string]any{"operations": []any{map[string]any{"path": "n", "mode": "set", "value": 4}}}}, RelayMode: relayconstant.RelayModeImagesEdits, RelayFormat: types.RelayFormatOpenAIImage}
	adaptor, err := GetChannelAdaptor(constant.APITypeOpenAI, protocol.OpenAIImages)
	require.NoError(t, err)
	prepared, err := adaptor.PrepareMediaRequest(c, info, parsed)
	require.NoError(t, err)
	require.NotNil(t, prepared.Input.ImageCount)
	assert.Equal(t, 4, *prepared.Input.ImageCount)
	contentType, _, err := mime.ParseMediaType(prepared.Prepared.Headers().Get("Content-Type"))
	require.NoError(t, err)
	assert.Equal(t, "multipart/form-data", contentType)
	response, err := adaptor.DoPreparedRequest(c, info, prepared.Prepared)
	require.NoError(t, err)
	require.NoError(t, response.(*http.Response).Body.Close())
	assert.Equal(t, "4", received)
}

func TestMediaReplicateEditsPrepareWithoutProviderCalls(t *testing.T) {
	var picture bytes.Buffer
	require.NoError(t, png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 1, 1))))
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprint("invalid=", invalid), func(t *testing.T) {
			service.InitHttpClient()
			uploads, sends := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/files" {
					uploads++
					_, _ = io.WriteString(w, `{"urls":{"get":"https://image.invalid/upload"}}`)
					return
				}
				sends++
				var wire struct {
					Input struct {
						Image string `json:"image_prompt"`
					} `json:"input"`
				}
				require.NoError(t, common.DecodeJson(r.Body, &wire))
				prefix := "data:image/png;base64,"
				require.True(t, strings.HasPrefix(wire.Input.Image, prefix))
				decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(wire.Input.Image, prefix))
				require.NoError(t, err)
				assert.Equal(t, picture.Bytes(), decoded)
				_, _ = io.WriteString(w, `{}`)
			}))
			t.Cleanup(server.Close)
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			require.NoError(t, writer.WriteField("model", "image"))
			require.NoError(t, writer.WriteField("prompt", "edit"))
			file, err := writer.CreateFormFile("image", "input.png")
			require.NoError(t, err)
			_, err = file.Write(picture.Bytes())
			require.NoError(t, err)
			require.NoError(t, writer.Close())
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
			c.Request.Header.Set("Content-Type", writer.FormDataContentType())
			parsed, err := helper.ParseGatewayMediaRequest(c)
			require.NoError(t, err)
			info := &relaycommon.RelayInfo{ProviderMeta: &relaycommon.ProviderMeta{ChannelType: constant.ChannelTypeReplicate, ChannelBaseUrl: server.URL, UpstreamModelName: "black-forest-labs/flux-1.1-pro", ApiKey: "fake-replicate-key"}, RelayMode: relayconstant.RelayModeImagesEdits, RelayFormat: types.RelayFormatOpenAIImage, ResponseFacts: &relaycommon.ResponseFacts{}}
			if invalid {
				info.ParamOverride = map[string]any{"input": map[string]any{"num_outputs": 129}}
			}
			adaptor, err := GetChannelAdaptor(constant.APITypeReplicate, protocol.OpenAIImages)
			require.NoError(t, err)
			prepared, err := adaptor.PrepareMediaRequest(c, info, parsed)
			assert.Zero(t, uploads)
			assert.Zero(t, sends)
			if invalid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			response, err := adaptor.DoPreparedRequest(c, info, prepared.Prepared)
			require.NoError(t, err)
			require.NoError(t, response.(*http.Response).Body.Close())
			assert.Equal(t, 1, sends)
			assert.Zero(t, uploads)
		})
	}
}

func TestMediaImagenUsesNativeWireAndCanonicalImageDelivery(t *testing.T) {
	service.InitHttpClient()
	var sent []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		sent, err = io.ReadAll(r.Body)
		require.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"predictions":[{"bytesBase64Encoded":"aW1hZ2U=","mimeType":"image/png"}]}`)
	}))
	t.Cleanup(upstream.Close)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"public-image","prompt":"draw","n":2}`))
	c.Request.Header.Set("Content-Type", "application/json")
	parsed, err := helper.ParseGatewayMediaRequest(c)
	require.NoError(t, err)
	info := &relaycommon.RelayInfo{ProviderMeta: &relaycommon.ProviderMeta{ChannelType: constant.ChannelTypeGemini, ChannelBaseUrl: upstream.URL, UpstreamModelName: "imagen-4.0-generate-001", ParamOverride: map[string]any{"operations": []any{map[string]any{"path": "parameters.sampleCount", "mode": "set", "value": 4}}}}, RelayMode: relayconstant.RelayModeImagesGenerations, RelayFormat: types.RelayFormatOpenAIImage, ResponseFacts: &relaycommon.ResponseFacts{}}
	adaptor, err := GetChannelAdaptor(constant.APITypeGemini, protocol.OpenAIImages)
	require.NoError(t, err)
	prepared, err := adaptor.PrepareMediaRequest(c, info, parsed)
	require.NoError(t, err)
	assert.Equal(t, 4, *prepared.Input.ImageCount)
	response, err := adaptor.DoPreparedRequest(c, info, prepared.Prepared)
	require.NoError(t, err)
	assert.Equal(t, int64(4), ojson.GetBytes(sent, "parameters.sampleCount").Int())
	_, apiErr := adaptor.DoResponse(c, response.(*http.Response), info)
	require.Nil(t, apiErr)
	assert.True(t, info.ResponseFacts.BusinessOutput)
	require.NotNil(t, info.ResponseFacts.ImageCount)
	assert.Equal(t, int64(1), *info.ResponseFacts.ImageCount)
}

func TestMediaNativeImageProtocolsPreserveFinalQuantities(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		apiType, channelType int
		model                string
		count                uint
		response             string
	}{
		{"Replicate", constant.APITypeReplicate, constant.ChannelTypeReplicate, "black-forest-labs/flux-1.1-pro", 2, `{"status":"succeeded","output":["https://image.invalid/one"]}`},
		{"Jimeng", constant.APITypeJimeng, constant.ChannelTypeJimeng, "jimeng_high_aes_general_v21_L", 1, `{"code":10000,"data":{"image_urls":["https://image.invalid/one"]}}`},
		{"MiniMax", constant.APITypeMiniMax, constant.ChannelTypeMiniMax, "image-01", 2, `{"data":{"image_urls":["https://image.invalid/one"]},"base_resp":{"status_code":0}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(fmt.Sprintf(`{"model":"image","prompt":"draw","n":%d}`, tc.count)))
			c.Request.Header.Set("Content-Type", "application/json")
			parsed, err := helper.ParseGatewayMediaRequest(c)
			require.NoError(t, err)
			info := &relaycommon.RelayInfo{ProviderMeta: &relaycommon.ProviderMeta{ChannelType: tc.channelType, ChannelBaseUrl: "https://provider.invalid", UpstreamModelName: tc.model, ApiKey: "access|secret"}, RelayMode: relayconstant.RelayModeImagesGenerations, RelayFormat: types.RelayFormatOpenAIImage, ResponseFacts: &relaycommon.ResponseFacts{}}
			adaptor, err := GetChannelAdaptor(tc.apiType, protocol.OpenAIImages)
			require.NoError(t, err)
			prepared, err := adaptor.PrepareMediaRequest(c, info, parsed)
			require.NoError(t, err)
			assert.Equal(t, int(tc.count), *prepared.Input.ImageCount)
			response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(tc.response))}
			_, apiErr := adaptor.DoResponse(c, response, info)
			require.Nil(t, apiErr)
			assert.True(t, info.ResponseFacts.BusinessOutput)
			require.NotNil(t, info.ResponseFacts.ImageCount)
			assert.Equal(t, int64(1), *info.ResponseFacts.ImageCount)
		})
	}
}

func TestMediaNativeSpeechProtocolsPrepareAndDeliverActualAudio(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		apiType, channelType int
		response             string
	}{
		{"MiniMax", constant.APITypeMiniMax, constant.ChannelTypeMiniMax, `{"data":{"audio":"0000","status":2},"base_resp":{"status_code":0}}`},
		{"Volcengine", constant.APITypeVolcEngine, constant.ChannelTypeVolcEngine, `{"code":3000,"data":"AAA="}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"speech","input":"hello","voice":"alloy","response_format":"pcm"}`))
			c.Request.Header.Set("Content-Type", "application/json")
			parsed, err := helper.ParseGatewayMediaRequest(c)
			require.NoError(t, err)
			info := &relaycommon.RelayInfo{ProviderMeta: &relaycommon.ProviderMeta{ChannelType: tc.channelType, ChannelBaseUrl: "https://provider.invalid", UpstreamModelName: "speech-model", ApiKey: "access|secret"}, RelayMode: relayconstant.RelayModeAudioSpeech, RelayFormat: types.RelayFormatOpenAIAudio, ResponseFacts: &relaycommon.ResponseFacts{}}
			info.OriginModelName = "speech-model"
			adaptor, err := GetChannelAdaptor(tc.apiType, protocol.OpenAIAudioSpeech)
			require.NoError(t, err)
			prepared, err := adaptor.PrepareMediaRequest(c, info, parsed)
			require.NoError(t, err)
			assert.Equal(t, "hello", prepared.Request.(*dto.AudioRequest).Input)
			response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.response))}
			_, apiErr := adaptor.DoResponse(c, response, info)
			require.Nil(t, apiErr)
			assert.True(t, info.ResponseFacts.BusinessOutput)
			assert.Equal(t, int64(2), info.ResponseFacts.GeneratedAudioBytes)
		})
	}
}

func TestMediaTranscriptionResponseUsesEffectiveFormat(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("model", "transcription"))
	require.NoError(t, writer.WriteField("response_format", "json"))
	file, err := writer.CreateFormFile("file", "input.wav")
	require.NoError(t, err)
	_, err = file.Write([]byte("audio bytes"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())
	parsed, err := helper.ParseGatewayMediaRequest(c)
	require.NoError(t, err)
	info := &relaycommon.RelayInfo{ProviderMeta: &relaycommon.ProviderMeta{ChannelType: constant.ChannelTypeOpenAI, ChannelBaseUrl: "https://provider.invalid", UpstreamModelName: "whisper-1", ParamOverride: map[string]any{"response_format": "text"}}, RelayMode: relayconstant.RelayModeAudioTranscription, RelayFormat: types.RelayFormatOpenAIAudio, ResponseFacts: &relaycommon.ResponseFacts{}}
	adaptor, err := GetChannelAdaptor(constant.APITypeOpenAI, protocol.OpenAIAudioTranscription)
	require.NoError(t, err)
	prepared, err := adaptor.PrepareMediaRequest(c, info, parsed)
	require.NoError(t, err)
	assert.Equal(t, "text", prepared.Request.(*dto.AudioRequest).ResponseFormat)
	_, apiErr := adaptor.DoResponse(c, &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/plain"}}, Body: io.NopCloser(strings.NewReader("hello"))}, info)
	require.Nil(t, apiErr)
	assert.Equal(t, "hello", recorder.Body.String())
	assert.True(t, info.ResponseFacts.BusinessOutput)
	assert.Equal(t, "hello", info.ResponseFacts.GeneratedText)
}

func TestMediaCloudflareTranscriptionUsesRawAudioAndCanonicalText(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("model", "transcription"))
	file, err := writer.CreateFormFile("file", "input.wav")
	require.NoError(t, err)
	_, err = file.Write([]byte("audio bytes"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())
	parsed, err := helper.ParseGatewayMediaRequest(c)
	require.NoError(t, err)
	info := &relaycommon.RelayInfo{ProviderMeta: &relaycommon.ProviderMeta{ChannelType: constant.ChannelCloudflare, ChannelBaseUrl: "https://provider.invalid", UpstreamModelName: "@cf/openai/whisper", ApiKey: "account|secret"}, RelayMode: relayconstant.RelayModeAudioTranscription, RelayFormat: types.RelayFormatOpenAIAudio, ResponseFacts: &relaycommon.ResponseFacts{}}
	adaptor, err := GetChannelAdaptor(constant.APITypeCloudflare, protocol.OpenAIAudioTranscription)
	require.NoError(t, err)
	prepared, err := adaptor.PrepareMediaRequest(c, info, parsed)
	require.NoError(t, err)
	assert.Equal(t, "application/octet-stream", prepared.Prepared.Headers().Get("Content-Type"))
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"success":true,"result":{"text":"hello"}}`))}
	_, apiErr := adaptor.DoResponse(c, response, info)
	require.Nil(t, apiErr)
	assert.True(t, info.ResponseFacts.BusinessOutput)
	assert.Equal(t, "hello", info.ResponseFacts.GeneratedText)
}

func TestImageRequestReservesFinalQuantityBeforeUpstream(t *testing.T) {
	service.InitHttpClient()
	for _, tc := range []struct {
		name, body                                    string
		tiered, passThrough, insufficient, retryReset bool
		override                                      any
		count, status                                 int
	}{
		{name: "top-level count", body: `{"model":"gpt-image-2","n":2,"parameters":{}}`, count: 2},
		{name: "legacy channel quantity override", body: `{"model":"gpt-image-2","n":1}`, override: 4, count: 4},
		{name: "expression channel quantity override", body: `{"model":"gpt-image-2","n":1}`, override: 4, count: 4, tiered: true},
		{name: "pass-through quantity", body: `{"model":"gpt-image-2","n":2,"parameters":{}}`, count: 2, passThrough: true},
		{name: "oversized override rejected", body: `{"model":"gpt-image-2","n":1}`, override: 129, status: http.StatusBadRequest},
		{name: "insufficient reservation blocks upstream", body: `{"model":"gpt-image-2","n":1}`, override: 4, count: 4, insufficient: true, status: http.StatusForbidden},
		{name: "retry drops the previous attempt's quantity", body: `{"model":"gpt-image-2","n":1}`, count: 1, retryReset: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			received := make(chan []byte, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err == nil {
					received <- body
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadGateway)
				_, _ = io.WriteString(w, `{"error":{"message":"fixture upstream failure","type":"upstream_error"}}`)
			}))
			t.Cleanup(upstream.Close)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
			common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, upstream.URL)
			common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ProviderSettings{PassThroughBodyEnabled: tc.passThrough})
			if tc.override != nil {
				common.SetContextKey(c, constant.ContextKeyChannelParamOverride, map[string]any{"operations": []any{map[string]any{"path": "n", "mode": "set", "value": tc.override}}})
			}
			request, err := helper.GetAndValidOpenAIImageRequest(c, relayconstant.RelayModeImagesGenerations)
			require.NoError(t, err)
			reservation := &imageReservation{held: 20000, limit: 500000}
			if tc.insufficient {
				reservation.limit = reservation.held
			}
			info := &relaycommon.RelayInfo{Request: request, OriginModelName: "gpt-image-2", RelayMode: relayconstant.RelayModeImagesGenerations,
				RequestURLPath: c.Request.URL.Path, Billing: reservation,
				PriceData: hosttypes.PriceData{UsePrice: true, ModelPrice: 0.04, GroupRatioInfo: hosttypes.GroupRatioInfo{GroupRatio: 1}},
			}
			if tc.tiered {
				expr := `tier("image", fixed(0.04)) * image_count`
				info.TieredBillingSnapshot = &billingexpr.BillingSnapshot{BillingMode: "tiered_expr", ExprString: expr, ExprHash: billingexpr.ExprHashString(expr), GroupRatio: 1, QuotaPerUnit: common.QuotaPerUnit, EstimatedImageCount: common.GetPointer(1)}
				info.BillingRequestInput = &billingexpr.RequestInput{Body: []byte(tc.body), ImageCount: common.GetPointer(1)}
				info.PriceData.UsePrice = false
			}
			if tc.retryReset {
				reservation.held = 80000
				info.PriceData.AddOtherRatio("n", 4)
				info.BillingImageCount = common.GetPointer(3)
			}
			apiErr := ImageHelper(c, info)
			require.NotNil(t, apiErr)
			if tc.status != 0 {
				assert.Equal(t, tc.status, apiErr.StatusCode)
				assert.Empty(t, received, "no upstream submission before quantity validation and reservation")
				return
			}
			assert.Equal(t, http.StatusBadGateway, apiErr.StatusCode)
			require.Len(t, received, 1)
			body := <-received
			assert.Equal(t, int64(tc.count), ojson.GetBytes(body, "n").Int())
			assert.Equal(t, tc.count*20000, info.PriceData.QuotaToPreConsume)
			assert.GreaterOrEqual(t, reservation.held, info.PriceData.QuotaToPreConsume)
			assert.Nil(t, info.BillingImageCount)
			if tc.tiered {
				assert.Equal(t, tc.body, string(info.BillingRequestInput.Body))
				assert.Equal(t, 1, *info.BillingRequestInput.ImageCount)
			}
		})
	}
}

func TestOptInSafeToolLossRejectedAsBadRequestWithAdminDiagnostics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	info := &relaycommon.RelayInfo{
		OriginModelName: "gpt-4o",
		ProviderMeta: &relaycommon.ProviderMeta{
			UpstreamModelName: "gpt-4o",
			ChannelOtherSettings: dto.ProviderOtherSettings{
				ToolLossPolicy: string(types.ConversionLossPolicySafe),
			},
		},
	}

	tools, err := common.Marshal([]map[string]any{{"codeExecution": map[string]any{}}})
	require.NoError(t, err)
	req := &dto.GeminiChatRequest{
		Contents: []dto.GeminiChatContent{
			{Role: "user", Parts: []dto.GeminiPart{{Text: "run this"}}},
		},
		Tools: tools,
	}

	result, convErr := service.ConvertRequest(c, info, types.RelayFormatOpenAI, req)
	require.Error(t, convErr)
	var loss *types.ConversionLossError
	require.ErrorAs(t, convErr, &loss)
	require.NotEmpty(t, loss.Diagnostics)
	require.NotNil(t, result)

	apiErr := newConvertRequestFailedError(c, info, convErr)
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
	assert.Equal(t, types.ErrorCodeConvertRequestFailed, apiErr.GetErrorCode())
	assert.True(t, types.IsSkipRetryError(apiErr))

	diagnostics := info.ConversionDiagnostics()
	require.NotEmpty(t, diagnostics)
	assert.True(t, hasHostDiagnosticCode(diagnostics, "unsupported_hosted_tool"))

	other := service.GenerateTextOtherInfo(c, info, 1, 1, 1, 0, 0, 0, 1)
	adminInfo, ok := other.Snapshot()["admin_info"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, adminInfo, "conversion_diagnostics")
}

func TestUnknownModelModifierIsBadRequestWithoutRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	info := &relaycommon.RelayInfo{
		OriginModelName: "m@thinkin:on",
		ProviderMeta: &relaycommon.ProviderMeta{
			UpstreamModelName: "m@thinkin:on",
		},
	}
	err := helper.ApplyReasoningModelSuffix(c, info)
	require.Error(t, err)
	require.True(t, kitreasoning.IsClientError(err))
	assert.Contains(t, err.Error(), `unsupported model modifier "thinkin"`)
	assert.Contains(t, err.Error(), "re:")

	apiErr := newConvertRequestFailedError(c, info, err)
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
	assert.Equal(t, types.ErrorCodeConvertRequestFailed, apiErr.GetErrorCode())
	assert.True(t, types.IsSkipRetryError(apiErr))
}

func hasHostDiagnosticCode(diagnostics []types.ConversionDiagnostic, code string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}

func TestGeminiThinkingControlsConvertBestEffort(t *testing.T) {
	gin.SetMode(gin.TestMode)
	settings := model_setting.GetGeminiSettings()
	originalAdapter := settings.ThinkingAdapterEnabled
	t.Cleanup(func() { settings.ThinkingAdapterEnabled = originalAdapter })

	t.Run("openai thinking_budget on gemini 3 becomes thinkingLevel", func(t *testing.T) {
		settings.ThinkingAdapterEnabled = false
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req := &dto.GeneralOpenAIRequest{
			Model:     "gemini-3.1-pro-preview-thinking",
			Messages:  []dto.Message{{Role: "user", Content: "hello"}},
			ExtraBody: []byte(`{"google":{"thinking_config":{"thinking_budget":8192}}}`),
		}
		info := &relaycommon.RelayInfo{
			RelayFormat:     types.RelayFormatOpenAI,
			OriginModelName: "gemini-3.1-pro-preview-thinking",
			Request:         req,
			ProviderMeta:    &relaycommon.ProviderMeta{UpstreamModelName: "gemini-3.1-pro-preview"},
		}
		require.NoError(t, helper.ApplyReasoningModelSuffix(c, info, req))

		result, err := service.ConvertRequest(c, info, types.RelayFormatGemini, req)
		require.NoError(t, err)
		converted, ok := result.Value.(*dto.GeminiChatRequest)
		require.True(t, ok)
		require.NotNil(t, converted.GenerationConfig.ThinkingConfig)
		assert.Equal(t, "medium", converted.GenerationConfig.ThinkingConfig.ThinkingLevel)
		assert.Nil(t, converted.GenerationConfig.ThinkingConfig.ThinkingBudget)
		assert.True(t, hasHostDiagnosticCode(info.ConversionDiagnostics(), "gemini_budget_to_level"))
	})

	t.Run("thinking alias with native thinking_level keeps the level", func(t *testing.T) {
		settings.ThinkingAdapterEnabled = true
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req := &dto.GeneralOpenAIRequest{
			Model:     "gemini-3.1-pro-preview-thinking",
			Messages:  []dto.Message{{Role: "user", Content: "hello"}},
			ExtraBody: []byte(`{"google":{"thinking_config":{"thinking_level":"low"}}}`),
		}
		info := &relaycommon.RelayInfo{
			RelayFormat:     types.RelayFormatOpenAI,
			OriginModelName: "gemini-3.1-pro-preview-thinking",
			Request:         req,
			ProviderMeta:    &relaycommon.ProviderMeta{UpstreamModelName: "gemini-3.1-pro-preview-thinking"},
		}
		require.NoError(t, helper.ApplyReasoningModelSuffix(c, info, req))
		assert.Equal(t, "gemini-3.1-pro-preview", info.UpstreamModelName)

		result, err := service.ConvertRequest(c, info, types.RelayFormatGemini, req)
		require.NoError(t, err)
		converted, ok := result.Value.(*dto.GeminiChatRequest)
		require.True(t, ok)
		require.NotNil(t, converted.GenerationConfig.ThinkingConfig)
		assert.Equal(t, "low", converted.GenerationConfig.ThinkingConfig.ThinkingLevel)
		assert.Nil(t, converted.GenerationConfig.ThinkingConfig.ThinkingBudget)
	})

	t.Run("gemini thinkingBudget converts to openai reasoning_effort", func(t *testing.T) {
		settings.ThinkingAdapterEnabled = false
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-3.1-pro-preview:generateContent", nil)
		budget := 8192
		req := &dto.GeminiChatRequest{
			Contents: []dto.GeminiChatContent{{Role: "user", Parts: []dto.GeminiPart{{Text: "hello"}}}},
			GenerationConfig: dto.GeminiChatGenerationConfig{
				ThinkingConfig: &dto.GeminiThinkingConfig{ThinkingBudget: &budget},
			},
		}
		info := &relaycommon.RelayInfo{
			RelayFormat:     types.RelayFormatGemini,
			OriginModelName: "gemini-3.1-pro-preview",
			Request:         req,
			ProviderMeta:    &relaycommon.ProviderMeta{UpstreamModelName: "gpt-5"},
		}

		result, err := service.ConvertRequest(c, info, types.RelayFormatOpenAI, req)
		require.NoError(t, err)
		converted, ok := result.Value.(*dto.GeneralOpenAIRequest)
		require.True(t, ok)
		assert.Equal(t, "medium", converted.ReasoningEffort)
		assert.True(t, hasHostDiagnosticCode(info.ConversionDiagnostics(), "gemini_budget_to_level"))
	})
}

// An Ali image model the alibaba task plugin does not claim is rejected by the
// adaptor with a classified 400 that skips retries. ImageHelper must surface
// that classification unchanged instead of wrapping it into a retryable
// conversion failure that other channels would then be asked to serve.
func TestImageHelperKeepsAdaptorClassifiedConvertError(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"wanx-style-repaint-v1","prompt":"a cat"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeAli)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://dashscope.invalid")
	request, err := helper.GetAndValidOpenAIImageRequest(c, relayconstant.RelayModeImagesGenerations)
	require.NoError(t, err)
	info := &relaycommon.RelayInfo{Request: request, OriginModelName: "wanx-style-repaint-v1", RelayMode: relayconstant.RelayModeImagesGenerations,
		RequestURLPath: c.Request.URL.Path, Billing: &imageReservation{limit: 500000},
		PriceData: hosttypes.PriceData{UsePrice: true, ModelPrice: 0.04, GroupRatioInfo: hosttypes.GroupRatioInfo{GroupRatio: 1}},
	}

	apiErr := ImageHelper(c, info)

	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
	assert.Equal(t, types.ErrorCodeInvalidRequest, apiErr.GetErrorCode())
	assert.True(t, types.IsSkipRetryError(apiErr), "other channels must not be asked to serve the same name")
	assert.Contains(t, apiErr.Error(), "not served by the alibaba task plugin")
}
