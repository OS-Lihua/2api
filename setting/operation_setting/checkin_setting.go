package operation_setting

import (
	"bytes"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/setting/config"
)

// CheckinRewardRange retains the original denomination of both inclusive bounds.
type CheckinRewardRange struct {
	Min money.Amount `json:"min"`
	Max money.Amount `json:"max"`
}

func (r CheckinRewardRange) Validate() error {
	if _, err := money.ParseCurrency(string(r.Min.Currency)); err != nil {
		return common.NewMessage("Invalid amount")
	}
	if r.Min.Currency != r.Max.Currency || r.Min.Nanos < 0 || r.Max.Nanos < r.Min.Nanos {
		return common.NewMessage("Invalid amount")
	}
	return nil
}

// UnmarshalJSON requires complete bounds, string nanos and no unknown fields.
// Decode into fresh values so a partial or invalid range cannot mutate settings.
func (r *CheckinRewardRange) UnmarshalJSON(data []byte) error {
	type bound struct {
		Nanos    *int64         `json:"nanos,string"`
		Currency money.Currency `json:"currency"`
	}
	var input struct {
		Min *bound `json:"min"`
		Max *bound `json:"max"`
	}
	if err := common.DecodeJsonStrict(bytes.NewReader(data), &input); err != nil {
		return common.NewMessage("Invalid amount")
	}
	if input.Min == nil || input.Max == nil || input.Min.Nanos == nil || input.Max.Nanos == nil {
		return common.NewMessage("Invalid amount")
	}
	next := CheckinRewardRange{
		Min: money.Amount{Nanos: *input.Min.Nanos, Currency: input.Min.Currency},
		Max: money.Amount{Nanos: *input.Max.Nanos, Currency: input.Max.Currency},
	}
	if err := next.Validate(); err != nil {
		return err
	}
	*r = next
	return nil
}

// CheckinSetting publishes both bounds together through one reward_range option.
type CheckinSetting struct {
	Enabled     bool               `json:"enabled"`
	RewardRange CheckinRewardRange `json:"reward_range"`
}

func init() {
	config.GlobalConfig.Register("checkin_setting", &CheckinSetting{
		RewardRange: CheckinRewardRange{
			Min: money.Amount{Nanos: 2_000_000, Currency: "USD"},
			Max: money.Amount{Nanos: 20_000_000, Currency: "USD"},
		},
	})
}

// GetCheckinSetting returns a snapshot. Production updates replace the registered
// pointer, so a request keeps the same enabled flag and complete reward range.
func GetCheckinSetting() *CheckinSetting {
	current := config.GlobalConfig.Get("checkin_setting").(*CheckinSetting)
	snapshot := *current
	return &snapshot
}

func IsCheckinEnabled() bool {
	return GetCheckinSetting().Enabled
}

func IsCheckinOption(key string) bool {
	return key == "checkin_setting" || strings.HasPrefix(key, "checkin_setting.")
}

func ValidateCheckinOption(key, value string) error {
	if !IsCheckinOption(key) {
		return nil
	}
	switch key {
	case "checkin_setting.enabled":
		if value != "true" && value != "false" {
			return common.NewMessage("Invalid parameters")
		}
		return nil
	case "checkin_setting.reward_range":
		var rewardRange CheckinRewardRange
		return common.DecodeJsonStrict(strings.NewReader(value), &rewardRange)
	default:
		return common.NewMessage("Invalid parameters")
	}
}

// ApplyCheckinOption is serialized by the option map lock. Validated snapshots
// bypass the generic configuration updater's permissive in-place reflection.
func ApplyCheckinOption(key, value string) error {
	if err := ValidateCheckinOption(key, value); err != nil {
		return err
	}
	next := GetCheckinSetting()
	switch key {
	case "checkin_setting.enabled":
		next.Enabled = value == "true"
	case "checkin_setting.reward_range":
		if err := common.DecodeJsonStrict(strings.NewReader(value), &next.RewardRange); err != nil {
			return err
		}
	default:
		return common.NewMessage("Invalid parameters")
	}
	config.GlobalConfig.Register("checkin_setting", next)
	return nil
}
