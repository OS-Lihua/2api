package model

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/capability"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const MaxBulkImportRows = 500

// BulkImportRow is an ordered operation. A ref names a newly created resource
// within this batch; references never persist outside the submitted batch.
type BulkImportRow struct {
	Resource string            `json:"resource"`
	Action   string            `json:"action"`
	Ref      string            `json:"ref,omitempty"`
	Data     common.RawMessage `json:"data"`
	Line     int               `json:"-"`
}

type BulkImportResult struct {
	DryRun    bool                `json:"dry_run"`
	Valid     bool                `json:"valid"`
	Committed bool                `json:"committed"`
	Rows      []BulkImportPreview `json:"rows"`
}

// Preview deliberately includes no user supplied strings or configuration.
type BulkImportPreview struct {
	Row              int            `json:"row"`
	Resource         string         `json:"resource"`
	Action           string         `json:"action"`
	ID               int            `json:"id,omitempty"`
	Status           ChannelStatus  `json:"status,omitempty"`
	MappingConfirmed *bool          `json:"mapping_confirmed,omitempty"`
	Error            string         `json:"error,omitempty"`
	ErrorCode        string         `json:"error_code,omitempty"`
	ProviderID       int            `json:"provider_id,omitempty"`
	ProviderRow      int            `json:"provider_row,omitempty"`
	ModelID          int            `json:"model_id,omitempty"`
	Protocol         protocol.Type  `json:"protocol,omitempty"`
	ProviderType     int            `json:"provider_type,omitempty"`
	OwnerType        string         `json:"owner_type,omitempty"`
	OwnerID          int            `json:"owner_id,omitempty"`
	OwnerRow         int            `json:"owner_row,omitempty"`
	Currency         money.Currency `json:"currency,omitempty"`
	EffectiveAt      int64          `json:"effective_at,omitempty"`
}

type BulkProviderInput struct {
	Name               string  `json:"name"`
	Type               int     `json:"type"`
	Key                string  `json:"key"`
	BaseURL            *string `json:"base_url"`
	HeaderOverride     *string `json:"header_override"`
	Setting            *string `json:"setting"`
	Settings           string  `json:"settings"`
	Other              string  `json:"other"`
	OpenAIOrganization *string `json:"openai_organization"`
}

func (input BulkProviderInput) Provider() (*Provider, error) {
	record := &Provider{Name: strings.TrimSpace(input.Name), Type: input.Type, Key: input.Key, BaseURL: input.BaseURL, HeaderOverride: input.HeaderOverride, Setting: input.Setting, OtherSettings: input.Settings, Other: input.Other, OpenAIOrganization: input.OpenAIOrganization, Status: common.ChannelStatusEnabled, CreatedTime: common.GetTimestamp()}
	if record.Name == "" || len(record.Name) > 128 || input.Type <= 0 {
		return nil, errors.New("invalid provider")
	}
	if err := ValidateProvider(record, true); err != nil {
		return nil, err
	}
	return record, nil
}

type BulkChannelInput struct {
	ProviderID         int               `json:"provider_id"`
	ProviderRef        string            `json:"provider_ref"`
	ModelID            int               `json:"model_id"`
	Protocol           protocol.Type     `json:"protocol"`
	UpstreamModelName  string            `json:"upstream_model_name"`
	Capabilities       *capability.Set   `json:"capabilities"`
	ParameterOverrides common.RawMessage `json:"parameter_overrides"`
	RPM                int64             `json:"rpm"`
	TPM                int64             `json:"tpm"`
	MaxConcurrency     int64             `json:"max_concurrency"`
	Weight             *int64            `json:"weight"`
}

type BulkChannelActionInput struct {
	ID         int    `json:"id"`
	ChannelRef string `json:"channel_ref"`
}

type BulkPriceInput struct {
	OwnerType   string         `json:"owner_type"`
	OwnerID     int            `json:"owner_id"`
	OwnerRef    string         `json:"owner_ref"`
	Currency    money.Currency `json:"currency"`
	Expression  string         `json:"expression"`
	EffectiveAt *int64         `json:"effective_at"`
}

type bulkReference struct {
	resource string
	id       int
}
type bulkMutation struct {
	provider *Provider
	channel  *Channel
	price    *PriceVersion
	create   bool
}

