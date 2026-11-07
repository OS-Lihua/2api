package service

import (
	"context"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/shopspring/decimal"
)

// ExactGatewayGroupRatio reads the configured decimal before legacy float
// caches deserialize it. The user group discounts the sale only.
func ExactGatewayGroupRatio(group string) (decimal.Decimal, error) {
	common.OptionMapRWMutex.RLock()
	text := common.OptionMap["GroupRatio"]
	if configured := common.OptionMap["group_ratio_setting.group_ratio"]; configured != "" {
		text = configured
	}
	common.OptionMapRWMutex.RUnlock()
	if text == "" {
		return decimal.NewFromInt(1), nil
	}
	var ratios map[string]decimal.Decimal
	if err := common.UnmarshalJsonStr(text, &ratios); err != nil {
		return decimal.Decimal{}, err
	}
	ratio, found := ratios[group]
	if !found {
		return decimal.NewFromInt(1), nil
	}
	if ratio.IsNegative() {
		return decimal.Decimal{}, errors.New("invalid group discount")
	}
	return ratio, nil
}

// GatewayFX freezes the actual directional quote. Same-currency accounting
// records the identity rate explicitly and never reads a synthetic FX row.
func GatewayFX(ctx context.Context, from money.Currency) (*money.Rate, string, error) {
	if from == model.SettlementCurrency() {
		return nil, "1", nil
	}
	rate, err := model.GetAccountingFXRate(ctx, model.DB, from, model.SettlementCurrency())
	if err != nil {
		return nil, "", err
	}
	return &rate, rate.Decimal().String(), nil
}

// GatewayPrice evaluates one frozen expression and applies the exact discount
// and frozen accounting quote. Rounding belongs to the caller's ledger boundary.
func GatewayPrice(price *model.PriceVersion, usage *dto.Usage, request billingexpr.RequestInput, at time.Time, ratio decimal.Decimal, rate *money.Rate) (money.Value, money.Value, billingexpr.MoneyTraceResult, error) {
	used, err := billingexpr.MoneyUsedVars(price.Expression)
	if err != nil {
		return money.Value{}, money.Value{}, billingexpr.MoneyTraceResult{}, err
	}
	quantities, err := exactGatewayTokens(usage, used)
	if err != nil {
		return money.Value{}, money.Value{}, billingexpr.MoneyTraceResult{}, err
	}
	value, trace, err := billingexpr.RunMoneyExpr(price.Expression, billingexpr.MoneyInput{TokenQuantities: quantities, Request: request, Time: at}, price.Currency)
	if err != nil {
		return money.Value{}, money.Value{}, trace, err
	}
	value, err = money.MulUnitPrice(ratio, value)
	if err != nil {
		return money.Value{}, money.Value{}, trace, err
	}
	converted := value
	if rate != nil {
		converted, err = value.Convert(*rate)
	}
	return value, converted, trace, err
}

