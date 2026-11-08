package model

import (
	"errors"

	"github.com/QuantumNous/new-api/common"
	"github.com/shopspring/decimal"
)

// ExactGroupDiscounts reads decimal sale discounts without the legacy float
// cache. Nil groups selects configured groups; unconfigured groups use one.
// Each group's own special discount takes precedence over its ordinary value.
func ExactGroupDiscounts(groups []string) (map[string]decimal.Decimal, error) {
	common.OptionMapRWMutex.RLock()
	ordinaryText := common.OptionMap["GroupRatio"]
	if configured := common.OptionMap["group_ratio_setting.group_ratio"]; configured != "" {
		ordinaryText = configured
	}
	specialText := common.OptionMap["GroupGroupRatio"]
	if configured := common.OptionMap["group_ratio_setting.group_group_ratio"]; configured != "" {
		specialText = configured
	}
	common.OptionMapRWMutex.RUnlock()

	var special map[string]map[string]decimal.Decimal
	if specialText != "" {
		if err := common.UnmarshalJsonStr(specialText, &special); err != nil {
			return nil, err
		}
	}
	var ordinary map[string]decimal.Decimal
	ordinaryLoaded := false
	if groups == nil {
		if ordinaryText != "" {
			if err := common.UnmarshalJsonStr(ordinaryText, &ordinary); err != nil {
				return nil, err
			}
		}
		ordinaryLoaded = true
		names := make(map[string]bool, len(ordinary)+len(special))
		for group := range ordinary {
			names[group] = true
		}
		for group, discounts := range special {
			if _, found := discounts[group]; found {
				names[group] = true
			}
		}
		for group := range names {
			groups = append(groups, group)
		}
	}
	result := make(map[string]decimal.Decimal, len(groups))
	for _, group := range groups {
		ratio, found := special[group][group]
		if !found {
			if !ordinaryLoaded {
				if ordinaryText != "" {
					if err := common.UnmarshalJsonStr(ordinaryText, &ordinary); err != nil {
						return nil, err
					}
				}
				ordinaryLoaded = true
			}
			ratio, found = ordinary[group]
			if !found {
				ratio = decimal.NewFromInt(1)
			}
		}
		if ratio.IsNegative() {
			return nil, errors.New("invalid group discount")
		}
		result[group] = ratio
	}
	return result, nil
}
