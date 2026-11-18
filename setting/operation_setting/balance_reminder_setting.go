package operation_setting

import (
	"bytes"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/setting/config"
)

const BalanceReminderConfigOption = "balance_reminder_setting.config"

type BalanceReminderConfig struct {
	Enabled   bool         `json:"enabled"`
	Threshold money.Amount `json:"threshold"`
}

func (c *BalanceReminderConfig) UnmarshalJSON(data []byte) error {
	var input struct {
		Enabled   *bool `json:"enabled"`
		Threshold *struct {
			Nanos    *int64         `json:"nanos,string"`
			Currency money.Currency `json:"currency"`
		} `json:"threshold"`
	}
	if err := common.DecodeJsonStrict(bytes.NewReader(data), &input); err != nil {
		return common.NewMessage("Invalid parameters")
	}
	if input.Enabled == nil || input.Threshold == nil || input.Threshold.Nanos == nil || *input.Threshold.Nanos < 0 {
		return common.NewMessage("Invalid amount")
	}
	if _, err := money.ParseCurrency(string(input.Threshold.Currency)); err != nil {
		return common.NewMessage("Invalid currency")
	}
	*c = BalanceReminderConfig{Enabled: *input.Enabled, Threshold: money.Amount{Nanos: *input.Threshold.Nanos, Currency: input.Threshold.Currency}}
	return nil
}

type BalanceReminderSetting struct {
	Config BalanceReminderConfig `json:"config"`
}

func init() {
	// Startup migration supplies the actual settlement-currency default. No runtime
	// consumer should infer a migrated USD threshold from this registration value.
	config.GlobalConfig.Register("balance_reminder_setting", &BalanceReminderSetting{})
}

func IsBalanceReminderOption(key string) bool {
	return key == "balance_reminder_setting" || strings.HasPrefix(key, "balance_reminder_setting.") || key == "QuotaRemindEnabled" || key == "QuotaRemindThreshold"
}

func ParseBalanceReminderConfig(value string) (BalanceReminderConfig, error) {
	var result BalanceReminderConfig
	err := common.DecodeJsonStrict(strings.NewReader(value), &result)
	return result, err
}

func ValidateBalanceReminderOption(key, value string) error {
	if !IsBalanceReminderOption(key) {
		return nil
	}
	if key != BalanceReminderConfigOption {
		return common.NewMessage("Invalid parameters")
	}
	_, err := ParseBalanceReminderConfig(value)
	return err
}

func ApplyBalanceReminderOption(key, value string) error {
	if err := ValidateBalanceReminderOption(key, value); err != nil {
		return err
	}
	parsed, err := ParseBalanceReminderConfig(value)
	if err != nil {
		return err
	}
	config.GlobalConfig.Register("balance_reminder_setting", &BalanceReminderSetting{Config: parsed})
	return nil
}
