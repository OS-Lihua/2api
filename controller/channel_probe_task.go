package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/relay/channel"
	taskadaptor "github.com/QuantumNous/new-api/relay/channel/task/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const channelProbeArtifactLimit = 8 << 20

// probeTaskChannel exercises the installed plugin's existing decode, submit,
// poll and artifact paths without creating a billed gateway task. All private
// task data stays request-local; only the bounded observation is persisted.
func probeTaskChannel(ctx context.Context, record model.Channel, provider *model.Provider, result *model.ChannelProbeResult, request *dto.GeneralOpenAIRequest) (string, string) {
	key, _ := record.Protocol.TaskPluginKey()
	plugin, ok := pluginruntime.DefaultRegistry.Get(key)
	if !ok {
		return "inconclusive", "protocol_unavailable"
	}
	candidates := []string{"openai_responses", "openai_video", "openai_image"}
	switch result.Capability {
	case "output_video", "max_duration_seconds":
		candidates = []string{"openai_video", "openai_responses"}
	case "output_image", "max_images":
		candidates = []string{"openai_image", "openai_responses"}
	case "embedding_dimensions":
		return "inconclusive", "request_not_representable"
	}
	wire := ""
	stream := false
	for _, candidate := range candidates {
		for _, claim := range plugin.Meta.Protocols {
			if claim.Name != candidate {
				continue
			}
			if len(claim.Models) > 0 && !slices.Contains(claim.Models, record.UpstreamModelName) {
				continue
			}
			if candidate == "openai_responses" {
				if result.Capability == "streaming" && !slices.Contains(claim.Supports, "stream") {
					continue
				}
				stream = result.Capability == "streaming" || !slices.Contains(claim.Supports, "sync") && !slices.Contains(claim.Supports, "background")
			}
			wire = candidate
			break
		}
		if wire != "" {
			break
		}
	}
	if wire == "" {
		return "inconclusive", "request_not_representable"
	}
	apiKey, index, keyErr := provider.GetNextEnabledKey()
	if keyErr != nil {
		return "failed", "credential_unavailable"
	}
	apiType, _ := common.ChannelType2APIType(provider.Type)
	info := &relaycommon.RelayInfo{ProviderMeta: &relaycommon.ProviderMeta{ChannelType: provider.Type, ChannelId: provider.Id, ChannelBaseUrl: provider.GetBaseURL(), ApiKey: apiKey, ApiType: apiType, ChannelMultiKeyIndex: index, ChannelSetting: provider.GetSetting(), ChannelOtherSettings: provider.GetOtherSettings(), HeadersOverride: provider.GetHeaderOverride(), UpstreamModelName: record.UpstreamModelName}, OriginModelName: record.UpstreamModelName, TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: model.GenerateTaskID()}, DisablePing: true, IsChannelTest: true}
	cleanRequest, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/videos", http.NoBody)
	cleanRequest.Header.Set("Content-Type", "application/json")
	writer := &probeResponseWriter{header: make(http.Header)}
	c := &gin.Context{Request: cleanRequest, Writer: writer}
	probeDuration := int64(4)
	schema, examples := plugin.Meta.UsageForModel(record.UpstreamModelName)
	var shortest int64
	for _, example := range examples {
		encoded, err := common.Marshal(example.Facts)
		if err != nil {
			continue
		}
		facts := gjson.ParseBytes(encoded).Map()
		for field, definition := range schema {
			value := facts[field]
			if definition.Unit == "second" && value.Type == gjson.Number && value.Float() >= 1 && value.Float() <= 60 && value.Float() == float64(value.Int()) {
				if shortest == 0 || value.Int() < shortest {
					shortest = value.Int()
				}
			}
		}
	}
	if shortest > 0 {
		probeDuration = shortest
	}
	sampledDuration := false
	if result.Capability == "max_duration_seconds" {
		var parent model.Model
		if err := model.DB.WithContext(ctx).First(&parent, record.ModelID).Error; err != nil {
			return "failed", "request_invalid"
		}
		effective, err := record.EffectiveCapabilities(parent.Capabilities)
		if err != nil {
			return "failed", "request_invalid"
		}
		if effective.Limits.MaxDurationSeconds != nil {
			probeDuration = min(*effective.Limits.MaxDurationSeconds, 60)
			sampledDuration = *effective.Limits.MaxDurationSeconds > 60
		}
	}
	body := map[string]any{"model": record.UpstreamModelName, "prompt": "A small solid red square", "seconds": strconv.FormatInt(probeDuration, 10)}
	recipe := channelProbeRecipe{}
	operation := "create"
	switch wire {
	case "openai_responses":
		recipeRecord := record
		recipeRecord.Protocol = protocol.OpenAIResponses
		var err error
		probeFeature := result.Capability
		switch result.Capability {
		case "output_image", "output_video", "output_audio", "output_pdf", "max_duration_seconds":
			probeFeature = "input_text"
			request.Messages[0].Content = map[string]string{"output_image": "Generate a small solid red square image.", "output_video": "Generate a short video of a solid red square.", "output_audio": "Generate a short audio saying seven.", "output_pdf": "Create a one-page PDF containing GWP4831.", "max_duration_seconds": "Generate the shortest possible audio or video for this model."}[result.Capability]
			request.Metadata = []byte(`{"seconds":` + strconv.FormatInt(probeDuration, 10) + `}`)
		}
		recipe, err = channelProbeRecipeFor(ctx, recipeRecord, probeFeature, request)
		if err != nil {
			return "inconclusive", "request_not_representable"
		}
		if probeFeature != result.Capability {
			recipe.ExpectedText = ""
		}
		if result.Capability == "max_duration_seconds" {
			recipe.TestedValue = probeDuration
			recipe.SampledLimit = sampledDuration
		}
		info.RelayFormat = recipe.Format
		info.Request = recipe.Request
		info.IsStream = stream
		request.Stream = common.GetPointer(stream)
		handler, err := relay.GetProtocolHandler(protocol.OpenAIResponses)
		if err != nil {
			return "inconclusive", "protocol_unavailable"
		}
		converted, err := handler.ConvertRequest(c, info, recipe.Request)
		if err != nil {
			return "inconclusive", "request_not_representable"
		}
		encoded, err := common.Marshal(converted)
		body = nil
		if err != nil || common.Unmarshal(encoded, &body) != nil {
			return "failed", "request_invalid"
		}
	case "openai_image":
		if result.Capability != "input_text" && result.Capability != "input_image" && result.Capability != "output_image" && result.Capability != "max_images" {
			return "inconclusive", "request_not_representable"
		}
		recipeRecord := record
		recipeRecord.Protocol = protocol.OpenAIImages
		var err error
		recipe, err = channelProbeRecipeFor(ctx, recipeRecord, result.Capability, request)
		if err != nil {
			return "inconclusive", "request_not_representable"
		}
		encoded, err := common.Marshal(recipe.Request)
		body = nil
		if err != nil || common.Unmarshal(encoded, &body) != nil {
			return "failed", "request_invalid"
		}
		operation = "generate"
		if result.Capability == "input_image" {
			operation = "edit"
		}
	case "openai_video":
		switch result.Capability {
		case "input_text", "output_video":
		case "max_duration_seconds":
			recipe.TestedValue = probeDuration
			recipe.SampledLimit = sampledDuration
		case "input_image":
			body["input_reference"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(probeRedImage)
		default:
			return "inconclusive", "request_not_representable"
		}
	}
	result.TestedValue = recipe.TestedValue
	// A decoder may inspect the canonical body but cannot receive gateway
	// Authorization or change the trusted Provider destination policy.
	protocolContext := pluginruntime.ProtocolRequestContext{RouteRequestContext: pluginruntime.RouteRequestContext{Method: http.MethodPost, Body: map[string]any{"kind": "json", "value": body}, Headers: map[string]string{"Content-Type": "application/json", "Accept": "application/json"}}, Protocol: wire, Operation: operation, Model: record.UpstreamModelName, UpstreamModel: record.UpstreamModelName, Stream: stream}
	definition, _ := pluginruntime.HostProtocol(wire)
	var selectedOperation pluginruntime.HostProtocolOperation
	for _, candidate := range definition.Operations {
		if candidate.Name == operation {
			selectedOperation = candidate
			break
		}
	}
	if selectedOperation.Name == "" {
		return "inconclusive", "request_not_representable"
	}
	cleanRequest.URL.Path = selectedOperation.Path
	protocolContext.Path = selectedOperation.Path
	c.Set(pluginruntime.ContextKeyPinnedEndpoint, pluginruntime.PinnedEndpoint{Generation: pluginruntime.DefaultRegistry.Generation(), Plugin: plugin, Protocol: wire, Operation: selectedOperation, Model: record.UpstreamModelName})
	c.Set(pluginruntime.ContextKeyProtocolRequest, protocolContext)
	c.Set("resolved_task_model", record.UpstreamModelName)
	adaptor := taskadaptor.New(plugin)
	adaptor.Init(info)
	if taskErr := adaptor.ValidateRequestAndSetAction(c, info); taskErr != nil {
		return "inconclusive", "request_not_representable"
	}
	reader, err := adaptor.BuildRequestBody(c, info)
	if err != nil || reader == nil {
		return "failed", "request_invalid"
	}
	encoded, err := io.ReadAll(io.LimitReader(reader, (1<<20)+1))
	if err != nil || len(encoded) > 1<<20 {
		return "failed", "request_invalid"
	}
	if !strings.HasPrefix(cleanRequest.Header.Get("Content-Type"), "multipart/") {
		if !gjson.ValidBytes(encoded) {
			return "failed", "request_invalid"
		}
		info.ParamOverride = provider.GetParamOverride()
		encoded, err = relaycommon.ApplyParamOverrideWithRelayInfo(encoded, info)
		if err != nil {
			return "failed", "request_invalid"
		}
		info.ParamOverride = nil
		if common.UnmarshalJsonStr(record.ParameterOverrides, &info.ParamOverride) != nil {
			return "failed", "request_invalid"
		}
		encoded, err = relaycommon.ApplyParamOverrideWithRelayInfo(encoded, info)
		if err != nil || !probeTaskRequestWithinBudget(gjson.ParseBytes(encoded)) {
			return "failed", "request_invalid"
		}
	} else {
		if len(provider.GetParamOverride()) > 0 || strings.TrimSpace(record.ParameterOverrides) != "{}" || !probeTaskMultipartWithinBudget(encoded, cleanRequest.Header.Get("Content-Type")) {
			return "failed", "request_invalid"
		}
	}
	if wire == "openai_responses" && !slices.Contains([]string{"output_image", "output_video", "output_audio", "output_pdf", "max_duration_seconds"}, result.Capability) && !probeHasOutputBudget(gjson.ParseBytes(encoded)) {
		return "failed", "request_invalid"
	}
	if result.Capability == "input_image" && !bytes.Contains(encoded, probeRedImage) && !bytes.Contains(encoded, []byte(base64.StdEncoding.EncodeToString(probeRedImage))) {
		return "inconclusive", "request_not_representable"
	}
	response, err := adaptor.DoRequest(c, info, bytes.NewReader(encoded))
	if err != nil {
		return "failed", "transport_failed"
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "failed", "upstream_rejected"
	}
	captured := &bytes.Buffer{}
	response.Body = struct {
		io.Reader
		io.Closer
	}{Reader: io.TeeReader(io.LimitReader(response.Body, (1<<20)+1), captured), Closer: response.Body}
	submitted, taskErr := adaptor.ParseResponse(c, response, info)
	if taskErr != nil || submitted == nil || captured.Len() > 1<<20 {
		return "failed", "response_invalid"
	}
	task := &model.Task{TaskID: info.PublicTaskID, ChannelId: provider.Id, Action: info.Action, Status: model.TaskStatusInProgress, Data: submitted.TaskData, Properties: model.Properties{OriginModelName: record.UpstreamModelName, UpstreamModelName: record.UpstreamModelName}}
	task.PrivateData.UpstreamTaskID = submitted.UpstreamTaskID
	task.PrivateData.PluginState = submitted.PluginState
	task.PrivateData.Key = apiKey
	terminal := submitted.Immediate
	// Poll a bounded number of times. A still-running Provider job is explicitly
	// inconclusive, never a successful capability check based on acceptance.
	for attempt := 0; terminal == nil || terminal.Status != model.TaskStatusSuccess && terminal.Status != model.TaskStatusFailure; attempt++ {
		if attempt == 45 {
			return "inconclusive", "task_pending"
		}
		if attempt > 0 {
			timer := time.NewTimer(2 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "failed", "request_timeout"
			case <-timer.C:
			}
		}
		polled, err := adaptor.FetchTaskWithContext(ctx, info.ChannelBaseUrl, apiKey, task, info.ChannelSetting.Proxy)
		if err != nil {
			return "failed", "transport_failed"
		}
		data, err := io.ReadAll(io.LimitReader(polled.Body, (1<<20)+1))
		polled.Body.Close()
		if err != nil || len(data) > 1<<20 {
			return "failed", "response_invalid"
		}
		if polled.StatusCode < 200 || polled.StatusCode >= 300 {
			return "failed", "upstream_rejected"
		}
		terminal, err = adaptor.ParseTaskResultWithContext(ctx, task, polled, data)
		if err != nil {
			return "failed", "response_invalid"
		}
		task.Data = data
		if terminal.PluginState != nil {
			task.PrivateData.PluginState = terminal.PluginState
		}
	}
	if terminal.Status != model.TaskStatusSuccess {
		return "failed", "upstream_rejected"
	}
	task.Status = model.TaskStatusSuccess
	if result.Capability == "caching" && (observeProbeJSON(protocol.OpenAIResponses, "caching", task.Data) || observeProbeJSON(protocol.GeminiGenerate, "caching", task.Data) || observeProbeJSON(protocol.AnthropicMessages, "caching", task.Data)) {
		return "passed", "verified"
	}
	if wire == "openai_responses" || wire == "openai_image" {
		view, err := service.BuildTaskPluginView(task)
		if err != nil {
			return "failed", "response_invalid"
		}
		viewValue, err := taskPluginProtocolJSONValue(view)
		if err != nil {
			return "failed", "response_invalid"
		}
		renderContext := protocolContext.JSValueFor(plugin.Meta)
		artifacts, artifactErr := taskadaptor.RendererArtifactsWithContext(ctx, plugin, task, service.BuildTaskArtifactContentURL)
		if artifactErr != nil {
			return "failed", "response_invalid"
		}
		renderContext["artifacts"] = artifacts
		if wire == "openai_responses" && stream {
			value, err := plugin.Engine.CallPath(ctx, "protocols", []string{wire, "renderEvents"}, renderContext, viewValue, nil)
			if err != nil {
				return "inconclusive", "behavior_not_observed"
			}
			limits := relay.DefaultPluginProtocolLimits()
			eventResult, err := relay.DecodePluginProtocolEventResult(value, limits)
			if err != nil {
				return "failed", "response_invalid"
			}
			machine := relay.NewPluginResponsesMachine(task.TaskID, record.UpstreamModelName, task.SubmitTime, limits)
			created, err := machine.CreatedEvent()
			if err != nil {
				return "failed", "response_invalid"
			}
			if writeTaskPluginProtocolEvent(c, created) != nil {
				return "failed", "response_too_large"
			}
			events, err := machine.ApplyTick(eventResult, string(task.Status))
			if err != nil {
				return "failed", "response_invalid"
			}
			for _, event := range events {
				if writeTaskPluginProtocolEvent(c, event) != nil {
					return "failed", "response_too_large"
				}
			}
			if result.Capability == "streaming" {
				return observeProbeStream(protocol.OpenAIResponses, "text/event-stream", writer.body.Bytes())
			}
			final, err := common.Marshal(events[len(events)-1].Response)
			if err == nil && observeChannelProbe(recipe, protocol.OpenAIResponses, result.Capability, final) {
				if recipe.SampledLimit {
					return "inconclusive", "limit_sample_only"
				}
				return "passed", "verified"
			}
		} else {
			hook := "renderFinal"
			if wire == "openai_image" {
				hook = "render"
			}
			rendered, err := plugin.Engine.CallPath(ctx, "protocols", []string{wire, hook}, renderContext, viewValue)
			if err == nil && wire == "openai_responses" {
				machine := relay.NewPluginResponsesMachine(task.TaskID, record.UpstreamModelName, task.SubmitTime, relay.DefaultPluginProtocolLimits())
				rendered, err = machine.FinalResponse(rendered, string(task.Status))
			}
			if err == nil {
				data, encodeErr := common.Marshal(rendered)
				if encodeErr == nil {
					kind := protocol.OpenAIResponses
					if wire == "openai_image" {
						kind = protocol.OpenAIImages
					}
					if observeChannelProbe(recipe, kind, result.Capability, data) {
						if recipe.SampledLimit {
							return "inconclusive", "limit_sample_only"
						}
						return "passed", "verified"
					}
				}
			}
		}
	}
	artifacts, err := adaptor.ListArtifactsWithContext(ctx, task)
	if err != nil {
		return "inconclusive", "behavior_not_observed"
	}
	wanted := ""
	switch result.Capability {
	case "output_video":
		wanted = "video"
	case "input_text":
		if wire == "openai_video" {
			wanted = "video"
		} else if wire == "openai_image" {
			wanted = "image"
		}
	case "input_image":
		// The final request already proved that the reference bytes survived
		// plugin decoding and overrides. Native video returns media rather
		// than a text answer; image edits must demonstrate the requested color.
		if wire == "openai_video" {
			wanted = "video"
		} else if wire == "openai_image" {
			wanted = "image"
		}
	case "max_duration_seconds":
		wanted = "timed_media"
	case "output_image", "max_images":
		wanted = "image"
	case "output_audio":
		wanted = "audio"
	case "output_pdf":
		wanted = "pdf"
	}
	if wanted == "" {
		return "inconclusive", "behavior_not_observed"
	}
	observed := int64(0)
	for _, artifact := range artifacts {
		if wanted == "timed_media" {
			if artifact.Type != "video" && artifact.Type != "audio" {
				continue
			}
		} else if artifact.Type != wanted && !(wanted == "pdf" && artifact.MimeType == "application/pdf") {
			continue
		}
		descriptor, err := adaptor.BuildContentRequest(ctx, task, artifact.Key, channel.TaskArtifactClientRequest{Method: http.MethodGet})
		if err != nil || descriptor == nil {
			continue
		}
		if !descriptor.Credentialless && !strings.HasPrefix(descriptor.URL, "data:") && pluginruntime.ValidateRequestURL(descriptor.URL, info.ChannelBaseUrl, plugin.Meta.AllowedHosts) != nil {
			continue
		}
		payload, err := fetchProbeArtifact(ctx, provider, descriptor)
		if err != nil {
			continue
		}
		valid := false
		mediaKind := wanted
		if wanted == "timed_media" {
			mediaKind = artifact.Type
		}
		switch mediaKind {
		case "video":
			seconds, validVideo := probeVideoSeconds(payload)
			valid = validVideo && (result.Capability != "max_duration_seconds" || seconds >= recipe.TestedValue)
		case "image":
			encodedImage := base64.StdEncoding.EncodeToString(payload)
			valid = validProbeImage(encodedImage) && (result.Capability != "input_image" || probeImageColor(encodedImage, "blue"))
		case "audio":
			seconds, audioValid := probeAudioSeconds(ctx, payload, artifact.MimeType)
			valid = audioValid && (result.Capability != "max_duration_seconds" || seconds >= recipe.TestedValue)
		case "pdf":
			valid = bytes.HasPrefix(payload, []byte("%PDF-")) && bytes.Contains(payload, []byte("%%EOF"))
		}
		if valid {
			observed++
		}
		if observed >= 2 {
			break
		}
	}
	if observed > 0 {
		if recipe.SampledLimit {
			return "inconclusive", "limit_sample_only"
		}
		if result.Capability == "max_images" && observed != recipe.TestedValue {
			return "inconclusive", "behavior_not_observed"
		}
		return "passed", "verified"
	}
	return "inconclusive", "behavior_not_observed"
}

