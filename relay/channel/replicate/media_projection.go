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
			Prompt string `json:"prompt"`
			Count  *uint  `json:"num_outputs"`
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
	public.N, public.Prompt = &count, wire.Input.Prompt
	return &public, nil
}
