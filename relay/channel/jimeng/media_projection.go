package jimeng

import (
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

func (imageRequestPayload) ProjectImageRequest(body []byte, public dto.ImageRequest) (*dto.ImageRequest, error) {
	count, err := public.ImageCount(false)
	if err != nil {
		return nil, err
	}
	if count != 1 {
		return nil, fmt.Errorf("Jimeng synchronous image requests generate one image")
	}
	var wire struct {
		Model      string   `json:"req_key"`
		Prompt     string   `json:"prompt"`
		N          *uint    `json:"n"`
		ImageURLs  []string `json:"image_urls"`
		BinaryData []string `json:"binary_data_base64"`
	}
	if err := common.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	if wire.Model != public.Model || wire.Prompt == "" || wire.N != nil && *wire.N != 1 {
		return nil, fmt.Errorf("invalid Jimeng image parameters")
	}
	inputs := append(wire.ImageURLs, wire.BinaryData...)
	if len(inputs) > dto.MaxImageN {
		return nil, fmt.Errorf("Jimeng image inputs exceed the request limit")
	}
	for _, input := range inputs {
		if strings.TrimSpace(input) == "" {
			return nil, fmt.Errorf("invalid Jimeng image input")
		}
	}
	public.Image = nil
	public.Images, err = common.Marshal(inputs)
	if err != nil {
		return nil, err
	}
	public.N, public.Prompt = common.GetPointer(uint(1)), wire.Prompt
	return &public, nil
}
