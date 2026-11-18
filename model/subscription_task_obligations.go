package model

import (
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/clock"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// LegacySubscriptionTaskObligation preserves task evidence independently of
// preconsume records. Neither a terminal task nor zero quota proves settlement.
// The source has no proven original period or refundable/final money amount.
type LegacySubscriptionTaskObligation struct {
	SourceTaskID    int64  `gorm:"primaryKey;autoIncrement:false"`
	UserID          int    `gorm:"not null;index"`
	SubscriptionID  int    `gorm:"not null;index"`
	ProviderID      int    `gorm:"not null"`
	TaskID          string `gorm:"type:text;not null"`
	Platform        string `gorm:"type:text;not null"`
	RequestID       string `gorm:"type:text;not null"`
	SourceQuota     int64  `gorm:"not null"`
	SourceStatus    string `gorm:"type:text;not null"`
	SourceCreatedAt int64  `gorm:"not null"`
	SourceUpdatedAt int64  `gorm:"not null"`
	State           string `gorm:"type:varchar(32);not null;index"`
	CapturedAt      int64  `gorm:"not null"`
}

// migrateLegacySubscriptionTaskObligations runs only inside the caller's locked
// migration transaction. Schema, immutable guards and source locks belong to it.
func migrateLegacySubscriptionTaskObligations(tx *gorm.DB) error {
	if tx == nil || tx.Dialector.Name() != "postgres" {
		return fmt.Errorf("legacy subscription task evidence requires PostgreSQL")
	}
	// A PostgreSQL JSON error may quote private values; never trace this projection.
	tx = tx.Session(&gorm.Session{Logger: logger.Discard})
	if !tx.Migrator().HasTable(&Task{}) {
		return nil
	}
	type sourceTask struct {
		ID            int64
		UserID        int
		ChannelID     int
		TaskID        string
		Platform      string
		Status        string
		Quota         int64
		CreatedAt     int64
		UpdatedAt     int64
		PrivateType   *string
		GatewayType   *string
		ExecutionType *string
		FundingJSON   string
	}
	// Do not fetch PrivateData, Data, plugin state, URLs or credentials into the
	// migrator. PostgreSQL projects only the provenance fields listed here. Keep
	// opaque values as json: jsonb would reject valid RawMessage numeric ranges.
	const projection = `"id", "user_id", "channel_id", "task_id", "platform", "status", "quota", "created_at", "updated_at",
		json_typeof("private_data"::json) AS "private_type",
		json_typeof("private_data"::json -> 'gateway') AS "gateway_type",
		json_typeof("private_data"::json -> 'execution') AS "execution_type",
		json_build_object(
			'billing_source', "private_data"::json -> 'billing_source',
			'subscription_id', "private_data"::json -> 'subscription_id',
			'request_id', "private_data"::json -> 'execution' -> 'request_id'
		)::text AS "funding_json"`
	var last int64
	first := true
	for {
		var sources []sourceTask
		query := tx.Table("tasks").Select(projection).Order(`"id" ASC`).Limit(250)
		if !first {
			query = query.Where(`"id" > ?`, last)
		}
		if err := query.Find(&sources).Error; err != nil {
			return fmt.Errorf("cannot read legacy subscription task evidence")
		}
		if len(sources) == 0 {
			return nil
		}
		ids := make([]int64, len(sources))
		for i := range sources {
			ids[i] = sources[i].ID
		}
		var existing []LegacySubscriptionTaskObligation
		if err := tx.Where("source_task_id IN ?", ids).Find(&existing).Error; err != nil {
			return err
		}
		byID := make(map[int64]LegacySubscriptionTaskObligation, len(existing))
		for _, obligation := range existing {
			byID[obligation.SourceTaskID] = obligation
		}
		type fundingEvidence struct {
			BillingSource  string `json:"billing_source"`
			SubscriptionID int    `json:"subscription_id"`
			RequestID      string `json:"request_id"`
		}
		fundings := make(map[int64]fundingEvidence)
		subscriptionIDs := make([]int, 0, len(sources))
		for _, source := range sources {
			_, captured := byID[source.ID]
			invalid := func() error {
				return fmt.Errorf("invalid legacy subscription task evidence for task %d", source.ID)
			}
			if source.PrivateType != nil && *source.PrivateType != "null" && *source.PrivateType != "object" {
				return invalid()
			}
			if source.GatewayType != nil && *source.GatewayType != "null" {
				if *source.GatewayType != "object" || captured {
					return invalid()
				}
				// New precise Gateway funding is never historical quota evidence.
				continue
			}
			var funding fundingEvidence
			if err := common.UnmarshalJsonStr(source.FundingJSON, &funding); err != nil {
				return invalid()
			}
			if funding.BillingSource != "subscription" {
				if captured {
					return invalid()
				}
				continue
			}
			if source.ExecutionType != nil && *source.ExecutionType != "null" && *source.ExecutionType != "object" {
				return invalid()
			}
			fundings[source.ID] = funding
			subscriptionIDs = append(subscriptionIDs, funding.SubscriptionID)
		}
		var parents []struct {
			ID     int
			UserID int
		}
		if len(subscriptionIDs) > 0 {
			if err := tx.Model(&UserSubscription{}).Select("id", "user_id").Where("id IN ?", subscriptionIDs).Find(&parents).Error; err != nil {
				return err
			}
		}
		owners := make(map[int]int, len(parents))
		for _, parent := range parents {
			owners[parent.ID] = parent.UserID
		}
		var pending []LegacySubscriptionTaskObligation
		for _, source := range sources {
			funding, legacy := fundings[source.ID]
			if !legacy {
				continue
			}
			// Released admin deletion left tasks behind. Absence is unverified
			// evidence, but an existing instance cannot belong to another user.
			if owner, exists := owners[funding.SubscriptionID]; exists && owner != source.UserID {
				return fmt.Errorf("legacy subscription task owner conflict for task %d", source.ID)
			}
			if previous, captured := byID[source.ID]; captured {
				if previous.UserID != source.UserID || previous.SubscriptionID != funding.SubscriptionID ||
					previous.ProviderID != source.ChannelID || previous.TaskID != source.TaskID ||
					previous.Platform != source.Platform || previous.RequestID != funding.RequestID {
					return fmt.Errorf("legacy subscription task identity changed for task %d", source.ID)
				}
				// Polling may legitimately change status, quota and timestamps.
				// Keep the first evidence; those changes do not prove settlement.
				continue
			}
			pending = append(pending, LegacySubscriptionTaskObligation{
				SourceTaskID: source.ID, UserID: source.UserID, SubscriptionID: funding.SubscriptionID,
				ProviderID: source.ChannelID, TaskID: source.TaskID, Platform: source.Platform,
				RequestID: funding.RequestID, SourceQuota: source.Quota, SourceStatus: source.Status,
				SourceCreatedAt: source.CreatedAt, SourceUpdatedAt: source.UpdatedAt,
				State: "unverified", CapturedAt: clock.Now().Unix(),
			})
		}
		if len(pending) > 0 {
			if err := tx.Create(&pending).Error; err != nil {
				return err
			}
		}
		last, first = sources[len(sources)-1].ID, false
	}
}