// bulkPrices resolves the latest effective price across persisted and staged
// versions without writing staged prices or allocating a sequence value.
type bulkPrices struct{ versions []*PriceVersion }

func (prices bulkPrices) HasPurchasePrice(ctx context.Context, db *gorm.DB, channel *Channel) (bool, error) {
	var latest *PriceVersion
	if channel.ID > 0 {
		persisted, err := CurrentPriceVersion(ctx, db, PriceOwnerChannel, channel.ID, clock.Now().Unix())
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return false, err
		}
		if err == nil {
			latest = persisted
		}
	}
	for _, price := range prices.versions {
		if price.OwnerType == PriceOwnerChannel && price.OwnerID == channel.ID && price.EffectiveAt <= clock.Now().Unix() && (latest == nil || price.EffectiveAt > latest.EffectiveAt) {
			latest = price
		}
	}
	if latest == nil {
		return false, nil
	}
	err := validatePriceExpression(ctx, db, latest.Expression, nil, channel)
	return err == nil, err
}

var errInvalidBulk = errors.New("invalid bulk import")
var errDuplicateBulkChannel = errors.New("duplicate channel identity")
var errDuplicateBulkPrice = errors.New("duplicate price effective time")

// bulkValidationError only exposes fixed, reviewed messages. Raw decoder,
// database, expression and capability errors can contain submitted secrets.
func bulkValidationError(err error) (string, string) {
	if errors.Is(err, errDuplicateBulkChannel) {
		return "duplicate_identity", "This provider, protocol, and upstream model already have a channel"
	}
	if errors.Is(err, errDuplicateBulkPrice) {
		return "duplicate_price_time", "A price version already exists at this effective time"
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "reference_not_found", "Channel, provider, or model not found"
	}
	var message *common.Message
	if errors.As(err, &message) {
		switch message.Key {
		case "Invalid price owner", "Invalid price currency", "Invalid price effective time", "Invalid price expression", "Invalid provider type",
			"Invalid upstream model name", "Invalid channel protocol", "Channel limits and weight must be non-negative integers", "Parameter overrides must be a JSON object",
			"Confirm the channel mapping before enabling", "Enable the provider before enabling its channel", "Configure a purchase price before enabling the channel", "Task plugin is not available",
			"Task price requires an available plugin usage schema":
			return "invalid_declaration", message.Key
		}
	}
	return "invalid_fields", "Invalid import row or unmet activation requirements"
}

