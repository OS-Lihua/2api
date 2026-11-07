package service

import (
	"context"
	"errors"
	"strings"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

const gatewayMediaContextKey = "gateway_prepared_media"

type gatewayMediaSource struct{ data, mime string }
type gatewayMediaContent struct{ base64, mime string }

func gatewayMediaIdentity(source types.FileSource) gatewayMediaSource {
	key := gatewayMediaSource{data: source.GetRawData()}
	if inline, ok := source.(*types.Base64Source); ok {
		key.mime = inline.MimeType
	}
	return key
}

// PrepareGatewayMedia resolves client media once before candidate preconversion.
// Both token estimation and converters subsequently read memory, including when
// the ordinary file loader initially chose its disk cache.
func PrepareGatewayMedia(c *gin.Context, request dto.Request) error {
	prepared := map[gatewayMediaSource]gatewayMediaContent{}
	for _, file := range request.GetTokenCountMeta().Files {
		source := file.Source
		if source == nil {
			continue
		}
		identity := gatewayMediaIdentity(source)
		if _, exists := prepared[identity]; exists {
			continue
		}
		cached, err := LoadFileSource(c, source, "preparing gateway media")
		if err != nil {
			return errors.New("could not prepare request media")
		}
		data, err := cached.GetBase64Data()
		if err != nil {
			return errors.New("could not prepare request media")
		}
		prepared[identity] = gatewayMediaContent{data, cached.MimeType}
		if !cached.IsDisk() {
			continue
		}
		memory := types.NewMemoryCachedData(data, cached.MimeType, cached.Size)
		memory.ImageConfig, memory.ImageFormat = cached.ImageConfig, cached.ImageFormat
		if err := cached.Close(); err != nil {
			return errors.New("could not prepare request media")
		}
		source.SetCache(memory)
		switch original := source.(type) {
		case *types.URLSource:
			c.Set(getContextCacheKey(original.URL), memory)
		case *types.Base64Source:
			c.Set(getBase64ContextCacheKey(identity.data, identity.mime), memory)
		}
	}
	c.Set(gatewayMediaContextKey, prepared)
	return nil
}

func gatewayConversionContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	var prepared map[gatewayMediaSource]gatewayMediaContent
	if c, ok := ctx.(*gin.Context); ok {
		if value, exists := c.Get(gatewayMediaContextKey); exists {
			prepared, _ = value.(map[gatewayMediaSource]gatewayMediaContent)
		}
	}
	return relayconvert.WithMediaResolver(ctx, relayconvert.MediaResolver{
		GetBase64Data: func(_ context.Context, source types.FileSource, _ ...string) (string, string, error) {
			if content, exists := prepared[gatewayMediaIdentity(source)]; exists {
				return content.base64, content.mime, nil
			}
			if inline, ok := source.(*types.Base64Source); ok {
				if inline.MimeType != "" && !strings.HasPrefix(inline.Base64Data, "data:") {
					return inline.Base64Data, inline.MimeType, nil
				}
				mime, data, err := DecodeBase64FileData(inline.Base64Data)
				return data, mime, err
			}
			return "", "", errors.New("request media was not prepared")
		},
		DecodeBase64FileData: DecodeBase64FileData,
	})
}
