package jimeng

import (
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
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
		Width      *int     `json:"width"`
		Height     *int     `json:"height"`
		UseSR      *bool    `json:"use_sr"`
		ReturnURL  *bool    `json:"return_url"`
		ImageURLs  []string `json:"image_urls"`
		BinaryData []string `json:"binary_data_base64"`
	}
	if err := common.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	if wire.Model != public.Model || wire.Prompt == "" || wire.N != nil && *wire.N != 1 {
		return nil, fmt.Errorf("invalid Jimeng image parameters")
	}
	for _, input := range []common.RawMessage{public.Image, public.Images, public.Mask} {
		if len(input) > 0 && string(input) != "null" {
			return nil, types.NewMediaParameterError("images", "media_input_unsupported")
		}
	}
	if len(wire.ImageURLs) > 0 || len(wire.BinaryData) > 0 {
		return nil, types.NewMediaParameterError("images", "media_input_unsupported")
	}
	// V2.1_L dimensions are before super resolution; omitted use_sr is true.
	// https://www.volcengine.com/docs/85621/1537648
	width, height := 512, 512
	if wire.Width != nil {
		width = *wire.Width
	}
	if wire.Height != nil {
		height = *wire.Height
	}
	if width < 256 || width > 768 || height < 256 || height > 768 {
		return nil, types.NewMediaParameterError("size", "media_format_unsupported")
	}
	factor := 1
	if wire.UseSR == nil || *wire.UseSR {
		factor = 2
	}
	public.Size = fmt.Sprintf("%dx%d", width*factor, height*factor)
	if wire.ReturnURL == nil {
		return nil, types.NewMediaParameterError("response_format", "media_format_unsupported")
	}
	public.ResponseFormat = "b64_json"
	if *wire.ReturnURL {
		public.ResponseFormat = "url"
	}
	public.Image, public.Images, public.Mask = nil, nil, nil
	public.N, public.Prompt = common.GetPointer(uint(1)), wire.Prompt
	return &public, nil
}
