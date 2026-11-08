package relay

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/pkg/wsmanager"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

type realtimeProtocol struct{}

func (realtimeProtocol) ConvertRequest(_ *gin.Context, _ *relaycommon.RelayInfo, request any) (any, error) {
	switch request.(type) {
	case *dto.RealtimeEvent, dto.RealtimeEvent:
		return request, nil
	}
	return nil, errors.New("Realtime requires a native WebSocket request")
}

func (realtimeProtocol) DoResponse(c *gin.Context, _ *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	apiErr, usage := openai.OpenaiRealtimeHandler(c, info)
	return usage, apiErr
}

func WssHelper(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	usage, apiErr := exchangeRealtime(c, info, nil, nil)
	if apiErr == nil {
		service.PostWssConsumeQuota(c, info, info.UpstreamModelName, usage.(*dto.RealtimeUsage), nil)
	}
	return apiErr
}

// ExchangeRealtime uses the existing native transport with a prepared account.
func ExchangeRealtime(c *gin.Context, info *relaycommon.RelayInfo, prepared *relaychannel.PreparedWssRequest, connected func()) (any, *types.NewAPIError) {
	return exchangeRealtime(c, info, prepared, connected)
}

// exchangeRealtime is the native session transport shared by legacy and wallet
// requests. The caller owns the request's reservation and final settlement.
func exchangeRealtime(c *gin.Context, info *relaycommon.RelayInfo, prepared *relaychannel.PreparedWssRequest, connected func()) (any, *types.NewAPIError) {
	if prepared == nil {
		info.InitChannelMeta(c)
	}
	adaptor := GetAdaptor(info.ApiType)
	if adaptor == nil {
		return nil, types.NewError(fmt.Errorf("invalid api type: %d", info.ApiType), types.ErrorCodeInvalidApiType, types.ErrOptionWithSkipRetry())
	}
	adaptor.Init(info)
	var target *websocket.Conn
	var err error
	if prepared == nil {
		var response any
		response, err = adaptor.DoRequest(c, info, nil)
		if response != nil {
			target, _ = response.(*websocket.Conn)
		}
	} else {
		target, err = relaychannel.DoPreparedWssRequest(c, prepared)
	}
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeDoRequestFailed)
	}
	if target == nil {
		return nil, types.NewError(fmt.Errorf("missing upstream WebSocket connection"), types.ErrorCodeBadResponse)
	}
	info.TargetWs = target
	defer target.Close()
	var closeOnce sync.Once
	unregister := wsmanager.Register(info.ChannelId, wsmanager.KindRealtime, func(reason string) {
		closeOnce.Do(func() {
			deadline := time.Now().Add(time.Second)
			message := websocket.FormatCloseMessage(websocket.ClosePolicyViolation, reason)
			_ = info.ClientWs.WriteControl(websocket.CloseMessage, message, deadline)
			_ = target.WriteControl(websocket.CloseMessage, message, deadline)
			_ = info.ClientWs.Close()
			_ = target.Close()
		})
	})
	defer unregister()
	if connected != nil {
		connected()
	}
	var usage any
	var apiErr *types.NewAPIError
	if prepared != nil {
		apiErr, usage = openai.OpenaiRealtimeHandler(c, info)
	} else {
		usage, apiErr = adaptor.DoResponse(c, nil, info)
	}
	if apiErr != nil {
		service.ResetStatusCode(apiErr, c.GetString("status_code_mapping"))
	}
	return usage, apiErr
}
