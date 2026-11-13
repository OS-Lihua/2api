package service

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/MoeForge/moego/base64"
	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/capability"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/tokenkit"
	"github.com/gin-gonic/gin"
)

type realtimeInputQuantity struct{ total, audio, image, audioItems, imageItems, toolItems int }

func (quantity realtimeInputQuantity) usage() *dto.Usage {
	return &dto.Usage{PromptTokens: quantity.total, TotalTokens: quantity.total, PromptTokensDetails: dto.InputTokenDetails{TextTokens: quantity.total - quantity.audio - quantity.image, AudioTokens: quantity.audio, ImageTokens: quantity.image}}
}

func (quantity realtimeInputQuantity) add(other realtimeInputQuantity) realtimeInputQuantity {
	return realtimeInputQuantity{
		total: quantity.total + other.total, audio: quantity.audio + other.audio, image: quantity.image + other.image,
		audioItems: quantity.audioItems + other.audioItems, imageItems: quantity.imageItems + other.imageItems, toolItems: quantity.toolItems + other.toolItems,
	}
}

func (quantity realtimeInputQuantity) subtract(other realtimeInputQuantity) realtimeInputQuantity {
	return realtimeInputQuantity{
		total: quantity.total - other.total, audio: quantity.audio - other.audio, image: quantity.image - other.image,
		audioItems: quantity.audioItems - other.audioItems, imageItems: quantity.imageItems - other.imageItems, toolItems: quantity.toolItems - other.toolItems,
	}
}

type realtimeConversationItem struct {
	sequence       uint64
	quantity       realtimeInputQuantity
	response       string
	accepted       bool
	audioCommitted bool
	deleted        bool
	truncatedAtMS  *int64
}
type realtimeInputEvidence struct {
	sequence           uint64
	baseline, quantity realtimeInputQuantity
	valid              bool
}

type realtimeConversationOutput struct {
	sequence            uint64
	evidence            realtimeInputEvidence
	quantity            realtimeInputQuantity
	defaultConversation bool
}

// RealtimeConversation retains quantitative native items, without their text,
// images or audio. One socket owner observes events and freezes input snapshots.
type RealtimeConversation struct {
	context                                   *gin.Context
	model, inputFormat                        string
	items                                     map[string]realtimeConversationItem
	destructiveEdits                          map[uint64]uint64
	outputs                                   map[string]realtimeConversationOutput
	epoch, sequence                           uint64
	current                                   realtimeInputQuantity
	identityBytes, limit                      int
	audioBytes                                int64
	bufferedAudio                             int
	pendingID, pendingEvent, pendingOperation string
}

func NewRealtimeConversation(c *gin.Context, model string) *RealtimeConversation {
	maximum := constant.MaxRequestBodyMB
	if maximum <= 0 {
		maximum = 128
	}
	return &RealtimeConversation{context: c.Copy(), model: model, inputFormat: "pcm16", items: map[string]realtimeConversationItem{}, outputs: map[string]realtimeConversationOutput{}, limit: maximum << 20}
}

func (conversation *RealtimeConversation) Pending() bool { return conversation.pendingID != "" }

func (conversation *RealtimeConversation) changeInput(previous, next realtimeInputQuantity, sequence uint64) {
	conversation.current = conversation.current.subtract(previous).add(next)
	delta := next.subtract(previous)
	if delta.total < 0 || delta.audio < 0 || delta.image < 0 || delta.audioItems < 0 || delta.imageItems < 0 || delta.toolItems < 0 {
		conversation.epoch++
		if conversation.destructiveEdits == nil {
			conversation.destructiveEdits = map[uint64]uint64{}
		}
		conversation.destructiveEdits[sequence] = conversation.epoch
		for id, output := range conversation.outputs {
			if output.evidence.valid && sequence <= output.evidence.sequence {
				output.evidence.valid = false
				conversation.outputs[id] = output
			}
		}
	}
}