func probeTaskRequestWithinBudget(value gjson.Result) bool {
	if !probeRequestWithinBudget(value, 2) {
		return false
	}
	valid := true
	value.ForEach(func(key, item gjson.Result) bool {
		if item.IsObject() || item.IsArray() {
			valid = probeTaskRequestWithinBudget(item)
			return valid
		}
		if item.Type == gjson.String {
			parsed := gjson.Parse(item.String())
			if parsed.IsObject() || parsed.IsArray() {
				valid = probeTaskRequestWithinBudget(parsed)
				return valid
			}
		}
		switch key.String() {
		case "duration", "seconds", "durationSeconds", "duration_seconds", "duration_in_seconds", "video_duration", "audio_duration":
			number, err := strconv.ParseInt(item.String(), 10, 64)
			valid = err == nil && number >= 1 && number <= min(60, relaycommon.MaxTaskDurationSeconds)
		case "numberOfVideos", "number_of_videos", "num_videos", "video_count":
			number, err := strconv.ParseInt(item.String(), 10, 64)
			valid = err == nil && number == 1
		}
		return valid
	})
	return valid
}

func probeTaskMultipartWithinBudget(body []byte, contentType string) bool {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		return false
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return true
		}
		if err != nil {
			return false
		}
		if part.FileName() != "" {
			part.Close()
			continue
		}
		value, err := io.ReadAll(io.LimitReader(part, (1<<20)+1))
		part.Close()
		if err != nil || len(value) > 1<<20 {
			return false
		}
		text := strings.TrimSpace(string(value))
		switch part.FormName() {
		case "n", "batch_size", "batch_count", "num_images", "num_outputs", "sampleCount", "numberOfImages", "number_of_images", "numImages":
			number, err := strconv.ParseInt(text, 10, 64)
			if err != nil || number < 1 || number > 2 {
				return false
			}
		case "numberOfVideos", "number_of_videos", "num_videos", "video_count":
			number, err := strconv.ParseInt(text, 10, 64)
			if err != nil || number != 1 {
				return false
			}
		case "seconds", "duration", "durationSeconds", "duration_seconds", "duration_in_seconds", "video_duration", "audio_duration":
			number, err := strconv.ParseInt(text, 10, 64)
			if err != nil || number < 1 || number > min(60, relaycommon.MaxTaskDurationSeconds) {
				return false
			}
		default:
			parsed := gjson.Parse(text)
			if (parsed.IsArray() || parsed.IsObject()) && !probeTaskRequestWithinBudget(parsed) {
				return false
			}
		}
	}
}

