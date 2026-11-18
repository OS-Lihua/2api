package operation_setting

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/shopspring/decimal"
)

const CheckoutConfigOption = "checkout_setting.config"

var CheckoutProviders = []string{"epay", "stripe", "creem", "waffo", "waffo_pancake"}

type CheckoutDiscount struct {
	SourceAmount money.Amount `json:"source_amount"`
	Ratio        string       `json:"ratio"`
}

type CheckoutProduct struct {
	ProductID    string       `json:"product_id"`
	Name         string       `json:"name"`
	SourceAmount money.Amount `json:"source_amount"`
}

type CheckoutProvider struct {
	Enabled     bool               `json:"enabled"`
	SourceUnit  *money.Amount      `json:"source_unit,omitempty"`
	ChargeUnit  *money.Amount      `json:"charge_unit,omitempty"`
	Minimum     *money.Amount      `json:"minimum,omitempty"`
	Presets     []money.Amount     `json:"presets,omitempty"`
	GroupRatios map[string]string  `json:"group_ratios,omitempty"`
	Discounts   []CheckoutDiscount `json:"discounts,omitempty"`
	Products    []CheckoutProduct  `json:"products,omitempty"`
}

func (p *CheckoutProvider) UnmarshalJSON(data []byte) error {
	type plain CheckoutProvider
	var next plain
	if common.DecodeJsonStrict(bytes.NewReader(data), &next) != nil {
		return common.NewMessage("Invalid parameters")
	}
	var fields map[string]json.RawMessage
	if common.Unmarshal(data, &fields) != nil || fields == nil {
		return common.NewMessage("Invalid parameters")
	}
	for _, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return common.NewMessage("Invalid parameters")
		}
	}
	*p = CheckoutProvider(next)
	return nil
}

type CheckoutConfig struct {
	Providers map[string]CheckoutProvider `json:"providers"`
}

type CheckoutSetting struct {
	Config CheckoutConfig `json:"config"`
}

func init() {
	config.GlobalConfig.Register("checkout_setting", &CheckoutSetting{Config: CheckoutConfig{Providers: map[string]CheckoutProvider{}}})
}

func ValidCheckoutAmount(amount money.Amount) bool {
	_, err := money.ParseCurrency(string(amount.Currency))
	return err == nil && amount.Nanos > 0
}

func CheckoutRatio(text string) (decimal.Decimal, error) {
	if text == "" || strings.TrimSpace(text) != text || len(text) > 40 {
		return decimal.Decimal{}, common.NewMessage("Invalid amount")
	}
	for _, c := range text {
		if (c < '0' || c > '9') && c != '.' {
			return decimal.Decimal{}, common.NewMessage("Invalid amount")
		}
	}
	ratio, err := decimal.NewFromString(text)
	if err != nil || !ratio.IsPositive() || ratio.GreaterThan(decimal.NewFromInt(1_000_000_000)) || ratio.Exponent() < -18 {
		return decimal.Decimal{}, common.NewMessage("Invalid amount")
	}
	return ratio, nil
}

func (p CheckoutProvider) Configured(provider string) bool {
	if provider == "stripe" || provider == "creem" {
		return len(p.Products) > 0
	}
	return p.SourceUnit != nil && p.ChargeUnit != nil && p.Minimum != nil
}

