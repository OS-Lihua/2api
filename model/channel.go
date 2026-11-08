package model

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/samber/lo"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormlogger "gorm.io/gorm/logger"
)

type Provider struct {
	Id                 int     `json:"id"`
	Type               int     `json:"type" gorm:"default:0"`
	Key                string  `json:"key" gorm:"not null"`
	OpenAIOrganization *string `json:"openai_organization"`
	TestModel          *string `json:"test_model"`
	Status             int     `json:"status" gorm:"default:1"`
	Name               string  `json:"name" gorm:"index"`
	Weight             *uint   `json:"weight" gorm:"default:0"`
	CreatedTime        int64   `json:"created_time" gorm:"bigint"`
	TestTime           int64   `json:"test_time" gorm:"bigint"`
	ResponseTime       int     `json:"response_time"` // in milliseconds
	BaseURL            *string `json:"base_url" gorm:"column:base_url;default:''"`
	Other              string  `json:"other"`
	Balance            float64 `json:"balance"` // in USD
	BalanceUpdatedTime int64   `json:"balance_updated_time" gorm:"bigint"`
	Models             string  `json:"models"`
	Group              string  `json:"group" gorm:"type:varchar(64);default:'default'"`
	UsedQuota          int64   `json:"used_quota" gorm:"bigint;default:0"`
	ModelMapping       *string `json:"model_mapping" gorm:"type:text"`
	//MaxInputTokens     *int    `json:"max_input_tokens" gorm:"default:0"`
	StatusCodeMapping *string `json:"status_code_mapping" gorm:"type:varchar(1024);default:''"`
	Priority          *int64  `json:"priority" gorm:"bigint;default:0"`
	AutoBan           *int    `json:"auto_ban" gorm:"default:1"`
	OtherInfo         string  `json:"other_info"`
	Tag               *string `json:"tag" gorm:"index"`
	Setting           *string `json:"setting" gorm:"type:text"` // 渠道额外设置
	ParamOverride     *string `json:"param_override" gorm:"type:text"`
	HeaderOverride    *string `json:"header_override" gorm:"type:text"`
	Remark            *string `json:"remark" gorm:"type:varchar(255)" validate:"max=255"`
	// add after v0.8.5
	ProviderInfo ProviderInfo `json:"channel_info" gorm:"column:channel_info;type:json"`

	OtherSettings string `json:"settings" gorm:"column:settings"` // 其他设置，存储azure版本等不需要检索的信息，详见dto.ProviderOtherSettings

	RouteLimits *ProviderRouteLimits `json:"route_limits,omitempty" gorm:"-"`
	// cache info
	Keys []string `json:"-" gorm:"-"`
}

const ProviderStatusReasonAllKeysDisabled = "All keys are disabled"

type ProviderRouteLimits struct {
	RPM            int64 `json:"rpm"`
	TPM            int64 `json:"tpm"`
	MaxConcurrency int64 `json:"max_concurrency"`
}

func (limits ProviderRouteLimits) Valid() bool {
	return limits.RPM >= 0 && limits.TPM >= 0 && limits.MaxConcurrency >= 0 &&
		limits.RPM <= 1_000_000_000 && limits.TPM <= 1_000_000_000_000 && limits.MaxConcurrency <= 1_000_000
}

type ProviderInfo struct {
	RouteLimits            ProviderRouteLimits   `json:"route_limits"`
	IsMultiKey             bool                  `json:"is_multi_key"`                        // 是否多Key模式
	MultiKeySize           int                   `json:"multi_key_size"`                      // 多Key模式下的Key数量
	MultiKeyStatusList     map[int]int           `json:"multi_key_status_list"`               // key状态列表，key index -> status
	MultiKeyDisabledReason map[int]string        `json:"multi_key_disabled_reason,omitempty"` // key禁用原因列表，key index -> reason
	MultiKeyDisabledTime   map[int]int64         `json:"multi_key_disabled_time,omitempty"`   // key禁用时间列表，key index -> time
	MultiKeyPollingIndex   int                   `json:"multi_key_polling_index"`             // 多Key模式下轮询的key索引
	MultiKeyMode           constant.MultiKeyMode `json:"multi_key_mode"`
}

type ProviderSortOptions struct {
	SortBy    string
	SortOrder string
	IDSort    bool
}

var channelSortColumns = map[string]string{
	"id":            "id",
	"name":          "name",
	"priority":      "priority",
	"balance":       "balance",
	"response_time": "response_time",
	"test_time":     "test_time",
}

func NewProviderSortOptions(sortBy string, sortOrder string, idSort bool) ProviderSortOptions {
	normalizedSortBy := strings.ToLower(strings.TrimSpace(sortBy))
	normalizedSortOrder := strings.ToLower(strings.TrimSpace(sortOrder))
	if _, ok := channelSortColumns[normalizedSortBy]; !ok {
		normalizedSortBy = ""
		normalizedSortOrder = ""
	} else if normalizedSortOrder != "asc" {
		normalizedSortOrder = "desc"
	}

	return ProviderSortOptions{
		SortBy:    normalizedSortBy,
		SortOrder: normalizedSortOrder,
		IDSort:    idSort,
	}
}

func (options ProviderSortOptions) Apply(query *gorm.DB) *gorm.DB {
	if columnName, ok := channelSortColumns[options.SortBy]; ok {
		column := clause.Column{Name: columnName}
		if columnName == "priority" {
			// Routing treats a missing priority as zero on every database.
			column = clause.Column{Name: "COALESCE(priority, 0)", Raw: true}
		}
		return query.Order(clause.OrderByColumn{
			Column: column,
			Desc:   options.SortOrder != "asc",
		}).Order(clause.OrderByColumn{Column: clause.Column{Name: "id"}, Desc: true})
	}
	if options.IDSort {
		return query.Order(clause.OrderByColumn{
			Column: clause.Column{Name: "id"},
			Desc:   true,
		})
	}
	return query.Order(clause.OrderByColumn{
		Column: clause.Column{Name: "COALESCE(priority, 0)", Raw: true},
		Desc:   true,
	}).Order(clause.OrderByColumn{Column: clause.Column{Name: "id"}, Desc: true})
}

