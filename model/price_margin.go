package model

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

type MarginEstimate struct {
	ModelOfferingID        int           `json:"model_offering_id"`
	ChannelID              int           `json:"channel_id"`
	ProviderID             int           `json:"provider_id"`
	SalePriceVersionID     int           `json:"sale_price_version_id"`
	PurchasePriceVersionID int           `json:"purchase_price_version_id"`
	Samples                int           `json:"samples"`
	Revenue                *money.Amount `json:"revenue"`
	Cost                   *money.Amount `json:"cost"`
	Margin                 *money.Amount `json:"margin"`
	Negative               bool          `json:"negative"`
	Status                 string        `json:"status"`
}

type PriceMarginReport struct {
	EffectiveAt          int64            `json:"effective_at"`
	Revision             string           `json:"revision"`
	Estimates            []MarginEstimate `json:"estimates"`
	RequiresConfirmation bool             `json:"requires_confirmation"`
}

type MarginConfirmationError struct{ Report *PriceMarginReport }

func (e *MarginConfirmationError) Error() string { return "Negative margin requires confirmation" }

type MarginPriceCatalog struct {
	Prices   []*PriceVersion
	Channels []*Channel
}

// CheckPriceMargins evaluates the average measured usage in the previous seven
// days. Missing prices, FX, usage or request probes produce an unknown estimate.
// It never substitutes zero for an unconfigured procurement price.
func CheckPriceMargins(ctx context.Context, tx *gorm.DB, proposal *PriceVersion, fx *FXRate, lockCatalog bool, catalogs ...MarginPriceCatalog) (*PriceMarginReport, error) {
	var staged MarginPriceCatalog
	if len(catalogs) > 0 {
		staged = catalogs[0]
	}
	report := &PriceMarginReport{Estimates: []MarginEstimate{}}
	at := clock.Now()
	if proposal != nil {
		copy := *proposal
		copy.ID, copy.CreatedBy, copy.CreatedAt = 0, 0, 0
		copy.MarginCheck = nil
		proposal = &copy
		// Backdated versions can affect new requests, whose counterpart prices
		// are current. Planned versions use their future effective instant.
		if effectiveAt := time.Unix(proposal.EffectiveAt, 0); effectiveAt.After(at) {
			at = effectiveAt
		}
		report.EffectiveAt = proposal.EffectiveAt
	}
	if fx != nil {
		copy := *fx
		copy.UpdatedAt, copy.UpdatedBy = 0, 0
		copy.MarginCheck = nil
		fx = &copy
	}
	var offerings []ModelOffering
	query := tx.WithContext(ctx).Preload("Members").Order("id")
	if lockCatalog {
		query = lockForUpdate(query)
	}
	if proposal != nil && proposal.OwnerType == PriceOwnerOffering {
		query = query.Where("id = ?", proposal.OwnerID)
	}
	if err := query.Find(&offerings).Error; err != nil {
		return nil, err
	}
	fingerprint := []any{SettlementCurrency()}
	for _, offering := range offerings {
		groups := offering.Groups
		if offering.Visibility != "groups" {
			groups = nil
		}
		discounts, err := ExactGroupDiscounts(groups)
		if err != nil {
			return nil, err
		}
		// All-user offerings also admit unconfigured groups at the identity
		// discount. Use the lowest eligible sale discount to expose any loss.
		discount := decimal.NewFromInt(1)
		hasDiscount := offering.Visibility != "groups"
		for _, ratio := range discounts {
			if !hasDiscount || ratio.LessThan(discount) {
				discount = ratio
			}
			hasDiscount = true
		}
		ids := make([]int, len(offering.Members))
		for i := range offering.Members {
			ids[i] = offering.Members[i].ModelID
		}
		var channels []Channel
		channelQuery := OfferingChannelsQuery(tx, &offering, ids).Order("id")
		if lockCatalog {
			channelQuery = lockForUpdate(channelQuery)
		}
		if err := channelQuery.Find(&channels).Error; err != nil {
			return nil, err
		}
		for _, candidate := range staged.Channels {
			channels = slices.DeleteFunc(channels, func(channel Channel) bool { return channel.ID == candidate.ID })
			if !slices.Contains(ids, candidate.ModelID) {
				continue
			}
			scope := offering.ChannelScope
			included := len(scope.IncludeProviderIDs)+len(scope.IncludeChannelIDs) == 0 || slices.Contains(scope.IncludeProviderIDs, candidate.ProviderID) || slices.Contains(scope.IncludeChannelIDs, candidate.ID)
			if !included || slices.Contains(scope.ExcludeProviderIDs, candidate.ProviderID) || slices.Contains(scope.ExcludeChannelIDs, candidate.ID) {
				continue
			}
			copy := *candidate
			copy.CreatedAt, copy.UpdatedAt = 0, 0
			channels = append(channels, copy)
		}
		slices.SortFunc(channels, func(a, b Channel) int { return cmp.Compare(a.ID, b.ID) })
		sale, saleErr := marginPriceVersion(ctx, tx, PriceOwnerOffering, offering.ID, at.Unix(), staged.Prices)
		if saleErr != nil && saleErr != gorm.ErrRecordNotFound {
			return nil, saleErr
		}
		if proposal != nil && proposal.OwnerType == PriceOwnerOffering {
			sale = proposal
			saleErr = nil
		}
		for _, channel := range channels {
			if proposal != nil && proposal.OwnerType == PriceOwnerChannel && channel.ID != proposal.OwnerID {
				continue
			}
			estimate := MarginEstimate{ModelOfferingID: offering.ID, ChannelID: channel.ID, ProviderID: channel.ProviderID, Status: "missing_sale_price"}
			purchase, purchaseErr := marginPriceVersion(ctx, tx, PriceOwnerChannel, channel.ID, at.Unix(), staged.Prices)
			if purchaseErr != nil && purchaseErr != gorm.ErrRecordNotFound {
				return nil, purchaseErr
			}
			if proposal != nil && proposal.OwnerType == PriceOwnerChannel {
				purchase = proposal
				purchaseErr = nil
			}
			if fx != nil && (fx.ToCurrency != SettlementCurrency() || sale.Currency != fx.FromCurrency && purchase.Currency != fx.FromCurrency) {
				continue
			}
			// Bind private overrides without returning their contents. The outer
			// confirmation MAC prevents offline guesses of configuration values.
			overridesDigest := sha256.Sum256([]byte(channel.ParameterOverrides))
			fingerprint = append(fingerprint, offering, channel, sale, purchase, fmt.Sprintf("%x", overridesDigest), discount.String())
			switch {
			case saleErr != nil:
			case purchaseErr != nil:
				estimate.Status = "missing_purchase_price"
			default:
				estimate.SalePriceVersionID, estimate.PurchasePriceVersionID = sale.ID, purchase.ID
				saleRate, err := marginFX(ctx, tx, sale.Currency, fx, false)
				if err != nil && err != gorm.ErrRecordNotFound {
					return nil, err
				}
				purchaseRate, purchaseFXErr := marginFX(ctx, tx, purchase.Currency, fx, true)
				if purchaseFXErr != nil && purchaseFXErr != gorm.ErrRecordNotFound {
					return nil, purchaseFXErr
				}
				if err != nil || purchaseFXErr != nil {
					estimate.Status = "missing_fx"
					break
				}
				for _, rate := range []*money.Rate{saleRate, purchaseRate} {
					if rate != nil {
						fingerprint = append(fingerprint, rate.Decimal().String())
					}
				}
				saleValue, samples, saleStatus, err := meanPriceValue(ctx, tx, sale, offering.ID, at)
				if err != nil {
					return nil, err
				}
				purchaseValue, _, purchaseStatus, err := meanPriceValue(ctx, tx, purchase, offering.ID, at)
				if err != nil {
					return nil, err
				}
				estimate.Samples = samples
				if saleStatus != "" {
					estimate.Status = saleStatus
					break
				}
				if purchaseStatus != "" {
					estimate.Status = purchaseStatus
					break
				}
				saleValue, err = money.MulUnitPrice(discount, saleValue)
				if err != nil {
					return nil, err
				}
				if saleRate != nil {
					saleValue, err = saleValue.Convert(*saleRate)
					if err != nil {
						return nil, err
					}
				}
				if purchaseRate != nil {
					purchaseValue, err = purchaseValue.Convert(*purchaseRate)
					if err != nil {
						return nil, err
					}
				}
				margin, err := saleValue.Sub(purchaseValue)
				if err != nil {
					return nil, err
				}
				revenue, err := saleValue.Receivable()
				if err != nil {
					return nil, err
				}
				cost, err := purchaseValue.Cost()
				if err != nil {
					return nil, err
				}
				profit, err := margin.Cost()
				if err != nil {
					return nil, err
				}
				estimate.Revenue, estimate.Cost, estimate.Margin = &revenue, &cost, &profit
				estimate.Negative = strings.HasPrefix(margin.String(), "-")
				estimate.Status = "estimated"
				report.RequiresConfirmation = report.RequiresConfirmation || estimate.Negative
			}
			report.Estimates = append(report.Estimates, estimate)
		}
	}
	// Bind the draft and financial configuration, not moving usage statistics.
	// Every write still recomputes its estimate; normal traffic cannot revoke
	// an explicit loss approval for the same price and configuration.
	fingerprint = append(fingerprint, proposal, fx)
	revision, err := marginConfirmationRevision("price", fingerprint)
	if err != nil {
		return nil, err
	}
	report.Revision = revision
	return report, nil
}

