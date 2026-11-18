package operation_setting

import (
	"bytes"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/setting/config"
)

// RewardConfig keeps the three independently denominated promises in one snapshot.
type RewardConfig struct {
	Signup  money.Amount `json:"signup"`
	Inviter money.Amount `json:"inviter"`
	Invitee money.Amount `json:"invitee"`
}

func (r *RewardConfig) UnmarshalJSON(data []byte) error {
	type amount struct {
		Nanos    *int64         `json:"nanos,string"`
		Currency money.Currency `json:"currency"`
	}
	var input struct {
		Signup  *amount `json:"signup"`
		Inviter *amount `json:"inviter"`
		Invitee *amount `json:"invitee"`
	}
	if err := common.DecodeJsonStrict(bytes.NewReader(data), &input); err != nil {
		return common.NewMessage("Invalid amount")
	}
	var next RewardConfig
	for i, source := range []*amount{input.Signup, input.Inviter, input.Invitee} {
		if source == nil || source.Nanos == nil || *source.Nanos < 0 {
			return common.NewMessage("Invalid amount")
		}
		if _, err := money.ParseCurrency(string(source.Currency)); err != nil {
			return common.NewMessage("Invalid currency")
		}
		*[]*money.Amount{&next.Signup, &next.Inviter, &next.Invitee}[i] = money.Amount{Nanos: *source.Nanos, Currency: source.Currency}
	}
	*r = next
	return nil
}

type RewardSetting struct {
	Config RewardConfig `json:"config"`
}

func init() {
	config.GlobalConfig.Register("reward_setting", &RewardSetting{Config: RewardConfig{
		Signup: money.Amount{Currency: "USD"}, Inviter: money.Amount{Currency: "USD"}, Invitee: money.Amount{Currency: "USD"},
	}})
}

func GetRewardSetting() RewardConfig {
	return config.GlobalConfig.Get("reward_setting").(*RewardSetting).Config
}

func IsRewardOption(key string) bool {
	return key == "reward_setting" || strings.HasPrefix(key, "reward_setting.") || key == "QuotaForNewUser" || key == "QuotaForInviter" || key == "QuotaForInvitee"
}

func ValidateRewardOption(key, value string) error {
	if !IsRewardOption(key) {
		return nil
	}
	if key != "reward_setting.config" {
		return common.NewMessage("Invalid parameters")
	}
	var next RewardConfig
	return common.DecodeJsonStrict(strings.NewReader(value), &next)
}

// ApplyRewardOption publishes only a complete, validated replacement.
func ApplyRewardOption(key, value string) error {
	if err := ValidateRewardOption(key, value); err != nil {
		return err
	}
	var next RewardConfig
	if err := common.DecodeJsonStrict(strings.NewReader(value), &next); err != nil {
		return err
	}
	config.GlobalConfig.Register("reward_setting", &RewardSetting{Config: next})
	return nil
}