// exactGatewayTokens follows BuildTieredTokenParams with decimal quantities;
// its 1.6 long-lived cache multiplier never passes through a monetary float.
func exactGatewayTokens(usage *dto.Usage, used map[string]bool) (map[string]decimal.Decimal, error) {
	if usage == nil {
		return nil, errors.New("missing billing usage")
	}
	for _, count := range []int{usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens,
		usage.PromptTokensDetails.CachedTokens, usage.PromptTokensDetails.CacheCreationTokensTotal(),
		usage.PromptTokensDetails.ImageTokens, usage.PromptTokensDetails.AudioTokens,
		usage.CompletionTokenDetails.ImageTokens, usage.CompletionTokenDetails.AudioTokens,
		usage.ClaudeCacheCreation5mTokens, usage.ClaudeCacheCreation1hTokens} {
		if count < 0 || int64(count) > (1<<31)-1 {
			return nil, errors.New("invalid billing usage")
		}
	}
	p, c := decimal.NewFromInt(int64(usage.PromptTokens)), decimal.NewFromInt(int64(usage.CompletionTokens))
	cr := decimal.NewFromInt(int64(usage.PromptTokensDetails.CachedTokens))
	cc := decimal.NewFromInt(int64(usage.PromptTokensDetails.CacheCreationTokensTotal()))
	cc1h, cc12h := decimal.Zero, decimal.Zero
	anthropic := usage.UsageSemantic == "anthropic"
	if anthropic {
		short, long := relayconvert.NormalizeCacheCreationSplit(usage.PromptTokensDetails.CacheCreationTokensTotal(), usage.ClaudeCacheCreation5mTokens, usage.ClaudeCacheCreation1hTokens)
		cc, cc1h = decimal.NewFromInt(int64(short)), decimal.NewFromInt(int64(long))
	}
	img, ai := decimal.NewFromInt(int64(usage.PromptTokensDetails.ImageTokens)), decimal.NewFromInt(int64(usage.PromptTokensDetails.AudioTokens))
	imgO, ao := decimal.NewFromInt(int64(usage.CompletionTokenDetails.ImageTokens)), decimal.NewFromInt(int64(usage.CompletionTokenDetails.AudioTokens))
	imgCR := decimal.Zero
	if used["img_cr"] && !anthropic {
		details := usage.PromptTokensDetails.CachedTokensDetails
		if details != nil && details.ImageTokens != nil {
			cachedImage := *details.ImageTokens
			cached, image := usage.PromptTokensDetails.CachedTokens, usage.PromptTokensDetails.ImageTokens
			valid := cachedImage >= 0 && cached >= cachedImage && image >= cachedImage && cached <= usage.PromptTokens && image <= usage.PromptTokens-(cached-cachedImage)
			remaining := cached - cachedImage
			for _, count := range []*int{details.TextTokens, details.AudioTokens} {
				if count == nil {
					continue
				}
				if *count < 0 || *count > remaining {
					valid = false
					break
				}
				remaining -= *count
			}
			if valid {
				imgCR = decimal.NewFromInt(int64(cachedImage))
				cr = cr.Sub(imgCR)
				img = img.Sub(imgCR)
			}
		}
	}
	inputLen := p
	if anthropic {
		inputLen = p.Add(cr).Add(cc).Add(cc1h)
		if !used["cr"] {
			p = p.Add(cr)
		}
		if !used["cc1h"] {
			cc = cc.Add(cc1h.Mul(decimal.New(16, -1)))
			cc1h = decimal.Zero
		}
		if !used["cc"] {
			p = p.Add(cc)
			cc = decimal.Zero
		}
	} else {
		if used["cr"] {
			p = p.Sub(cr)
		}
		cc12h = decimal.NewFromInt(int64(usage.PromptTokensDetails.CacheWrite12hTokens()))
		cc = cc.Sub(cc12h)
		ccTokens := cc
		if used["cc12h"] {
			p = p.Sub(cc12h)
		} else {
			ccTokens = ccTokens.Add(cc12h)
			cc = cc.Add(cc12h.Mul(decimal.NewFromInt(2)))
			cc12h = decimal.Zero
		}
		if used["cc"] {
			p = p.Sub(ccTokens)
		} else {
			p = p.Add(cc.Sub(ccTokens))
			cc = decimal.Zero
		}
		if used["img"] {
			p = p.Sub(img)
		}
		if used["img_cr"] {
			p = p.Sub(imgCR)
		}
		if used["ai"] {
			p = p.Sub(ai)
		}
		if used["img_o"] {
			c = c.Sub(imgO)
		}
		if used["ao"] {
			c = c.Sub(ao)
		}
	}
	return map[string]decimal.Decimal{"p": decimal.Max(p, decimal.Zero), "c": decimal.Max(c, decimal.Zero), "len": inputLen,
		"cr": cr, "cc": cc, "cc1h": cc1h, "cc12h": cc12h, "img": img, "img_cr": imgCR, "img_o": imgO, "ai": ai, "ao": ao}, nil
}

func GatewayRecordUsage(usage *dto.Usage) model.GatewayUsage {
	if usage == nil {
		return model.GatewayUsage{}
	}
	return model.GatewayUsage{PromptTokens: int64(usage.PromptTokens), CompletionTokens: int64(usage.CompletionTokens), TotalTokens: int64(usage.PromptTokens) + int64(usage.CompletionTokens), CachedTokens: int64(usage.PromptTokensDetails.CachedTokens), CacheCreationTokens: int64(usage.PromptTokensDetails.CacheCreationTokensTotal()), CacheCreation12hTokens: int64(usage.PromptTokensDetails.CacheWrite12hTokens()), ImageInputTokens: int64(usage.PromptTokensDetails.ImageTokens), AudioInputTokens: int64(usage.PromptTokensDetails.AudioTokens), ImageOutputTokens: int64(usage.CompletionTokenDetails.ImageTokens), AudioOutputTokens: int64(usage.CompletionTokenDetails.AudioTokens)}
}
