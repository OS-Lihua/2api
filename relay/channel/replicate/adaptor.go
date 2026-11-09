package replicate

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
)

type Protocol struct {
}

func (a *Protocol) Init(info *relaycommon.RelayInfo) {
}

func (a *Provider) RequestURL(metadata channel.ProviderMetadata) (string, error) {
	info := metadata.ProviderRequest()
	if info == nil {
		return "", errors.New("replicate adaptor: relay info is nil")
	}
	if info.ChannelBaseUrl == "" {
		info.ChannelBaseUrl = constant.GetChannelBaseURL(constant.ChannelTypeReplicate)
	}
	requestPath := info.RequestURLPath
	if requestPath == "" {
		return info.ChannelBaseUrl, nil
	}
	return channel.ProviderRequestURL(info.ChannelBaseUrl, requestPath, info.ChannelType), nil
}

func (a *Provider) SetupHeader(c *gin.Context, req *http.Header, metadata channel.ProviderMetadata) error {
	info := metadata.ProviderRequest()
	if info == nil {
		return errors.New("replicate adaptor: relay info is nil")
	}
	if info.ApiKey == "" {
		return errors.New("replicate adaptor: api key is required")
	}
	channel.SetupProviderRequestHeader(info, c, req)
	req.Set("Authorization", "Bearer "+info.ApiKey)
	req.Set("Prefer", "wait")
	if req.Get("Content-Type") == "" {
		req.Set("Content-Type", "application/json")
	}
	if req.Get("Accept") == "" {
		req.Set("Accept", "application/json")
	}
	return nil
}

