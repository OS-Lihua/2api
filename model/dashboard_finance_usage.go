package model

import (
	"fmt"
	"slices"
	"strings"

	"github.com/shopspring/decimal"
)

// Quantities are frozen request facts, not billing-expression inputs. An estimate
// remains labelled, and an absent measurement never becomes a measured zero.
// Schema-specific TaskFacts are intentionally not summed across incompatible units.
type DashboardFinanceUsage struct {
	KnownTotal     string  `json:"known_total"`
	Total          *string `json:"total"`
	MissingCount   string  `json:"missing_count"`
	EstimatedCount string  `json:"estimated_count"`
}

type dashboardUsageAccumulator struct {
	value, missing, estimated decimal.Decimal
}

var dashboardUsageFields = []string{
	"prompt_tokens", "completion_tokens", "total_tokens", "cached_tokens", "cache_creation_tokens", "cache_creation_12h_tokens",
	"image_input_tokens", "image_output_tokens", "audio_input_tokens", "audio_output_tokens", "image_count", "audio_input_seconds", "audio_output_seconds",
}

func newDashboardUsage() map[string]*dashboardUsageAccumulator {
	result := map[string]*dashboardUsageAccumulator{}
	for _, name := range dashboardUsageFields {
		result[name] = &dashboardUsageAccumulator{}
	}
	return result
}

func dashboardUsageResult(usage map[string]*dashboardUsageAccumulator) map[string]DashboardFinanceUsage {
	result := map[string]DashboardFinanceUsage{}
	for _, name := range dashboardUsageFields {
		value := usage[name]
		text := value.value.String()
		item := DashboardFinanceUsage{KnownTotal: text, MissingCount: value.missing.StringFixed(0), EstimatedCount: value.estimated.StringFixed(0)}
		if value.missing.IsZero() {
			item.Total = &text
		}
		result[name] = item
	}
	return result
}

func addDashboardUsage(target, source map[string]*dashboardUsageAccumulator) {
	for _, name := range dashboardUsageFields {
		target[name].value = target[name].value.Add(source[name].value)
		target[name].missing = target[name].missing.Add(source[name].missing)
		target[name].estimated = target[name].estimated.Add(source[name].estimated)
	}
}

func dashboardAddUsage(target map[string]*dashboardUsageAccumulator, request GatewayRequest, inFlight bool) error {
	usage := request.Usage
	missing := func(codes ...string) bool {
		if inFlight || request.UsageSource == "" || request.UsageSource == "none" {
			return true
		}
		for _, code := range codes {
			if slices.Contains(usage.UnmeasuredTokens, code) {
				return true
			}
		}
		return false
	}
	add := func(name string, value decimal.Decimal, absent bool) error {
		if value.IsNegative() {
			return fmt.Errorf("invalid frozen usage quantity")
		}
		if absent {
			target[name].missing = target[name].missing.Add(decimal.NewFromInt(1))
			return nil
		}
		target[name].value = target[name].value.Add(value)
		if request.UsageSource == "estimated" {
			target[name].estimated = target[name].estimated.Add(decimal.NewFromInt(1))
		}
		return nil
	}
	if !slices.Contains([]string{"", "none", "upstream", "estimated"}, request.UsageSource) {
		return fmt.Errorf("invalid frozen usage provenance")
	}
	for _, field := range []struct {
		name  string
		value int64
		codes []string
	}{
		{"prompt_tokens", usage.PromptTokens, []string{"p"}}, {"completion_tokens", usage.CompletionTokens, []string{"c"}}, {"total_tokens", usage.TotalTokens, []string{"p", "c"}},
		{"cached_tokens", usage.CachedTokens, []string{"cr"}}, {"cache_creation_tokens", usage.CacheCreationTokens, []string{"cc"}}, {"cache_creation_12h_tokens", usage.CacheCreation12hTokens, []string{"cc12h"}},
	} {
		if err := add(field.name, decimal.NewFromInt(field.value), missing(field.codes...)); err != nil {
			return err
		}
	}
	for _, field := range []struct {
		name  string
		value *int64
		code  string
	}{
		{"image_input_tokens", usage.ImageInputTokens, "img"}, {"image_output_tokens", usage.ImageOutputTokens, "img_o"},
		{"audio_input_tokens", usage.AudioInputTokens, "ai"}, {"audio_output_tokens", usage.AudioOutputTokens, "ao"}, {"image_count", usage.ImageCount, "img_count"},
	} {
		value := decimal.Zero
		if field.value != nil {
			value = decimal.NewFromInt(*field.value)
		}
		if err := add(field.name, value, field.value == nil || missing(field.code)); err != nil {
			return err
		}
	}
	for _, field := range []struct {
		name  string
		value *string
	}{
		{"audio_input_seconds", usage.AudioInputSeconds}, {"audio_output_seconds", usage.AudioOutputSeconds},
	} {
		value := decimal.Zero
		if field.value != nil {
			text := *field.value
			if text == "" || len(text) > 80 || strings.TrimSpace(text) != text {
				return fmt.Errorf("invalid frozen usage quantity")
			}
			for _, r := range text {
				if (r < '0' || r > '9') && r != '.' {
					return fmt.Errorf("invalid frozen usage quantity")
				}
			}
			var err error
			value, err = decimal.NewFromString(text)
			if err != nil {
				return fmt.Errorf("invalid frozen usage quantity")
			}
		}
		if err := add(field.name, value, field.value == nil || missing()); err != nil {
			return err
		}
	}
	return nil
}