func (c CheckoutConfig) Validate() error {
	if c.Providers == nil || len(c.Providers) > len(CheckoutProviders) {
		return common.NewMessage("Invalid parameters")
	}
	for name, provider := range c.Providers {
		switch name {
		case "epay", "stripe", "creem", "waffo", "waffo_pancake":
		default:
			return common.NewMessage("Invalid parameters")
		}
		if !provider.Configured(name) {
			return common.NewMessage("Payment configuration error")
		}
		for _, amount := range []*money.Amount{provider.SourceUnit, provider.ChargeUnit, provider.Minimum} {
			if amount != nil && !ValidCheckoutAmount(*amount) {
				return common.NewMessage("Invalid amount")
			}
		}
		if name == "stripe" || name == "creem" {
			if provider.SourceUnit != nil || provider.ChargeUnit != nil || provider.Minimum != nil || len(provider.Presets) != 0 || len(provider.Discounts) != 0 {
				return common.NewMessage("Invalid parameters")
			}
		} else {
			if len(provider.Products) != 0 || (provider.SourceUnit != nil && provider.Minimum != nil && provider.SourceUnit.Currency != provider.Minimum.Currency) {
				return common.NewMessage("Invalid amount")
			}
		}
		if name == "waffo_pancake" && provider.ChargeUnit != nil && provider.ChargeUnit.Currency != "USD" {
			return common.NewMessage("Invalid amount")
		}
		for group, text := range provider.GroupRatios {
			if group == "" || strings.TrimSpace(group) != group || len(group) > 64 {
				return common.NewMessage("Invalid parameters")
			}
			if _, err := CheckoutRatio(text); err != nil {
				return err
			}
		}
		if name == "creem" && len(provider.GroupRatios) != 0 {
			return common.NewMessage("Invalid parameters")
		}
		seenProducts := map[string]bool{}
		for _, product := range provider.Products {
			if product.ProductID == "" || strings.TrimSpace(product.ProductID) != product.ProductID || len(product.ProductID) > 255 || product.Name == "" || len(product.Name) > 255 || seenProducts[product.ProductID] || !ValidCheckoutAmount(product.SourceAmount) {
				return common.NewMessage("Invalid parameters")
			}
			seenProducts[product.ProductID] = true
		}
		seenAmounts := map[int64]bool{}
		for _, amount := range provider.Presets {
			if !ValidCheckoutAmount(amount) || provider.SourceUnit == nil || provider.Minimum == nil || amount.Currency != provider.SourceUnit.Currency || amount.Nanos < provider.Minimum.Nanos || seenAmounts[amount.Nanos] {
				return common.NewMessage("Invalid amount")
			}
			seenAmounts[amount.Nanos] = true
		}
		clear(seenAmounts)
		for _, discount := range provider.Discounts {
			if !ValidCheckoutAmount(discount.SourceAmount) || provider.SourceUnit == nil || discount.SourceAmount.Currency != provider.SourceUnit.Currency || seenAmounts[discount.SourceAmount.Nanos] {
				return common.NewMessage("Invalid amount")
			}
			ratio, err := CheckoutRatio(discount.Ratio)
			if err != nil || ratio.GreaterThan(decimal.NewFromInt(1)) {
				return common.NewMessage("Invalid amount")
			}
			seenAmounts[discount.SourceAmount.Nanos] = true
		}
	}
	return nil
}

func ParseCheckoutConfig(text string) (CheckoutConfig, error) {
	var next CheckoutConfig
	if err := common.DecodeJsonStrict(strings.NewReader(text), &next); err != nil {
		return CheckoutConfig{}, common.NewMessage("Invalid parameters")
	}
	if err := next.Validate(); err != nil {
		return CheckoutConfig{}, err
	}
	return next, nil
}

func IsCheckoutOption(key string) bool {
	return key == "checkout_setting" || strings.HasPrefix(key, "checkout_setting.")
}

func ValidateCheckoutOption(key, value string) error {
	if !IsCheckoutOption(key) {
		return nil
	}
	if key != CheckoutConfigOption {
		return common.NewMessage("Invalid parameters")
	}
	_, err := ParseCheckoutConfig(value)
	return err
}

// GetCheckoutConfig returns an isolated snapshot of the complete configuration.
func GetCheckoutConfig() CheckoutConfig {
	current := config.GlobalConfig.Get("checkout_setting").(*CheckoutSetting)
	data, err := common.Marshal(current.Config)
	if err != nil {
		return CheckoutConfig{Providers: map[string]CheckoutProvider{}}
	}
	var snapshot CheckoutConfig
	if common.Unmarshal(data, &snapshot) != nil {
		return CheckoutConfig{Providers: map[string]CheckoutProvider{}}
	}
	return snapshot
}

// ApplyCheckoutOption is called under OptionMapRWMutex after full validation.
func ApplyCheckoutOption(key, value string) error {
	if key != CheckoutConfigOption {
		return common.NewMessage("Invalid parameters")
	}
	next, err := ParseCheckoutConfig(value)
	if err != nil {
		return err
	}
	config.GlobalConfig.Register("checkout_setting", &CheckoutSetting{Config: next})
	return nil
}
