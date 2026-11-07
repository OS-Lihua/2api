package convdiag

import (
	"context"
	"fmt"
	"slices"

	"github.com/QuantumNous/new-api/relaykit/types"
)

// InspectCacheControls reports explicit cache boundaries that the target text
// protocol cannot express, even when an extended DTO retains the JSON member.
func InspectCacheControls(ctx context.Context, value any, path string, from, to types.RelayFormat) {
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
				Add(ctx, types.ConversionDiagnostic{Code: "cache_control_dropped", Path: child, Message: "The target protocol cannot express explicit cache_control boundaries", Severity: types.ConversionDiagnosticWarning, From: from, To: to})
				continue
			}
			// Schema properties and opaque tool inputs are user data, not controls.
			if key == "properties" || key == "input" || key == "arguments" || key == "parameters" || key == "input_schema" {
				continue
			}
			InspectCacheControls(ctx, value[key], child, from, to)
		}
	case []any:
		for i, child := range value {
			InspectCacheControls(ctx, child, fmt.Sprintf("%s[%d]", path, i), from, to)
		}
	}
}