func (conversation *RealtimeConversation) inputQuantity() realtimeInputQuantity {
	quantity := conversation.current
	for _, output := range conversation.outputs {
		if !output.evidence.valid {
			continue
		}
		calibrated := output.evidence.quantity.add(conversation.current.subtract(output.evidence.baseline))
		quantity.total = max(quantity.total, calibrated.total)
		quantity.audio = max(quantity.audio, calibrated.audio)
		quantity.image = max(quantity.image, calibrated.image)
		quantity.audioItems = max(quantity.audioItems, calibrated.audioItems)
		quantity.imageItems = max(quantity.imageItems, calibrated.imageItems)
		quantity.toolItems = max(quantity.toolItems, calibrated.toolItems)
	}
	quantity.total = max(quantity.total, quantity.audio+quantity.image)
	return quantity
}

func (conversation *RealtimeConversation) store(id string, item realtimeConversationItem) error {
	if id == "" {
		return errors.New("Realtime conversation item has no identity")
	}
	if _, exists := conversation.items[id]; !exists {
		if len(id)+256 > conversation.limit-conversation.identityBytes {
			return errors.New("Realtime conversation identity budget exceeded")
		}
		conversation.identityBytes += len(id) + 256
	}
	before, after := realtimeInputQuantity{}, realtimeInputQuantity{}
	if previous, exists := conversation.items[id]; exists && previous.accepted && previous.response == "" {
		before = previous.quantity
	}
	if item.accepted && item.response == "" {
		after = item.quantity
	}
	if previous, exists := conversation.items[id]; exists {
		item.sequence = previous.sequence
	} else {
		conversation.sequence++
		item.sequence = conversation.sequence
	}
	conversation.changeInput(before, after, item.sequence)
	conversation.items[id] = item
	return nil
}

