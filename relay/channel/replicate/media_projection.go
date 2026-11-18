package replicate

import (
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
)

type imageRequest struct {
	Input        map[string]any `json:"input"`
	requireImage bool
}

func (request imageRequest) ProjectImageRequest(body []byte, public dto.ImageRequest) (*dto.ImageRequest, error) {
	var wire struct {
		Input struct {
			Prompt      string            `json:"prompt"`
			Count       *uint             `json:"num_outputs"`
			Image       common.RawMessage `json:"image_prompt"`
			AspectRatio string            `json:"aspect_ratio"`
			Width       *int              `json:"width"`
			Height      *int              `json:"height"`
		} `json:"input"`
	}
	if err := common.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	count := uint(1)
	if wire.Input.Count != nil {
		count = *wire.Input.Count
	}
	if count == 0 || count > dto.MaxImageN || wire.Input.Prompt == "" {
		return nil, fmt.Errorf("invalid Replicate image quantities")
	}
	if wire.Input.AspectRatio == "custom" {
		if public.Model != ModelFlux11Pro || request.requireImage || len(wire.Input.Image) > 0 && string(wire.Input.Image) != "null" || wire.Input.Width == nil || wire.Input.Height == nil {
			return nil, types.NewMediaParameterError("size", "media_format_unsupported")
		}
		width, height := *wire.Input.Width, *wire.Input.Height
		if err := validateFluxCustomDimensions(width, height); err != nil {
			return nil, err
		}
		public.Size = fmt.Sprintf("%dx%d", width, height)
	} else {
		switch wire.Input.AspectRatio {
		case "", "1:1", "16:9", "3:2", "2:3", "4:5", "5:4", "9:16", "3:4", "4:3":
		default:
			return nil, types.NewMediaParameterError("size", "media_format_unsupported")
		}
		if public.Size != "" && public.Size != "auto" {
			return nil, types.NewMediaParameterError("size", "media_format_unsupported")
		}
		public.Size = ""
	}
	public.Image, public.Images = nil, nil
	if request.requireImage && (len(wire.Input.Image) == 0 || string(wire.Input.Image) == "null") {
		return nil, fmt.Errorf("Replicate image edits require an image input")
	}
	if len(wire.Input.Image) > 0 && string(wire.Input.Image) != "null" {
		var image string
		if common.Unmarshal(wire.Input.Image, &image) != nil || image == "" {
			return nil, fmt.Errorf("Replicate image_prompt must be a nonempty string")
		}
		public.Image = wire.Input.Image
	}
	public.N, public.Prompt = &count, wire.Input.Prompt
	return &public, nil
}
