package zhipu

import (
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

func (*ZhipuRequest) ChatRequestCapabilities() channel.NativeChatCapabilities {
	return channel.NativeChatCapabilities{Parameters: []string{"temperature", "top_p"}, ContentTypes: []string{dto.ContentTypeText}}
}

func (*ZhipuRequest) ProjectChatRequest(body []byte, public dto.GeneralOpenAIRequest) (*dto.GeneralOpenAIRequest, error) {
	var wire ZhipuRequest
	if err := common.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	public.Temperature, public.TopP = wire.Temperature, wire.TopP
	public.Messages = nil
	for _, message := range wire.Prompt {
		public.Messages = append(public.Messages, dto.Message{Role: message.Role, Content: message.Content})
	}
	return &public, nil
}

type ZhipuMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ZhipuRequest struct {
	Prompt      []ZhipuMessage `json:"prompt"`
	Temperature *float64       `json:"temperature,omitempty"`
	TopP        *float64       `json:"top_p,omitempty"`
	RequestId   string         `json:"request_id,omitempty"`
	Incremental bool           `json:"incremental,omitempty"`
}

type ZhipuResponseData struct {
	TaskId     string         `json:"task_id"`
	RequestId  string         `json:"request_id"`
	TaskStatus string         `json:"task_status"`
	Choices    []ZhipuMessage `json:"choices"`
	dto.Usage  `json:"usage"`
}

type ZhipuResponse struct {
	Code    int               `json:"code"`
	Msg     string            `json:"msg"`
	Success bool              `json:"success"`
	Data    ZhipuResponseData `json:"data"`
}

type ZhipuStreamMetaResponse struct {
	RequestId  string `json:"request_id"`
	TaskId     string `json:"task_id"`
	TaskStatus string `json:"task_status"`
	dto.Usage  `json:"usage"`
}

type zhipuTokenData struct {
	Token      string
	ExpiryTime time.Time
}
