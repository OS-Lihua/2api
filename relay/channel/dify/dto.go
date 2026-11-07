package dify

import (
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

type DifyChatRequest struct {
	Inputs           map[string]any `json:"inputs"`
	Query            string         `json:"query"`
	ResponseMode     string         `json:"response_mode"`
	User             string         `json:"user"`
	AutoGenerateName bool           `json:"auto_generate_name"`
	Files            []DifyFile     `json:"files"`
}

func (*DifyChatRequest) ProjectChatRequest(body []byte, public dto.GeneralOpenAIRequest) (*dto.GeneralOpenAIRequest, error) {
	var wire DifyChatRequest
	if err := common.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	if wire.Query == "" || wire.ResponseMode != "blocking" && wire.ResponseMode != "streaming" {
		return nil, fmt.Errorf("invalid Dify chat request")
	}
	effective, err := channel.ProjectChatRequestControls(body, public)
	if err != nil {
		return nil, err
	}
	if effective.N != nil && *effective.N > 1 {
		return nil, fmt.Errorf("Dify supports one choice per request")
	}
	effective.Stream = common.GetPointer(wire.ResponseMode == "streaming")
	content := []dto.MediaContent{{Type: dto.ContentTypeText, Text: wire.Query}}
	for _, file := range wire.Files {
		if file.Type != "image" && !strings.HasPrefix(file.Type, "image/") {
			return nil, fmt.Errorf("unsupported Dify file capability")
		}
		content = append(content, dto.MediaContent{Type: dto.ContentTypeImageURL, ImageUrl: &dto.MessageImageUrl{Url: file.URL}})
	}
	effective.Messages = []dto.Message{{Role: "user", Content: content}}
	return effective, nil
}

type DifyFile struct {
	Type         string `json:"type"`
	TransferMode string `json:"transfer_mode"`
	URL          string `json:"url,omitempty"`
	UploadFileId string `json:"upload_file_id,omitempty"`
}

type DifyMetaData struct {
	Usage dto.Usage `json:"usage"`
}

type DifyData struct {
	WorkflowId string `json:"workflow_id"`
	NodeId     string `json:"node_id"`
	NodeType   string `json:"node_type"`
	Status     string `json:"status"`
}

type DifyChatCompletionResponse struct {
	ConversationId string       `json:"conversation_id"`
	Answer         string       `json:"answer"`
	CreateAt       int64        `json:"create_at"`
	MetaData       DifyMetaData `json:"metadata"`
}

type DifyChunkChatCompletionResponse struct {
	Event          string       `json:"event"`
	ConversationId string       `json:"conversation_id"`
	Answer         string       `json:"answer"`
	Data           DifyData     `json:"data"`
	MetaData       DifyMetaData `json:"metadata"`
}