func (a *Protocol) ConvertImageRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.ImageRequest) (any, error) {
	if info == nil {
		return nil, errors.New("replicate adaptor: relay info is nil")
	}
	if info.ResponseFacts != nil && info.RelayMode != relayconstant.RelayModeImagesEdits {
		for _, value := range []common.RawMessage{request.Image, request.Images, request.Mask} {
			if len(value) > 0 && string(value) != "null" {
				return nil, errors.New("replicate adaptor cannot preserve standard image inputs for generation")
			}
		}
	}
	if strings.TrimSpace(request.Prompt) == "" {
		if v := c.PostForm("prompt"); strings.TrimSpace(v) != "" {
			request.Prompt = v
		}
	}
	if strings.TrimSpace(request.Prompt) == "" {
		return nil, errors.New("replicate adaptor: prompt is required")
	}

	modelName := strings.TrimSpace(info.UpstreamModelName)
	if modelName == "" {
		modelName = strings.TrimSpace(request.Model)
	}
	if modelName == "" {
		modelName = ModelFlux11Pro
	}
	info.UpstreamModelName = modelName

	info.RequestURLPath = fmt.Sprintf("/v1/models/%s/predictions", modelName)

	inputPayload := make(map[string]any)
	inputPayload["prompt"] = request.Prompt

	if size := strings.TrimSpace(request.Size); size != "" {
		if aspect, width, height, ok := mapOpenAISizeToFlux(size); ok {
			if aspect != "" {
				if aspect == "custom" {
					inputPayload["aspect_ratio"] = "custom"
					if width > 0 {
						inputPayload["width"] = width
					}
					if height > 0 {
						inputPayload["height"] = height
					}
				} else {
					inputPayload["aspect_ratio"] = aspect
				}
			}
		}
	}

	if len(request.OutputFormat) > 0 {
		var outputFormat string
		if err := common.Unmarshal(request.OutputFormat, &outputFormat); err == nil && strings.TrimSpace(outputFormat) != "" {
			inputPayload["output_format"] = outputFormat
		}
	}

	if imageN := lo.FromPtrOr(request.N, uint(0)); imageN > 0 {
		inputPayload["num_outputs"] = int(imageN)
	}

	if strings.EqualFold(request.Quality, "hd") || strings.EqualFold(request.Quality, "high") {
		inputPayload["prompt_upsampling"] = true
	}

	if info.RelayMode == relayconstant.RelayModeImagesEdits && info.ResponseFacts != nil {
		if len(request.Mask) > 0 && string(request.Mask) != "null" {
			return nil, errors.New("replicate adaptor cannot preserve an image mask")
		}
		form := c.Request.MultipartForm
		if form == nil {
			var err error
			form, err = common.ParseMultipartFormReusable(c)
			if err != nil {
				return nil, err
			}
		}
		var fileHeader *multipart.FileHeader
		for field, files := range form.File {
			if field == "mask" && len(files) > 0 {
				return nil, errors.New("replicate adaptor cannot preserve an image mask")
			}
			if field != "image" && !strings.HasPrefix(field, "image[") && field != "image_prompt" {
				continue
			}
			for _, file := range files {
				if fileHeader != nil {
					return nil, errors.New("replicate adaptor cannot preserve multiple input images")
				}
				fileHeader = file
			}
		}
		if len(form.Value["mask"]) > 0 {
			return nil, errors.New("replicate adaptor cannot preserve an image mask")
		}
		if fileHeader == nil {
			return nil, errors.New("replicate adaptor: image file is required for edits")
		}
		file, err := fileHeader.Open()
		if err != nil {
			return nil, errors.New("replicate adaptor: failed to open image file")
		}
		defer file.Close()
		image, err := io.ReadAll(file)
		if err != nil {
			return nil, errors.New("replicate adaptor: failed to read image file")
		}
		// Replicate accepts data URIs as prediction file inputs. Send the image
		// in the funded prediction instead of uploading during conversion.
		inputPayload["image_prompt"] = "data:" + http.DetectContentType(image) + ";base64," + base64.StdEncoding.EncodeToString(image)
	} else if info.RelayMode == relayconstant.RelayModeImagesEdits {
		metadata := channel.NewProviderMetadata(info)
		overrides, err := channel.ResolveHeaderOverride(info, c)
		if err != nil {
			return nil, errors.New("replicate adaptor: invalid upload header override")
		}
		metadata.RequestHeaders = make(http.Header, len(overrides))
		for name, value := range overrides {
			metadata.RequestHeaders.Set(name, value)
		}
		imageURL, err := (&Provider{}).uploadFileFromForm(c, metadata, "image", "image[]", "image_prompt")
		if err != nil {
			return nil, err
		}
		if imageURL == "" {
			return nil, errors.New("replicate adaptor: image file is required for edits")
		}
		inputPayload["image_prompt"] = imageURL
	}

	if len(request.ExtraFields) > 0 {
		var extra map[string]any
		if err := common.Unmarshal(request.ExtraFields, &extra); err != nil {
			return nil, fmt.Errorf("replicate adaptor: failed to decode extra_fields: %w", err)
		}
		for key, val := range extra {
			inputPayload[key] = val
		}
	}

	for key, raw := range request.Extra {
		if strings.EqualFold(key, "input") {
			var extraInput map[string]any
			if err := common.Unmarshal(raw, &extraInput); err != nil {
				return nil, fmt.Errorf("replicate adaptor: failed to decode extra input: %w", err)
			}
			for k, v := range extraInput {
				inputPayload[k] = v
			}
			continue
		}
		if raw == nil {
			continue
		}
		var val any
		if err := common.Unmarshal(raw, &val); err != nil {
			return nil, fmt.Errorf("replicate adaptor: failed to decode extra field %s: %w", key, err)
		}
		inputPayload[key] = val
	}

	if rawCount, ok := inputPayload["num_outputs"]; ok {
		encoded, err := common.Marshal(rawCount)
		if err != nil {
			return nil, fmt.Errorf("invalid num_outputs: %w", err)
		}
		var count uint
		if err := common.Unmarshal(encoded, &count); err != nil || count == 0 || count > dto.MaxImageN {
			return nil, fmt.Errorf("num_outputs must be an integer between 1 and %d", dto.MaxImageN)
		}
	}
	if info.ResponseFacts != nil {
		c.Request.Header.Set("Content-Type", "application/json")
		return imageRequest{Input: inputPayload, requireImage: info.RelayMode == relayconstant.RelayModeImagesEdits}, nil
	}
	return map[string]any{
		"input": inputPayload,
	}, nil
}