// marginConfirmationRevision binds a confirmation to its complete financial
// context. Reuse the shared session secret with a separate message domain;
// raw import credentials and private configuration never leave this function.
func marginConfirmationRevision(domain string, value any) (string, error) {
	encoded, err := common.Marshal(value)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, []byte(common.SessionSecret))
	_, _ = mac.Write([]byte("gateway-margin-confirmation:" + domain + "\x00"))
	_, _ = mac.Write(encoded)
	return fmt.Sprintf("%x", mac.Sum(nil)), nil
}

func marginPriceVersion(ctx context.Context, tx *gorm.DB, owner string, id int, at int64, staged []*PriceVersion) (*PriceVersion, error) {
	price, err := CurrentPriceVersion(ctx, tx, owner, id, at)
	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, err
	}
	for _, candidate := range staged {
		if candidate.OwnerType == owner && candidate.OwnerID == id && candidate.EffectiveAt <= at && (err == gorm.ErrRecordNotFound || candidate.EffectiveAt >= price.EffectiveAt) {
			copy := *candidate
			copy.ID, copy.CreatedBy, copy.CreatedAt = 0, 0, 0
			copy.MarginCheck = nil
			price = &copy
			err = nil
		}
	}
	return price, err
}

func marginFX(ctx context.Context, tx *gorm.DB, from money.Currency, proposal *FXRate, buffered bool) (*money.Rate, error) {
	if from == SettlementCurrency() {
		return nil, nil
	}
	var rate money.Rate
	var err error
	if proposal != nil && proposal.FromCurrency == from && proposal.ToCurrency == SettlementCurrency() {
		value := proposal.Rate
		if buffered {
			value = value.Mul(decimal.NewFromInt(1).Add(proposal.BufferRate))
		}
		rate, err = money.NewRate(from, SettlementCurrency(), value)
	} else if buffered {
		rate, err = GetComparisonFXRate(ctx, tx, from, SettlementCurrency())
	} else {
		rate, err = GetAccountingFXRate(ctx, tx, from, SettlementCurrency())
	}
	return &rate, err
}

