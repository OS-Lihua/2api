package controller

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Model metadata may declare endpoints either as a type array or as a path map.
// Conversion must understand both; reading only the map form made every model
// with array-form metadata look like a broken routing configuration.
func TestModelPricingConversionAcceptsDeclaredEndpointArray(t *testing.T) {
	db := modelManagementDB(t)
	previousQuota := common.QuotaPerUnit
	common.QuotaPerUnit = 500000
	t.Cleanup(func() { common.QuotaPerUnit = previousQuota })

	for _, tc := range []struct {
		name        string
		endpoints   string
		draft       model.PricingValues
		wantImageCt bool
	}{
		// Legacy source JSON decodes numeric values as float64. Keep that
		// representation when exercising the captured source directly.
		{"array-endpoints-image", `["image-generation","openai"]`,
			model.PricingValues{"ModelRatio": 1.7875, "CompletionRatio": 6.0, "ImageRatio": 1.6}, false},
		{"array-endpoints-image-fixed", `["image-generation","openai"]`,
			model.PricingValues{"ModelPrice": 1.0}, true},
		{"array-endpoints-gemini", `["gemini","openai"]`,
			model.PricingValues{"ModelRatio": 0.10725, "CompletionRatio": 100.0}, false},
		{"array-endpoints-gemini-cache", `["gemini","openai"]`,
			model.PricingValues{"ModelRatio": 0.715, "CompletionRatio": 60.0, "CacheRatio": 0.1}, false},
		{"array-endpoints-openai-response", `["openai","openai-response","openai-response-compact","gemini"]`,
			model.PricingValues{"ModelRatio": 0.17875, "CompletionRatio": 120.0}, false},
		{"map-endpoints-image", `{"image-generation": {"path": "/v1/images/generations", "method": "POST"}}`,
			model.PricingValues{"ModelPrice": 1.0}, true},
		// Endpoint names must be trimmed before the switch matches them: a
		// padded name would otherwise match nothing and silently downgrade a
		// fixed-price image model to per-request billing.
		{"array-endpoints-image-padded", `[" image-generation ","openai"]`,
			model.PricingValues{"ModelPrice": 1.0}, true},
		{"map-endpoints-image-padded", `{"image-generation ": {"path": "/v1/images/generations", "method": "POST"}}`,
			model.PricingValues{"ModelPrice": 1.0}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, db.Create(&model.Model{
				ModelName: tc.name, Endpoints: tc.endpoints, Status: 1, NameRule: model.NameRuleExact,
			}).Error)
			converted, err := legacyPricingSource(t, db).Preview(tc.name, tc.draft)
			require.NoError(t, err)
			t.Logf("PROBE %-34s expression=%q unsupported=%q image_count=%v",
				tc.name, converted.Expression, converted.UnsupportedReason,
				converted.BillingDetails.ImageCount)
			require.Empty(t, converted.UnsupportedReason)
			require.NotEmpty(t, converted.Expression)
			// Declared endpoint types must actually reach the endpoint switch;
			// an array of names read as array indices would silently skip it.
			assert.Equal(t, tc.wantImageCt, converted.BillingDetails.ImageCount,
				"declared image-generation endpoint must drive image-count billing")
		})
	}
}
