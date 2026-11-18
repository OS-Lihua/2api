package model

import (
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/clock"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// LegacyTaskObligation is first-observed provenance, not a charge, refund or
// authorization. Missing identities stay missing; source quota is not money.
// No foreign key ties this evidence to deletable operational records.
type LegacyTaskObligation struct {
	SourceTaskID     int64  `gorm:"primaryKey;autoIncrement:false"`
	UserID           int    `gorm:"not null;index"`
	ProviderID       int    `gorm:"not null"`
	TaskID           string `gorm:"type:text;not null"`
	UpstreamTaskID   string `gorm:"type:text;not null"`
	Platform         string `gorm:"type:text;not null"`
	BillingSource    string `gorm:"type:text;not null"`
	TokenID          int    `gorm:"not null"`
	SubscriptionID   int    `gorm:"not null"`
	RequestID        string `gorm:"type:text;not null"`
	PluginKey        string `gorm:"type:text;not null"`
	PluginVersion    string `gorm:"type:text;not null"`
	PluginGeneration uint64 `gorm:"type:numeric(20,0);not null"`
	SourceQuota      int64  `gorm:"not null"`
	SourceStatus     string `gorm:"type:text;not null"`
	SourceCreatedAt  int64  `gorm:"not null"`
	SourceUpdatedAt  int64  `gorm:"not null"`
	SourceSubmitTime int64  `gorm:"not null"`
	SourceStartTime  int64  `gorm:"not null"`
	SourceFinishTime int64  `gorm:"not null"`
	State            string `gorm:"type:varchar(32);not null;check:legacy_task_obligation_state,state = 'unverified'"`
	CapturedAt       int64  `gorm:"not null"`
}

// MigrateLegacyTaskObligations atomically installs and captures independent
// historical evidence. It never creates money, prices, Gateway facts or grants.
func MigrateLegacyTaskObligations(db *gorm.DB) error {
	if db == nil || db.Dialector.Name() != "postgres" {
		return fmt.Errorf("legacy task evidence requires PostgreSQL")
	}
	return db.Session(&gorm.Session{Logger: logger.Discard}).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SELECT pg_advisory_xact_lock(510047)`).Error; err != nil {
			return fmt.Errorf("cannot lock legacy task evidence migration")
		}
		for _, table := range []string{"tasks", "legacy_subscription_task_obligations"} {
			if tx.Migrator().HasTable(table) {
				if err := tx.Exec(fmt.Sprintf(`LOCK TABLE "%s" IN SHARE ROW EXCLUSIVE MODE`, table)).Error; err != nil {
					return fmt.Errorf("cannot lock legacy task evidence source")
				}
			}
		}
		if err := tx.AutoMigrate(&LegacyTaskObligation{}); err != nil {
			return fmt.Errorf("cannot migrate legacy task evidence schema")
		}
		if err := tx.Exec(`CREATE OR REPLACE FUNCTION protect_legacy_task_obligations() RETURNS trigger AS $$
BEGIN
 RAISE EXCEPTION 'legacy task evidence is immutable';
END; $$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS legacy_task_obligations_immutable ON "legacy_task_obligations";
CREATE TRIGGER legacy_task_obligations_immutable BEFORE UPDATE OR DELETE ON "legacy_task_obligations"
 FOR EACH ROW EXECUTE FUNCTION protect_legacy_task_obligations();
DROP TRIGGER IF EXISTS legacy_task_obligations_no_truncate ON "legacy_task_obligations";
CREATE TRIGGER legacy_task_obligations_no_truncate BEFORE TRUNCATE ON "legacy_task_obligations"
 FOR EACH STATEMENT EXECUTE FUNCTION protect_legacy_task_obligations();`).Error; err != nil {
			return fmt.Errorf("cannot protect legacy task evidence")
		}
		if !tx.Migrator().HasTable(&Task{}) {
			return nil
		}
		return captureLegacyTaskObligations(tx)
	})
}

// Only these scalar fields leave PostgreSQL. Never scan Task.PrivateData (or
// Task.Data): both may contain credentials, private results and request bodies.
// Keep opaque JSON out of jsonb's numeric domain, and inspect identity key
// multiplicity before selecting the scalar fields so duplicates cannot collapse.
const legacyTaskEvidenceProjection = `t.id AS source_task_id, t.user_id, t.channel_id AS provider_id,
 t.task_id, t.platform, t.quota AS source_quota, t.status AS source_status,
 t.created_at AS source_created_at, t.updated_at AS source_updated_at,
 t.submit_time AS source_submit_time, t.start_time AS source_start_time, t.finish_time AS source_finish_time,
 COALESCE(json_typeof(t.private_data::json), '') AS private_type,
 COALESCE(json_typeof(t.private_data::json->'gateway'), '') AS gateway_type,
 COALESCE(json_typeof(t.private_data::json->'legacy_obligation'), '') AS midjourney_type,
 COALESCE(json_typeof(t.private_data::json->'execution'), '') AS execution_type,
 COALESCE(json_typeof(t.private_data::json->'execution'->'task_plugin'), '') AS plugin_type,
 (EXISTS (SELECT 1 FROM json_each(CASE WHEN json_typeof(t.private_data::json) = 'object'
   THEN t.private_data::json ELSE '{}'::json END) AS fields
   WHERE fields.key IN ('gateway','legacy_obligation','billing_source','subscription_id','token_id','upstream_task_id','execution')
   GROUP BY fields.key HAVING COUNT(*) > 1)
 OR EXISTS (SELECT 1 FROM json_each(CASE WHEN json_typeof(t.private_data::json->'execution') = 'object'
   THEN t.private_data::json->'execution' ELSE '{}'::json END) AS fields
   WHERE fields.key IN ('request_id','task_plugin') GROUP BY fields.key HAVING COUNT(*) > 1)
 OR EXISTS (SELECT 1 FROM json_each(CASE WHEN json_typeof(t.private_data::json->'execution'->'task_plugin') = 'object'
   THEN t.private_data::json->'execution'->'task_plugin' ELSE '{}'::json END) AS fields
   WHERE fields.key IN ('key','version','generation') GROUP BY fields.key HAVING COUNT(*) > 1)) AS duplicate_identity,
 json_build_object(
 'billing_source', t.private_data::json->'billing_source',
 'subscription_id', t.private_data::json->'subscription_id',
 'token_id', t.private_data::json->'token_id',
 'upstream_task_id', t.private_data::json->'upstream_task_id',
 'request_id', t.private_data::json->'execution'->'request_id',
 'plugin_key', t.private_data::json->'execution'->'task_plugin'->'key',
 'plugin_version', t.private_data::json->'execution'->'task_plugin'->'version',
 'plugin_generation', t.private_data::json->'execution'->'task_plugin'->'generation'
 )::text AS identity_json`

type legacyTaskEvidenceSource struct {
	LegacyTaskObligation `gorm:"embedded"`
	PrivateType          string
	GatewayType          string
	MidjourneyType       string
	ExecutionType        string
	PluginType           string
	DuplicateIdentity    bool
	IdentityJSON         string
}

type legacyTaskEvidenceIdentity struct {
	BillingSource    string `json:"billing_source"`
	SubscriptionID   int    `json:"subscription_id"`
	TokenID          int    `json:"token_id"`
	UpstreamTaskID   string `json:"upstream_task_id"`
	RequestID        string `json:"request_id"`
	PluginKey        string `json:"plugin_key"`
	PluginVersion    string `json:"plugin_version"`
	PluginGeneration uint64 `json:"plugin_generation"`
}

func captureLegacyTaskObligations(tx *gorm.DB) error {
	hasSubscriptionArchive := tx.Migrator().HasTable(&LegacySubscriptionTaskObligation{})
	capturedAt := clock.Now().Unix()
	var lastID int64
	first := true
	for {
		var batch []legacyTaskEvidenceSource
		query := tx.Table("tasks AS t").Select(legacyTaskEvidenceProjection).Order("t.id").Limit(250)
		if !first {
			query = query.Where("t.id > ?", lastID)
		}
		if err := query.Scan(&batch).Error; err != nil {
			// PostgreSQL JSON parsing errors can quote private source data.
			return fmt.Errorf("cannot read legacy task evidence source")
		}
		if len(batch) == 0 {
			return nil
		}
		ids := make([]int64, len(batch))
		for i := range batch {
			ids[i] = batch[i].SourceTaskID
		}
		var existing []LegacyTaskObligation
		if err := tx.Where("source_task_id IN ?", ids).Find(&existing).Error; err != nil {
			return fmt.Errorf("cannot read captured legacy task evidence")
		}
		byID := make(map[int64]LegacyTaskObligation, len(existing))
		for _, row := range existing {
			byID[row.SourceTaskID] = row
		}
		subscriptionIDs := map[int64]bool{}
		if hasSubscriptionArchive {
			var captured []int64
			if err := tx.Model(&LegacySubscriptionTaskObligation{}).Where("source_task_id IN ?", ids).Pluck("source_task_id", &captured).Error; err != nil {
				return fmt.Errorf("cannot read legacy subscription task evidence")
			}
			for _, id := range captured {
				subscriptionIDs[id] = true
			}
		}
		pending := make([]LegacyTaskObligation, 0, len(batch))
		for _, source := range batch {
			if source.DuplicateIdentity {
				return fmt.Errorf("ambiguous legacy task evidence identity for task %d", source.SourceTaskID)
			}
			for _, kind := range []string{source.PrivateType, source.GatewayType, source.MidjourneyType, source.ExecutionType, source.PluginType} {
				if kind != "" && kind != "null" && kind != "object" {
					return fmt.Errorf("invalid legacy task evidence structure for task %d", source.SourceTaskID)
				}
			}
			var identity legacyTaskEvidenceIdentity
			if err := common.DecodeJsonStrict(strings.NewReader(source.IdentityJSON), &identity); err != nil {
				return fmt.Errorf("invalid legacy task evidence identity for task %d", source.SourceTaskID)
			}
			row := source.LegacyTaskObligation
			row.BillingSource = identity.BillingSource
			row.SubscriptionID = identity.SubscriptionID
			row.TokenID = identity.TokenID
			row.UpstreamTaskID = identity.UpstreamTaskID
			row.RequestID = identity.RequestID
			row.PluginKey = identity.PluginKey
			row.PluginVersion = identity.PluginVersion
			row.PluginGeneration = identity.PluginGeneration
			excluded := source.GatewayType == "object" || source.MidjourneyType == "object" ||
				identity.BillingSource == "subscription" || subscriptionIDs[row.SourceTaskID]
			if previous, ok := byID[row.SourceTaskID]; ok {
				if excluded || previous.State != "unverified" || !sameLegacyTaskEvidenceIdentity(previous, row) {
					return fmt.Errorf("legacy task evidence identity changed for task %d", row.SourceTaskID)
				}
				// Runtime polling may advance status, quota and timestamps. Do not
				// refresh that first snapshot or reinterpret it as financial truth.
				continue
			}
			if excluded {
				continue
			}
			row.State = "unverified"
			row.CapturedAt = capturedAt
			pending = append(pending, row)
		}
		if len(pending) > 0 {
			if err := tx.Create(&pending).Error; err != nil {
				return fmt.Errorf("cannot preserve legacy task evidence")
			}
		}
		lastID = batch[len(batch)-1].SourceTaskID
		first = false
	}
}

func sameLegacyTaskEvidenceIdentity(a, b LegacyTaskObligation) bool {
	return a.SourceTaskID == b.SourceTaskID && a.UserID == b.UserID && a.ProviderID == b.ProviderID &&
		a.TaskID == b.TaskID && a.UpstreamTaskID == b.UpstreamTaskID && a.Platform == b.Platform &&
		a.BillingSource == b.BillingSource && a.TokenID == b.TokenID && a.SubscriptionID == b.SubscriptionID &&
		a.RequestID == b.RequestID && a.PluginKey == b.PluginKey && a.PluginVersion == b.PluginVersion &&
		a.PluginGeneration == b.PluginGeneration
}