func resolveChannelSortOptions(idSort bool, sortOptions []ProviderSortOptions) ProviderSortOptions {
	if len(sortOptions) == 0 {
		return NewProviderSortOptions("", "", idSort)
	}
	options := sortOptions[0]
	options.IDSort = options.IDSort || idSort
	return options
}

func NormalizeProviderGroupFilter(group string) string {
	group = strings.TrimSpace(group)
	if group == "" || strings.EqualFold(group, "all") || strings.EqualFold(group, "null") {
		return ""
	}
	return group
}

func channelGroupFilterPattern(group string) string {
	group = strings.NewReplacer(
		"!", "!!",
		"%", "!%",
		"_", "!_",
	).Replace(group)
	return "%," + group + ",%"
}

func ApplyProviderGroupFilter(query *gorm.DB, group string) *gorm.DB {
	group = NormalizeProviderGroupFilter(group)
	if group == "" {
		return query
	}
	return query.Where(`(',' || "group" || ',') LIKE ? ESCAPE '!'`, channelGroupFilterPattern(group))
}

// Value implements driver.Valuer interface
// 必须返回 string 而非 []byte:PG simple protocol 下 []byte 参数按 bytea
// 编码,写 json 列会触发 SQLSTATE 22P02。
func (c ProviderInfo) Value() (driver.Value, error) {
	b, err := common.Marshal(&c)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// Scan implements sql.Scanner interface
func (c *ProviderInfo) Scan(value any) error {
	return common.Unmarshal(jsonScanBytes(value), c)
}

func (channel *Provider) GetKeys() []string {
	if channel.Key == "" {
		return []string{}
	}
	if len(channel.Keys) > 0 {
		return channel.Keys
	}
	trimmed := strings.TrimSpace(channel.Key)
	// If the key starts with '[', try to parse it as a JSON array (e.g., for Vertex AI scenarios)
	if strings.HasPrefix(trimmed, "[") {
		var arr []json.RawMessage
		if err := common.Unmarshal([]byte(trimmed), &arr); err == nil {
			res := make([]string, len(arr))
			for i, v := range arr {
				res[i] = string(v)
			}
			return res
		}
	}
	// Otherwise, fall back to splitting by newline
	keys := strings.Split(strings.Trim(channel.Key, "\n"), "\n")
	return keys
}

func (channel *Provider) GetNextEnabledKey() (string, int, *types.NewAPIError) {
	return channel.nextEnabledKey(true)
}

// PreviewEnabledKey uses the authoritative Route snapshot without advancing a
// cursor or reading the legacy cache. Only the selected candidate commits it.
func (channel *Provider) PreviewEnabledKey() (string, int, *types.NewAPIError) {
	snapshot := *channel
	return snapshot.nextEnabledKey(false)
}

func (channel *Provider) nextEnabledKey(advance bool) (string, int, *types.NewAPIError) {
	// If not in multi-key mode, return the original key string directly.
	if !channel.ProviderInfo.IsMultiKey {
		return channel.Key, 0, nil
	}

	// Obtain all keys (split by \n)
	keys := channel.GetKeys()
	if len(keys) == 0 {
		// No keys available, return error, should disable the channel
		return "", 0, types.NewError(errors.New("no keys available"), types.ErrorCodeChannelNoAvailableKey)
	}

	lock := GetProviderPollingLock(channel.Id)
	lock.Lock()
	defer lock.Unlock()

	statusList := channel.ProviderInfo.MultiKeyStatusList
	// helper to get key status, default to enabled when missing
	getStatus := func(idx int) int {
		if statusList == nil {
			return common.ChannelStatusEnabled
		}
		if status, ok := statusList[idx]; ok {
			return status
		}
		return common.ChannelStatusEnabled
	}

	// Collect indexes of enabled keys
	enabledIdx := make([]int, 0, len(keys))
	for i := range keys {
		if getStatus(i) == common.ChannelStatusEnabled {
			enabledIdx = append(enabledIdx, i)
		}
	}
	// If no specific status list or none enabled, return an explicit error so caller can
	// properly handle a channel with no available keys (e.g. mark channel disabled).
	// Returning the first key here caused requests to keep using an already-disabled key.
	if len(enabledIdx) == 0 {
		return "", 0, types.NewError(errors.New("no enabled keys"), types.ErrorCodeChannelNoAvailableKey)
	}

	switch channel.ProviderInfo.MultiKeyMode {
	case constant.MultiKeyModeRandom:
		// Randomly pick one enabled key
		selectedIdx := enabledIdx[rand.Intn(len(enabledIdx))]
		return keys[selectedIdx], selectedIdx, nil
	case constant.MultiKeyModePolling:
		// Use channel-specific lock to ensure thread-safe polling

		channelInfo := &channel.ProviderInfo
		if advance {
			var err error
			channelInfo, err = CacheGetProviderInfo(channel.Id)
			if err != nil {
				return "", 0, types.NewError(err, types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
			}
		}
		defer func() {
			if !advance {
				return
			}
			if common.DebugEnabled {
				logger.LogDebug(nil, "channel %d polling index: %d", channel.Id, channel.ProviderInfo.MultiKeyPollingIndex)
			}
			if !common.MemoryCacheEnabled {
				_ = channel.SaveProviderInfo()
			} else {
				// CacheUpdateProvider(channel)
			}
		}()
		// Start from the saved polling index and look for the next enabled key
		start := channelInfo.MultiKeyPollingIndex
		if start < 0 || start >= len(keys) {
			start = 0
		}
		for i := range keys {
			idx := (start + i) % len(keys)
			if getStatus(idx) == common.ChannelStatusEnabled {
				// update polling index for next call (point to the next position)
				channel.ProviderInfo.MultiKeyPollingIndex = (idx + 1) % len(keys)
				return keys[idx], idx, nil
			}
		}
		// Fallback – should not happen, but return first enabled key
		return keys[enabledIdx[0]], enabledIdx[0], nil
	default:
		// Unknown mode, default to first enabled key (or original key string)
		return keys[enabledIdx[0]], enabledIdx[0], nil
	}
}

// CommitRouteKey rechecks the exact Provider credential and slot under the
// existing row lock, then advances only the selected polling cursor.
func (channel *Provider) CommitRouteKey(ctx context.Context, key string, index int) error {
	return providerCredentialSession().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Provider
		if err := lockForUpdate(tx).First(&current, channel.Id).Error; err != nil {
			return err
		}
		if current.Status != common.ChannelStatusEnabled || current.Key != channel.Key {
			return ErrProviderStatusConflict
		}
		keys := current.GetKeys()
		if !current.ProviderInfo.IsMultiKey {
			if index != 0 || key != current.Key {
				return ErrProviderStatusConflict
			}
			return nil
		}
		if index < 0 || index >= len(keys) || keys[index] != key {
			return ErrProviderStatusConflict
		}
		if status, exists := current.ProviderInfo.MultiKeyStatusList[index]; exists && status != common.ChannelStatusEnabled {
			return ErrProviderStatusConflict
		}
		if current.ProviderInfo.MultiKeyMode != constant.MultiKeyModePolling {
			return nil
		}
		return tx.Model(&Provider{}).Where("id = ?", current.Id).
			Update("channel_info", gorm.Expr(`jsonb_set("channel_info"::jsonb, '{multi_key_polling_index}', to_jsonb(?::integer), true)::json`, (index+1)%len(keys))).Error
	})
}

