package model_setting

import "strings"

const RetiredViolationFeeSettingsMessage = "Legacy violation fee settings have been retired."

func IsRetiredViolationFeeOption(key string) bool {
	return strings.EqualFold(key, "grok.violation_deduction_enabled") || strings.EqualFold(key, "grok.violation_deduction_amount")
}
