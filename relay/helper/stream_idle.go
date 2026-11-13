package helper

import (
	"errors"
	"io"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/clock"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
)

// GatewayStreamIdleBody bounds all native readers after business delivery.
// Reads and business writes renew the idle deadline; local preparation and
// pre-business metadata remain governed by the shared first-byte budget.
func GatewayStreamIdleBody(body io.ReadCloser, info *relaycommon.RelayInfo) io.ReadCloser {
	if info.ResponseFacts == nil || info.ResponseFacts.OnBusinessOutput == nil || constant.StreamingTimeout <= 0 {
		return body
	}
	guard := &gatewayIdleBody{body: body, status: info.StreamStatus, timeout: time.Duration(constant.StreamingTimeout) * time.Second}
	delivered := info.ResponseFacts.OnBusinessOutput
	info.ResponseFacts.OnBusinessOutput = func() { delivered(); guard.touch(true) }
	return guard
}

type gatewayIdleBody struct {
	body           io.ReadCloser
	status         *relaycommon.StreamStatus
	timeout        time.Duration
	mu             sync.Mutex
	active, closed bool
	generation     uint64
	stop           func() bool
}

func (b *gatewayIdleBody) touch(activate bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || !b.active && !activate {
		return
	}
	b.active = true
	b.generation++
	generation := b.generation
	if b.stop != nil {
		b.stop()
	}
	b.stop = clock.AfterFunc(b.timeout, func() {
		b.mu.Lock()
		if b.closed || b.generation != generation {
			b.mu.Unlock()
			return
		}
		b.closed = true
		b.mu.Unlock()
		b.status.MarkFailed("stream_idle_timeout", "", 0)
		b.status.SetEndReason(relaycommon.StreamEndReasonTimeout, errors.New("upstream stream idle timeout"))
		_ = b.body.Close()
	})
}

func (b *gatewayIdleBody) UnwrapResponseBody() io.ReadCloser { return b.body }

func (b *gatewayIdleBody) Read(data []byte) (int, error) {
	n, err := b.body.Read(data)
	if n > 0 {
		b.touch(false)
	}
	return n, err
}

func (b *gatewayIdleBody) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	if b.stop != nil {
		b.stop()
	}
	b.mu.Unlock()
	return b.body.Close()
}
