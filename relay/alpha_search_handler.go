package relay

import (
	"errors"

	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/MoeForge/moego/ojson"
)

// buildAlphaSearchRequestBody returns RawBody unchanged unless the model was
// mapped, in which case only the "model" field is rewritten so unknown fields
// are preserved.
func buildAlphaSearchRequestBody(rawBody []byte, originModel, upstreamModel string) ([]byte, error) {
	if len(rawBody) == 0 {
		return nil, errors.New("empty alpha search request body")
	}
	if upstreamModel == "" || upstreamModel == originModel {
		return rawBody, nil
	}
	if !ojson.ValidBytes(rawBody) || !ojson.GetBytesNoCopy(rawBody, "@this").IsObject() {
		return nil, errors.New("alpha search request body must be a JSON object")
	}
	body := ojson.NewDoc(rawBody)
	if err := relaycommon.SetJSONMember(body, "", "model", upstreamModel); err != nil {
		return nil, err
	}
	return body.Bytes(), nil
}