// SaveProviderInfo persists the polling cursor without replacing credential
// status written by another node after this Provider snapshot was loaded.
func (channel *Provider) SaveProviderInfo() error {
	return providerCredentialSession().Model(&Provider{}).
		Where(`"id" = ? AND "key" = ? AND "channel_info"::jsonb ->> 'is_multi_key' = 'true' AND "channel_info"::jsonb ->> 'multi_key_mode' = ?`,
			channel.Id, channel.Key, constant.MultiKeyModePolling).
		Update("channel_info", gorm.Expr(`jsonb_set("channel_info"::jsonb, '{multi_key_polling_index}', to_jsonb(?::integer), true)::json`, channel.ProviderInfo.MultiKeyPollingIndex)).Error
}

func (channel *Provider) GetModels() []string {
	if channel.Models == "" {
		return []string{}
	}
	return strings.Split(strings.Trim(channel.Models, ","), ",")
}

func (channel *Provider) GetGroups() []string {
	if channel.Group == "" {
		return []string{}
	}
	groups := strings.Split(strings.Trim(channel.Group, ","), ",")
	for i, group := range groups {
		groups[i] = strings.TrimSpace(group)
	}
	return groups
}

func (channel *Provider) GetOtherInfo() map[string]any {
	otherInfo := make(map[string]any)
	if channel.OtherInfo != "" {
		err := common.Unmarshal([]byte(channel.OtherInfo), &otherInfo)
		if err != nil {
			common.SysLog(common.LogText("failed to unmarshal other info: channel_id=%d, tag=%s, name=%s, error=%v", channel.Id, channel.GetTag(), channel.Name, err))
		}
	}
	return otherInfo
}

func (channel *Provider) SetOtherInfo(otherInfo map[string]any) {
	otherInfoBytes, err := json.Marshal(otherInfo)
	if err != nil {
		common.SysLog(common.LogText("failed to marshal other info: channel_id=%d, tag=%s, name=%s, error=%v", channel.Id, channel.GetTag(), channel.Name, err))
		return
	}
	channel.OtherInfo = string(otherInfoBytes)
}

func (channel *Provider) GetTag() string {
	if channel.Tag == nil {
		return ""
	}
	return *channel.Tag
}

func (channel *Provider) SetTag(tag string) {
	channel.Tag = &tag
}

func (channel *Provider) GetAutoBan() bool {
	if channel.AutoBan == nil {
		return false
	}
	return *channel.AutoBan == 1
}

func (channel *Provider) Save() error {
	return DB.Save(channel).Error
}

func GetAllProviders(startIdx int, num int, selectAll bool, idSort bool, sortOptions ...ProviderSortOptions) ([]*Provider, error) {
	var channels []*Provider
	var err error
	order := resolveChannelSortOptions(idSort, sortOptions)
	if selectAll {
		err = order.Apply(DB).Find(&channels).Error
	} else {
		err = order.Apply(DB).Limit(num).Offset(startIdx).Omit("key").Find(&channels).Error
	}
	return channels, err
}

func GetProvidersByTag(tag string, idSort bool, selectAll bool, sortOptions ...ProviderSortOptions) ([]*Provider, error) {
	var channels []*Provider
	order := resolveChannelSortOptions(idSort, sortOptions)
	query := order.Apply(DB.Where("tag = ?", tag))
	if !selectAll {
		query = query.Omit("key")
	}
	err := query.Find(&channels).Error
	return channels, err
}

func SearchProviders(keyword string, group string, model string, idSort bool, sortOptions ...ProviderSortOptions) ([]*Provider, error) {
	var channels []*Provider
	order := resolveChannelSortOptions(idSort, sortOptions)

	// 构造基础查询
	baseQuery := DB.Model(&Provider{}).Omit("key")

	// 构造WHERE子句
	whereClause := `(id = ? OR name LIKE ? OR "key" = ? OR "base_url" LIKE ?) AND "models" LIKE ?`
	args := []any{common.String2Int(keyword), "%" + keyword + "%", keyword, "%" + keyword + "%", "%" + model + "%"}
	baseQuery = ApplyProviderGroupFilter(baseQuery.Where(whereClause, args...), group)

	// 执行查询
	err := order.Apply(baseQuery).Find(&channels).Error
	if err != nil {
		return nil, err
	}
	return channels, nil
}

