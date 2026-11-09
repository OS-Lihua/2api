package convdiag

import (
	"context"
	"fmt"
	"slices"

	"github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/QuantumNous/new-api/relaykit/types"
)

type cacheBoundary struct {
	path     string
	identity string
}

// InspectCacheControls permits a supported cache boundary only when the
// converted request actually retained it. Provider identity alone is insufficient.
func InspectCacheControls(ctx context.Context, source, target any, from, to types.RelayFormat, supportsBoundaries bool) {
	retained := map[string]int{}
	if supportsBoundaries {
		for _, boundary := range cacheBoundaries(target, "") {
			retained[boundary.identity]++
		}
	}
	for _, boundary := range cacheBoundaries(source, "") {
		if retained[boundary.identity] > 0 {
			retained[boundary.identity]--
			continue
		}
		Add(ctx, types.ConversionDiagnostic{Code: "cache_control_dropped", Path: boundary.path, Message: "The target request did not retain this cache_control boundary", Severity: types.ConversionDiagnosticWarning, From: from, To: to})
	}
}

func cacheBoundaries(value any, path string) []cacheBoundary {
	var boundaries []cacheBoundary
	switch value := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			child := key
			if path != "" {
				child = path + "." + key
			}
			if key == "cache_control" && value[key] != nil {
				encoded, err := kitutil.Marshal(value[key])
				if err != nil {
					encoded = []byte("invalid")
				}
				anchor, _ := value["text"].(string)
				if name, ok := value["name"].(string); ok {
					anchor = "tool:" + name
				}
				if function, ok := value["function"].(map[string]any); ok {
					if name, ok := function["name"].(string); ok {
						anchor = "tool:" + name
					}
				}
				boundaries = append(boundaries, cacheBoundary{path: child, identity: string(encoded) + "\x00" + anchor})
				continue
			}
			// Schemas, metadata and opaque tool inputs are data, not controls.
			if key == "properties" || key == "input" || key == "arguments" || key == "parameters" || key == "input_schema" || key == "metadata" || key == "responseSchema" || key == "responseJsonSchema" {
				continue
			}
			boundaries = append(boundaries, cacheBoundaries(value[key], child)...)
		}
	case []any:
		for i, child := range value {
			boundaries = append(boundaries, cacheBoundaries(child, fmt.Sprintf("%s[%d]", path, i))...)
		}
	}
	return boundaries
}