func (a *Protocol) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (any, error) {
	return channel.DoApiRequest(a, c, info, requestBody)
}

func (a *Protocol) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	if resp == nil {
		return nil, types.NewError(errors.New("replicate adaptor: empty response"), types.ErrorCodeBadResponse)
	}

	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if info.ResponseFacts != nil {
		var data []dto.ImageData
		output := ojson.GetBytes(responseBody, "output")
		if output.Type == ojson.String {
			if value := strings.TrimSpace(output.Str); value != "" {
				data = append(data, dto.ImageData{Url: value})
			}
		} else if output.IsArray() {
			for _, item := range output.Array() {
				if item.Type == ojson.String && strings.TrimSpace(item.Str) != "" {
					data = append(data, dto.ImageData{Url: item.Str})
				}
			}
		}
		openai.ObserveImageGeneration(info, data)
	}
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeReadResponseBodyFailed)
	}

	var prediction PredictionResponse
	if err := common.Unmarshal(responseBody, &prediction); err != nil {
		return nil, types.NewError(fmt.Errorf("replicate adaptor: failed to decode response: %w", err), types.ErrorCodeBadResponseBody)
	}

	if prediction.Error != nil {
		errMsg := prediction.Error.Message
		if errMsg == "" {
			errMsg = prediction.Error.Detail
		}
		if errMsg == "" {
			errMsg = prediction.Error.Code
		}
		if errMsg == "" {
			errMsg = "replicate adaptor: prediction error"
		}
		return nil, types.NewError(errors.New(errMsg), types.ErrorCodeBadResponse)
	}

	if prediction.Status != "" && !strings.EqualFold(prediction.Status, "succeeded") {
		return nil, types.NewError(fmt.Errorf("replicate adaptor: prediction status %q", prediction.Status), types.ErrorCodeBadResponse)
	}

	var urls []string

	appendOutput := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		urls = append(urls, value)
	}

	switch output := prediction.Output.(type) {
	case string:
		appendOutput(output)
	case []any:
		for _, item := range output {
			if str, ok := item.(string); ok {
				appendOutput(str)
			}
		}
	case nil:
		// no output
	default:
		if str, ok := output.(fmt.Stringer); ok {
			appendOutput(str.String())
		}
	}

	if len(urls) == 0 {
		return nil, types.NewError(errors.New("replicate adaptor: empty prediction output"), types.ErrorCodeBadResponseBody)
	}
	if info.ResponseFacts != nil && len(urls) <= dto.MaxImageN {
		count := int64(len(urls))
		info.ResponseFacts.GeneratedImageCount = count
		info.ResponseFacts.ImageCount = &count
	}

	var imageReq *dto.ImageRequest
	if info != nil {
		if req, ok := info.Request.(*dto.ImageRequest); ok {
			imageReq = req
		}
	}

	wantsBase64 := imageReq != nil && strings.EqualFold(imageReq.ResponseFormat, "b64_json")

	imageResponse := dto.ImageResponse{
		Created: common.GetTimestamp(),
		Data:    make([]dto.ImageData, 0),
	}

	if wantsBase64 {
		converted, convErr := downloadImagesToBase64(c.Request.Context(), urls)
		if convErr != nil {
			return nil, types.NewError(convErr, types.ErrorCodeBadResponse)
		}
		for _, content := range converted {
			if content == "" {
				continue
			}
			imageResponse.Data = append(imageResponse.Data, dto.ImageData{B64Json: content})
		}
	} else {
		for _, url := range urls {
			if url == "" {
				continue
			}
			imageResponse.Data = append(imageResponse.Data, dto.ImageData{Url: url})
		}
	}

	if len(imageResponse.Data) == 0 {
		return nil, types.NewError(errors.New("replicate adaptor: no usable image data"), types.ErrorCodeBadResponse)
	}

	responseBytes, err := common.Marshal(imageResponse)
	if err != nil {
		return nil, types.NewError(fmt.Errorf("replicate adaptor: encode response failed: %w", err), types.ErrorCodeBadResponseBody)
	}

	if info.ResponseFacts != nil {
		return openai.WriteImageResponse(c, info, resp, responseBytes)
	}
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(http.StatusOK)
	_, _ = c.Writer.Write(responseBytes)

	usage := &dto.Usage{}
	return usage, nil
}