// marginLegacyUsage reconstructs quantities only when the expression does not
// depend on missing cache semantics, TTL or overlapping modality details. Known
// output counters remain usable even when the input cache breakdown is unknown.
func marginLegacyUsage(usage GatewayUsage, used map[string]bool) *dto.Usage {
	hasCache := usage.CachedTokens != 0 || usage.CacheCreationTokens != 0 || usage.CacheCreation12hTokens != 0
	if (used["p"] || used["len"]) && hasCache {
		return nil
	}
	if (used["cc"] || used["cc1h"]) && usage.CacheCreationTokens != 0 {
		return nil
	}
	if used["img_cr"] && usage.CachedTokens != 0 && usage.ImageInputTokens != nil && *usage.ImageInputTokens != 0 {
		return nil
	}
	if usage.PromptTokens < 0 || usage.CompletionTokens < 0 || usage.PromptTokens > 1<<31-1 || usage.CompletionTokens > 1<<31-1 || usage.TotalTokens != usage.PromptTokens+usage.CompletionTokens {
		return nil
	}
	result := &dto.Usage{
		PromptTokens: int(usage.PromptTokens), CompletionTokens: int(usage.CompletionTokens), TotalTokens: int(usage.TotalTokens),
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: int(usage.CachedTokens), CacheWriteTokens: int(usage.CacheCreationTokens),
			CacheWriteTokensDetails: &dto.CacheWriteTokenDetails{TTL12hTokens: int(usage.CacheCreation12hTokens)},
		},
	}
	for _, detail := range []struct {
		name   string
		value  *int64
		target *int
	}{
		{"img", usage.ImageInputTokens, &result.PromptTokensDetails.ImageTokens},
		{"ai", usage.AudioInputTokens, &result.PromptTokensDetails.AudioTokens},
		{"img_o", usage.ImageOutputTokens, &result.CompletionTokenDetails.ImageTokens},
		{"ao", usage.AudioOutputTokens, &result.CompletionTokenDetails.AudioTokens},
	} {
		if detail.value == nil {
			if used[detail.name] && !slices.Contains(usage.UnmeasuredTokens, detail.name) {
				return nil
			}
			continue
		}
		if *detail.value < 0 || *detail.value > 1<<31-1 {
			return nil
		}
		*detail.target = int(*detail.value)
	}
	return result
}