// GetProviderById loads a channel directly from the database, bypassing the
// in-memory channel cache.
//
// WARNING: do NOT call this on request hot paths (middleware, distribution,
// relay submit/retry, polling). Every call is a synchronous DB query and will
// not see cache-only state. Use CacheGetProvider instead: it serves from the
// in-memory cache and falls back to this function automatically when
// MemoryCacheEnabled is false. Direct use is appropriate only where fresh DB
// state is required, e.g. admin CRUD, channel testing, or cache (re)building.
// ProviderEdit keeps the current Provider locked until its configuration and
// abilities commit together. Callers must defer Rollback after beginning an edit.
type ProviderEdit struct {
	Provider  *Provider
	Authorize func(*gorm.DB) bool `json:"-"`
	tx        *gorm.DB
	id        int
	key       string
}

// Credential predicates and Provider writes must stay out of SQL logs even
// when DEBUG enables parameter interpolation on ordinary sessions.
func providerCredentialSession() *gorm.DB {
	return DB.Session(&gorm.Session{Logger: DB.Logger.LogMode(gormlogger.Silent)})
}

func BeginProviderEdit(ctx context.Context, id int) (*ProviderEdit, error) {
	tx := providerCredentialSession().WithContext(ctx).Begin()
	if tx.Error != nil {
		return nil, tx.Error
	}
	provider := &Provider{}
	if err := lockForUpdate(tx).First(provider, id).Error; err != nil {
		tx.Rollback()
		return nil, err
	}
	return &ProviderEdit{Provider: provider, tx: tx, id: id, key: provider.Key}, nil
}

func (edit *ProviderEdit) Rollback() {
	edit.tx.Rollback()
}

func GetProviderById(id int, selectAll bool) (*Provider, error) {
	channel := &Provider{Id: id}
	var err error = nil
	if selectAll {
		err = DB.First(channel, "id = ?", id).Error
	} else {
		err = DB.Omit("key").First(channel, "id = ?", id).Error
	}
	if err != nil {
		return nil, err
	}
	return channel, nil
}

func BatchInsertProviders(channels []Provider) error {
	if len(channels) == 0 {
		return nil
	}
	tx := DB.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
			panic(r)
		}
	}()

	// Keep each chunk backed by channels so generated IDs reach the caller.
	for chunk := range slices.Chunk(channels, 50) {
		if err := tx.Create(&chunk).Error; err != nil {
			tx.Rollback()
			return err
		}
		for _, channel_ := range chunk {
			if err := channel_.AddAbilities(tx); err != nil {
				tx.Rollback()
				return err
			}
		}
	}
	return tx.Commit().Error
}

func BatchDeleteProviders(ids []int) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	// 使用事务 分批删除channel表和abilities表
	tx := DB.Begin()
	if tx.Error != nil {
		return 0, tx.Error
	}
	var deletedCount int64
	for _, chunk := range lo.Chunk(ids, 200) {
		result := tx.Where("id in (?)", chunk).Delete(&Provider{})
		if result.Error != nil {
			tx.Rollback()
			return 0, result.Error
		}
		deletedCount += result.RowsAffected
		if err := tx.Where("channel_id in (?)", chunk).Delete(&Ability{}).Error; err != nil {
			tx.Rollback()
			return 0, err
		}
	}
	if err := tx.Commit().Error; err != nil {
		return 0, err
	}
	return deletedCount, nil
}

func (channel *Provider) GetPriority() int64 {
	if channel.Priority == nil {
		return 0
	}
	return *channel.Priority
}

func (channel *Provider) GetWeight() int {
	if channel.Weight == nil {
		return 0
	}
	return int(*channel.Weight)
}

func (channel *Provider) GetBaseURL() string {
	if channel.BaseURL == nil {
		return ""
	}
	url := *channel.BaseURL
	if url == "" {
		url = constant.GetChannelBaseURL(channel.Type)
	}
	return url
}

func (channel *Provider) GetModelMapping() string {
	if channel.ModelMapping == nil {
		return ""
	}
	return *channel.ModelMapping
}

func (channel *Provider) GetStatusCodeMapping() string {
	if channel.StatusCodeMapping == nil {
		return ""
	}
	return *channel.StatusCodeMapping
}

func (channel *Provider) Insert() error {
	var err error
	err = DB.Create(channel).Error
	if err != nil {
		return err
	}
	err = channel.AddAbilities(nil)
	return err
}

func (edit *ProviderEdit) Commit(channel *Provider) error {
	defer edit.Rollback()
	if channel.Id != edit.id {
		return gorm.ErrRecordNotFound
	}
	// If this is a multi-key channel, recalculate MultiKeySize based on the current key list to avoid inconsistency after editing keys
	if channel.ProviderInfo.IsMultiKey {
		keyStr := channel.Key
		if keyStr == "" {
			keyStr = edit.key
		}
		// Parse the key list (supports newline separation or JSON array)
		keys := []string{}
		if keyStr != "" {
			trimmed := strings.TrimSpace(keyStr)
			if strings.HasPrefix(trimmed, "[") {
				var arr []json.RawMessage
				if err := common.Unmarshal([]byte(trimmed), &arr); err == nil {
					keys = make([]string, len(arr))
					for i, v := range arr {
						keys[i] = string(v)
					}
				}
			}
			if len(keys) == 0 { // fallback to newline split
				keys = strings.Split(strings.Trim(keyStr, "\n"), "\n")
			}
		}
		channel.ProviderInfo.MultiKeySize = len(keys)
		// Clean up status data that exceeds the new key count to prevent index out of range
		if channel.ProviderInfo.MultiKeyStatusList != nil {
			for idx := range channel.ProviderInfo.MultiKeyStatusList {
				if idx >= channel.ProviderInfo.MultiKeySize {
					delete(channel.ProviderInfo.MultiKeyStatusList, idx)
				}
			}
		}
	}
	result := edit.tx.Model(channel).Updates(channel)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	if err := edit.tx.First(channel, channel.Id).Error; err != nil {
		return err
	}
	if err := channel.UpdateAbilities(edit.tx); err != nil {
		return err
	}
	if edit.Authorize != nil && !edit.Authorize(edit.tx) {
		return ErrProviderMutationForbidden
	}
	return edit.tx.Commit().Error
}