func (a *Protocol) GetModelList() []string {
	return ModelList
}

func (a *Protocol) GetChannelName() string {
	return ChannelName
}

func downloadImagesToBase64(ctx context.Context, urls []string) ([]string, error) {
	results := make([]string, 0, len(urls))
	for _, url := range urls {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strings.TrimSpace(url) == "" {
			continue
		}
		_, data, err := service.GetImageFromURLWithContext(ctx, url)
		if err != nil {
			return nil, fmt.Errorf("replicate adaptor: failed to download image from %s: %w", url, err)
		}
		results = append(results, data)
	}
	return results, nil
}

func mapOpenAISizeToFlux(size string) (aspect string, width int, height int, ok bool) {
	parts := strings.Split(size, "x")
	if len(parts) != 2 {
		return "", 0, 0, false
	}
	w, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	h, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return "", 0, 0, false
	}

	switch {
	case w == h:
		return "1:1", 0, 0, true
	case w == 1792 && h == 1024:
		return "16:9", 0, 0, true
	case w == 1024 && h == 1792:
		return "9:16", 0, 0, true
	case w == 1536 && h == 1024:
		return "3:2", 0, 0, true
	case w == 1024 && h == 1536:
		return "2:3", 0, 0, true
	}

	rw, rh := reduceRatio(w, h)
	ratioStr := fmt.Sprintf("%d:%d", rw, rh)
	switch ratioStr {
	case "1:1", "16:9", "9:16", "3:2", "2:3", "4:5", "5:4", "3:4", "4:3":
		return ratioStr, 0, 0, true
	}

	width = normalizeFluxDimension(w)
	height = normalizeFluxDimension(h)
	return "custom", width, height, true
}

func reduceRatio(w, h int) (int, int) {
	g := gcd(w, h)
	if g == 0 {
		return w, h
	}
	return w / g, h / g
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	if a < 0 {
		return -a
	}
	return a
}

func normalizeFluxDimension(value int) int {
	const (
		minDim = 256
		maxDim = 1440
		step   = 32
	)
	if value < minDim {
		value = minDim
	}
	if value > maxDim {
		value = maxDim
	}
	remainder := value % step
	if remainder != 0 {
		if remainder >= step/2 {
			value += step - remainder
		} else {
			value -= remainder
		}
	}
	if value < minDim {
		value = minDim
	}
	if value > maxDim {
		value = maxDim
	}
	return value
}

func imageFileFromForm(c *gin.Context, fieldCandidates ...string) (*multipart.FileHeader, error) {
	mf := c.Request.MultipartForm
	if mf == nil {
		if _, err := c.MultipartForm(); err != nil {
			return nil, errors.New("replicate adaptor: parse multipart form failed")
		}
		mf = c.Request.MultipartForm
	}
	if mf == nil || len(mf.File) == 0 {
		return nil, nil
	}
	if len(fieldCandidates) == 0 {
		fieldCandidates = []string{"image", "image[]", "image_prompt"}
	}
	for _, key := range fieldCandidates {
		if files := mf.File[key]; len(files) > 0 {
			return files[0], nil
		}
	}
	for _, files := range mf.File {
		if len(files) > 0 {
			return files[0], nil
		}
	}
	return nil, nil
}

