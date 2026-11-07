package sub2api

import (
	"github.com/QuantumNous/new-api/relay/channel/newapi"
)

// Protocol reuses the newapi public protocol implementation.
type Protocol struct {
	newapi.Adaptor
}

type Adaptor = Protocol

func (a *Protocol) GetModelList() []string {
	return ModelList
}

func (a *Protocol) GetChannelName() string {
	return ChannelName
}