// ImportBatch validates before writing. dryRun executes only reads: it does not
// insert and roll back, allocate IDs, update caches, or contact Providers.
// Execution revalidates against locked rows in the same transaction as writes.
func ImportBatch(ctx context.Context, rows []BulkImportRow, dryRun bool, actorID int) (*BulkImportResult, error) {
	if len(rows) == 0 || len(rows) > MaxBulkImportRows {
		return nil, errInvalidBulk
	}
	result := &BulkImportResult{DryRun: dryRun, Valid: true, Rows: make([]BulkImportPreview, len(rows))}
	err := DB.WithContext(ctx).Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)}).Transaction(func(tx *gorm.DB) error {
		if dryRun {
			if err := tx.Exec("SET TRANSACTION READ ONLY").Error; err != nil {
				return err
			}
		}
		refs := map[string]bulkReference{}
		providers := map[int]*Provider{}
		channels := map[int]*Channel{}
		paths := map[string]bool{}
		prices := bulkPrices{}
		mutations := make([]bulkMutation, len(rows))
		for i, row := range rows {
			line := row.Line
			if line == 0 {
				line = i + 1
			}
			preview := BulkImportPreview{Row: line}
			mutation := bulkMutation{}
			var rowErr error
			_, duplicateRef := refs[row.Ref]
			if row.Ref != "" && (len(row.Ref) > 128 || row.Action != "create" || row.Resource == "price-versions" || duplicateRef) {
				rowErr = errInvalidBulk
			}
			switch {
			case row.Resource == "providers" && row.Action == "create":
				preview.Resource, preview.Action = row.Resource, row.Action
				var input BulkProviderInput
				if err := common.DecodeJsonStrict(strings.NewReader(string(row.Data)), &input); err != nil {
					rowErr = err
				} else {
					record, err := input.Provider()
					if err != nil {
						rowErr = err
					} else {
						record.Id = -(i + 1)
						mutation.provider = record
						mutation.create = true
					}
				}
			case row.Resource == "channels" && (row.Action == "create" || row.Action == "confirm" || row.Action == "enable"):
				preview.Resource, preview.Action = row.Resource, row.Action
				record := Channel{}
				previous := Channel{}
				action := ChannelAction(row.Action)
				if row.Action == "create" {
					var input BulkChannelInput
					if err := common.DecodeJsonStrict(strings.NewReader(string(row.Data)), &input); err != nil {
						rowErr = err
						break
					}
					if input.ProviderRef != "" {
						ref := refs[input.ProviderRef]
						if input.ProviderID != 0 || ref.resource != "providers" || ref.id == 0 {
							rowErr = errInvalidBulk
							break
						}
						input.ProviderID = ref.id
					} else if input.ProviderID <= 0 {
						rowErr = errInvalidBulk
						break
					}
					overrides := "{}"
					if len(input.ParameterOverrides) > 0 {
						overrides = string(input.ParameterOverrides)
					}
					weight := int64(1)
					if input.Weight != nil {
						weight = *input.Weight
					}
					record = Channel{ProviderID: input.ProviderID, ModelID: input.ModelID, Protocol: input.Protocol, UpstreamModelName: input.UpstreamModelName, Capabilities: input.Capabilities, ParameterOverrides: overrides, RPM: input.RPM, TPM: input.TPM, MaxConcurrency: input.MaxConcurrency, Weight: weight}
					action = ChannelSave
					mutation.create = true
				} else {
					var input BulkChannelActionInput
					if err := common.DecodeJsonStrict(strings.NewReader(string(row.Data)), &input); err != nil {
						rowErr = err
						break
					}
					if input.ChannelRef != "" {
						ref := refs[input.ChannelRef]
						if input.ID != 0 || ref.resource != "channels" || ref.id == 0 {
							rowErr = errInvalidBulk
							break
						}
						input.ID = ref.id
					} else if input.ID <= 0 {
						rowErr = errInvalidBulk
						break
					}
					if staged := channels[input.ID]; staged != nil {
						record = *staged
					} else {
						query := tx
						if !dryRun {
							query = lockForUpdate(query)
						}
						if err := query.First(&record, input.ID).Error; err != nil {
							rowErr = err
							break
						}
					}
					previous = record
				}
				var unified Model
				query := tx
				if !dryRun {
					query = lockForUpdate(query)
				}
				if err := query.Where("id = ? AND name_rule = ?", record.ModelID, NameRuleExact).First(&unified).Error; err != nil {
					rowErr = err
					break
				}
				provider := providers[record.ProviderID]
				if provider == nil {
					provider = &Provider{}
					query := tx
					if !dryRun {
						query = lockForUpdate(query)
					}
					if err := query.First(provider, record.ProviderID).Error; err != nil {
						rowErr = err
						break
					}
					providers[provider.Id] = provider
				}
				if err := validateChannelMutation(ctx, tx, &record, previous, action, unified, *provider, prices); err != nil {
					rowErr = err
					break
				}
				if mutation.create {
					key := fmt.Sprintf("%d/%s/%s", record.ProviderID, record.Protocol, record.UpstreamModelName)
					var count int64
					if record.ProviderID > 0 {
						if err := tx.Model(&Channel{}).Where("provider_id = ? AND protocol = ? AND upstream_model_name = ?", record.ProviderID, record.Protocol, record.UpstreamModelName).Count(&count).Error; err != nil {
							rowErr = err
							break
						}
					}
					if count > 0 || paths[key] {
						rowErr = errDuplicateBulkChannel
						break
					}
					paths[key] = true
					record.ID = -(i + 1)
				}
				record.UpdatedAt = common.GetTimestamp()
				mutation.channel = &record
				preview.Status = record.Status
				confirmed := record.MappingConfirmed
				preview.MappingConfirmed = &confirmed
			case row.Resource == "price-versions" && row.Action == "create":
				preview.Resource, preview.Action = row.Resource, row.Action
				var input BulkPriceInput
				if err := common.DecodeJsonStrict(strings.NewReader(string(row.Data)), &input); err != nil {
					rowErr = err
					break
				}
				if input.OwnerRef != "" {
					ref := refs[input.OwnerRef]
					if input.OwnerID != 0 || input.OwnerType != PriceOwnerChannel || ref.resource != "channels" || ref.id == 0 {
						rowErr = errInvalidBulk
						break
					}
					input.OwnerID = ref.id
				} else if input.OwnerID <= 0 {
					rowErr = errInvalidBulk
					break
				}
				at := clock.Now().Unix()
				if input.EffectiveAt != nil {
					at = *input.EffectiveAt
				}
				record := &PriceVersion{OwnerType: input.OwnerType, OwnerID: input.OwnerID, Currency: input.Currency, Expression: input.Expression, EffectiveAt: at}
				channel := channels[input.OwnerID]
				// Offering IDs and Channel IDs occupy independent namespaces.
				if input.OwnerType != PriceOwnerChannel {
					channel = nil
				}
				if err := preparePriceVersion(ctx, tx, record, actorID, channel, !dryRun); err != nil {
					rowErr = err
					break
				}
				var count int64
				if input.OwnerID > 0 {
					if err := tx.Model(&PriceVersion{}).Where("owner_type = ? AND owner_id = ? AND effective_at = ?", input.OwnerType, input.OwnerID, at).Count(&count).Error; err != nil {
						rowErr = err
						break
					}
				}
				if count > 0 {
					rowErr = errDuplicateBulkPrice
					break
				}
				for _, price := range prices.versions {
					if price.OwnerType == record.OwnerType && price.OwnerID == record.OwnerID && price.EffectiveAt == record.EffectiveAt {
						rowErr = errDuplicateBulkPrice
					}
				}
				mutation.price = record
			default:
				rowErr = errInvalidBulk
			}
			if rowErr != nil {
				preview.ErrorCode, preview.Error = bulkValidationError(rowErr)
				result.Valid = false
			} else {
				mutations[i] = mutation
				id := 0
				if mutation.provider != nil {
					preview.ProviderType = mutation.provider.Type
					id = mutation.provider.Id
					providers[id] = mutation.provider
				}
				if mutation.channel != nil {
					preview.ModelID = mutation.channel.ModelID
					preview.Protocol = mutation.channel.Protocol
					if mutation.channel.ProviderID > 0 {
						preview.ProviderID = mutation.channel.ProviderID
					} else {
						preview.ProviderRow = -mutation.channel.ProviderID
					}
					id = mutation.channel.ID
					channels[id] = mutation.channel
				}
				if mutation.price != nil {
					preview.OwnerType = mutation.price.OwnerType
					preview.Currency = mutation.price.Currency
					preview.EffectiveAt = mutation.price.EffectiveAt
					if mutation.price.OwnerID > 0 {
						preview.OwnerID = mutation.price.OwnerID
					} else {
						preview.OwnerRow = -mutation.price.OwnerID
					}
					prices.versions = append(prices.versions, mutation.price)
				}
				if row.Ref != "" {
					refs[row.Ref] = bulkReference{resource: row.Resource, id: id}
				}
				if id > 0 {
					preview.ID = id
				}
			}
			result.Rows[i] = preview
		}
		if !result.Valid || dryRun {
			return nil
		}
		ids := map[int]int{}
		for i, mutation := range mutations {
			switch {
			case mutation.provider != nil:
				record := *mutation.provider
				temporary := record.Id
				record.Id = 0
				if err := tx.Create(&record).Error; err != nil {
					return err
				}
				ids[temporary] = record.Id
				result.Rows[i].ID = record.Id
			case mutation.channel != nil:
				record := *mutation.channel
				temporary := record.ID
				if record.ProviderID < 0 {
					record.ProviderID = ids[record.ProviderID]
				}
				if mutation.create {
					record.ID = 0
					if err := tx.Omit("Provider", "Model").Create(&record).Error; err != nil {
						return err
					}
					ids[temporary] = record.ID
				} else {
					if record.ID < 0 {
						record.ID = ids[record.ID]
					}
					if err := tx.Omit("Provider", "Model").Save(&record).Error; err != nil {
						return err
					}
				}
				result.Rows[i].ID = record.ID
			case mutation.price != nil:
				record := *mutation.price
				if record.OwnerID < 0 {
					record.OwnerID = ids[record.OwnerID]
					record.ChannelID = &record.OwnerID
				}
				if err := tx.Omit("Creator", "Channel", "Offering").Create(&record).Error; err != nil {
					return err
				}
				result.Rows[i].ID = record.ID
			}
		}
		return nil
	})
	result.Committed = err == nil && result.Valid && !dryRun
	return result, err
}
