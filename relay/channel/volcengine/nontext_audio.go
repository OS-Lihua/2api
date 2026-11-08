package volcengine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// ttsProtocol owns the speech DTO and audio response; credentials and the
// WebSocket connection belong to Provider.ExecuteRequest.
type ttsProtocol struct{}

const contextKeyDecodedSpeechResponse = "volcengine_decoded_speech_response"

func (p *ttsProtocol) ConvertRequest(c *gin.Context, info *relaycommon.RelayInfo, request any) (any, error) {
	r, ok := request.(dto.AudioRequest)
	if !ok {
		return nil, fmt.Errorf("expected speech request, got %T", request)
	}
	body, err := convertTTSRequest(c, info, r, VolcengineTTSApp{Cluster: "volcano_tts"})
	if err != nil {
		return nil, err
	}
	if info.ResponseFacts != nil {
		return speechRequestBody{body}, nil
	}
	return body, nil
}

func (p *ttsProtocol) DoResponse(c *gin.Context, response *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	encoding := c.GetString(contextKeyResponseFormat)
	_, socketBody := response.Body.(*ttsSocketBody)
	if !socketBody && !c.GetBool(contextKeyDecodedSpeechResponse) {
		return handleTTSResponse(c, response, info, encoding)
	}
	defer response.Body.Close()
	if info.ResponseFacts != nil {
		audio, err := io.ReadAll(response.Body)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeReadResponseBodyFailed)
		}
		return openai.WriteAudioResponse(c, info, response, audio), nil
	}
	c.Header("Content-Type", getContentTypeByEncoding(encoding))
	var buffer [32 * 1024]byte
	for {
		n, readErr := response.Body.Read(buffer[:])
		if n > 0 {
			if _, err := c.Writer.Write(buffer[:n]); err != nil {
				return nil, types.NewError(err, types.ErrorCodeBadResponse)
			}
			c.Writer.Flush()
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, types.NewError(readErr, types.ErrorCodeBadResponse)
		}
	}
	return &dto.Usage{PromptTokens: info.GetEstimatePromptTokens(), TotalTokens: info.GetEstimatePromptTokens()}, nil
}

func (p *Provider) ExecuteRequest(c *gin.Context, metadata channel.ProviderMetadata, body io.Reader) (*http.Response, error) {
	account := metadata.ProviderRequest()
	if account.Protocol != protocol.OpenAIAudioSpeech {
		return nil, channel.ErrUseHTTPTransport
	}
	c.Set(contextKeyDecodedSpeechResponse, false)
	payload, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	var request VolcengineTTSRequest
	if err = common.Unmarshal(payload, &request); err != nil {
		return nil, err
	}
	appID, token, err := parseVolcengineAuth(account.ApiKey)
	if err != nil {
		return nil, err
	}
	request.App.AppID, request.App.Token = appID, token
	payload, err = common.Marshal(request)
	if err != nil {
		return nil, err
	}
	destination, err := p.RequestURL(metadata)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(destination, "ws://") && !strings.HasPrefix(destination, "wss://") {
		upstream, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, destination, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		upstream.Header = account.RequestHeaders.Clone()
		client, err := service.GetHttpClientWithProxySettings(account.ChannelSetting.Proxy, account.ChannelSetting.HTTPSettings())
		if err != nil {
			return nil, err
		}
		return client.Do(upstream)
	}
	dialer := *websocket.DefaultDialer
	if account.ChannelSetting.Proxy != "" {
		proxyURL, _, err := common.ParseProxyURLRuntime(account.ChannelSetting.Proxy)
		if err != nil {
			return nil, err
		}
		dialer.Proxy = http.ProxyURL(proxyURL)
	}
	conn, response, err := dialer.DialContext(c.Request.Context(), destination, account.RequestHeaders)
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		return nil, err
	}
	if err := FullClientRequest(conn, payload); err != nil {
		conn.Close()
		return nil, err
	}
	stop := context.AfterFunc(c.Request.Context(), func() { conn.Close() })
	c.Set(contextKeyDecodedSpeechResponse, true)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/octet-stream"}}, Body: &ttsSocketBody{conn: conn, stop: stop}}, nil
}

// ttsSocketBody exposes audio frames as a normal HTTP body. Closing the body or
// cancelling the request closes the socket, including on early client abort.
type ttsSocketBody struct {
	conn    *websocket.Conn
	stop    func() bool
	pending *bytes.Reader
	done    bool
}

func (b *ttsSocketBody) Read(p []byte) (int, error) {
	for {
		if b.pending != nil && b.pending.Len() > 0 {
			return b.pending.Read(p)
		}
		if b.done {
			return 0, io.EOF
		}
		message, err := ReceiveMessage(b.conn)
		if err != nil {
			return 0, err
		}
		switch message.MsgType {
		case MsgTypeError:
			return 0, errors.New("volcengine speech transport returned an error")
		case MsgTypeAudioOnlyServer:
			b.done = message.Sequence < 0
			b.pending = bytes.NewReader(message.Payload)
		}
	}
}
func (b *ttsSocketBody) Close() error {
	b.stop()
	return b.conn.Close()
}
