package model

import (
	"encoding/json"
	"math/big"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"gorm.io/gorm"
)

// migrateCheckout preserves every legacy order and subscription history row.
// New exact facts and verified receipts live in their own tables.
func migrateCheckout(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.AutoMigrate(&Option{}, &TopUp{}, &CheckoutOrder{}, &CheckoutPayment{}); err != nil {
			return err
		}
		for _, statement := range []string{
			`CREATE OR REPLACE FUNCTION protect_checkout_facts() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
			IF ROW(OLD.trade_no, OLD.expires_at, OLD.charge_nanos, OLD.charge_currency, OLD.charge_final, OLD.unit_charge_nanos, OLD.unit_charge_currency, OLD.promotion_codes, OLD.tax_mode, OLD.source_nanos, OLD.source_currency, OLD.credit_nanos, OLD.credit_currency, OLD.fx_rate, OLD.fx_frozen_at, OLD.product_id, OLD.quantity, OLD.provider_mode, OLD.provider_store_id, OLD.provider_merchant_id, OLD.pay_method_type, OLD.pay_method_name)
			IS DISTINCT FROM ROW(NEW.trade_no, NEW.expires_at, NEW.charge_nanos, NEW.charge_currency, NEW.charge_final, NEW.unit_charge_nanos, NEW.unit_charge_currency, NEW.promotion_codes, NEW.tax_mode, NEW.source_nanos, NEW.source_currency, NEW.credit_nanos, NEW.credit_currency, NEW.fx_rate, NEW.fx_frozen_at, NEW.product_id, NEW.quantity, NEW.provider_mode, NEW.provider_store_id, NEW.provider_merchant_id, NEW.pay_method_type, NEW.pay_method_name)
			OR (OLD.creation_state <> 'quoted' AND OLD.accepted_at <> NEW.accepted_at)
			OR (OLD.provider_session_id <> '' AND OLD.provider_session_id <> NEW.provider_session_id)
			THEN RAISE EXCEPTION 'checkout financial facts are immutable'; END IF; RETURN NEW; END $$`,
			`DROP TRIGGER IF EXISTS "checkout_facts_immutable" ON "checkout_orders"`,
			`CREATE TRIGGER "checkout_facts_immutable" BEFORE UPDATE ON "checkout_orders" FOR EACH ROW EXECUTE FUNCTION protect_checkout_facts()`,
			`CREATE OR REPLACE FUNCTION protect_checkout_payment() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'verified checkout payments are immutable'; END $$`,
			`DROP TRIGGER IF EXISTS "checkout_payment_immutable" ON "checkout_payments"`,
			`CREATE TRIGGER "checkout_payment_immutable" BEFORE UPDATE OR DELETE OR TRUNCATE ON "checkout_payments" FOR EACH STATEMENT EXECUTE FUNCTION protect_checkout_payment()`,
			`CREATE UNIQUE INDEX IF NOT EXISTS "idx_checkout_provider_payment" ON "checkout_payments" ("provider", "provider_order_id")`,
			`CREATE OR REPLACE FUNCTION protect_checkout_parent() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
			IF (OLD.contract_version = 1 OR NEW.contract_version = 1) AND ROW(OLD.user_id, OLD.trade_no, OLD.payment_method, OLD.payment_provider, OLD.contract_version, OLD.purpose, OLD.amount, OLD.money)
			IS DISTINCT FROM ROW(NEW.user_id, NEW.trade_no, NEW.payment_method, NEW.payment_provider, NEW.contract_version, NEW.purpose, NEW.amount, NEW.money)
			THEN RAISE EXCEPTION 'checkout owner and provider are immutable'; END IF; RETURN NEW; END $$`,
			`DROP TRIGGER IF EXISTS "checkout_parent_immutable" ON "top_ups"`,
			`CREATE TRIGGER "checkout_parent_immutable" BEFORE UPDATE ON "top_ups" FOR EACH ROW EXECUTE FUNCTION protect_checkout_parent()`,
			`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'checkout_parent_reference' AND conrelid = 'checkout_orders'::regclass) THEN
			ALTER TABLE "checkout_orders" ADD CONSTRAINT "checkout_parent_reference" FOREIGN KEY ("trade_no") REFERENCES "top_ups" ("trade_no"); END IF; END $$`,
			`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'checkout_payment_reference' AND conrelid = 'checkout_payments'::regclass) THEN
			ALTER TABLE "checkout_payments" ADD CONSTRAINT "checkout_payment_reference" FOREIGN KEY ("trade_no") REFERENCES "checkout_orders" ("trade_no"); END IF; END $$`,
			`CREATE INDEX IF NOT EXISTS "idx_checkout_pending_fulfillment" ON "top_ups" ("status", "trade_no") WHERE "contract_version" = 1`,
			`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'checkout_money_valid' AND conrelid = 'checkout_orders'::regclass) THEN
			ALTER TABLE "checkout_orders" ADD CONSTRAINT "checkout_money_valid" CHECK (
			"charge_nanos" > 0 AND "unit_charge_nanos" > 0 AND "source_nanos" > 0 AND "credit_nanos" > 0
			AND "unit_charge_currency" = "charge_currency" AND "charge_currency" ~ '^[A-Z]{3}$' AND "source_currency" ~ '^[A-Z]{3}$' AND "credit_currency" ~ '^[A-Z]{3}$'
			AND (("source_currency" = "credit_currency" AND "fx_rate" IS NULL AND "source_nanos" = "credit_nanos")
			OR ("source_currency" <> "credit_currency" AND "fx_rate" IS NOT NULL AND "fx_rate" > 0 AND "fx_rate" <= 1000000000))
			AND "creation_state" IN ('quoted', 'creating', 'created', 'uncertain'));
			END IF; END $$`,
			`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'checkout_paid_valid' AND conrelid = 'checkout_payments'::regclass) THEN
			ALTER TABLE "checkout_payments" ADD CONSTRAINT "checkout_paid_valid" CHECK ("paid_nanos" >= 0 AND "paid_currency" ~ '^[A-Z]{3}$');
			END IF; END $$`,
		} {
			if err := tx.Exec(statement).Error; err != nil {
				return err
			}
		}
		return migrateCheckoutConfig(tx)
	})
}