// Observe preserves native payloads, assigning optional client item/event IDs
// when needed to correlate their acknowledgements before admitting inference.
func (conversation *RealtimeConversation) Observe(raw []byte, client bool) ([]byte, error) {
	kind := ojson.GetBytes(raw, "type").String()
	if client {
		switch kind {
		case "input_audio_buffer.append":
			encoded := ojson.GetBytes(raw, "audio")
			if encoded.Type != ojson.String {
				return nil, errors.New("audio must contain base64 data")
			}
			audio, err := base64.StdEncoding.DecodeString(encoded.String())
			if err != nil || int64(len(audio)) > int64(conversation.limit)-conversation.audioBytes {
				return nil, errors.New("Realtime audio buffer budget exceeded")
			}
			conversation.audioBytes += int64(len(audio))
		case "input_audio_buffer.clear":
			conversation.audioBytes, conversation.bufferedAudio = 0, 0
		case "conversation.item.create", "conversation.item.delete", "conversation.item.truncate":
			document := ojson.NewDoc(append([]byte(nil), raw...))
			id := document.Get("item_id").String()
			if kind == "conversation.item.create" {
				id = document.Get("item.id").String()
				if id == "" {
					id = "item_" + common.GetUUID()[:24]
					if err := document.Set("item.id", id); err != nil {
						return nil, err
					}
				}
				if _, exists := conversation.items[id]; exists {
					return nil, errors.New("Realtime conversation item already exists")
				}
				snapshot := &RealtimeInputSnapshot{context: conversation.context.Copy(), model: conversation.model, inputFormat: conversation.inputFormat, items: conversation.items}
				quantity, err := snapshot.countItem(document.Get("item"))
				if err != nil {
					return nil, err
				}
				if err = conversation.store(id, realtimeConversationItem{quantity: quantity}); err != nil {
					return nil, err
				}
			} else if _, exists := conversation.items[id]; !exists {
				return nil, errors.New("Unknown Realtime conversation item")
			}
			if kind == "conversation.item.truncate" {
				var milliseconds int64
				if common.UnmarshalJsonStr(document.Get("audio_end_ms").Raw, &milliseconds) != nil || milliseconds < 0 || milliseconds > (1<<31)-1 {
					return nil, errors.New("Invalid Realtime audio truncation")
				}
			}
			event := document.Get("event_id")
			if event.Exists() && event.Type != ojson.String {
				return nil, errors.New("event_id must be a string")
			}
			eventID := event.String()
			if eventID == "" {
				eventID = "event_" + common.GetUUID()[:24]
				if err := document.Set("event_id", eventID); err != nil {
					return nil, err
				}
			}
			conversation.pendingID, conversation.pendingEvent, conversation.pendingOperation = id, eventID, kind
			return document.Bytes(), nil
		}
		return raw, nil
	}
	switch kind {
	case "session.created", "session.updated":
		format := ojson.GetBytes(raw, "session.input_audio_format")
		if ga := ojson.GetBytes(raw, "session.audio.input.format.type"); ga.Exists() {
			format = ga
		}
		if normalized, valid := NormalizeRealtimeAudioFormat(format.String()); valid && normalized != conversation.inputFormat {
			conversation.bufferedAudio += CountRealtimeAudioTokens(conversation.audioBytes, conversation.inputFormat, true)
			conversation.audioBytes = 0
			conversation.inputFormat = normalized
		}
	case "input_audio_buffer.committed":
		id := ojson.GetBytes(raw, "item_id").String()
		item := conversation.items[id]
		if item.audioCommitted {
			return raw, nil
		}
		tokens := conversation.bufferedAudio + CountRealtimeAudioTokens(conversation.audioBytes, conversation.inputFormat, true)
		item.quantity.total += tokens - item.quantity.audio
		item.quantity.audio = tokens
		item.quantity.audioItems = 1
		item.accepted, item.audioCommitted = true, true
		if err := conversation.store(id, item); err != nil {
			return nil, err
		}
		conversation.audioBytes, conversation.bufferedAudio = 0, 0
	case "conversation.item.created", "conversation.item.added", "conversation.item.done", "response.output_item.added", "response.output_item.done":
		item := ojson.GetBytes(raw, "item")
		id := item.Get("id").String()
		if id == "" {
			return raw, nil
		}
		previous, exists := conversation.items[id]
		if previous.deleted {
			return raw, nil
		}
		response := previous.response
		if value := ojson.GetBytes(raw, "response_id"); value.Exists() {
			response = value.String()
		}
		quantity := previous.quantity
		if !exists || response != "" {
			snapshot := &RealtimeInputSnapshot{context: conversation.context.Copy(), model: conversation.model, inputFormat: conversation.inputFormat, items: conversation.items}
			var err error
			quantity, err = snapshot.countItem(item)
			if err != nil {
				return nil, err
			}
			if quantity.audio == 0 && previous.quantity.audio > 0 {
				quantity.total += previous.quantity.audio
				quantity.audio = previous.quantity.audio
			}
		}
		if previous.truncatedAtMS != nil {
			quantity.audio = min(quantity.audio, int(*previous.truncatedAtMS/50))
			quantity.total = quantity.audio + quantity.image
		}
		if err := conversation.store(id, realtimeConversationItem{quantity: quantity, response: response, accepted: true, audioCommitted: previous.audioCommitted, truncatedAtMS: previous.truncatedAtMS}); err != nil {
			return nil, err
		}
		if id == conversation.pendingID {
			conversation.pendingID, conversation.pendingEvent, conversation.pendingOperation = "", "", ""
		}
	case "conversation.item.deleted":
		id := ojson.GetBytes(raw, "item_id").String()
		conversation.remove(id)
		if id == conversation.pendingID {
			conversation.pendingID, conversation.pendingEvent, conversation.pendingOperation = "", "", ""
		}
	case "conversation.item.truncated":
		id := ojson.GetBytes(raw, "item_id").String()
		if item, exists := conversation.items[id]; exists {
			var milliseconds int64
			if common.UnmarshalJsonStr(ojson.GetBytes(raw, "audio_end_ms").Raw, &milliseconds) != nil || milliseconds < 0 || milliseconds > (1<<31)-1 {
				return nil, errors.New("Invalid Realtime audio truncation")
			}
			if item.truncatedAtMS != nil {
				milliseconds = min(milliseconds, *item.truncatedAtMS)
			}
			item.truncatedAtMS = &milliseconds
			previous := item.quantity
			item.quantity.audio = min(previous.audio, int(milliseconds/50))
			item.quantity.total = item.quantity.audio + item.quantity.image
			if item.accepted && item.response == "" {
				conversation.changeInput(previous, item.quantity, item.sequence)
			}
			conversation.items[id] = item
			if output, exists := conversation.outputs[item.response]; exists {
				previousOutput := output.quantity
				output.quantity.total -= previous.total - item.quantity.total
				output.quantity.audio -= previous.audio - item.quantity.audio
				if output.defaultConversation {
					conversation.changeInput(previousOutput, output.quantity, output.sequence)
				}
				conversation.outputs[item.response] = output
			}
		}
		if id == conversation.pendingID {
			conversation.pendingID, conversation.pendingEvent, conversation.pendingOperation = "", "", ""
		}
	case "error":
		if conversation.pendingEvent != "" && ojson.GetBytes(raw, "error.event_id").String() == conversation.pendingEvent {
			if conversation.pendingOperation == "conversation.item.create" {
				conversation.remove(conversation.pendingID)
			}
			conversation.pendingID, conversation.pendingEvent, conversation.pendingOperation = "", "", ""
		}
	}
	return raw, nil
}

