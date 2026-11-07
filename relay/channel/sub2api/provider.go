package sub2api

import (
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/newapi"
)

// Provider uses the shared upstream addressing and authentication implementation.
type Provider struct{ newapi.Provider }

var _ channel.ProviderAdapter = (*Provider)(nil)
