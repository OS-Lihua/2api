package channel

import (
	"errors"
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
)

// ErrUseHTTPTransport asks the host to use its ordinary HTTP transport after a
// provider has determined that no SDK or WebSocket transport is required.
var ErrUseHTTPTransport = errors.New("use ordinary provider HTTP transport")

// ProviderRequestExecutor owns non-HTTP transport. The payload and request
// headers must already include administrator overrides before execution.
type ProviderRequestExecutor interface {
	ExecuteRequest(*gin.Context, ProviderMetadata, io.Reader) (*http.Response, error)
}