func (channel *Provider) UpdateResponseTime(responseTime int64) {
	err := DB.Model(channel).Select("response_time", "test_time").Updates(Provider{
		TestTime:     common.GetTimestamp(),
		ResponseTime: int(responseTime),
	}).Error
	if err != nil {
		common.SysLog(common.LogText("failed to update response time: channel_id=%d, error=%v", channel.Id, err))
	}
}

func (channel *Provider) UpdateBalance(balance float64) {
	err := DB.Model(channel).Select("balance_updated_time", "balance").Updates(Provider{
		BalanceUpdatedTime: common.GetTimestamp(),
		Balance:            balance,
	}).Error
	if err != nil {
		common.SysLog(common.LogText("failed to update balance: channel_id=%d, error=%v", channel.Id, err))
	}
}

func (channel *Provider) Delete() error {
	var err error
	err = DB.Delete(channel).Error
	if err != nil {
		return err
	}
	err = channel.DeleteAbilities()
	return err
}

var channelStatusLock sync.Mutex

// channelPollingLocks stores locks for each channel.id to ensure thread-safe polling
var channelPollingLocks sync.Map

// GetProviderPollingLock returns or creates a mutex for the given channel ID
func GetProviderPollingLock(channelId int) *sync.Mutex {
	if lock, exists := channelPollingLocks.Load(channelId); exists {
		return lock.(*sync.Mutex)
	}
	// Create new lock for this channel
	newLock := &sync.Mutex{}
	actual, _ := channelPollingLocks.LoadOrStore(channelId, newLock)
	return actual.(*sync.Mutex)
}

// CleanupProviderPollingLocks removes locks for channels that no longer exist
// This is optional and can be called periodically to prevent memory leaks
func CleanupProviderPollingLocks() {
	var activeChannelIds []int
	DB.Model(&Provider{}).Pluck("id", &activeChannelIds)

	activeChannelSet := make(map[int]bool)
	for _, id := range activeChannelIds {
		activeChannelSet[id] = true
	}

	channelPollingLocks.Range(func(key, value any) bool {
		channelId := key.(int)
		if !activeChannelSet[channelId] {
			channelPollingLocks.Delete(channelId)
		}
		return true
	})
}

func handlerMultiKeyUpdate(channel *Provider, change ProviderStatusChange) bool {
	keys := channel.GetKeys()
	keyIndex := -1
	if change.KeyIndex != nil {
		index := *change.KeyIndex
		if index < 0 || index >= len(keys) || keys[index] != change.UsingKey {
			return false
		}
		keyIndex = index
	} else if change.UsingKey != "" {
		for i, key := range keys {
			if key == change.UsingKey {
				keyIndex = i
				break
			}
		}
		if keyIndex < 0 {
			common.SysLog(common.LogText("failed to update multi-key status: channel_id=%d, using key not found", channel.Id))
			return false
		}
	}
	if keyIndex >= 0 {
		if current, exists := channel.ProviderInfo.MultiKeyStatusList[keyIndex]; exists &&
			(change.Status == common.ChannelStatusAutoDisabled && current != common.ChannelStatusEnabled ||
				change.Status == common.ChannelStatusEnabled && current == common.ChannelStatusManuallyDisabled) {
			return false
		}
	}
	if keyIndex < 0 || change.ProviderWide {
		channel.Status = change.Status
		info := channel.GetOtherInfo()
		info["status_reason"] = change.Reason
		info["status_time"] = common.GetTimestamp()
		channel.SetOtherInfo(info)
		return true
	}
	if channel.ProviderInfo.MultiKeyStatusList == nil {
		channel.ProviderInfo.MultiKeyStatusList = make(map[int]int)
	}
	if change.Status == common.ChannelStatusEnabled {
		delete(channel.ProviderInfo.MultiKeyStatusList, keyIndex)
	} else {
		channel.ProviderInfo.MultiKeyStatusList[keyIndex] = change.Status
		if channel.ProviderInfo.MultiKeyDisabledReason == nil {
			channel.ProviderInfo.MultiKeyDisabledReason = make(map[int]string)
		}
		if channel.ProviderInfo.MultiKeyDisabledTime == nil {
			channel.ProviderInfo.MultiKeyDisabledTime = make(map[int]int64)
		}
		channel.ProviderInfo.MultiKeyDisabledReason[keyIndex] = change.Reason
		channel.ProviderInfo.MultiKeyDisabledTime[keyIndex] = common.GetTimestamp()
	}
	if !hasEnabledMultiKey(keys, channel.ProviderInfo.MultiKeyStatusList) {
		channel.Status = common.ChannelStatusAutoDisabled
		info := channel.GetOtherInfo()
		info["status_reason"] = ProviderStatusReasonAllKeysDisabled
		info["status_time"] = common.GetTimestamp()
		channel.SetOtherInfo(info)
	} else if change.Status == common.ChannelStatusEnabled {
		channel.Status = common.ChannelStatusEnabled
	}
	return true
}

func hasEnabledMultiKey(keys []string, statusList map[int]int) bool {
	for i := range keys {
		if statusList == nil {
			return true
		}
		status, ok := statusList[i]
		if !ok || status == common.ChannelStatusEnabled {
			return true
		}
	}
	return false
}

var (
	ErrProviderStatusConflict    = errors.New("provider changed during status update")
	ErrProviderMutationForbidden = errors.New("Unauthorized, insufficient privileges")
)

// ProviderStatusChange keeps the failed credential identity separate from the
// scope of the transition. KeyIndex also pins a credential whose value is empty.
type ProviderStatusChange struct {
	Status       int
	Reason       string
	UsingKey     string
	KeyIndex     *int
	ProviderWide bool
	Authorize    func(*gorm.DB) bool `json:"-"`
}

