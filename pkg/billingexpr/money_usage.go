package billingexpr

import (
	"errors"
	"maps"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/shopspring/decimal"
)

// NormalizeUnmeasuredOutputTokens applies the expression's output split to
// gateway markers, whose c flag also covers any unmeasured output modality.
func NormalizeUnmeasuredOutputTokens(unmeasured, used map[string]bool) map[string]bool {
	result := maps.Clone(unmeasured)
	if !result["img_o"] && !result["ao"] {
		return result
	}
	delete(result, "c")
	for _, modality := range []string{"img_o", "ao"} {
		if result[modality] && !used[modality] {
			result["c"] = true
		}
	}
	return result
}

// MoneyTokenQuantities normalizes measured token categories for one expression.
// Decimal quantities retain fractional long-lived cache units without rounding.
func MoneyTokenQuantities(usage *dto.Usage, used map[string]bool) (map[string]decimal.Decimal, error) {
	if usage == nil {
		return nil, errors.New("missing billing usage")
	}
	for _, count := range []int{usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, usage.PromptTokensDetails.CachedTokens, usage.PromptTokensDetails.CacheCreationTokensTotal(), usage.PromptTokensDetails.ImageTokens, usage.PromptTokensDetails.AudioTokens, usage.CompletionTokenDetails.ImageTokens, usage.CompletionTokenDetails.AudioTokens, usage.ClaudeCacheCreation5mTokens, usage.ClaudeCacheCreation1hTokens} {
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
	return map[string]decimal.Decimal{"p": decimal.Max(p, decimal.Zero), "c": decimal.Max(c, decimal.Zero), "len": inputLen, "cr": cr, "cc": cc, "cc1h": cc1h, "cc12h": cc12h, "img": img, "img_cr": imgCR, "img_o": imgO, "ai": ai, "ao": ao}, nil
}