func (p *Provider) uploadFileFromForm(c *gin.Context, metadata channel.ProviderMetadata, fieldCandidates ...string) (string, error) {
	if metadata == nil || metadata.ProviderRequest() == nil || c == nil || c.Request == nil {
		return "", errors.New("replicate adaptor: missing upload metadata")
	}
	info := metadata.ProviderRequest()
	fileHeader, err := imageFileFromForm(c, fieldCandidates...)
	if err != nil {
		return "", err
	}
	if fileHeader == nil {
		return "", nil
	}

	file, err := fileHeader.Open()
	if err != nil {
		return "", fmt.Errorf("replicate adaptor: failed to open image file: %w", err)
	}
	defer file.Close()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "content", "filename": fileHeader.Filename}))
	contentType := fileHeader.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	hdr.Set("Content-Type", contentType)

	part, err := writer.CreatePart(hdr)
	if err != nil {
		writer.Close()
		return "", fmt.Errorf("replicate adaptor: create upload form failed: %w", err)
	}
	if _, err := io.Copy(part, file); err != nil {
		writer.Close()
		return "", fmt.Errorf("replicate adaptor: copy image content failed: %w", err)
	}
	formContentType := writer.FormDataContentType()
	writer.Close()

	uploadMetadata := *info
	uploadMetadata.RequestURLPath = "/v1/files"
	uploadURL, err := p.RequestURL(&uploadMetadata)
	if err != nil {
		return "", errors.New("replicate adaptor: invalid upload destination")
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, uploadURL, &body)
	if err != nil {
		return "", errors.New("replicate adaptor: invalid upload URL")
	}
	if err := p.SetupHeader(c, &req.Header, &uploadMetadata); err != nil {
		return "", errors.New("replicate adaptor: invalid upload authentication")
	}
	req.Header.Set("Content-Type", formContentType)
	for name, values := range info.RequestHeaders {
		req.Header[name] = append([]string(nil), values...)
		if strings.EqualFold(name, "Host") {
			req.Host = req.Header.Get(name)
		}
	}
	configuredClient, err := service.GetHttpClientWithProxySettings(info.ChannelSetting.Proxy, info.ChannelSetting.HTTPSettings())
	if err != nil {
		return "", errors.New("replicate adaptor: invalid upload transport configuration")
	}
	client := *configuredClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		if c.Request.Context().Err() != nil {
			return "", c.Request.Context().Err()
		}
		return "", errors.New("replicate adaptor: upload image failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", types.NewErrorWithStatusCode(errors.New("replicate adaptor: upload image failed"), types.ErrorCodeDoRequestFailed, resp.StatusCode)
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", errors.New("replicate adaptor: read upload response failed")
	}

	var uploadResp FileUploadResponse
	if err := common.Unmarshal(respBody, &uploadResp); err != nil {
		return "", fmt.Errorf("replicate adaptor: decode upload response failed: %w", err)
	}
	if uploadResp.Urls.Get == "" {
		return "", errors.New("replicate adaptor: upload response missing url")
	}
	return uploadResp.Urls.Get, nil
}

func (a *Protocol) ConvertOpenAIRequest(*gin.Context, *relaycommon.RelayInfo, *dto.GeneralOpenAIRequest) (any, error) {
	return nil, errors.New("replicate adaptor: ConvertOpenAIRequest is not implemented")
}

func (a *Protocol) ConvertRerankRequest(*gin.Context, int, dto.RerankRequest) (any, error) {
	return nil, errors.New("replicate adaptor: ConvertRerankRequest is not implemented")
}

func (a *Protocol) ConvertEmbeddingRequest(*gin.Context, *relaycommon.RelayInfo, dto.EmbeddingRequest) (any, error) {
	return nil, errors.New("replicate adaptor: ConvertEmbeddingRequest is not implemented")
}

func (a *Protocol) ConvertAudioRequest(*gin.Context, *relaycommon.RelayInfo, dto.AudioRequest) (io.Reader, error) {
	return nil, errors.New("replicate adaptor: ConvertAudioRequest is not implemented")
}

func (a *Protocol) ConvertOpenAIResponsesRequest(*gin.Context, *relaycommon.RelayInfo, dto.OpenAIResponsesRequest) (any, error) {
	return nil, errors.New("replicate adaptor: ConvertOpenAIResponsesRequest is not implemented")
}

func (a *Protocol) ConvertClaudeRequest(*gin.Context, *relaycommon.RelayInfo, *dto.ClaudeRequest) (any, error) {
	return nil, errors.New("replicate adaptor: ConvertClaudeRequest is not implemented")
}

func (a *Protocol) ConvertGeminiRequest(*gin.Context, *relaycommon.RelayInfo, *dto.GeminiChatRequest) (any, error) {
	return nil, errors.New("replicate adaptor: ConvertGeminiRequest is not implemented")
}