func (conversation *RealtimeConversation) remove(id string) {
	item, exists := conversation.items[id]
	if !exists || item.deleted {
		return
	}
	if item.accepted && item.response == "" {
		conversation.changeInput(item.quantity, realtimeInputQuantity{}, item.sequence)
	}
	if output, exists := conversation.outputs[item.response]; exists {
		previous := output.quantity
		output.quantity.total = max(0, output.quantity.total-item.quantity.total)
		output.quantity.audio = max(0, output.quantity.audio-item.quantity.audio)
		output.quantity.image = max(0, output.quantity.image-item.quantity.image)
		output.quantity.audioItems = max(0, output.quantity.audioItems-item.quantity.audioItems)
		output.quantity.imageItems = max(0, output.quantity.imageItems-item.quantity.imageItems)
		output.quantity.toolItems = max(0, output.quantity.toolItems-item.quantity.toolItems)
		if output.defaultConversation {
			conversation.changeInput(previous, output.quantity, output.sequence)
		}
		conversation.outputs[item.response] = output
	}
	item.deleted, item.accepted = true, false
	conversation.items[id] = item
}

// Complete reuses the existing native response counter's measured quantities.
// Default-conversation output becomes subsequent input; out-of-band output is
// retained only for explicit item references.
func (conversation *RealtimeConversation) Complete(result *relaycommon.RealtimeResponseFacts, defaultConversation bool, observations ...RealtimeInputObservation) error {
	output := conversation.outputs[result.ID]
	evidence := output.evidence
	observationValid := len(observations) > 0
	if observationValid && observations[0].epoch != conversation.epoch {
		for sequence, epoch := range conversation.destructiveEdits {
			if sequence <= observations[0].sequence && epoch > observations[0].epoch {
				observationValid = false
				break
			}
		}
	}
	if observationValid && observations[0].defaultInput && result.Facts != nil && (result.Facts.PromptTokensReported || result.Facts.InputDetailsReported) {
		observation := observations[0]
		quantity := observation.base
		quantity.total = max(quantity.total, max(0, result.Usage.PromptTokens-observation.configuration))
		quantity.audio = max(quantity.audio, result.Usage.PromptTokensDetails.AudioTokens)
		quantity.image = max(quantity.image, result.Usage.PromptTokensDetails.ImageTokens)
		if quantity.audio > 0 {
			quantity.audioItems = max(1, quantity.audioItems)
		}
		if quantity.image > 0 {
			quantity.imageItems = max(1, quantity.imageItems)
		}
		quantity.total = max(quantity.total, quantity.audio+quantity.image)
		evidence = realtimeInputEvidence{sequence: observation.sequence, baseline: observation.raw, quantity: quantity, valid: true}
	}
	if _, exists := conversation.outputs[result.ID]; !exists {
		if len(result.ID)+512 > conversation.limit-conversation.identityBytes {
			return errors.New("Realtime conversation identity budget exceeded")
		}
		conversation.identityBytes += len(result.ID) + 512
		conversation.sequence++
		output.sequence = conversation.sequence
	}
	generated := result.Usage.CompletionTokens
	audio := result.Usage.CompletionTokenDetails.AudioTokens
	if result.Facts.SourceOutputTokens != nil {
		generated = max(generated, *result.Facts.SourceOutputTokens)
		audio = max(audio, result.Facts.SourceOutputDetails.AudioTokens)
	}
	quantity := realtimeInputQuantity{total: generated, audio: min(audio, generated)}
	ids := []string{}
	quantity.audioItems = 0
	edited, audioItems := false, 0
	for id, item := range conversation.items {
		if item.response != result.ID {
			continue
		}
		ids = append(ids, id)
		edited = edited || item.deleted || item.truncatedAtMS != nil
		audioItems += item.quantity.audioItems
		quantity.audioItems += item.quantity.audioItems
		quantity.imageItems += item.quantity.imageItems
		quantity.toolItems += item.quantity.toolItems
	}
	if len(ids) == 0 && result.Facts.GeneratedToolCalls {
		quantity.toolItems = 1
	}
	if len(ids) == 0 && (audio > 0 || result.Facts.GeneratedAudioBytes > 0) {
		quantity.audioItems = 1
	}
	retained := realtimeInputQuantity{}
	for _, id := range ids {
		item := conversation.items[id]
		if item.deleted {
			continue
		}
		if len(ids) == 1 {
			quantity.audioItems = max(quantity.audioItems, item.quantity.audioItems)
			quantity.imageItems = item.quantity.imageItems
			item.quantity = quantity
		} else if item.quantity.audioItems > 0 && audioItems == 1 {
			item.quantity.total += quantity.audio - item.quantity.audio
			item.quantity.audio = quantity.audio
		}
		if item.truncatedAtMS != nil {
			item.quantity.audio = min(item.quantity.audio, int(*item.truncatedAtMS/50))
			item.quantity.total = item.quantity.audio + item.quantity.image
		}
		conversation.items[id] = item
		retained = retained.add(item.quantity)
	}
	if edited {
		quantity = retained
	}
	before, after := realtimeInputQuantity{}, realtimeInputQuantity{}
	if previous, exists := conversation.outputs[result.ID]; exists && previous.defaultConversation {
		before = previous.quantity
	}
	if defaultConversation {
		after = quantity
	}
	conversation.changeInput(before, after, output.sequence)
	conversation.outputs[result.ID] = realtimeConversationOutput{sequence: output.sequence, evidence: evidence, quantity: quantity, defaultConversation: defaultConversation}
	return nil
}

