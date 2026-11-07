package model

import (
	"net/url"
	"sort"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RecommendationRule suggests form values only. Saving a Provider or Model
// never consults this table. Higher priorities and longer patterns rank first;
// ties use the oldest rule ID.
type RecommendationRule struct {
	ID           int    `json:"id"`
	Kind         string `json:"kind" gorm:"size:16;not null;index"`
	Pattern      string `json:"pattern" gorm:"size:253;not null"`
	Priority     int    `json:"priority"`
	Enabled      bool   `json:"enabled"`
	ProviderName string `json:"provider_name" gorm:"size:128"`
	ProviderType int    `json:"provider_type"`
	PublisherID  int    `json:"publisher_id"`
	// Built-in rules resolve a name against existing Publishers at query time.
	// They do not create a Publisher or infer a Model identity.
	PublisherName string `json:"publisher_name" gorm:"size:128"`
}

type RecommendationSeed struct {
	Key string `gorm:"primaryKey;size:64"`
}

// MigrateRecommendationRules installs the built-in rules exactly once. The
// marker and rules commit together, including when several nodes start at once.
func MigrateRecommendationRules(db *gorm.DB) error {
	if err := db.AutoMigrate(&RecommendationRule{}, &RecommendationSeed{}); err != nil {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error {
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&RecommendationSeed{Key: "builtin-initial"})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return nil
		}
		rules := []RecommendationRule{
			{Kind: "provider", Pattern: "api.openai.com", ProviderName: "OpenAI", ProviderType: constant.ChannelTypeOpenAI, Enabled: true},
			{Kind: "provider", Pattern: "*.openai.azure.com", ProviderName: "Azure OpenAI", ProviderType: constant.ChannelTypeAzure, Enabled: true},
			{Kind: "provider", Pattern: "api.anthropic.com", ProviderName: "Anthropic", ProviderType: constant.ChannelTypeAnthropic, Enabled: true},
			{Kind: "provider", Pattern: "generativelanguage.googleapis.com", ProviderName: "Google Gemini", ProviderType: constant.ChannelTypeGemini, Enabled: true},
			{Kind: "provider", Pattern: "api.deepseek.com", ProviderName: "DeepSeek", ProviderType: constant.ChannelTypeDeepSeek, Enabled: true},
			{Kind: "provider", Pattern: "dashscope.aliyuncs.com", ProviderName: "Alibaba Cloud", ProviderType: constant.ChannelTypeAli, Enabled: true},
			{Kind: "publisher", Pattern: "claude-", PublisherName: "Anthropic", Enabled: true},
			{Kind: "publisher", Pattern: "gpt-", PublisherName: "OpenAI", Enabled: true},
			{Kind: "publisher", Pattern: "gemini-", PublisherName: "Google", Enabled: true},
			{Kind: "publisher", Pattern: "deepseek-", PublisherName: "DeepSeek", Enabled: true},
			{Kind: "publisher", Pattern: "qwen", PublisherName: "Alibaba", Enabled: true},
		}
		return tx.Create(&rules).Error
	})
}

// Normalize validates administrator-authored rules. Wildcards match one or more
// complete subdomain labels, never the apex itself or a hostname suffix fragment.
func (r *RecommendationRule) Normalize() error {
	r.Pattern = strings.ToLower(strings.TrimSpace(r.Pattern))
	if r.Pattern == "" || len(r.Pattern) > 253 {
		return common.NewMessage("Invalid recommendation rule")
	}
	switch r.Kind {
	case "provider":
		if !validRecommendationHostname(strings.TrimPrefix(r.Pattern, "*.")) {
			return common.NewMessage("Invalid recommendation rule")
		}
		r.ProviderName = strings.TrimSpace(r.ProviderName)
		if r.ProviderName == "" || len(r.ProviderName) > 128 || r.ProviderType <= 0 || r.ProviderType >= constant.ChannelTypeDummy {
			return common.NewMessage("Invalid recommendation rule")
		}
		r.PublisherID = 0
		r.PublisherName = ""
	case "publisher":
		var publisher Vendor
		if r.PublisherID <= 0 {
			return common.NewMessage("Publisher is required")
		}
		if err := DB.First(&publisher, r.PublisherID).Error; err != nil {
			return common.NewMessage("Publisher does not exist")
		}
		r.PublisherName = ""
		r.ProviderName = ""
		r.ProviderType = 0
	default:
		return common.NewMessage("Invalid recommendation rule")
	}
	return nil
}

func validRecommendationHostname(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func FindRecommendations(kind, address, modelName string, providerType int) ([]RecommendationRule, error) {
	items := make([]RecommendationRule, 0)
	var rules []RecommendationRule
	if err := DB.Where("kind = ? AND enabled = ?", kind, true).Find(&rules).Error; err != nil {
		return nil, err
	}
	var host string
	if kind == "provider" {
		if strings.TrimSpace(address) == "" {
			address = constant.GetChannelBaseURL(providerType)
		}
		parsed, err := url.Parse(strings.TrimSpace(address))
		if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
			return items, nil
		}
		host = strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
		if !validRecommendationHostname(host) {
			return items, nil
		}
	}
	for _, rule := range rules {
		if kind == "provider" {
			matched := host == rule.Pattern
			if suffix, wildcard := strings.CutPrefix(rule.Pattern, "*."); wildcard {
				matched = host != suffix && strings.HasSuffix(host, "."+suffix)
			}
			if !matched {
				continue
			}
		} else {
			if !strings.HasPrefix(strings.ToLower(modelName), rule.Pattern) {
				continue
			}
			var publisher Vendor
			query := DB.Order("id ASC")
			if rule.PublisherID > 0 {
				query = query.Where("id = ?", rule.PublisherID)
			} else {
				query = query.Where("LOWER(name) = ?", strings.ToLower(rule.PublisherName))
			}
			if err := query.First(&publisher).Error; err != nil {
				if err == gorm.ErrRecordNotFound {
					continue
				}
				return nil, err
			}
			rule.PublisherID = publisher.Id
			rule.PublisherName = publisher.Name
		}
		items = append(items, rule)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Priority != items[j].Priority {
			return items[i].Priority > items[j].Priority
		}
		if len(items[i].Pattern) != len(items[j].Pattern) {
			return len(items[i].Pattern) > len(items[j].Pattern)
		}
		return items[i].ID < items[j].ID
	})
	return items, nil
}
