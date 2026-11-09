package xunfei

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

var _ channel.ProviderRequestExecutor = (*Provider)(nil)
var _ channel.ProviderRequestPreparer = (*Provider)(nil)

type sparkSourceFrame struct {
	Sequence uint64 `json:"sequence"`
	Data     []byte `json:"data"`
}

type sparkResponseBody struct {
	*io.PipeReader
	conn        *websocket.Conn
	cancel      context.CancelFunc
	sourceLimit int64
	finished    <-chan struct{}
	mu          sync.Mutex
	pending     sparkSourceFrame
}

func (b *sparkResponseBody) Close() error {
	b.cancel()
	b.conn.Close()
	err := b.PipeReader.Close()
	<-b.finished
	return err
}

func (b *sparkResponseBody) pendingSource() sparkSourceFrame {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pending
}

// The WebSocket owner bounds raw messages before private frame encoding.
func (b *sparkResponseBody) GatewayResponseSourceBound() int64 { return b.sourceLimit }

func (p *Provider) PrepareExecution(c *gin.Context, metadata channel.ProviderMetadata, body io.Reader) (channel.ProviderExecution, error) {
	request := metadata.ProviderRequest()
	payload, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("invalid Spark wire request")
	}
	var wire XunfeiChatRequest
	if err := common.Unmarshal(payload, &wire); err != nil {
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
	domain := apiVersion2domain(path.Base(path.Dir(parsed.Path)))
	managed := false
	if c != nil {
		facts, _ := common.GetContextKey(c, constant.ContextKeyResponseFacts)
		managed = facts != nil
	}
	if managed {
		if wire.Header.AppId != parts[0] || wire.Parameter.Chat.Domain != domain {
			return nil, fmt.Errorf("Spark wire request does not match the prepared account and version")
		}
	} else {
		wire.Header.AppId = parts[0]
		wire.Parameter.Chat.Domain = domain
		payload, err = common.Marshal(wire)
		if err != nil {
			return nil, err
		}
	}
	return func(current *gin.Context) (*http.Response, error) {
		return p.executeSpark(current, metadata, destination, payload)
	}, nil
}

func (p *Provider) ExecuteRequest(c *gin.Context, metadata channel.ProviderMetadata, body io.Reader) (*http.Response, error) {
	execute, err := p.PrepareExecution(c, metadata, body)
	if err != nil {
		return nil, err
	}
	return execute(c)
}

func (p *Provider) executeSpark(c *gin.Context, metadata channel.ProviderMetadata, destination string, payload []byte) (*http.Response, error) {
	request := metadata.ProviderRequest()
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
	stopCancellation := context.AfterFunc(ctx, func() { _ = conn.Close() })
	if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		stopCancellation()
		cancel()
		conn.Close()
		return nil, fmt.Errorf("Xunfei WebSocket request failed")
	}
	reader, writer := io.Pipe()
	sourceLimit := channel.GatewayResponseSourceLimit()
	done := make(chan struct{})
	source := &sparkResponseBody{PipeReader: reader, conn: conn, cancel: cancel, sourceLimit: sourceLimit, finished: done}
	go func() {
		defer close(done)
		defer cancel()
		defer stopCancellation()
		defer conn.Close()
		defer writer.Close()
		remaining := sourceLimit
		var sequence uint64
		for {
			if remaining <= 0 {
				writer.CloseWithError(fmt.Errorf("Spark response exceeds source limit"))
				return
			}
			conn.SetReadLimit(remaining)
			_, message, readErr := conn.ReadMessage()
			if len(message) > 0 {
				remaining -= int64(len(message))
				// Preserve one received WebSocket message, including malformed JSON and
				// embedded whitespace, for the existing source parser before propagating failure.
				sequence++
				frame := sparkSourceFrame{Sequence: sequence, Data: message}
				source.mu.Lock()
				source.pending = frame
				source.mu.Unlock()
				encoded, err := common.Marshal(frame)
				if err != nil {
					writer.CloseWithError(err)
					return
				}
				if _, err := writer.Write(append(encoded, '\n')); err != nil {
					return
				}
				source.mu.Lock()
				source.pending = sparkSourceFrame{}
				source.mu.Unlock()
			}
			if readErr != nil {
				writer.CloseWithError(fmt.Errorf("Xunfei WebSocket response ended before completion"))
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
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: source}
	response.Header.Set("Content-Type", "application/x-spark-frame")
	return response, nil
}
