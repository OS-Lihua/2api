package model

import (
	"cmp"
	"context"
	"crypto/sha256"
	"fmt"
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
		at = time.Unix(proposal.EffectiveAt, 0)
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
	var fingerprint []any
	for _, offering := range offerings {
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
			fingerprint = append(fingerprint, offering, channel, sale, purchase)
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
	// Include the exact proposal and evaluated estimates. A stale confirmation
	// cannot authorize a different price, FX quote or affected combination.
	fingerprint = append(fingerprint, proposal, fx, report.Estimates)
	encoded, err := common.Marshal(fingerprint)
	if err != nil {
		return nil, err
	}
	report.Revision = fmt.Sprintf("%x", sha256.Sum256(encoded))
	return report, nil
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

// meanPriceValue normalizes each quantitative snapshot before taking an exact
// rational mean. The expression then selects its tier using that mean.
func meanPriceValue(ctx context.Context, tx *gorm.DB, price *PriceVersion, offeringID int, at time.Time) (money.Value, int, string, error) {
	used, err := billingexpr.MoneyUsedVars(price.Expression)
	if err != nil {
		return money.Value{}, 0, "", err
	}
	for _, probe := range []string{"param", "header", "u", "image_count"} {
		if used[probe] {
			return money.Value{}, 0, "request_context_unavailable", nil
		}
	}
	quantities := map[string]*big.Rat{}
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
		var usages, channelUsages []*dto.Usage
		for _, record := range records {
			var intent GatewaySettlement
			if err := common.UnmarshalJsonStr(record.Settlement, &intent); err != nil {
				return money.Value{}, 0, "", err
			}
			if price.OwnerType == PriceOwnerOffering {
				if intent.SaleUsage != nil {
					usages = append(usages, intent.SaleUsage)
				}
				continue
			}
			attempts := intent.Attempts
			if intent.Attempt != nil {
				attempts = []GatewayAttempt{*intent.Attempt}
			}
			for _, attempt := range attempts {
				usage := intent.AttemptUsage[attempt.ID]
				if usage == nil {
					continue
				}
				usages = append(usages, usage)
				if attempt.ChannelID == price.OwnerID {
					channelUsages = append(channelUsages, usage)
				}
			}
		}
		if len(channelUsages) > 0 {
			usages = channelUsages
		}
		for _, usage := range usages {
			normalized, err := billingexpr.MoneyTokenQuantities(usage, used)
			if err != nil {
				return money.Value{}, 0, "", err
			}
			for key, value := range normalized {
				if quantities[key] == nil {
					quantities[key] = new(big.Rat)
				}
				quantities[key].Add(quantities[key], value.Rat())
			}
			samples++
		}
	}
	if samples == 0 {
		for key := range used {
			switch key {
			case "p", "c", "len", "cr", "cc", "cc1h", "cc12h", "img", "img_cr", "img_o", "ai", "ao":
				return money.Value{}, 0, "no_usage_history", nil
			}
		}
	} else {
		for _, value := range quantities {
			value.Quo(value, big.NewRat(int64(samples), 1))
		}
	}
	value, _, err := billingexpr.RunMoneyExpr(price.Expression, billingexpr.MoneyInput{RationalTokenQuantities: quantities, Time: at}, price.Currency)
	if err != nil {
		return money.Value{}, samples, "", err
	}
	return value, samples, "", nil
}
