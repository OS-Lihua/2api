package relayconvert

import (
	"context"
	relaymedia "github.com/QuantumNous/new-api/relaykit/relayconvert/internal/media"
)

type MediaResolver = relaymedia.MediaResolver

// WithMediaResolver overrides media reads for one conversion context.
func WithMediaResolver(ctx context.Context, resolver MediaResolver) context.Context {
	return relaymedia.WithResolver(ctx, resolver)
}

func SetMediaResolver(resolver MediaResolver) {
	relaymedia.SetMediaResolver(resolver)
}