func migrateCheckoutConfig(tx *gorm.DB) error {
	if err := tx.Exec(`LOCK TABLE "options" IN ACCESS EXCLUSIVE MODE`).Error; err != nil {
		return err
	}
	var options []Option
	if err := tx.Where(`"key" IN ?`, []string{operation_setting.CheckoutConfigOption, "general_setting.quota_display_type", "DisplayInCurrencyEnabled", operation_setting.LegacyTopupGroupRatioOption, operation_setting.LegacyCheckoutPresetsOption, operation_setting.LegacyCheckoutDiscountsOption, "StripePriceId", "CreemProducts", "WaffoCurrency", "WaffoUnitPrice", "WaffoMinTopUp", "WaffoEnabled", "WaffoPancakeUnitPrice", "WaffoPancakeMinTopUp"}).Find(&options).Error; err != nil {
		return err
	}
	stored := map[string]string{}
	for _, option := range options {
		stored[option.Key] = option.Value
	}
	if text, exists := stored[operation_setting.CheckoutConfigOption]; exists {
		_, err := operation_setting.ParseCheckoutConfig(text)
		return err
	}
	configuration := operation_setting.CheckoutConfig{Providers: map[string]operation_setting.CheckoutProvider{}}
	// Token-denominated legacy options have inconsistent gateway units. Keep
	// them untouched and visibly unconfigured rather than guessing a conversion.
	display, displayValid := stored["general_setting.quota_display_type"], true
	if legacyDisplay, exists := stored["DisplayInCurrencyEnabled"]; exists {
		mapped := operation_setting.QuotaDisplayTypeUSD
		if legacyDisplay == "false" {
			mapped = operation_setting.QuotaDisplayTypeTokens
		} else if legacyDisplay != "true" {
			displayValid = false
		}
		if display != "" && display != mapped {
			displayValid = false
		}
		if display == "" {
			display = mapped
		}
	}
	if display != "" && display != operation_setting.QuotaDisplayTypeUSD && display != operation_setting.QuotaDisplayTypeCNY && display != operation_setting.QuotaDisplayTypeTokens {
		displayValid = false
	}
	if displayValid && display != operation_setting.QuotaDisplayTypeTokens {
		ratios := map[string]string{}
		var rawRatios map[string]json.RawMessage
		ratiosValid := true
		if text := stored[operation_setting.LegacyTopupGroupRatioOption]; text != "" {
			if common.UnmarshalJsonStr(text, &rawRatios) != nil {
				ratiosValid = false
			}
			for group, raw := range rawRatios {
				text := string(raw)
				if text == "0" {
					text = "1"
				}
				if _, err := operation_setting.CheckoutRatio(text); err != nil {
					ratiosValid = false
				}
				ratios[group] = text
			}
		}
		if ratiosValid {
			for _, name := range []string{PaymentProviderWaffo, PaymentProviderWaffoPancake} {
				prefix := "Waffo"
				currency := money.Currency(stored["WaffoCurrency"])
				enabled := stored["WaffoEnabled"] == "true"
				if name == PaymentProviderWaffoPancake {
					prefix, currency, enabled = "WaffoPancake", "USD", true
				}
				if currency == "" {
					currency = "USD"
				}
				priceText := stored[prefix+"UnitPrice"]
				if priceText == "" {
					priceText = "1"
				}
				minimumText := stored[prefix+"MinTopUp"]
				if minimumText == "" {
					minimumText = "1"
				}
				charge, chargeErr := legacyCheckoutAmount(priceText, currency)
				minimum, minimumErr := legacyCheckoutAmount(minimumText, "USD")
				if chargeErr != nil || minimumErr != nil {
					continue
				}
				sourceUnit := money.Amount{Nanos: 1_000_000_000, Currency: "USD"}
				provider := operation_setting.CheckoutProvider{Enabled: enabled, SourceUnit: &sourceUnit, ChargeUnit: &charge, Minimum: &minimum, Presets: []money.Amount{}, GroupRatios: ratios, Discounts: []operation_setting.CheckoutDiscount{}, Products: []operation_setting.CheckoutProduct{}}
				if err := importCheckoutPresets(stored, &provider); err != nil {
					continue
				}
				candidate := operation_setting.CheckoutConfig{Providers: map[string]operation_setting.CheckoutProvider{name: provider}}
				if candidate.Validate() == nil {
					configuration.Providers[name] = provider
				}
			}
			if priceID := stored["StripePriceId"]; priceID != "" {
				provider := operation_setting.CheckoutProvider{Enabled: true, GroupRatios: ratios, Products: []operation_setting.CheckoutProduct{{ProductID: priceID, Name: "Stripe", SourceAmount: money.Amount{Nanos: 1_000_000_000, Currency: "USD"}}}}
				if (operation_setting.CheckoutConfig{Providers: map[string]operation_setting.CheckoutProvider{PaymentProviderStripe: provider}}).Validate() == nil {
					configuration.Providers[PaymentProviderStripe] = provider
				}
			}
		}
	}
	// Creem's reward denomination was fixed and independent of display settings.
	var products []struct {
		ProductID string `json:"productId"`
		Name      string `json:"name"`
		Quota     int64  `json:"quota"`
	}
	if text := stored["CreemProducts"]; text != "" && common.UnmarshalJsonStr(text, &products) == nil && len(products) > 0 {
		provider := operation_setting.CheckoutProvider{Enabled: true, Products: []operation_setting.CheckoutProduct{}}
		valid := true
		for _, product := range products {
			value, err := money.NewRationalValue(big.NewRat(product.Quota, 500000), "USD")
			if err != nil {
				valid = false
				break
			}
			amount, err := value.Credit()
			if err != nil || amount.Nanos <= 0 {
				valid = false
				break
			}
			provider.Products = append(provider.Products, operation_setting.CheckoutProduct{ProductID: product.ProductID, Name: product.Name, SourceAmount: amount})
		}
		if valid && (operation_setting.CheckoutConfig{Providers: map[string]operation_setting.CheckoutProvider{PaymentProviderCreem: provider}}).Validate() == nil {
			configuration.Providers[PaymentProviderCreem] = provider
		}
	}
	// Epay has no authoritative legacy currency. It requires explicit new setup.
	encoded, err := common.Marshal(configuration)
	if err != nil {
		return err
	}
	return tx.Create(&Option{Key: operation_setting.CheckoutConfigOption, Value: string(encoded)}).Error
}