func fetchProbeArtifact(ctx context.Context, provider *model.Provider, descriptor *channel.TaskContentRequest) ([]byte, error) {
	if len(descriptor.URL) > (channelProbeArtifactLimit*4/3)+1024 || len(descriptor.Body) > 1<<20 {
		return nil, errors.New("invalid probe artifact")
	}
	if strings.HasPrefix(descriptor.URL, "data:") {
		_, encoded, ok := strings.Cut(descriptor.URL, ",")
		if !ok {
			return nil, errors.New("invalid probe artifact")
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(decoded) > channelProbeArtifactLimit {
			return nil, errors.New("invalid probe artifact")
		}
		return decoded, nil
	}
	setting := provider.GetSetting()
	client, err := service.GetHttpClientWithProxySettings(setting.Proxy, setting)
	if err != nil {
		return nil, errors.New("probe transport failed")
	}
	if descriptor.Credentialless {
		if (descriptor.Method != http.MethodGet && descriptor.Method != http.MethodHead) || descriptor.Body != nil || len(descriptor.Headers) != 0 || validateTaskMediaURL(descriptor.URL, setting.Proxy) != nil {
			return nil, errors.New("invalid probe artifact")
		}
		if setting.Proxy == "" {
			client = service.GetSSRFProtectedHTTPClient()
		}
	}
	if client == nil {
		return nil, errors.New("probe transport failed")
	}
	isolated := *client
	isolated.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	request, err := http.NewRequestWithContext(ctx, descriptor.Method, descriptor.URL, bytes.NewReader(descriptor.Body))
	if err != nil {
		return nil, errors.New("invalid probe artifact")
	}
	for key, value := range descriptor.Headers {
		request.Header.Set(key, value)
	}
	response, err := isolated.Do(request)
	if err != nil {
		return nil, errors.New("probe transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("probe artifact rejected")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, channelProbeArtifactLimit+1))
	if err != nil || len(body) > channelProbeArtifactLimit {
		return nil, errors.New("invalid probe artifact")
	}
	if len(descriptor.ContentPath) > 0 {
		writer := &probeResponseWriter{header: make(http.Header), maxBytes: channelProbeArtifactLimit}
		c := &gin.Context{Request: request, Writer: writer}
		if err = writeTaskMediaContentPath(c, bytes.NewReader(body), descriptor); err != nil || writer.overflow {
			return nil, errors.New("invalid probe artifact")
		}
		return writer.body.Bytes(), nil
	}
	return body, nil
}