func (conversation *RealtimeConversation) Usage(buffered bool) *dto.Usage {
	quantity := conversation.inputQuantity()
	if buffered {
		audio := conversation.bufferedAudio + CountRealtimeAudioTokens(conversation.audioBytes, conversation.inputFormat, true)
		quantity = quantity.add(realtimeInputQuantity{total: audio, audio: audio})
	}
	return quantity.usage()
}

type RealtimeInputObservation struct {
	epoch, sequence uint64
	base, raw       realtimeInputQuantity
	configuration   int
	defaultInput    bool
}

// Observation is a constant-size receipt for an immutable input plan. A later
// destructive edit invalidates its aggregate evidence rather than reallocating
// reported totals to individual items.
func (snapshot *RealtimeInputSnapshot) Observation(wire []byte) RealtimeInputObservation {
	body := ojson.GetBytes(wire, "response")
	if !body.IsObject() {
		body = ojson.ParseBytes(wire)
	}
	input := body.Get("input")
	configuration := tokenkit.Count(snapshot.model, body.Get("instructions").String())
	for _, tool := range body.Get("tools").Array() {
		configuration += tokenkit.Count(snapshot.model, tool.Raw)
	}
	return RealtimeInputObservation{epoch: snapshot.epoch, sequence: snapshot.sequence, base: snapshot.conversation, raw: snapshot.raw, configuration: configuration, defaultInput: !input.Exists() || input.Type == ojson.Null}
}

