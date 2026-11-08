package cohere

import (
	"fmt"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/tidwall/sjson"
)

type CohereRequest struct {
	Model            string        `json:"model"`
	ChatHistory      []ChatHistory `json:"chat_history"`
	Message          string        `json:"message"`
	Stream           bool          `json:"stream"`
	MaxTokens        uint          `json:"max_tokens"`
	Temperature      *float64      `json:"temperature,omitempty"`
	P                *float64      `json:"p,omitempty"`
	K                *int          `json:"k,omitempty"`
	FrequencyPenalty *float64      `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64      `json:"presence_penalty,omitempty"`
	Seed             *float64      `json:"seed,omitempty"`
	StopSequences    any           `json:"stop_sequences,omitempty"`
	SafetyMode       string        `json:"safety_mode,omitempty"`
}

func (*CohereRequest) ChatRequestCapabilities() channel.NativeChatCapabilities {
	return channel.NativeChatCapabilities{Parameters: []string{"max_tokens", "max_completion_tokens", "temperature", "top_p", "top_k", "frequency_penalty", "presence_penalty", "seed", "stop"}, ContentTypes: []string{dto.ContentTypeText}}
}

func (r *CohereRequest) ProjectChatRequest(body []byte, public dto.GeneralOpenAIRequest) (*dto.GeneralOpenAIRequest, error) {
	var wire CohereRequest
	if err := common.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	public.Model, public.Messages = wire.Model, nil
	for _, message := range wire.ChatHistory {
		role := "user"
		if message.Role == "SYSTEM" {
			role = "system"
		}
		if message.Role == "CHATBOT" {
			role = "assistant"
		}
		public.Messages = append(public.Messages, dto.Message{Role: role, Content: message.Message})
	}
	public.Messages = append(public.Messages, dto.Message{Role: "user", Content: wire.Message})
	public.Prompt, public.Input, public.Instruction = nil, nil, ""
	public.MaxTokens, public.MaxCompletionTokens = nil, &wire.MaxTokens
	public.Stream, public.N = common.GetPointer(wire.Stream), common.GetPointer(1)
	public.Temperature, public.TopP, public.TopK = wire.Temperature, wire.P, wire.K
	public.FrequencyPenalty, public.PresencePenalty, public.Seed, public.Stop = wire.FrequencyPenalty, wire.PresencePenalty, wire.Seed, wire.StopSequences
	result, err := channel.ProjectChatRequestControls(body, public)
	if err != nil {
		return nil, err
	}
	if result.N != nil && *result.N > 1 {
		return nil, fmt.Errorf("Cohere cannot represent multiple output choices (n)")
	}
	return result, nil
}

func (r *CohereRequest) SetChatOutputLimit(body []byte, limit uint) ([]byte, error) {
	return sjson.SetBytes(body, "max_tokens", limit)
}

type ChatHistory struct {
	Role    string `json:"role"`
	Message string `json:"message"`
}

type CohereResponse struct {
	IsFinished   bool                  `json:"is_finished"`
	EventType    string                `json:"event_type"`
	Text         string                `json:"text,omitempty"`
	FinishReason string                `json:"finish_reason,omitempty"`
	Response     *CohereResponseResult `json:"response"`
}

type CohereResponseResult struct {
	ResponseId   string     `json:"response_id"`
	FinishReason string     `json:"finish_reason,omitempty"`
	Text         string     `json:"text"`
	Meta         CohereMeta `json:"meta"`
}

type CohereRerankRequest struct {
	Documents       []any  `json:"documents"`
	Query           string `json:"query"`
	Model           string `json:"model"`
	TopN            int    `json:"top_n"`
	ReturnDocuments bool   `json:"return_documents"`
}

type CohereRerankResponseResult struct {
	Results []dto.RerankResponseResult `json:"results"`
	Meta    CohereMeta                 `json:"meta"`
}

type CohereMeta struct {
	//Tokens CohereTokens `json:"tokens"`
	BilledUnits CohereBilledUnits `json:"billed_units"`
}

type CohereBilledUnits struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type CohereTokens struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}
