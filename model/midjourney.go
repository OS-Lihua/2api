package model

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const legacyMidjourneyMigrationVersion = "legacy_midjourney_obligations_v2"

// A startup resource bound, not a claim that larger historical values are
// corrupt. Oversized candidates remain untouched and require offline review.
const legacyMidjourneyPrivateDataMaxBytes = 16 << 20

// Only this ASCII field name determines whether an otherwise unmapped Task
// claims legacy MJ identity. None of its letters have non-ASCII SimpleFold
// aliases; the full identity-key validator below still uses strings.EqualFold.
const legacyMidjourneyIdentityCandidateSQL = `(t.id IN (SELECT migrated_task_id FROM midjourneys)
 OR EXISTS (SELECT 1 FROM json_each(CASE WHEN json_typeof(t.private_data::json)='object'
  THEN t.private_data::json ELSE '{}'::json END) f
  WHERE translate(f.key,'ABCDEFGHIJKLMNOPQRSTUVWXYZ','abcdefghijklmnopqrstuvwxyz')='legacy_obligation'))`

var ErrLegacyMidjourneyMigrationRequired = errors.New("legacy Midjourney migration is incomplete; stop all nodes and run migrate-legacy")

// ValidateLegacyMidjourneyMigration checks one read-only source snapshot before
// either a master or a replica starts serving. It does not migrate anything and
// cannot replace the offline requirement to stop every old writer first.
func ValidateLegacyMidjourneyMigration(db *gorm.DB) error {
	if db == nil || db.Dialector.Name() != "postgres" {
		return errors.New("cannot verify legacy Midjourney migration")
	}
	err := db.Session(&gorm.Session{Logger: logger.Discard}).Transaction(func(tx *gorm.DB) error {
		var exists bool
		if err := tx.Raw(`SELECT to_regclass('midjourneys') IS NOT NULL`).Scan(&exists).Error; err != nil {
			return errors.New("cannot inspect legacy Midjourney source")
		}
		if !exists {
			return nil
		}
		if err := tx.Raw(`SELECT EXISTS (SELECT 1 FROM midjourneys)`).Scan(&exists).Error; err != nil {
			return errors.New("cannot read legacy Midjourney source")
		}
		// Released empty tables need neither the new column nor any marker.
		if !exists {
			return nil
		}
		return validateLegacyMidjourneyMappings(tx, false)
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil && !errors.Is(err, ErrLegacyMidjourneyMigrationRequired) {
		return errors.New("cannot verify legacy Midjourney mappings; stop all nodes and run migrate-legacy")
	}
	return err
}

// Validate size before any key aggregation or raw private-data transfer. Then
// stream only real mapping targets through the same Scanner used by Task reads.
// Decoded values are transient: never wrap decoder/driver errors or log them.
func validateLegacyMidjourneyPrivateData(tx *gorm.DB) error {
	quiet := tx.Session(&gorm.Session{Logger: logger.Discard})
	var oversized []struct{ ID int64 }
	if err := quiet.Raw(`SELECT t.id FROM tasks t
 WHERE octet_length(t.private_data::text)>? AND `+legacyMidjourneyIdentityCandidateSQL+`
 ORDER BY t.id LIMIT 1`, legacyMidjourneyPrivateDataMaxBytes).Scan(&oversized).Error; err != nil {
		return errors.New("cannot inspect legacy Midjourney private data bounds")
	}
	if len(oversized) != 0 {
		return fmt.Errorf("%w (task %d exceeds private data validation resource limit)", ErrLegacyMidjourneyMigrationRequired, oversized[0].ID)
	}
	rows, err := quiet.Raw(`SELECT t.id, t.private_data::text AS typed_private_data FROM tasks t
 WHERE t.id IN (SELECT migrated_task_id FROM midjourneys) ORDER BY t.id`).Rows()
	if err != nil {
		return errors.New("cannot inspect legacy Midjourney private data")
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var private TaskPrivateData
		if err := rows.Scan(&id, &private); err != nil {
			return fmt.Errorf("%w (task %d has unreadable private data)", ErrLegacyMidjourneyMigrationRequired, id)
		}
	}
	if err := rows.Err(); err != nil {
		return errors.New("cannot finish reading legacy Midjourney private data")
	}
	return nil
}

// encoding/json folds field names, including Unicode SimpleFold aliases. A
// case-sensitive JSON SQL projection alone would miss identities that Go reads.
// Inspect names only, retaining duplicates, and reject rather than normalize.
func validateLegacyMidjourneyIdentityKeys(tx *gorm.DB) error {
	rows, err := tx.Session(&gorm.Session{Logger: logger.Discard}).Raw(`SELECT t.id,
 t.id IN (SELECT migrated_task_id FROM midjourneys) AS mapped,
 COALESCE((SELECT json_agg(f.key) FROM json_each(CASE WHEN json_typeof(t.private_data::json)='object'
  THEN t.private_data::json ELSE '{}'::json END) f),'[]'::json)::text AS private_keys,
 COALESCE((SELECT json_agg(f.key) FROM json_each(CASE WHEN json_typeof(t.private_data::json->'legacy_obligation')='object'
  THEN t.private_data::json->'legacy_obligation' ELSE '{}'::json END) f),'[]'::json)::text AS obligation_keys
 FROM tasks t WHERE ` + legacyMidjourneyIdentityCandidateSQL).Rows()
	if err != nil {
		return errors.New("cannot inspect legacy Midjourney identity keys")
	}
	defer rows.Close()
	privateFields := []string{"gateway", "legacy_obligation", "upstream_task_id", "key", "billing_source", "subscription_id", "token_id", "billing_context", "execution"}
	obligationFields := []string{"version", "source_id", "provider_id", "provider_type", "source_quota", "reconciliation", "debit_evidence", "refund_evidence"}
	canonical := func(keys, fields []string) bool {
		seen := make(map[string]bool, len(fields))
		for _, key := range keys {
			for _, field := range fields {
				if strings.EqualFold(key, field) {
					if key != field || seen[field] {
						return false
					}
					seen[field] = true
				}
			}
		}
		return true
	}
	for rows.Next() {
		var id int64
		var mapped bool
		var privateJSON, obligationJSON string
		if err := rows.Scan(&id, &mapped, &privateJSON, &obligationJSON); err != nil {
			return errors.New("cannot read legacy Midjourney identity keys")
		}
		var privateKeys, obligationKeys []string
		if err := common.UnmarshalJsonStr(privateJSON, &privateKeys); err != nil {
			return errors.New("cannot decode legacy Midjourney identity keys")
		}
		for _, key := range privateKeys {
			mapped = mapped || strings.EqualFold(key, "legacy_obligation")
		}
		if !mapped {
			continue
		}
		if err := common.UnmarshalJsonStr(obligationJSON, &obligationKeys); err != nil {
			return errors.New("cannot decode legacy Midjourney obligation keys")
		}
		if !canonical(privateKeys, privateFields) || !canonical(obligationKeys, obligationFields) {
			return fmt.Errorf("%w (task %d has ambiguous identity keys)", ErrLegacyMidjourneyMigrationRequired, id)
		}
	}
	if err := rows.Err(); err != nil {
		return errors.New("cannot finish reading legacy Midjourney identity keys")
	}
	return nil
}

// validateLegacyMidjourneyMappings shares the exact source/target check between
// startup and offline migration. Only bounded mapped private_data is decoded
// transiently; identity SQL returns IDs/key names, never provider settings or
// unrelated Task payload. No private value is written, returned or logged.
// Results/progress/completion facts may advance; the original identity may not.
// Raw json preserves integer token spelling: jsonb would normalize 91e0 into
// 91 even though the Go int64 decoder rejects the original identity.
func validateLegacyMidjourneyMappings(tx *gorm.DB, allowUnmapped bool) error {
	if err := validateLegacyMidjourneyPrivateData(tx); err != nil {
		return err
	}
	if err := validateLegacyMidjourneyIdentityKeys(tx); err != nil {
		return err
	}
	var invalid []struct{ ID int64 }
	query := tx.Session(&gorm.Session{Logger: logger.Discard}).Raw(`
WITH identities AS MATERIALIZED (
 SELECT t.id, t.task_id, t.platform, t.user_id, t.channel_id, t.action, t.quota,
  t.private_data::json AS private, t.private_data::json->'legacy_obligation' AS obligation
 FROM tasks t WHERE `+legacyMidjourneyIdentityCandidateSQL+`
), claims AS (
 SELECT obligation->>'source_id' AS source_id, COUNT(*) AS count
 FROM identities WHERE obligation IS NOT NULL GROUP BY obligation->>'source_id'
), public_ids AS (
 SELECT task_id, COUNT(*) AS count FROM tasks
 WHERE task_id IN (SELECT task_id FROM identities) GROUP BY task_id
)
SELECT m.id FROM midjourneys m
 LEFT JOIN identities t ON t.id=m.migrated_task_id
 LEFT JOIN claims c ON c.source_id=m.id::text
 LEFT JOIN public_ids p ON p.task_id=t.task_id
WHERE NOT COALESCE(CASE WHEN ? AND m.migrated_task_id=0 THEN c.count IS NULL ELSE
 m.migrated_task_id>0 AND t.id IS NOT NULL AND c.count=1 AND p.count=1
 AND t.task_id<>'' AND t.task_id<>COALESCE(m.mj_id,'') AND t.platform='midjourney'
 AND t.user_id=COALESCE(m.user_id,0) AND t.channel_id=COALESCE(m.channel_id,0)
 AND t.action=COALESCE(m.action,'') AND t.quota=0
 AND json_typeof(t.private)='object' AND json_typeof(t.obligation)='object'
 AND COALESCE(t.private->>'upstream_task_id','')=COALESCE(m.mj_id,'')
 AND COALESCE(json_typeof(t.private->'upstream_task_id'),'null') IN ('null','string')
 AND COALESCE(json_typeof(t.private->'gateway'),'null')='null'
 AND COALESCE(json_typeof(t.private->'billing_context'),'null')='null'
 AND COALESCE(json_typeof(t.private->'execution'),'null')='null'
 AND COALESCE(t.private->>'key','')='' AND COALESCE(t.private->>'billing_source','')=''
 AND COALESCE(json_typeof(t.private->'key'),'null') IN ('null','string')
 AND COALESCE(json_typeof(t.private->'billing_source'),'null') IN ('null','string')
 AND COALESCE(t.private->>'subscription_id','0')='0'
 AND COALESCE(json_typeof(t.private->'subscription_id'),'null') IN ('null','number')
 AND COALESCE(t.private->>'token_id','0')='0'
 AND COALESCE(json_typeof(t.private->'token_id'),'null') IN ('null','number')
 AND t.obligation->>'version'=?
 AND json_typeof(t.obligation->'source_id')='number' AND t.obligation->>'source_id'=m.id::text
 AND json_typeof(t.obligation->'provider_id')='number' AND t.obligation->>'provider_id'=COALESCE(m.channel_id,0)::text
 AND json_typeof(t.obligation->'source_quota')='number' AND t.obligation->>'source_quota'=COALESCE(m.quota,0)::text
 AND json_typeof(t.obligation->'provider_type')='number' AND t.obligation->>'provider_type' IN ('0', ?::text, ?::text)
 AND t.obligation->>'reconciliation'='pending'
 AND t.obligation->>'debit_evidence'='unverified' AND t.obligation->>'refund_evidence'='unverified'
 END, false) ORDER BY m.id LIMIT 1`, allowUnmapped, legacyMidjourneyMigrationVersion, constant.ChannelTypeMidjourney, constant.ChannelTypeMidjourneyPlus).Scan(&invalid)
	if query.Error != nil {
		// PostgreSQL parse errors can contain private JSON; never wrap them here.
		return errors.New("cannot verify legacy Midjourney mappings; stop all nodes and run migrate-legacy")
	}
	if len(invalid) != 0 {
		return fmt.Errorf("%w (source %d)", ErrLegacyMidjourneyMigrationRequired, invalid[0].ID)
	}
	return nil
}

type Midjourney struct {
	Id          int    `json:"id"`
	Code        int    `json:"code"`
	UserId      int    `json:"user_id" gorm:"index"`
	Action      string `json:"action" gorm:"type:varchar(40);index"`
	MjId        string `json:"mj_id" gorm:"index"`
	Prompt      string `json:"prompt"`
	PromptEn    string `json:"prompt_en"`
	Description string `json:"description"`
	State       string `json:"state"`
	SubmitTime  int64  `json:"submit_time" gorm:"index"`
	StartTime   int64  `json:"start_time" gorm:"index"`
	FinishTime  int64  `json:"finish_time" gorm:"index"`
	ImageUrl    string `json:"image_url"`
	VideoUrl    string `json:"video_url"`
	VideoUrls   string `json:"video_urls"`
	Status      string `json:"status" gorm:"type:varchar(20);index"`
	Progress    string `json:"progress" gorm:"type:varchar(30);index"`
	FailReason  string `json:"fail_reason"`
	ChannelId   int    `json:"channel_id"`
	Quota       int    `json:"quota"`
	Buttons     string `json:"buttons"`
	Properties  string `json:"properties"`

	TokenId          int `json:"-" gorm:"default:0"`
	BillingChannelId int `json:"-" gorm:"default:0"`
	// MigratedTaskID retires this row's legacy writers without deleting history.
	MigratedTaskID int64 `json:"-" gorm:"not null;default:0;index"`
}

// TaskQueryParams 用于包含所有搜索条件的结构体，可以根据需求添加更多字段
type TaskQueryParams struct {
	ChannelID      string
	MjID           string
	StartTimestamp string
	EndTimestamp   string
}

func GetAllUserTask(userId int, startIdx int, num int, queryParams TaskQueryParams) []*Midjourney {
	var tasks []*Midjourney
	var err error

	// 初始化查询构建器
	query := DB.Where("user_id = ?", userId)

	if queryParams.MjID != "" {
		query = query.Where("mj_id = ?", queryParams.MjID)
	}
	if queryParams.StartTimestamp != "" {
		// 假设您已将前端传来的时间戳转换为数据库所需的时间格式，并处理了时间戳的验证和解析
		query = query.Where("submit_time >= ?", queryParams.StartTimestamp)
	}
	if queryParams.EndTimestamp != "" {
		query = query.Where("submit_time <= ?", queryParams.EndTimestamp)
	}

	// 获取数据
	err = query.Order("id desc").Limit(num).Offset(startIdx).Find(&tasks).Error
	if err != nil {
		return nil
	}

	return tasks
}

func GetAllTasks(startIdx int, num int, queryParams TaskQueryParams) []*Midjourney {
	var tasks []*Midjourney
	var err error

	// 初始化查询构建器
	query := DB

	// 添加过滤条件
	if queryParams.ChannelID != "" {
		query = query.Where("channel_id = ?", queryParams.ChannelID)
	}
	if queryParams.MjID != "" {
		query = query.Where("mj_id = ?", queryParams.MjID)
	}
	if queryParams.StartTimestamp != "" {
		query = query.Where("submit_time >= ?", queryParams.StartTimestamp)
	}
	if queryParams.EndTimestamp != "" {
		query = query.Where("submit_time <= ?", queryParams.EndTimestamp)
	}

	// 获取数据
	err = query.Order("id desc").Limit(num).Offset(startIdx).Find(&tasks).Error
	if err != nil {
		return nil
	}

	return tasks
}

func GetAllUnFinishTasks() []*Midjourney {
	var tasks []*Midjourney
	var err error
	// get all tasks progress is not 100%
	err = DB.Where("progress != ? AND migrated_task_id = ?", "100%", 0).Find(&tasks).Error
	if err != nil {
		return nil
	}
	return tasks
}

// HasUnfinishedMidjourneyTasks reports whether at least one Midjourney task is
// still in progress. It is a cheap existence check (LIMIT 1) used to decide
// whether the midjourney_poll system task needs to run; when no task is pending
// the scheduler skips creating a row entirely.
func HasUnfinishedMidjourneyTasks() bool {
	var id int
	err := DB.Model(&Midjourney{}).
		Where("progress != ? AND migrated_task_id = ?", "100%", 0).
		Limit(1).
		Pluck("id", &id).Error
	return err == nil && id != 0
}

func GetByOnlyMJId(mjId string) *Midjourney {
	var mj *Midjourney
	var err error
	err = DB.Where("mj_id = ?", mjId).First(&mj).Error
	if err != nil {
		return nil
	}
	return mj
}

func GetByMJId(userId int, mjId string) *Midjourney {
	var mj *Midjourney
	var err error
	err = DB.Where("user_id = ? and mj_id = ?", userId, mjId).First(&mj).Error
	if err != nil {
		return nil
	}
	return mj
}

func GetByMJIds(userId int, mjIds []string) []*Midjourney {
	var mj []*Midjourney
	var err error
	err = DB.Where("user_id = ? and mj_id in (?)", userId, mjIds).Find(&mj).Error
	if err != nil {
		return nil
	}
	return mj
}

func GetMjByuId(id int) *Midjourney {
	var mj *Midjourney
	var err error
	err = DB.Where("id = ?", id).First(&mj).Error
	if err != nil {
		return nil
	}
	return mj
}

func UpdateProgress(id int, progress string) error {
	return DB.Model(&Midjourney{}).Where("id = ? AND migrated_task_id = ?", id, 0).Update("progress", progress).Error
}

// LegacyWritable reloads the migration marker so a stale poller cannot refund
// an imported obligation. Migration itself runs offline and exclusively.
func (midjourney *Midjourney) LegacyWritable() bool {
	if midjourney == nil || midjourney.Id <= 0 || midjourney.MigratedTaskID != 0 {
		return false
	}
	var count int64
	err := DB.Model(&Midjourney{}).Where("id = ? AND migrated_task_id = ?", midjourney.Id, 0).Count(&count).Error
	return err == nil && count == 1
}

func (midjourney *Midjourney) Insert() error {
	var err error
	err = DB.Create(midjourney).Error
	return err
}

func (midjourney *Midjourney) Update() error {
	return DB.Model(midjourney).Where("migrated_task_id = ?", 0).Select("*").Updates(midjourney).Error
}

func (midjourney *Midjourney) UpdateBillingState() error {
	return DB.Model(midjourney).Where("migrated_task_id = ?", 0).
		Select("quota", "token_id", "billing_channel_id").
		Updates(midjourney).Error
}

func (midjourney *Midjourney) GetBillingChannelId() int {
	if midjourney.BillingChannelId > 0 {
		return midjourney.BillingChannelId
	}
	return midjourney.ChannelId
}

// UpdateWithStatus performs a conditional UPDATE guarded by fromStatus (CAS).
// Returns (true, nil) if this caller won the update, (false, nil) if
// another process already moved the task out of fromStatus.
// UpdateWithStatus performs a conditional UPDATE guarded by fromStatus (CAS).
// Uses Model().Select("*").Updates() to avoid GORM Save()'s INSERT fallback.
func (midjourney *Midjourney) UpdateWithStatus(fromStatus string) (bool, error) {
	result := DB.Model(midjourney).Where("status = ? AND migrated_task_id = ?", fromStatus, 0).Select("*").Updates(midjourney)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

func MjBulkUpdate(mjIds []string, params map[string]any) error {
	return DB.Model(&Midjourney{}).
		Where("mj_id in (?) AND migrated_task_id = ?", mjIds, 0).
		Updates(params).Error
}

func MjBulkUpdateByTaskIds(taskIDs []int, params map[string]any) error {
	return DB.Model(&Midjourney{}).
		Where("id in (?) AND migrated_task_id = ?", taskIDs, 0).
		Updates(params).Error
}

// CountAllTasks returns total midjourney tasks for admin query
func CountAllTasks(queryParams TaskQueryParams) int64 {
	var total int64
	query := DB.Model(&Midjourney{})
	if queryParams.ChannelID != "" {
		query = query.Where("channel_id = ?", queryParams.ChannelID)
	}
	if queryParams.MjID != "" {
		query = query.Where("mj_id = ?", queryParams.MjID)
	}
	if queryParams.StartTimestamp != "" {
		query = query.Where("submit_time >= ?", queryParams.StartTimestamp)
	}
	if queryParams.EndTimestamp != "" {
		query = query.Where("submit_time <= ?", queryParams.EndTimestamp)
	}
	_ = query.Count(&total).Error
	return total
}

// CountAllUserTask returns total midjourney tasks for user
func CountAllUserTask(userId int, queryParams TaskQueryParams) int64 {
	var total int64
	query := DB.Model(&Midjourney{}).Where("user_id = ?", userId)
	if queryParams.MjID != "" {
		query = query.Where("mj_id = ?", queryParams.MjID)
	}
	if queryParams.StartTimestamp != "" {
		query = query.Where("submit_time >= ?", queryParams.StartTimestamp)
	}
	if queryParams.EndTimestamp != "" {
		query = query.Where("submit_time <= ?", queryParams.EndTimestamp)
	}
	_ = query.Count(&total).Error
	return total
}
