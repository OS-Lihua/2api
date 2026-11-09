package xunfei

import (
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/sjson"
)

func (*XunfeiChatRequest) FinalizeChatRequest(_ *gin.Context, info *relaycommon.RelayInfo, body []byte) ([]byte, error) {
	metadata := channel.NewProviderMetadata(info)
	parts := strings.Split(metadata.ApiKey, "|")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid Xunfei credentials")
	}
	destination, err := (&Provider{}).RequestURL(metadata)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(destination)
	if err != nil {
		return nil, fmt.Errorf("invalid Xunfei destination")
	}
	body, err = sjson.SetBytes(body, "header.app_id", parts[0])
	if err != nil {
		return nil, err
	}
	return sjson.SetBytes(body, "parameter.chat.domain", apiVersion2domain(path.Base(path.Dir(parsed.Path))))
}

func (*XunfeiChatRequest) SetChatOutputLimit(body []byte, limit uint) ([]byte, error) {
	return sjson.SetBytes(body, "parameter.chat.max_tokens", limit)
}

func (*XunfeiChatRequest) ChatRequestCapabilities() channel.NativeChatCapabilities {
	return channel.NativeChatCapabilities{Parameters: []string{"max_tokens", "max_completion_tokens", "temperature", "top_k"}, ContentTypes: []string{dto.ContentTypeText}}
}

func (*XunfeiChatRequest) ProjectChatRequest(body []byte, public dto.GeneralOpenAIRequest) (*dto.GeneralOpenAIRequest, error) {
	var wire XunfeiChatRequest
	if err := common.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	public.Temperature, public.TopK = wire.Parameter.Chat.Temperature, wire.Parameter.Chat.TopK
	public.MaxTokens, public.MaxCompletionTokens = nil, wire.Parameter.Chat.MaxTokens
	public.Messages = nil
	for _, message := range wire.Payload.Message.Text {
		public.Messages = append(public.Messages, dto.Message{Role: message.Role, Content: message.Content})
	}
	return &public, nil
}

type XunfeiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type XunfeiChatRequest struct {
	Header struct {
		AppId string `json:"app_id"`
	} `json:"header"`
	Parameter struct {
		Chat struct {
			Domain      string   `json:"domain,omitempty"`
			Temperature *float64 `json:"temperature,omitempty"`
			TopK        *int     `json:"top_k,omitempty"`
			MaxTokens   *uint    `json:"max_tokens,omitempty"`
			Auditing    bool     `json:"auditing,omitempty"`
		} `json:"chat"`
	} `json:"parameter"`
	Payload struct {
		Message struct {
			Text []XunfeiMessage `json:"text"`
		} `json:"message"`
	} `json:"payload"`
}

type XunfeiChatResponseTextItem struct {
	Content string `json:"content"`
	Role    string `json:"role"`
	Index   int    `json:"index"`
}

type XunfeiChatResponse struct {
	Header struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Sid     string `json:"sid"`
		Status  int    `json:"status"`
	} `json:"header"`
	Payload struct {
		Choices struct {
			Status int                          `json:"status"`
			Seq    int                          `json:"seq"`
			Text   []XunfeiChatResponseTextItem `json:"text"`
		} `json:"choices"`
		Usage struct {
			//Text struct {
			//	QuestionTokens   string `json:"question_tokens"`
			//	PromptTokens     string `json:"prompt_tokens"`
			//	CompletionTokens string `json:"completion_tokens"`
			//	TotalTokens      string `json:"total_tokens"`
			//} `json:"text"`
			Text dto.Usage `json:"text"`
		} `json:"usage"`
	} `json:"payload"`
}