func legacyCheckoutAmount(text string, currency money.Currency) (money.Amount, error) {
	value, err := money.ParseValue(text, currency)
	if err != nil {
		return money.Amount{}, err
	}
	amount, err := value.Credit()
	if err != nil || amount.Nanos <= 0 {
		return money.Amount{}, common.NewMessage("Invalid amount")
	}
	comparison, err := value.Compare(amount.Value())
	if err != nil || comparison != 0 {
		return money.Amount{}, common.NewMessage("Invalid amount")
	}
	return amount, nil
}

func importCheckoutPresets(stored map[string]string, provider *operation_setting.CheckoutProvider) error {
	var presets []int64
	if text := stored[operation_setting.LegacyCheckoutPresetsOption]; text != "" {
		if err := common.UnmarshalJsonStr(text, &presets); err != nil {
			return err
		}
	} else {
		presets = []int64{10, 20, 50, 100, 200, 500}
	}
	for _, preset := range presets {
		amount, err := legacyCheckoutAmount(strconv.FormatInt(preset, 10), "USD")
		if err != nil {
			return err
		}
		if amount.Nanos >= provider.Minimum.Nanos {
			provider.Presets = append(provider.Presets, amount)
		}
	}
	var discounts map[string]json.RawMessage
	if text := stored[operation_setting.LegacyCheckoutDiscountsOption]; text != "" {
		if err := common.UnmarshalJsonStr(text, &discounts); err != nil {
			return err
		}
	}
	for key, raw := range discounts {
		amount, err := legacyCheckoutAmount(key, "USD")
		if err != nil {
			return err
		}
		provider.Discounts = append(provider.Discounts, operation_setting.CheckoutDiscount{SourceAmount: amount, Ratio: string(raw)})
	}
	return nil
}
