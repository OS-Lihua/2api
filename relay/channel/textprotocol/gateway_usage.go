package textprotocol

import (
	"errors"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/tidwall/gjson"
)

// gatewayUsageAccumulator overlays only present upstream fields. A zero resets
// its field; an omitted field retains the preceding cumulative fact.
type gatewayUsageAccumulator struct {
	format   types.RelayFormat
	snapshot map[string]any
}

func (a *gatewayUsageAccumulator) Observe(body []byte) (*dto.Usage, error) {
	root := gjson.ParseBytes(body)
	if a.format == types.RelayFormatOpenAIResponses && root.Get("response").Exists() {
		root = root.Get("response")
	}
	if a.format == types.RelayFormatClaude && root.Get("message").Exists() {
		root = root.Get("message")
	}
	key := "usage"
	if a.format == types.RelayFormatGemini {
		key = "usageMetadata"
	}
	raw := root.Get(key)
	if !raw.Exists() || raw.Type == gjson.Null {
		return nil, nil
	}
	var incoming map[string]any
	if err := common.UnmarshalJsonStr(raw.Raw, &incoming); err != nil {
		return nil, err
	}
	if incoming == nil {
		return nil, errors.New("invalid upstream usage")
	}
	if a.snapshot == nil {
		a.snapshot = map[string]any{}
	}
	if err := mergeGatewayUsageSnapshot(a.snapshot, incoming); err != nil {
		return nil, err
	}
	encoded, err := common.Marshal(a.snapshot)
	if err != nil {
		return nil, err
	}
	switch a.format {
	case types.RelayFormatClaude:
		var native dto.ClaudeUsage
		if err := common.Unmarshal(encoded, &native); err != nil {
			return nil, err
		}
		return relayconvert.UsageFromClaudeAPIUsage(&native), nil
	case types.RelayFormatGemini:
		var native dto.GeminiUsageMetadata
		if err := common.Unmarshal(encoded, &native); err != nil {
			return nil, err
		}
		return relayconvert.UsageFromGeminiMetadata(&native, 0), nil
	default:
		var native dto.Usage
		if err := common.Unmarshal(encoded, &native); err != nil {
			return nil, err
		}
		if a.format == types.RelayFormatOpenAIResponses {
			return relayconvert.UsageFromResponsesUsage(&native), nil
		}
		return relayconvert.UsageFromChatUsage(&native), nil
	}
}

func mergeGatewayUsageSnapshot(current, incoming map[string]any) error {
	for key, value := range incoming {
		if fields, ok := value.(map[string]any); ok {
			nested, _ := current[key].(map[string]any)
			if nested == nil {
				nested = map[string]any{}
			}
			if err := mergeGatewayUsageSnapshot(nested, fields); err != nil {
				return err
			}
			current[key] = nested
			continue
		}
		if values, ok := value.([]any); ok {
			for _, entry := range values {
				if fields, ok := entry.(map[string]any); ok {
					if err := mergeGatewayUsageSnapshot(map[string]any{}, fields); err != nil {
						return err
					}
				}
			}
			current[key] = value
			continue
		}
		if strings.Contains(strings.ToLower(key), "token") && value != nil {
			number, ok := value.(float64)
			if !ok || number < 0 || number > 1<<31-1 || number != float64(int64(number)) {
				return errors.New("invalid upstream token count")
			}
		}
		current[key] = value
	}
	return nil
}