func UpdateProviderStatus(channelId int, usingKey string, status int, reason string, selectedIndex ...int) bool {
	changed, _ := UpdateProviderStatusWithError(channelId, usingKey, status, reason, selectedIndex...)
	return changed
}

// UpdateProviderStatusWithError distinguishes an idempotent no-op from a
// rejected or failed write so management replies can report the actual outcome.
func UpdateProviderStatusWithError(channelId int, usingKey string, status int, reason string, selectedIndex ...int) (bool, error) {
	change := ProviderStatusChange{Status: status, Reason: reason, UsingKey: usingKey}
	if usingKey != "" && len(selectedIndex) > 0 {
		change.KeyIndex = &selectedIndex[0]
	}
	return ApplyProviderStatus(channelId, change)
}

// ApplyProviderStatus atomically validates the captured credential and applies
// either a Key or whole-Provider transition.
func ApplyProviderStatus(channelId int, change ProviderStatusChange) (bool, error) {
	status, reason, usingKey := change.Status, change.Reason, change.UsingKey
	hasCredential := usingKey != "" || change.KeyIndex != nil
	if common.MemoryCacheEnabled {
		channelStatusLock.Lock()
		defer channelStatusLock.Unlock()
	}

	pollingLock := GetProviderPollingLock(channelId)
	pollingLock.Lock()
	defer pollingLock.Unlock()

	changed := false
	var beforeProvider, committedProvider *Provider
	err := providerCredentialSession().Transaction(func(tx *gorm.DB) (err error) {
		// Validate current caller permissions after any row/ability lock wait,
		// before committing or publishing an otherwise successful transition.
		defer func() {
			if err == nil && change.Authorize != nil && !change.Authorize(tx) {
				err = ErrProviderMutationForbidden
			}
		}()
		// Keep the stored JSON, including NULL and legacy shapes, for the CAS.
		// Comparing the decoded/re-encoded ProviderInfo would reject legacy rows.
		var snapshot struct {
			Provider
			StoredInfo *string `gorm:"column:stored_info"`
		}
		if err := tx.Model(&Provider{}).Select(`*, "channel_info"::text AS "stored_info"`).First(&snapshot, "id = ?", channelId).Error; err != nil {
			return err
		}
		channel := &snapshot.Provider
		before := *channel
		before.ProviderInfo.MultiKeyStatusList = maps.Clone(channel.ProviderInfo.MultiKeyStatusList)
		beforeProvider = &before
		committedProvider = channel
		if status == common.ChannelStatusAutoDisabled && channel.Status != common.ChannelStatusEnabled ||
			hasCredential && status == common.ChannelStatusEnabled && channel.Status == common.ChannelStatusManuallyDisabled {
			return nil
		}
		beforeStatus, beforeOtherInfo := channel.Status, channel.OtherInfo
		overridesKeyExhaustion := channel.ProviderInfo.IsMultiKey && !hasCredential &&
			status == common.ChannelStatusManuallyDisabled && reason != ProviderStatusReasonAllKeysDisabled &&
			channel.GetOtherInfo()["status_reason"] == ProviderStatusReasonAllKeysDisabled
		if channel.Status == status && !overridesKeyExhaustion {
			return nil
		}

		if channel.ProviderInfo.IsMultiKey {
			if !handlerMultiKeyUpdate(channel, change) {
				return nil
			}
		} else {
			if hasCredential && (channel.Key != usingKey || change.KeyIndex != nil && *change.KeyIndex != 0) {
				return nil
			}
			info := channel.GetOtherInfo()
			info["status_reason"] = reason
			info["status_time"] = common.GetTimestamp()
			channel.SetOtherInfo(info)
			channel.Status = status
		}

		// PostgreSQL rechecks these conditions after waiting for a concurrent
		// editor. A stale failure cannot transfer its status to replacement keys
		// or overwrite another node's key status or polling state.
		updates := map[string]any{"status": channel.Status, "other_info": channel.OtherInfo}
		if channel.ProviderInfo.IsMultiKey {
			updates["channel_info"] = channel.ProviderInfo
		}
		result := tx.Model(&Provider{}).
			Where(`"id" = ? AND "key" = ? AND "status" = ? AND COALESCE("other_info", '') = ? AND "channel_info"::jsonb IS NOT DISTINCT FROM CAST(? AS jsonb)`,
				channelId, channel.Key, beforeStatus, beforeOtherInfo, snapshot.StoredInfo).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrProviderStatusConflict
		}
		if beforeStatus != channel.Status {
			if err := tx.Model(&Ability{}).Where("channel_id = ?", channelId).Update("enabled", channel.Status == common.ChannelStatusEnabled).Error; err != nil {
				return err
			}
		}
		changed = true
		return nil
	})
	if err != nil {
		common.SysLog(common.LogText("failed to update channel status: channel_id=%d, status=%d, error=%v", channelId, status, err))
		return false, err
	}
	if common.MemoryCacheEnabled && committedProvider != nil {
		cacheProviderStatusTransition(beforeProvider, committedProvider)
	}
	return changed, nil
}

type ProviderTagStatusError struct {
	CommittedIDs []int
	FailedID     int
	Err          error
}

func (err *ProviderTagStatusError) Error() string { return err.Err.Error() }
func (err *ProviderTagStatusError) Unwrap() error { return err.Err }

func EnableProviderByTag(tag string, authorize ...func(*gorm.DB) bool) error {
	return updateProviderStatusByTag(tag, common.ChannelStatusEnabled, authorize)
}

func DisableProviderByTag(tag string, authorize ...func(*gorm.DB) bool) error {
	return updateProviderStatusByTag(tag, common.ChannelStatusManuallyDisabled, authorize)
}

