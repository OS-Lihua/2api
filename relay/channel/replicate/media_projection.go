package replicate

import (
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

type imageRequest map[string]any

func (imageRequest) ProjectImageRequest(body []byte, public dto.ImageRequest) (*dto.ImageRequest, error) {
	var wire struct {
		Input struct {
			Prompt string            `json:"prompt"`
			Count  *uint             `json:"num_outputs"`
			Image  common.RawMessage `json:"image_prompt"`
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
