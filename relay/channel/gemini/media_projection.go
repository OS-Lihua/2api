package gemini

import (
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
)

// imagenRequest retains the native Imagen wire shape and its quantity contract.
type imagenRequest dto.GeminiImageRequest

func (imagenRequest) ProjectImageRequest(body []byte, public dto.ImageRequest) (*dto.ImageRequest, error) {
	if public.ResponseFormat != "" && public.ResponseFormat != "b64_json" {
		return nil, types.NewMediaParameterError("response_format", "media_format_unsupported")
	}
	if public.Size != "" && public.Size != "auto" {
		return nil, types.NewMediaParameterError("size", "media_format_unsupported")
	}
	for _, input := range []common.RawMessage{public.Image, public.Images, public.Mask} {
		if len(input) > 0 && string(input) != "null" {
			return nil, fmt.Errorf("Imagen generation does not support image or mask inputs")
		}
	}
	var wire struct {
		Instances  []dto.GeminiImageInstance `json:"instances"`
		Parameters struct {
			SampleCount *uint  `json:"sampleCount"`
			AspectRatio string `json:"aspectRatio"`
			ImageSize   string `json:"imageSize"`
		} `json:"parameters"`
	}
	if err := common.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	if len(wire.Instances) != 1 || wire.Instances[0].Prompt == "" {
		return nil, fmt.Errorf("Imagen requires one prompt instance")
	}
	count := uint(1)
	if wire.Parameters.SampleCount != nil {
		count = *wire.Parameters.SampleCount
	}
	if count == 0 || count > dto.MaxImageN {
		return nil, fmt.Errorf("Imagen sampleCount exceeds the image count limit")
	}
	switch wire.Parameters.AspectRatio {
	case "", "1:1", "3:4", "4:3", "9:16", "16:9":
	default:
		return nil, fmt.Errorf("unsupported Imagen aspect ratio")
	}
	switch wire.Parameters.ImageSize {
	case "", "1K":
		public.Quality = "standard"
	case "2K":
		public.Quality = "high"
	default:
		return nil, fmt.Errorf("Imagen imageSize exceeds the image dimension limit")
	}
	public.N = &count
	public.Prompt = wire.Instances[0].Prompt
	return &public, nil
}