// meanPriceValue normalizes each quantitative snapshot before taking an exact
// rational mean. The expression then selects its tier using that mean.
func meanPriceValue(ctx context.Context, tx *gorm.DB, price *PriceVersion, offeringID int, at time.Time) (money.Value, int, string, error) {
	used, err := billingexpr.MoneyUsedVars(price.Expression)
	if err != nil {
		return money.Value{}, 0, "", err
	}
	for _, probe := range []string{"param", "header", "u"} {
		if used[probe] {
			return money.Value{}, 0, "request_context_unavailable", nil
		}
	}
	quantities := map[string]*big.Rat{}
	unmeasured := map[string]bool{}
	var meanImages *big.Rat
	if used["image_count"] {
		meanImages = new(big.Rat)
	}
	needsUsage := false
	for _, key := range []string{"p", "c", "len", "cr", "cc", "cc1h", "cc12h", "img", "img_cr", "img_o", "ai", "ao"} {
		needsUsage = needsUsage || used[key]
	}
	samples := 0
	// Installations and isolated admin fixtures can configure prices before the
	// first gateway lifecycle is created. Such token estimates remain unknown.
	if tx.Migrator().HasTable(&GatewayBilling{}) {
		var records []struct{ Settlement string }
		historyAt := clock.Now()
		err = tx.WithContext(ctx).Model(&GatewayBilling{}).Select("settlement").Where(`"settlement" <> '' AND ("request_data"::jsonb ->> 'model_offering_id')::bigint = ? AND ("request_data"::jsonb ->> 'started_at')::bigint >= ? AND ("request_data"::jsonb ->> 'started_at')::bigint <= ?`, offeringID, historyAt.AddDate(0, 0, -7).UnixMilli(), historyAt.UnixMilli()).Find(&records).Error
		if err != nil {
			return money.Value{}, 0, "", err
		}
		// All Channel prices use the same Offering workload. Procurement keeps
		// its source quantities; it must not inherit the delivered sale usage.
		type usageSample struct {
			usage   *dto.Usage
			facts   GatewayUsage
			choices int64
			weight  *big.Rat
		}
		var usages []usageSample
		for _, record := range records {
			var intent GatewaySettlement
			if err := common.UnmarshalJsonStr(record.Settlement, &intent); err != nil {
				return money.Value{}, 0, "", err
			}
			choices := int64(max(intent.Request.ChoiceCount, 1))
			if price.OwnerType == PriceOwnerOffering {
				usage := intent.SaleUsage
				if usage == nil {
					usage = marginLegacyUsage(intent.Request.Usage, used)
				}
				if usage == nil && needsUsage {
					return money.Value{}, len(records), "usage_details_unavailable", nil
				}
				if usage != nil || used["image_count"] {
					usages = append(usages, usageSample{usage, intent.Request.Usage, choices, big.NewRat(1, 1)})
				}
				continue
			}
			attempts := intent.Attempts
			if intent.Attempt != nil {
				attempts = []GatewayAttempt{*intent.Attempt}
			}
			if len(attempts) == 0 && (needsUsage || used["image_count"]) {
				return money.Value{}, len(records), "usage_details_unavailable", nil
			}
			for _, attempt := range attempts {
				usage := intent.AttemptUsage[attempt.ID]
				if usage == nil {
					usage = marginLegacyUsage(attempt.Usage, used)
				}
				if usage == nil && needsUsage {
					return money.Value{}, len(records), "usage_details_unavailable", nil
				}
				if usage == nil && !used["image_count"] {
					continue
				}
				// Weight each request once even if it needed several Attempts.
				usages = append(usages, usageSample{usage, attempt.Usage, choices, big.NewRat(1, int64(len(attempts)))})
			}
		}
		weight := new(big.Rat)
		for _, sample := range usages {
			sampleUnknown := make(map[string]bool, len(sample.facts.UnmeasuredTokens))
			for _, name := range sample.facts.UnmeasuredTokens {
				sampleUnknown[name] = true
			}
			maps.Copy(unmeasured, billingexpr.NormalizeUnmeasuredOutputTokens(sampleUnknown, used))
			if meanImages != nil {
				count := sample.facts.ImageCount
				if count == nil || *count < 0 || *count > dto.MaxImageN {
					return money.Value{}, len(records), "usage_details_unavailable", nil
				}
				meanImages.Add(meanImages, new(big.Rat).Mul(big.NewRat(*count, 1), sample.weight))
			}
			normalized := map[string]decimal.Decimal{}
			if sample.usage != nil {
				normalized, err = billingexpr.MoneyTokenQuantities(sample.usage, used)
			}
			if err != nil {
				return money.Value{}, 0, "", err
			}
			for key, value := range normalized {
				if quantities[key] == nil {
					quantities[key] = new(big.Rat)
				}
				quantity := new(big.Rat).Mul(value.Rat(), sample.weight)
				if key == "c" || key == "img_o" || key == "ao" {
					quantity.Quo(quantity, big.NewRat(sample.choices, 1))
				}
				quantities[key].Add(quantities[key], quantity)
			}
			weight.Add(weight, sample.weight)
		}
		if weight.Sign() > 0 {
			if meanImages != nil {
				meanImages.Quo(meanImages, weight)
			}
			for _, value := range quantities {
				value.Quo(value, weight)
			}
			samples = len(records)
		}
	}
	if samples == 0 && used["image_count"] {
		return money.Value{}, 0, "no_usage_history", nil
	}
	if samples == 0 {
		for key := range used {
			switch key {
			case "p", "c", "len", "cr", "cc", "cc1h", "cc12h", "img", "img_cr", "img_o", "ai", "ao":
				return money.Value{}, 0, "no_usage_history", nil
			}
		}
	}
	value, trace, err := billingexpr.RunMoneyExpr(price.Expression, billingexpr.MoneyInput{RationalTokenQuantities: quantities, MeanImageCount: meanImages, Request: billingexpr.RequestInput{UnmeasuredTokens: unmeasured}, Time: at}, price.Currency)
	if len(trace.UnmeasuredTokens) > 0 {
		return money.Value{}, samples, "usage_details_unavailable", nil
	}
	if err != nil {
		return money.Value{}, samples, "", err
	}
	return value, samples, "", nil
}
