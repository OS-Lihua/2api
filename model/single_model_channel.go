package model

import (
	"context"
	"errors"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/capability"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"gorm.io/gorm"
)

type ChannelStatus string

const (
	ChannelPending  ChannelStatus = "pending"
	ChannelEnabled  ChannelStatus = "enabled"
	ChannelDisabled ChannelStatus = "disabled"
)

// Channel is one Provider × protocol × upstream model. A nil capability
// narrowing inherits the Model declaration; omitted numeric limits inherit too.
type Channel struct {
	ID                 int             `json:"id" gorm:"primaryKey"`
	ProviderID         int             `json:"provider_id" gorm:"not null;uniqueIndex:idx_channel_path;index"`
	Provider           Provider        `json:"-" gorm:"constraint:OnDelete:RESTRICT"`
	Protocol           protocol.Type   `json:"protocol" gorm:"type:varchar(80);not null;uniqueIndex:idx_channel_path"`
	UpstreamModelName  string          `json:"upstream_model_name" gorm:"type:varchar(256);not null;uniqueIndex:idx_channel_path"`
	ModelID            int             `json:"model_id" gorm:"not null;index"`
	Model              Model           `json:"-" gorm:"constraint:OnDelete:RESTRICT"`
	Capabilities       *capability.Set `json:"capabilities" gorm:"type:jsonb"`
	ParameterOverrides string          `json:"-" gorm:"type:jsonb;not null"`
	RPM                int64           `json:"rpm" gorm:"not null;check:channel_rpm_nonnegative,rpm >= 0"`
	TPM                int64           `json:"tpm" gorm:"not null;check:channel_tpm_nonnegative,tpm >= 0"`
	MaxConcurrency     int64           `json:"max_concurrency" gorm:"not null;check:channel_concurrency_nonnegative,max_concurrency >= 0"`
	Weight             int64           `json:"weight" gorm:"not null;check:channel_weight_nonnegative,weight >= 0"`
	Status             ChannelStatus   `json:"status" gorm:"type:varchar(16);not null;check:channel_status_valid,status IN ('pending','enabled','disabled')"`
	MappingConfirmed   bool            `json:"mapping_confirmed" gorm:"not null;check:channel_enabled_confirmed,status <> 'enabled' OR mapping_confirmed"`
	CreatedAt          int64           `json:"created_at" gorm:"not null"`
	UpdatedAt          int64           `json:"updated_at" gorm:"not null"`
}

// ChannelPurchasePriceChecker reports configuration, including an explicit zero
// price. It runs in the Channel mutation transaction. No checker denies enable.
// Price version management supplies the checker when that subsystem is ready.
type ChannelPurchasePriceChecker interface {
	HasPurchasePrice(context.Context, *gorm.DB, int) (bool, error)
}

var channelPrices struct {
	sync.RWMutex
	checker ChannelPurchasePriceChecker
}

func SetChannelPurchasePriceChecker(checker ChannelPurchasePriceChecker) {
	channelPrices.Lock()
	defer channelPrices.Unlock()
	channelPrices.checker = checker
}

// SaveChannel validates and persists a full declaration. The caller supplies
// the original mapping and status on updates; actions are validated here too.
func SaveChannel(ctx context.Context, record *Channel, action string) error {
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var previous Channel
		if record.ID != 0 {
			if err := lockForUpdate(tx).First(&previous, record.ID).Error; err != nil {
				return err
			}
			// Actions operate on the current mapping, never a stale HTTP snapshot.
			if action != "save" {
				*record = previous
			}
		}
		// Serialize with Model declaration updates before validating its narrowing.
		var unified Model
		if err := lockForUpdate(tx).Where("id = ? AND name_rule = ?", record.ModelID, NameRuleExact).First(&unified).Error; err != nil {
			return err
		}
		var provider Provider
		if err := lockForUpdate(tx).First(&provider, record.ProviderID).Error; err != nil {
			return err
		}
		if record.ID != 0 {
			record.CreatedAt = previous.CreatedAt
			record.MappingConfirmed = previous.MappingConfirmed
			record.Status = previous.Status
			if record.ModelID != previous.ModelID || record.ProviderID != previous.ProviderID || record.Protocol != previous.Protocol || record.UpstreamModelName != previous.UpstreamModelName {
				record.MappingConfirmed = false
				record.Status = ChannelPending
			}
		} else {
			record.Status = ChannelPending
			record.MappingConfirmed = false
			record.CreatedAt = common.GetTimestamp()
		}
		record.UpstreamModelName = strings.TrimSpace(record.UpstreamModelName)
		if record.UpstreamModelName == "" || utf8.RuneCountInString(record.UpstreamModelName) > 256 {
			return common.NewMessage("Invalid upstream model name")
		}
		if err := record.Protocol.Validate(); err != nil {
			return common.NewMessage("Invalid channel protocol")
		}
		if record.RPM < 0 || record.TPM < 0 || record.MaxConcurrency < 0 || record.Weight < 0 {
			return common.NewMessage("Channel limits and weight must be non-negative integers")
		}
		if record.Capabilities != nil {
			normalized, err := record.Capabilities.Normalize()
			if err != nil {
				return err
			}
			record.Capabilities = &normalized
		}
		if _, err := record.EffectiveCapabilities(unified.Capabilities); err != nil {
			return err
		}
		var overrides map[string]any
		if err := common.UnmarshalJsonStr(record.ParameterOverrides, &overrides); err != nil || overrides == nil {
			return common.NewMessage("Parameter overrides must be a JSON object")
		}
		switch action {
		case "confirm":
			if !record.MappingConfirmed {
				record.Status = ChannelDisabled
			}
			record.MappingConfirmed = true
		case "disable":
			record.Status = ChannelDisabled
			if !record.MappingConfirmed {
				record.Status = ChannelPending
			}
		case "enable":
			record.Status = ChannelEnabled
		case "save":
		default:
			return errors.New("unknown channel action")
		}
		if record.Status == ChannelEnabled {
			if !record.MappingConfirmed {
				return common.NewMessage("Confirm the channel mapping before enabling")
			}
			if provider.Status != common.ChannelStatusEnabled {
				return common.NewMessage("Enable the provider before enabling its channel")
			}
			channelPrices.RLock()
			checker := channelPrices.checker
			channelPrices.RUnlock()
			configured := false
			if checker != nil {
				var err error
				configured, err = checker.HasPurchasePrice(ctx, tx, record.ID)
				if err != nil {
					return err
				}
			}
			if !configured {
				return common.NewMessage("Configure a purchase price before enabling the channel")
			}
		}
		record.UpdatedAt = common.GetTimestamp()
		if record.ID == 0 {
			return tx.Omit("Provider", "Model").Create(record).Error
		}
		return tx.Omit("Provider", "Model").Save(record).Error
	})
}

// EffectiveCapabilities validates a narrowing and inherits undeclared numeric
// limits from the Model. It never adds a feature or modality to that Model.
func (record Channel) EffectiveCapabilities(parent capability.Set) (capability.Set, error) {
	if record.Capabilities == nil {
		return parent, nil
	}
	narrowed, err := record.Capabilities.Narrow(parent)
	return narrowed, err
}