// RealtimeInputSnapshot is immutable while a financial worker prepares a
// candidate. References and default context use the same frozen quantities.
type RealtimeInputSnapshot struct {
	context            *gin.Context
	model, inputFormat string
	epoch, sequence    uint64
	raw                realtimeInputQuantity
	conversation       realtimeInputQuantity
	captureErr         error
	referenceBytes     int
	items              map[string]realtimeConversationItem
}

// Snapshot freezes default quantities and only the references named by this input.
func (conversation *RealtimeConversation) Snapshot(body []byte) *RealtimeInputSnapshot {
	snapshot := &RealtimeInputSnapshot{context: conversation.context.Copy(), model: conversation.model, inputFormat: conversation.inputFormat, conversation: conversation.inputQuantity(), epoch: conversation.epoch, sequence: conversation.sequence, raw: conversation.current}
	snapshot.CaptureReferences(conversation, body)
	return snapshot
}

// CaptureReferences freezes candidate override references while the connection
// owner waits for request preparation. It never reads newer conversation state.
func (snapshot *RealtimeInputSnapshot) CaptureReferences(conversation *RealtimeConversation, body []byte) {
	if snapshot.captureErr != nil || conversation.epoch != snapshot.epoch {
		return
	}
	for _, item := range ojson.GetBytes(body, "input").Array() {
		if item.Get("type").String() != "item_reference" {
			continue
		}
		id := item.Get("id").String()
		if _, exists := snapshot.items[id]; exists {
			continue
		}
		known, exists := conversation.items[id]
		if !exists || !known.accepted || known.sequence > snapshot.sequence {
			continue
		}
		if len(id)+128 > conversation.limit-snapshot.referenceBytes {
			snapshot.captureErr = errors.New("Realtime conversation identity budget exceeded")
			break
		}
		snapshot.referenceBytes += len(id) + 128
		if snapshot.items == nil {
			snapshot.items = map[string]realtimeConversationItem{}
		}
		snapshot.items[id] = known
	}
}

func (snapshot *RealtimeInputSnapshot) Estimate(body []byte) (*dto.Usage, error) {
	return snapshot.estimate(body, nil)
}

func (snapshot *RealtimeInputSnapshot) EstimateForCapabilities(body []byte, caps capability.Set) (*dto.Usage, error) {
	return snapshot.estimate(body, &caps)
}