func updateProviderStatusByTag(tag string, status int, authorize []func(*gorm.DB) bool) error {
	var ids []int
	if err := DB.Model(&Provider{}).Where(`"tag" = ?`, tag).Order("id").Pluck("id", &ids).Error; err != nil {
		return err
	}
	change := ProviderStatusChange{Status: status, Reason: "manual tag operation"}
	if len(authorize) > 0 {
		change.Authorize = authorize[0]
	}
	var committedIDs []int
	for _, id := range ids {
		changed, err := ApplyProviderStatus(id, change)
		if err != nil {
			return &ProviderTagStatusError{CommittedIDs: committedIDs, FailedID: id, Err: err}
		}
		if changed {
			committedIDs = append(committedIDs, id)
		}
	}
	return nil
}

func EditProviderByTag(tag string, newTag *string, modelMapping *string, models *string, group *string, priority *int64, weight *uint, paramOverride *string, headerOverride *string) error {
	updateData := Provider{}
	shouldReCreateAbilities := false
	updatedTag := tag
	// 如果 newTag 不为空且不等于 tag，则更新 tag
	if newTag != nil && *newTag != tag {
		updateData.Tag = newTag
		updatedTag = *newTag
	}
	if modelMapping != nil {
		updateData.ModelMapping = modelMapping
	}
	if models != nil && *models != "" {
		shouldReCreateAbilities = true
		updateData.Models = *models
	}
	if group != nil && *group != "" {
		shouldReCreateAbilities = true
		updateData.Group = *group
	}
	if priority != nil {
		updateData.Priority = priority
	}
	if weight != nil {
		updateData.Weight = weight
	}
	if paramOverride != nil {
		updateData.ParamOverride = paramOverride
	}
	if headerOverride != nil {
		updateData.HeaderOverride = headerOverride
	}

	err := DB.Model(&Provider{}).Where("tag = ?", tag).Updates(updateData).Error
	if err != nil {
		return err
	}
	if shouldReCreateAbilities {
		channels, err := GetProvidersByTag(updatedTag, false, false)
		if err == nil {
			for _, channel := range channels {
				err = channel.UpdateAbilities(nil)
				if err != nil {
					common.SysLog(common.LogText("failed to update abilities: channel_id=%d, tag=%s, error=%v", channel.Id, channel.GetTag(), err))
				}
			}
		}
	} else {
		err := UpdateAbilityByTag(tag, newTag, priority, weight)
		if err != nil {
			return err
		}
	}
	return nil
}

func UpdateProviderUsedQuota(id int, quota int) {
	if common.BatchUpdateEnabled {
		addNewRecord(BatchUpdateTypeChannelUsedQuota, id, quota)
		return
	}
	updateChannelUsedQuota(id, quota)
}

func updateChannelUsedQuota(id int, quota int) {
	err := DB.Model(&Provider{}).Where("id = ?", id).Update("used_quota", gorm.Expr("used_quota + ?", quota)).Error
	if err != nil {
		common.SysLog(common.LogText("failed to update channel used quota: channel_id=%d, delta_quota=%d, error=%v", id, quota, err))
	}
}

func DeleteProviderByStatus(status int64) (int64, error) {
	result := DB.Where("status = ?", status).Delete(&Provider{})
	return result.RowsAffected, result.Error
}

func DeleteDisabledProvider() (int64, error) {
	result := DB.Where("status = ? or status = ?", common.ChannelStatusAutoDisabled, common.ChannelStatusManuallyDisabled).Delete(&Provider{})
	return result.RowsAffected, result.Error
}

func GetPaginatedTags(offset int, limit int) ([]*string, error) {
	return GetPaginatedProviderTags(DB.Model(&Provider{}), offset, limit)
}

func GetPaginatedProviderTags(query *gorm.DB, offset int, limit int) ([]*string, error) {
	var tags []*string
	err := query.
		Select("DISTINCT tag").
		Where("tag is not null AND tag != ''").
		Order(clause.OrderByColumn{Column: clause.Column{Name: "tag"}}).
		Offset(offset).
		Limit(limit).
		Find(&tags).Error
	return tags, err
}

func SearchTags(keyword string, group string, model string, idSort bool) ([]*string, error) {
	var tags []*string
	order := "priority desc"
	if idSort {
		order = "id desc"
	}

	// 构造基础查询
	baseQuery := DB.Model(&Provider{}).Omit("key")

	// 构造WHERE子句
	whereClause := `(id = ? OR name LIKE ? OR "key" = ? OR "base_url" LIKE ?) AND "models" LIKE ?`
	args := []any{common.String2Int(keyword), "%" + keyword + "%", keyword, "%" + keyword + "%", "%" + model + "%"}
	baseQuery = ApplyProviderGroupFilter(baseQuery.Where(whereClause, args...), group)

	subQuery := baseQuery.
		Select("tag").
		Where("tag != ''").
		Order(order)

	err := DB.Table("(?) as sub", subQuery).
		Select("DISTINCT tag").
		Find(&tags).Error

	if err != nil {
		return nil, err
	}

	return tags, nil
}

func (channel *Provider) ValidateSettings() error {
	channelParams := &dto.ProviderSettings{}
	if channel.Setting != nil && *channel.Setting != "" {
		err := common.Unmarshal([]byte(*channel.Setting), channelParams)
		if err != nil {
			return err
		}
	}
	if _, err := common.ParseProxyURLStrict(channelParams.Proxy); err != nil {
		return fmt.Errorf("invalid channel proxy: %w", err)
	}
	if err := channelParams.ValidateHTTPTransport(); err != nil {
		return err
	}
	channelOtherSettings := &dto.ProviderOtherSettings{}
	if channel.OtherSettings != "" {
		err := common.UnmarshalJsonStr(channel.OtherSettings, channelOtherSettings)
		if err != nil {
			return err
		}
	}
	if err := channelOtherSettings.ValidateToolLossPolicy(); err != nil {
		return err
	}
	if preset := common.GetAdvancedCustomPreset(channel.Type); preset != nil {
		channelOtherSettings.AdvancedCustom = preset
	}
	if constant.IsAdvancedCustomChannel(channel.Type) {
		if channelOtherSettings.AdvancedCustom == nil {
			return fmt.Errorf("advanced_custom is required")
		}
	}
	if channelOtherSettings.AdvancedCustom != nil {
		if err := channelOtherSettings.AdvancedCustom.Validate(); err != nil {
			return err
		}
	}
	if constant.IsAdvancedCustomChannel(channel.Type) && channelOtherSettings.UpstreamModelUpdateCheckEnabled {
		if _, ok := channelOtherSettings.AdvancedCustom.ModelListRoute(); !ok {
			return fmt.Errorf("advanced custom channels require a %s route when upstream model update checks are enabled", dto.AdvancedCustomModelListPath)
		}
	}
	return nil
}

