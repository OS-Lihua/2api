package palm

import (
	"fmt"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

func (*PaLMChatRequest) ChatRequestCapabilities() channel.NativeChatCapabilities {
	return channel.NativeChatCapabilities{Parameters: []string{"temperature", "top_p", "top_k", "n"}, ContentTypes: []string{dto.ContentTypeText}}
}

func (*PaLMChatRequest) ProjectChatRequest(body []byte, public dto.GeneralOpenAIRequest) (*dto.GeneralOpenAIRequest, error) {
	var wire PaLMChatRequest
	if err := common.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	public.Temperature, public.TopP, public.N = wire.Temperature, wire.TopP, wire.CandidateCount
	public.TopK = nil
	if wire.TopK != nil {
		if *wire.TopK > 1<<31-1 {
			return nil, fmt.Errorf("invalid PaLM top_k")
		}
		public.TopK = common.GetPointer(int(*wire.TopK))
	}
	public.Messages = nil
	for _, message := range wire.Prompt.Messages {
		public.Messages = append(public.Messages, dto.Message{Role: message.Author, Content: message.Content})
	}
	return &public, nil
}

type PaLMChatMessage struct {
	Author  string `json:"author"`
	Content string `json:"content"`
}

type PaLMFilter struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type PaLMPrompt struct {
	Messages []PaLMChatMessage `json:"messages"`
}

type PaLMChatRequest struct {
	Prompt         PaLMPrompt `json:"prompt"`
	Temperature    *float64   `json:"temperature,omitempty"`
	CandidateCount *int       `json:"candidateCount,omitempty"`
	TopP           *float64   `json:"topP,omitempty"`
	TopK           *uint      `json:"topK,omitempty"`
}

type PaLMError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

type PaLMChatResponse struct {
	Candidates []PaLMChatMessage `json:"candidates"`
	Messages   []dto.Message     `json:"messages"`
	Filters    []PaLMFilter      `json:"filters"`
	Error      PaLMError         `json:"error"`
}