func (snapshot *RealtimeInputSnapshot) estimate(body []byte, caps *capability.Set) (*dto.Usage, error) {
	if snapshot.captureErr != nil {
		return nil, snapshot.captureErr
	}
	quantity := snapshot.conversation
	value := ojson.ParseBytes(body)
	if prompt := value.Get("prompt"); prompt.Exists() && prompt.Type != ojson.Null {
		return nil, &GatewayParameterError{Path: "prompt", Reason: "Unknown prompt template; provide complete input and configuration"}
	}
	items := value.Get("input")
	toolPath := "input"
	if items.Exists() && items.Type != ojson.Null {
		if !items.IsArray() {
			return nil, &GatewayParameterError{Path: "input", Reason: "Realtime input must be an array"}
		}
		quantity = realtimeInputQuantity{}
		for index, item := range items.Array() {
			part, err := snapshot.countItem(item)
			if err != nil {
				path := fmt.Sprintf("input[%d]", index)
				var parameter *GatewayParameterError
				if errors.As(err, &parameter) && parameter.Path != "" {
					path += "." + parameter.Path
				}
				return nil, &GatewayParameterError{Path: path, Reason: err.Error()}
			}
			if part.toolItems > 0 && toolPath == "input" {
				field := "type"
				if item.Get("type").String() == "item_reference" {
					field = "id"
				}
				toolPath = fmt.Sprintf("input[%d].%s", index, field)
			}
			quantity = quantity.add(part)
		}
	}
	if caps != nil {
		if quantity.toolItems > 0 && !slices.Contains(caps.Features, capability.ToolCalling) {
			return nil, &GatewayParameterError{Path: toolPath, Reason: "Tool calling is not supported on this channel"}
		}
		if quantity.total-quantity.audio-quantity.image > 0 && !slices.Contains(caps.InputModalities, capability.Text) {
			return nil, errors.New("Realtime input modality is not supported on this connection")
		}
		if (quantity.audioItems > 0 || quantity.audio > 0) && !slices.Contains(caps.InputModalities, capability.Audio) || (quantity.imageItems > 0 || quantity.image > 0) && !slices.Contains(caps.InputModalities, capability.Image) {
			return nil, errors.New("Realtime input modality is not supported on this connection")
		}
		images := quantity.imageItems
		if quantity.image > 0 {
			images = max(images, 1)
		}
		if maximum := caps.Limits.MaxImages; maximum != nil && int64(images) > *maximum {
			return nil, errors.New("Realtime image count limit exceeded")
		}
	}
	input := quantity.usage()
	input.PromptTokens += tokenkit.Count(snapshot.model, value.Get("instructions").String())
	for _, tool := range value.Get("tools").Array() {
		input.PromptTokens += tokenkit.Count(snapshot.model, tool.Raw)
	}
	if input.PromptTokens < 0 || int64(input.PromptTokens) > (1<<31)-1 {
		return nil, errors.New("Realtime input token budget exceeded")
	}
	input.PromptTokensDetails.TextTokens = input.PromptTokens - input.PromptTokensDetails.AudioTokens - input.PromptTokensDetails.ImageTokens
	input.TotalTokens = input.PromptTokens
	return input, nil
}

func (snapshot *RealtimeInputSnapshot) countItem(item ojson.Result) (realtimeInputQuantity, error) {
	quantity := realtimeInputQuantity{}
	switch item.Get("type").String() {
	case "item_reference":
		known, exists := snapshot.items[item.Get("id").String()]
		if !exists || !known.accepted {
			return quantity, &GatewayParameterError{Path: "id", Reason: "Unknown Realtime input reference"}
		}
		return known.quantity, nil
	case "function_call":
		quantity.toolItems = 1
		quantity.total = tokenkit.Count(snapshot.model, item.Get("name").String()+item.Get("arguments").String())
		return quantity, nil
	case "function_call_output":
		quantity.toolItems = 1
		quantity.total = tokenkit.Count(snapshot.model, item.Get("output").String())
		return quantity, nil
	case "message", "":
		for _, part := range item.Get("content").Array() {
			switch part.Get("type").String() {
			case "input_text", "text", "output_text":
				quantity.total += tokenkit.Count(snapshot.model, part.Get("text").String())
			case "input_audio":
				quantity.audioItems++
				if audio := part.Get("audio"); audio.Exists() && audio.Type != ojson.Null {
					decoded, err := base64.StdEncoding.DecodeString(audio.String())
					if err != nil {
						return quantity, errors.New("Invalid Realtime input audio")
					}
					tokens := CountRealtimeAudioTokens(int64(len(decoded)), snapshot.inputFormat, true)
					quantity.total += tokens
					quantity.audio += tokens
				}
			case "audio", "output_audio": // Output bytes come from the shared response counter.
				quantity.audioItems++
			case "input_image":
				quantity.imageItems++
				url := part.Get("image_url").String()
				if strings.TrimSpace(url) == "" {
					return quantity, errors.New("Invalid Realtime input image")
				}
				tokens, err := getImageToken(snapshot.context, types.NewImageFileMeta(types.NewFileSourceFromData(url, ""), part.Get("detail").String()), snapshot.model, true)
				if err != nil {
					return quantity, errors.New("Could not estimate Realtime input image")
				}
				quantity.total += tokens
				quantity.image += tokens
			default:
				return quantity, errors.New("Unsupported Realtime input content")
			}
		}
		return quantity, nil
	default:
		return quantity, errors.New("Unsupported Realtime input item")
	}
}