func (channel *Provider) GetSetting() dto.ProviderSettings {
	setting := dto.ProviderSettings{}
	if channel.Setting != nil && *channel.Setting != "" {
		err := common.Unmarshal([]byte(*channel.Setting), &setting)
		if err != nil {
			common.SysLog(common.LogText("failed to unmarshal setting: channel_id=%d, error=%v", channel.Id, err))
			return dto.ProviderSettings{}
		}
	}
	return setting
}

func (channel *Provider) SetSetting(setting dto.ProviderSettings) {
	settingBytes, err := common.Marshal(setting)
	if err != nil {
		common.SysLog(common.LogText("failed to marshal setting: channel_id=%d, error=%v", channel.Id, err))
		return
	}
	channel.Setting = common.GetPointer[string](string(settingBytes))
}

func (channel *Provider) GetOtherSettings() dto.ProviderOtherSettings {
	setting := dto.ProviderOtherSettings{}
	if channel.OtherSettings != "" {
		err := common.UnmarshalJsonStr(channel.OtherSettings, &setting)
		if err != nil {
			common.SysLog(common.LogText("failed to unmarshal setting: channel_id=%d, error=%v", channel.Id, err))
			setting = dto.ProviderOtherSettings{}
		}
	}
	if preset := common.GetAdvancedCustomPreset(channel.Type); preset != nil {
		setting.AdvancedCustom = preset
	}
	return setting
}

func (channel *Provider) SetOtherSettings(setting dto.ProviderOtherSettings) {
	settingBytes, err := common.Marshal(setting)
	if err != nil {
		common.SysLog(common.LogText("failed to marshal setting: channel_id=%d, error=%v", channel.Id, err))
		return
	}
	channel.OtherSettings = string(settingBytes)
}

func (channel *Provider) GetParamOverride() map[string]any {
	paramOverride := make(map[string]any)
	if channel.ParamOverride != nil && *channel.ParamOverride != "" {
		err := common.Unmarshal([]byte(*channel.ParamOverride), &paramOverride)
		if err != nil {
			common.SysLog(common.LogText("failed to unmarshal param override: channel_id=%d, error=%v", channel.Id, err))
		}
	}
	return paramOverride
}

func (channel *Provider) GetHeaderOverride() map[string]any {
	headerOverride := make(map[string]any)
	if channel.HeaderOverride != nil && *channel.HeaderOverride != "" {
		err := common.Unmarshal([]byte(*channel.HeaderOverride), &headerOverride)
		if err != nil {
			common.SysLog(common.LogText("failed to unmarshal header override: channel_id=%d, error=%v", channel.Id, err))
		}
	}
	return headerOverride
}

func GetProvidersByIds(ids []int) ([]*Provider, error) {
	var channels []*Provider
	err := DB.Where("id in (?)", ids).Find(&channels).Error
	return channels, err
}

func BatchSetProviderTag(ids []int, tag *string) error {
	// 开启事务
	tx := DB.Begin()
	if tx.Error != nil {
		return tx.Error
	}

	// 更新标签
	err := tx.Model(&Provider{}).Where("id in (?)", ids).Update("tag", tag).Error
	if err != nil {
		tx.Rollback()
		return err
	}

	// update ability status
	channels, err := GetProvidersByIds(ids)
	if err != nil {
		tx.Rollback()
		return err
	}

	for _, channel := range channels {
		err = channel.UpdateAbilities(tx)
		if err != nil {
			tx.Rollback()
			return err
		}
	}

	// 提交事务
	return tx.Commit().Error
}

// CountAllProviders returns total channels in DB
func CountAllProviders() (int64, error) {
	var total int64
	err := DB.Model(&Provider{}).Count(&total).Error
	return total, err
}

// CountAllTags returns number of non-empty distinct tags
func CountAllTags() (int64, error) {
	return CountProviderTags(DB.Model(&Provider{}))
}

func CountProviderTags(query *gorm.DB) (int64, error) {
	var total int64
	err := query.Where("tag is not null AND tag != ''").Distinct("tag").Count(&total).Error
	return total, err
}

// Get channels of specified type with pagination
func GetProvidersByType(startIdx int, num int, idSort bool, channelType int) ([]*Provider, error) {
	var channels []*Provider
	order := "priority desc"
	if idSort {
		order = "id desc"
	}
	err := DB.Where("type = ?", channelType).Order(order).Limit(num).Offset(startIdx).Omit("key").Find(&channels).Error
	return channels, err
}

// Count channels of specific type
func CountProvidersByType(channelType int) (int64, error) {
	var count int64
	err := DB.Model(&Provider{}).Where("type = ?", channelType).Count(&count).Error
	return count, err
}

// Return map[type]count for all channels
func CountProvidersGroupByType() (map[int64]int64, error) {
	type result struct {
		Type  int64 `gorm:"column:type"`
		Count int64 `gorm:"column:count"`
	}
	var results []result
	err := DB.Model(&Provider{}).Select("type, count(*) as count").Group("type").Find(&results).Error
	if err != nil {
		return nil, err
	}
	counts := make(map[int64]int64)
	for _, r := range results {
		counts[r.Type] = r.Count
	}
	return counts, nil
}
