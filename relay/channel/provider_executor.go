package channel

import (
	"context"
	"errors"
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
)

// ErrUseHTTPTransport asks the host to use its ordinary HTTP transport after a
// provider has determined that no SDK or WebSocket transport is required.
var ErrUseHTTPTransport = errors.New("use ordinary provider HTTP transport")

// ProviderAccountPreparer resolves account credentials before a model Attempt.
// begin must be called immediately before upstream I/O and its context reused.
type ProviderAccountPreparer interface {
	PrepareAccount(context.Context, ProviderMetadata, func() (context.Context, error)) error
}

// ReplayableResponseBody retains protocol-owned metadata when the host buffers
// an HTTP error body and installs a reader for the protocol parser.
type ReplayableResponseBody interface{ SetReader(io.ReadCloser) }

// ProviderExecution performs only the actual upstream exchange. Preparation
// must finish before the host starts an Attempt and its send budget.
type ProviderExecution func(*gin.Context) (*http.Response, error)

type ProviderRequestPreparer interface {
	PrepareExecution(*gin.Context, ProviderMetadata, io.Reader) (ProviderExecution, error)
}

// ProviderRequestExecutor owns non-HTTP transport. The payload and request
// headers must already include administrator overrides before execution.
type ProviderRequestExecutor interface {
	ExecuteRequest(*gin.Context, ProviderMetadata, io.Reader) (*http.Response, error)
}
