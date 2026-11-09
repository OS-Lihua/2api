package xunfei

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

var _ channel.ProviderRequestExecutor = (*Provider)(nil)

type sparkResponseBody struct {
	*io.PipeReader
	conn   *websocket.Conn
	cancel context.CancelFunc
}

func (b *sparkResponseBody) Close() error {
	b.cancel()
	b.conn.Close()
	return b.PipeReader.Close()
}

func (p *Provider) ExecuteRequest(c *gin.Context, metadata channel.ProviderMetadata, body io.Reader) (*http.Response, error) {
	request := metadata.ProviderRequest()
	var wire XunfeiChatRequest
	if err := common.DecodeJson(body, &wire); err != nil {
		return nil, fmt.Errorf("invalid Spark wire request: %w", err)
	}
	parts := strings.Split(request.ApiKey, "|")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid Xunfei credentials")
	}
	destination, err := p.RequestURL(metadata)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(destination)
	if err != nil {
		return nil, fmt.Errorf("invalid Xunfei destination")
	}
	version := strings.TrimSuffix(strings.TrimPrefix(parsed.Path, "/"), "/chat")
	wire.Header.AppId = parts[0]
	wire.Parameter.Chat.Domain = apiVersion2domain(version)
	payload, err := common.Marshal(wire)
	if err != nil {
		return nil, err
	}
	parent := context.Background()
	if c != nil && c.Request != nil {
		parent = c.Request.Context()
	}
	ctx, cancel := context.WithCancel(parent)
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, handshake, err := dialer.DialContext(ctx, destination, request.RequestHeaders)
	if err != nil {
		cancel()
		if handshake != nil && handshake.Body != nil {
			handshake.Body.Close()
		}
		if handshake != nil && handshake.StatusCode >= 400 {
			return nil, types.NewOpenAIError(fmt.Errorf("Xunfei WebSocket connection failed"), types.ErrorCodeBadResponse, handshake.StatusCode)
		}
		return nil, fmt.Errorf("Xunfei WebSocket connection failed")
	}
	if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		cancel()
		conn.Close()
		return nil, fmt.Errorf("Xunfei WebSocket request failed")
	}
	reader, writer := io.Pipe()
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	go func() {
		defer close(done)
		defer cancel()
		defer conn.Close()
		defer writer.Close()
		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				writer.CloseWithError(fmt.Errorf("Xunfei WebSocket response ended before completion"))
				return
			}
			var valid common.RawMessage
			if err := common.Unmarshal(message, &valid); err != nil {
				writer.CloseWithError(fmt.Errorf("invalid Xunfei response"))
				return
			}
			if _, err := writer.Write(append(message, '\n')); err != nil {
				return
			}
			var envelope struct {
				Header struct {
					Code int `json:"code"`
				} `json:"header"`
				Payload struct {
					Choices struct {
						Status int `json:"status"`
					} `json:"choices"`
				} `json:"payload"`
			}
			if err := common.Unmarshal(message, &envelope); err != nil {
				writer.CloseWithError(fmt.Errorf("invalid Xunfei response"))
				return
			}
			if envelope.Header.Code != 0 || envelope.Payload.Choices.Status == 2 {
				return
			}
		}
	}()
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: &sparkResponseBody{PipeReader: reader, conn: conn, cancel: cancel}}
	response.Header.Set("Content-Type", "application/x-ndjson")
	return response, nil
}
