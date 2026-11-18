package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/cachex"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/google/uuid"
	"github.com/samber/hot"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// Subscription duration units
const (
	SubscriptionDurationYear   = "year"
	SubscriptionDurationMonth  = "month"
	SubscriptionDurationDay    = "day"
	SubscriptionDurationHour   = "hour"
	SubscriptionDurationCustom = "custom"
)

// Subscription quota reset period
const (
	SubscriptionResetNever   = "never"
	SubscriptionResetDaily   = "daily"
	SubscriptionResetWeekly  = "weekly"
	SubscriptionResetMonthly = "monthly"
	SubscriptionResetCustom  = "custom"
)

var (
	ErrSubscriptionOrderNotFound      = errors.New("subscription order not found")
	ErrSubscriptionOrderStatusInvalid = errors.New("subscription order status invalid")
)

const (
	subscriptionPlanCacheNamespace     = "new-api:subscription_plan:v1"
	subscriptionPlanInfoCacheNamespace = "new-api:subscription_plan_info:v1"
)

var (
	subscriptionPlanCacheOnce     sync.Once
	subscriptionPlanInfoCacheOnce sync.Once

	subscriptionPlanCache     *cachex.HybridCache[SubscriptionPlan]
	subscriptionPlanInfoCache *cachex.HybridCache[SubscriptionPlanInfo]
)

func subscriptionPlanCacheTTL() time.Duration {
	ttlSeconds := common.GetEnvOrDefault("SUBSCRIPTION_PLAN_CACHE_TTL", 300)
	if ttlSeconds <= 0 {
		ttlSeconds = 300
	}
	return time.Duration(ttlSeconds) * time.Second
}

func subscriptionPlanInfoCacheTTL() time.Duration {
	ttlSeconds := common.GetEnvOrDefault("SUBSCRIPTION_PLAN_INFO_CACHE_TTL", 120)
	if ttlSeconds <= 0 {
		ttlSeconds = 120
	}
	return time.Duration(ttlSeconds) * time.Second
}

func subscriptionPlanCacheCapacity() int {
	capacity := common.GetEnvOrDefault("SUBSCRIPTION_PLAN_CACHE_CAP", 5000)
	if capacity <= 0 {
		capacity = 5000
	}
	return capacity
}

func subscriptionPlanInfoCacheCapacity() int {
	capacity := common.GetEnvOrDefault("SUBSCRIPTION_PLAN_INFO_CACHE_CAP", 10000)
	if capacity <= 0 {
		capacity = 10000
	}
	return capacity
}

func getSubscriptionPlanCache() *cachex.HybridCache[SubscriptionPlan] {
	subscriptionPlanCacheOnce.Do(func() {
		ttl := subscriptionPlanCacheTTL()
		subscriptionPlanCache = cachex.NewHybridCache[SubscriptionPlan](cachex.HybridCacheConfig[SubscriptionPlan]{
			Namespace: cachex.Namespace(subscriptionPlanCacheNamespace),
			Redis:     common.RDB,
			RedisEnabled: func() bool {
				return common.RedisEnabled && common.RDB != nil
			},
			RedisCodec: cachex.JSONCodec[SubscriptionPlan]{},
			Memory: func() *hot.HotCache[string, SubscriptionPlan] {
				return hot.NewHotCache[string, SubscriptionPlan](hot.LRU, subscriptionPlanCacheCapacity()).
					WithTTL(ttl).
					WithJanitor().
					Build()
			},
		})
	})
	return subscriptionPlanCache
}

func getSubscriptionPlanInfoCache() *cachex.HybridCache[SubscriptionPlanInfo] {
	subscriptionPlanInfoCacheOnce.Do(func() {
		ttl := subscriptionPlanInfoCacheTTL()
		subscriptionPlanInfoCache = cachex.NewHybridCache[SubscriptionPlanInfo](cachex.HybridCacheConfig[SubscriptionPlanInfo]{
			Namespace: cachex.Namespace(subscriptionPlanInfoCacheNamespace),
			Redis:     common.RDB,
			RedisEnabled: func() bool {
				return common.RedisEnabled && common.RDB != nil
			},
			RedisCodec: cachex.JSONCodec[SubscriptionPlanInfo]{},
			Memory: func() *hot.HotCache[string, SubscriptionPlanInfo] {
				return hot.NewHotCache[string, SubscriptionPlanInfo](hot.LRU, subscriptionPlanInfoCacheCapacity()).
					WithTTL(ttl).
					WithJanitor().
					Build()
			},
		})
	})
	return subscriptionPlanInfoCache
}

func subscriptionPlanCacheKey(id int) string {
	if id <= 0 {
		return ""
	}
	return strconv.Itoa(id)
}

func InvalidateSubscriptionPlanCache(planId int) {
	if planId <= 0 {
		return
	}
	cache := getSubscriptionPlanCache()
	_, _ = cache.DeleteMany([]string{subscriptionPlanCacheKey(planId)})
	infoCache := getSubscriptionPlanInfoCache()
	_ = infoCache.Purge()
}

// Subscription plan
type SubscriptionPlan struct {
	Id int `json:"id"`

	Title    string `json:"title" gorm:"type:varchar(128);not null"`
	Subtitle string `json:"subtitle" gorm:"type:varchar(255);default:''"`

	// Legacy source columns are retained for offline migration only.
	PriceAmount    float64 `json:"-" gorm:"column:price_amount;->;-:migration"`
	LegacyCurrency string  `json:"-" gorm:"column:currency;->;-:migration"`

	Price        money.Amount `json:"price" gorm:"embedded;embeddedPrefix:price_"`
	Allowance    money.Amount `json:"allowance" gorm:"embedded;embeddedPrefix:allowance_"`
	Unlimited    bool         `json:"unlimited" gorm:"not null;default:false"`
	MoneyVersion int          `json:"money_version" gorm:"not null;default:0"`

	DurationUnit  string `json:"duration_unit" gorm:"type:varchar(16);not null;default:'month'"`
	DurationValue int    `json:"duration_value" gorm:"type:int;not null;default:1"`
	CustomSeconds int64  `json:"custom_seconds" gorm:"type:bigint;not null;default:0"`

	Enabled   bool `json:"enabled" gorm:"default:true"`
	SortOrder int  `json:"sort_order" gorm:"type:int;default:0"`

	AllowBalancePay *bool `json:"allow_balance_pay"`

	// Allow falling back to wallet balance after subscription quota is exhausted (empty = true)
	AllowWalletOverflow *bool `json:"allow_wallet_overflow"`

	StripePriceId         string `json:"stripe_price_id" gorm:"type:varchar(128);default:''"`
	CreemProductId        string `json:"creem_product_id" gorm:"type:varchar(128);default:''"`
	WaffoPancakeProductId string `json:"waffo_pancake_product_id" gorm:"type:varchar(128);default:''"`

	// Max purchases per user (0 = unlimited)
	MaxPurchasePerUser int `json:"max_purchase_per_user" gorm:"type:int;default:0"`

	// Upgrade user group after purchase (empty = no change)
	UpgradeGroup string `json:"upgrade_group" gorm:"type:varchar(64);default:''"`

	// Downgrade user group on expiry (empty = revert to the group held before purchase)
	DowngradeGroup string `json:"downgrade_group" gorm:"type:varchar(64);default:''"`

	// Historical source quota; new grants use Allowance and Unlimited only.
	TotalAmount int64 `json:"-" gorm:"column:total_amount;->;-:migration"`

	// Quota reset period for plan
	QuotaResetPeriod        string `json:"quota_reset_period" gorm:"type:varchar(16);default:'never'"`
	QuotaResetCustomSeconds int64  `json:"quota_reset_custom_seconds" gorm:"type:bigint;default:0"`

	CreatedAt int64 `json:"created_at" gorm:"bigint"`
	UpdatedAt int64 `json:"updated_at" gorm:"bigint"`
}

func (p *SubscriptionPlan) BeforeCreate(tx *gorm.DB) error {
	now := common.GetTimestamp()
	p.CreatedAt = now
	p.UpdatedAt = now
	return nil
}

func (p *SubscriptionPlan) BeforeUpdate(tx *gorm.DB) error {
	p.UpdatedAt = common.GetTimestamp()
	return nil
}

func (p *SubscriptionPlan) ValidateMoney() error {
	if p == nil || p.MoneyVersion != 1 {
		return common.NewMessage("Subscription plan monetary data is not configured")
	}
	if _, err := money.NewAmount(p.Price.Nanos, p.Price.Currency); err != nil {
		return common.NewMessage("Invalid parameters")
	}
	if p.Price.Nanos < 0 {
		return common.NewMessage("Price cannot be negative")
	}
	if p.Allowance.Currency != SettlementCurrency() || p.Allowance.Nanos < 0 || (p.Unlimited && p.Allowance.Nanos != 0) {
		return common.NewMessage("Invalid subscription allowance")
	}
	return nil
}

func (p *SubscriptionPlan) NormalizeDefaults() {
	if p.AllowBalancePay == nil {
		p.AllowBalancePay = common.GetPointer(true)
	}
	if p.AllowWalletOverflow == nil {
		p.AllowWalletOverflow = common.GetPointer(true)
	}
}

// SubscriptionOrderMoney records a new contract, not a verified external cash receipt.
// Old orders have no row: their historical Money cannot establish these facts.
type SubscriptionOrderMoney struct {
	GrantTermsVersion  int                 `json:"grant_terms_version" gorm:"not null;default:0"`
	GrantTerms         string              `json:"-" gorm:"type:text;not null;default:''"`
	TradeNo            string              `json:"-" gorm:"primaryKey;type:varchar(255)"`
	ContractPrice      money.Amount        `json:"contract_price" gorm:"embedded;embeddedPrefix:contract_price_"`
	WalletCharge       money.Amount        `json:"wallet_charge" gorm:"embedded;embeddedPrefix:wallet_charge_"`
	FXRate             decimal.NullDecimal `json:"fx_rate" gorm:"type:numeric(30,18)"`
	UserSubscriptionID int                 `json:"user_subscription_id" gorm:"index;not null;default:0"`
}

// Subscription order (payment -> webhook -> create UserSubscription)
type SubscriptionOrder struct {
	Id       int                     `json:"id"`
	UserId   int                     `json:"user_id" gorm:"index"`
	PlanId   int                     `json:"plan_id" gorm:"index"`
	Money    float64                 `json:"-" gorm:"column:money;->;-:migration"`
	Contract *SubscriptionOrderMoney `json:"contract" gorm:"-"`

	TradeNo         string `json:"trade_no" gorm:"unique;type:varchar(255);index"`
	PaymentMethod   string `json:"payment_method" gorm:"type:varchar(50)"`
	PaymentProvider string `json:"payment_provider" gorm:"type:varchar(50);default:''"`
	Status          string `json:"status"`
	CreateTime      int64  `json:"create_time"`
	CompleteTime    int64  `json:"complete_time"`

	ProviderPayload string `json:"provider_payload" gorm:"type:text"`
}

func (o *SubscriptionOrder) Insert() error {
	if o.CreateTime == 0 {
		o.CreateTime = common.GetTimestamp()
	}
	return DB.Create(o).Error
}

// subscriptionGrantTerms is the versioned, non-secret promise accepted at order
// creation. It is deliberately not the editable Plan or an external paid receipt.
type subscriptionGrantTerms struct {
	PlanID              int          `json:"plan_id"`
	Title               string       `json:"title"`
	Allowance           money.Amount `json:"allowance"`
	Unlimited           bool         `json:"unlimited"`
	DurationUnit        string       `json:"duration_unit"`
	DurationValue       int          `json:"duration_value"`
	CustomSeconds       int64        `json:"custom_seconds"`
	ResetPeriod         string       `json:"reset_period"`
	ResetCustomSeconds  int64        `json:"reset_custom_seconds"`
	AllowWalletOverflow bool         `json:"allow_wallet_overflow"`
	MaxPurchasePerUser  int          `json:"max_purchase_per_user"`
	UpgradeGroup        string       `json:"upgrade_group"`
	DowngradeGroup      string       `json:"downgrade_group"`
}

func subscriptionGrantTermsForPlan(plan *SubscriptionPlan) subscriptionGrantTerms {
	return subscriptionGrantTerms{PlanID: plan.Id, Title: plan.Title, Allowance: plan.Allowance,
		Unlimited: plan.Unlimited, DurationUnit: plan.DurationUnit, DurationValue: plan.DurationValue,
		CustomSeconds: plan.CustomSeconds, ResetPeriod: NormalizeResetPeriod(plan.QuotaResetPeriod),
		ResetCustomSeconds: plan.QuotaResetCustomSeconds, AllowWalletOverflow: plan.AllowWalletOverflow == nil || *plan.AllowWalletOverflow,
		MaxPurchasePerUser: plan.MaxPurchasePerUser, UpgradeGroup: plan.UpgradeGroup, DowngradeGroup: plan.DowngradeGroup}
}

// SubscriptionPurchaseRevision identifies the exact displayed price and grant.
// It uses only this data, not timestamps, caches, current settings or availability.
// The same whitelist also creates the immutable accepted order contract.
func SubscriptionPurchaseRevision(plan *SubscriptionPlan) (string, error) {
	if plan == nil || plan.Id <= 0 || plan.MoneyVersion != 1 {
		return "", errSubscriptionGrantTerms
	}
	payload := struct {
		Version int                    `json:"version"`
		Price   money.Amount           `json:"price"`
		Grant   subscriptionGrantTerms `json:"grant"`
	}{Version: 1, Price: plan.Price, Grant: subscriptionGrantTermsForPlan(plan)}
	encoded, err := common.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "v1:" + hex.EncodeToString(digest[:]), nil
}

var errSubscriptionGrantTerms = common.NewMessage("Subscription grant terms are unavailable")

func validateSubscriptionGrantPlan(plan *SubscriptionPlan) error {
	if plan == nil || plan.Id <= 0 {
		return errSubscriptionGrantTerms
	}
	if err := plan.ValidateMoney(); err != nil {
		return err
	}
	if plan.MaxPurchasePerUser < 0 {
		return errSubscriptionGrantTerms
	}
	if _, err := calcPlanEndTime(time.Unix(0, 0), plan); err != nil {
		return err
	}
	return nil
}

func newSubscriptionOrderMoney(plan *SubscriptionPlan, tradeNo string, walletCharge money.Amount, subscriptionID int) (*SubscriptionOrderMoney, error) {
	if err := validateSubscriptionGrantPlan(plan); err != nil {
		return nil, err
	}
	encoded, err := common.Marshal(subscriptionGrantTermsForPlan(plan))
	if err != nil {
		return nil, err
	}
	return &SubscriptionOrderMoney{TradeNo: tradeNo, ContractPrice: plan.Price, WalletCharge: walletCharge,
		UserSubscriptionID: subscriptionID, GrantTermsVersion: 1, GrantTerms: string(encoded)}, nil
}

func (contract *SubscriptionOrderMoney) grantPlan(planID int) (*SubscriptionPlan, error) {
	if contract.GrantTermsVersion == 0 && contract.GrantTerms == "" {
		return nil, nil
	}
	if contract.GrantTermsVersion != 1 {
		return nil, errSubscriptionGrantTerms
	}
	var terms subscriptionGrantTerms
	if err := common.DecodeJsonStrict(strings.NewReader(contract.GrantTerms), &terms); err != nil {
		return nil, errSubscriptionGrantTerms
	}
	var fields map[string]json.RawMessage
	if err := common.Unmarshal([]byte(contract.GrantTerms), &fields); err != nil {
		return nil, errSubscriptionGrantTerms
	}
	for _, key := range []string{"plan_id", "title", "allowance", "unlimited", "duration_unit", "duration_value", "custom_seconds", "reset_period", "reset_custom_seconds", "allow_wallet_overflow", "max_purchase_per_user", "upgrade_group", "downgrade_group"} {
		value, exists := fields[key]
		if !exists || strings.TrimSpace(string(value)) == "null" {
			return nil, errSubscriptionGrantTerms
		}
	}
	if terms.PlanID != planID || terms.ResetPeriod != NormalizeResetPeriod(terms.ResetPeriod) {
		return nil, errSubscriptionGrantTerms
	}
	plan := &SubscriptionPlan{Id: terms.PlanID, Title: terms.Title, Price: contract.ContractPrice,
		Allowance: terms.Allowance, Unlimited: terms.Unlimited, MoneyVersion: 1,
		DurationUnit: terms.DurationUnit, DurationValue: terms.DurationValue, CustomSeconds: terms.CustomSeconds,
		QuotaResetPeriod: terms.ResetPeriod, QuotaResetCustomSeconds: terms.ResetCustomSeconds,
		AllowWalletOverflow: common.GetPointer(terms.AllowWalletOverflow), MaxPurchasePerUser: terms.MaxPurchasePerUser,
		UpgradeGroup: terms.UpgradeGroup, DowngradeGroup: terms.DowngradeGroup}
	if err := validateSubscriptionGrantPlan(plan); err != nil {
		return nil, errSubscriptionGrantTerms
	}
	return plan, nil
}

// InsertWithContract freezes the price and grant together before remote checkout.
// The server-read plan must still match; stale state never mixes a new grant with
// an old wire price. No legacy order is retrospectively assigned these terms.
func (o *SubscriptionOrder) InsertWithContract(plan *SubscriptionPlan) error {
	if plan == nil || plan.Id != o.PlanId {
		return errSubscriptionGrantTerms
	}
	contract, err := newSubscriptionOrderMoney(plan, o.TradeNo, money.Amount{Currency: SettlementCurrency()}, 0)
	if err != nil {
		return err
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		var current SubscriptionPlan
		if err := lockForUpdate(tx).First(&current, o.PlanId).Error; err != nil {
			return err
		}
		if !current.Enabled {
			return common.NewMessage("Subscription plan is not enabled")
		}
		latest, err := newSubscriptionOrderMoney(&current, o.TradeNo, contract.WalletCharge, 0)
		if err != nil {
			return err
		}
		if latest.ContractPrice != contract.ContractPrice || latest.GrantTerms != contract.GrantTerms ||
			current.StripePriceId != plan.StripePriceId || current.CreemProductId != plan.CreemProductId || current.WaffoPancakeProductId != plan.WaffoPancakeProductId {
			return errSubscriptionGrantTerms
		}
		if o.CreateTime == 0 {
			o.CreateTime = common.GetTimestamp()
		}
		if err := tx.Create(o).Error; err != nil {
			return err
		}
		return tx.Create(contract).Error
	})
}

// Frozen reset terms are shared by lazy, scheduled and administrator resets.
// Historical grants without a contract deliberately retain their original policy.
func subscriptionResetPlanTx(tx *gorm.DB, sub *UserSubscription, fallback *SubscriptionPlan) (*SubscriptionPlan, error) {
	var contracts []SubscriptionOrderMoney
	if err := tx.Where("user_subscription_id = ?", sub.Id).Limit(2).Find(&contracts).Error; err != nil {
		return nil, err
	}
	if len(contracts) > 1 {
		return nil, errSubscriptionGrantTerms
	}
	if len(contracts) == 1 {
		plan, err := contracts[0].grantPlan(sub.PlanId)
		if err != nil {
			return nil, err
		}
		if plan != nil {
			return plan, nil
		}
	}
	return fallback, nil
}

func GetSubscriptionOrderMoney(tradeNo string) (*SubscriptionOrderMoney, error) {
	var value SubscriptionOrderMoney
	if err := DB.Where("trade_no = ?", tradeNo).First(&value).Error; err != nil {
		return nil, err
	}
	return &value, nil
}

func (o *SubscriptionOrder) Update() error {
	return DB.Save(o).Error
}

func GetSubscriptionOrderByTradeNo(tradeNo string) *SubscriptionOrder {
	if tradeNo == "" {
		return nil
	}
	var order SubscriptionOrder
	if err := DB.Where("trade_no = ?", tradeNo).First(&order).Error; err != nil {
		return nil
	}
	return &order
}

// User subscription instance
type UserSubscription struct {
	Id     int `json:"id"`
	UserId int `json:"user_id" gorm:"index;index:idx_user_sub_active,priority:1"`
	PlanId int `json:"plan_id" gorm:"index"`

	// Historical source quota; money consumption belongs to immutable periods.
	AmountTotal int64 `json:"-" gorm:"column:amount_total;->;-:migration"`
	AmountUsed  int64 `json:"-" gorm:"column:amount_used;->;-:migration"`

	Allowance       money.Amount `json:"allowance" gorm:"embedded;embeddedPrefix:allowance_"`
	Unlimited       bool         `json:"unlimited" gorm:"not null;default:false"`
	MoneyVersion    int          `json:"money_version" gorm:"not null;default:0"`
	CurrentPeriodID int64        `json:"current_period_id" gorm:"not null;default:0"`

	StartTime int64  `json:"start_time" gorm:"bigint"`
	EndTime   int64  `json:"end_time" gorm:"bigint;index;index:idx_user_sub_active,priority:3"`
	Status    string `json:"status" gorm:"type:varchar(32);index;index:idx_user_sub_active,priority:2"` // active/expired/cancelled

	Source string `json:"source" gorm:"type:varchar(32);default:'order'"` // order/admin

	LastResetTime int64 `json:"last_reset_time" gorm:"type:bigint;default:0"`
	NextResetTime int64 `json:"next_reset_time" gorm:"type:bigint;default:0;index"`

	UpgradeGroup  string `json:"upgrade_group" gorm:"type:varchar(64);default:''"`
	PrevUserGroup string `json:"prev_user_group" gorm:"type:varchar(64);default:''"`

	// Downgrade target group on expiry (snapshot from plan; empty = revert to PrevUserGroup)
	DowngradeGroup string `json:"downgrade_group" gorm:"type:varchar(64);default:''"`

	// Whether wallet fallback is allowed after this subscription's quota is exhausted (snapshot from plan)
	AllowWalletOverflow bool `json:"allow_wallet_overflow"`

	CreatedAt int64 `json:"created_at" gorm:"bigint"`
	UpdatedAt int64 `json:"updated_at" gorm:"bigint"`
}

func (s *UserSubscription) BeforeCreate(tx *gorm.DB) error {
	now := common.GetTimestamp()
	s.CreatedAt = now
	s.UpdatedAt = now
	return nil
}

func (s *UserSubscription) BeforeUpdate(tx *gorm.DB) error {
	s.UpdatedAt = common.GetTimestamp()
	return nil
}

type SubscriptionSummary struct {
	Subscription                *UserSubscription          `json:"subscription"`
	Period                      *SubscriptionPeriodSummary `json:"period"`
	LegacyReconciliationPending bool                       `json:"legacy_reconciliation_pending"`
}

type SubscriptionResetResult struct {
	PlanId           int    `json:"plan_id"`
	MatchedCount     int    `json:"matched_count"`
	ResetCount       int    `json:"reset_count"`
	UserCount        int    `json:"user_count"`
	AdvanceResetTime bool   `json:"advance_reset_time"`
	PlanTitle        string `json:"-"`
	AffectedUserIds  []int  `json:"-"`
}

func calcPlanEndTime(start time.Time, plan *SubscriptionPlan) (int64, error) {
	if plan == nil {
		return 0, errors.New("plan is nil")
	}
	if plan.DurationValue <= 0 && plan.DurationUnit != SubscriptionDurationCustom {
		return 0, errors.New("duration_value must be > 0")
	}
	switch plan.DurationUnit {
	case SubscriptionDurationYear:
		return start.AddDate(plan.DurationValue, 0, 0).Unix(), nil
	case SubscriptionDurationMonth:
		return start.AddDate(0, plan.DurationValue, 0).Unix(), nil
	case SubscriptionDurationDay:
		return start.Add(time.Duration(plan.DurationValue) * 24 * time.Hour).Unix(), nil
	case SubscriptionDurationHour:
		return start.Add(time.Duration(plan.DurationValue) * time.Hour).Unix(), nil
	case SubscriptionDurationCustom:
		if plan.CustomSeconds <= 0 {
			return 0, errors.New("custom_seconds must be > 0")
		}
		return start.Add(time.Duration(plan.CustomSeconds) * time.Second).Unix(), nil
	default:
		return 0, fmt.Errorf("invalid duration_unit: %s", plan.DurationUnit)
	}
}

func NormalizeResetPeriod(period string) string {
	switch strings.TrimSpace(period) {
	case SubscriptionResetDaily, SubscriptionResetWeekly, SubscriptionResetMonthly, SubscriptionResetCustom:
		return strings.TrimSpace(period)
	default:
		return SubscriptionResetNever
	}
}

func calcNextResetTime(base time.Time, plan *SubscriptionPlan, endUnix int64) int64 {
	if plan == nil {
		return 0
	}
	period := NormalizeResetPeriod(plan.QuotaResetPeriod)
	if period == SubscriptionResetNever {
		return 0
	}
	var next time.Time
	switch period {
	case SubscriptionResetDaily:
		next = time.Date(base.Year(), base.Month(), base.Day(), 0, 0, 0, 0, base.Location()).
			AddDate(0, 0, 1)
	case SubscriptionResetWeekly:
		// Align to next Monday 00:00
		weekday := int(base.Weekday()) // Sunday=0
		// Convert to Monday=1..Sunday=7
		if weekday == 0 {
			weekday = 7
		}
		daysUntil := 8 - weekday
		next = time.Date(base.Year(), base.Month(), base.Day(), 0, 0, 0, 0, base.Location()).
			AddDate(0, 0, daysUntil)
	case SubscriptionResetMonthly:
		// Align to first day of next month 00:00
		next = time.Date(base.Year(), base.Month(), 1, 0, 0, 0, 0, base.Location()).
			AddDate(0, 1, 0)
	case SubscriptionResetCustom:
		if plan.QuotaResetCustomSeconds <= 0 {
			return 0
		}
		next = base.Add(time.Duration(plan.QuotaResetCustomSeconds) * time.Second)
	default:
		return 0
	}
	if endUnix > 0 && next.Unix() > endUnix {
		return 0
	}
	return next.Unix()
}

func GetSubscriptionPlanById(id int) (*SubscriptionPlan, error) {
	return getSubscriptionPlanByIdTx(DB, id)
}

func getSubscriptionPlanByIdTx(tx *gorm.DB, id int) (*SubscriptionPlan, error) {
	if id <= 0 {
		return nil, errors.New("invalid plan id")
	}
	key := subscriptionPlanCacheKey(id)
	if tx == nil && key != "" {
		if cached, found, err := getSubscriptionPlanCache().Get(key); err == nil && found {
			cached.NormalizeDefaults()
			return &cached, nil
		}
	}
	var plan SubscriptionPlan
	query := DB
	if tx != nil {
		query = tx
	}
	if err := query.Where("id = ?", id).First(&plan).Error; err != nil {
		return nil, err
	}
	plan.NormalizeDefaults()
	if tx == nil {
		_ = getSubscriptionPlanCache().SetWithTTL(key, plan, subscriptionPlanCacheTTL())
	}
	return &plan, nil
}

func CountUserSubscriptionsByPlan(userId int, planId int) (int64, error) {
	if userId <= 0 || planId <= 0 {
		return 0, errors.New("invalid userId or planId")
	}
	var count int64
	if err := DB.Model(&UserSubscription{}).
		Where("user_id = ? AND plan_id = ?", userId, planId).
		Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

func getUserGroupByIdTx(tx *gorm.DB, userId int) (string, error) {
	if userId <= 0 {
		return "", errors.New("invalid userId")
	}
	if tx == nil {
		tx = DB
	}
	var group string
	if err := lockForUpdate(tx).Model(&User{}).Where("id = ?", userId).Select(`"group"`).Find(&group).Error; err != nil {
		return "", err
	}
	return group, nil
}

func downgradeUserGroupForSubscriptionTx(tx *gorm.DB, sub *UserSubscription, now int64) (string, error) {
	if tx == nil || sub == nil {
		return "", errors.New("invalid downgrade args")
	}
	downgradeGroup := strings.TrimSpace(sub.DowngradeGroup)
	upgradeGroup := strings.TrimSpace(sub.UpgradeGroup)
	// Nothing to do if neither an explicit downgrade target nor an upgrade snapshot exists.
	if downgradeGroup == "" && upgradeGroup == "" {
		return "", nil
	}
	currentGroup, err := getUserGroupByIdTx(tx, sub.UserId)
	if err != nil {
		return "", err
	}
	// If another active upgraded subscription exists, keep the current group.
	var activeSub UserSubscription
	activeQuery := tx.Where("user_id = ? AND status = ? AND end_time > ? AND id <> ? AND upgrade_group <> ''",
		sub.UserId, "active", now, sub.Id).
		Order("end_time desc, id desc").
		Limit(1).
		Find(&activeSub)
	if activeQuery.Error == nil && activeQuery.RowsAffected > 0 {
		return "", nil
	}
	// Determine the downgrade target: an explicit downgrade group takes precedence,
	// otherwise revert to the group held before purchase (legacy behavior).
	target := downgradeGroup
	if target == "" {
		// Legacy behavior: only revert when the subscription actually elevated the user.
		if currentGroup != upgradeGroup {
			return "", nil
		}
		target = strings.TrimSpace(sub.PrevUserGroup)
	}
	if target == "" || target == currentGroup {
		return "", nil
	}
	if err := tx.Model(&User{}).Where("id = ?", sub.UserId).
		Update("group", target).Error; err != nil {
		return "", err
	}
	return target, nil
}

func CreateUserSubscriptionFromPlanTx(tx *gorm.DB, userId int, plan *SubscriptionPlan, source string) (*UserSubscription, error) {
	if tx == nil {
		return nil, errors.New("tx is nil")
	}
	if plan == nil || plan.Id == 0 {
		return nil, errors.New("invalid plan")
	}
	if err := validateSubscriptionGrantPlan(plan); err != nil {
		return nil, err
	}
	if userId <= 0 {
		return nil, errors.New("invalid user id")
	}
	if plan.MaxPurchasePerUser > 0 {
		var count int64
		if err := tx.Model(&UserSubscription{}).
			Where("user_id = ? AND plan_id = ?", userId, plan.Id).
			Count(&count).Error; err != nil {
			return nil, err
		}
		if count >= int64(plan.MaxPurchasePerUser) {
			return nil, common.NewMessage("Purchase limit for this plan has been reached")
		}
	}
	nowUnix := GetDBTimestamp()
	now := time.Unix(nowUnix, 0)
	endUnix, err := calcPlanEndTime(now, plan)
	if err != nil {
		return nil, err
	}
	resetBase := now
	nextReset := calcNextResetTime(resetBase, plan, endUnix)
	lastReset := int64(0)
	if nextReset > 0 {
		lastReset = now.Unix()
	}
	upgradeGroup := strings.TrimSpace(plan.UpgradeGroup)
	prevGroup := ""
	if upgradeGroup != "" {
		currentGroup, err := getUserGroupByIdTx(tx, userId)
		if err != nil {
			return nil, err
		}
		if currentGroup != upgradeGroup {
			prevGroup = currentGroup
			if err := tx.Model(&User{}).Where("id = ?", userId).
				Update("group", upgradeGroup).Error; err != nil {
				return nil, err
			}
		}
	}
	allowWalletOverflow := true
	if plan.AllowWalletOverflow != nil {
		allowWalletOverflow = *plan.AllowWalletOverflow
	}
	sub := &UserSubscription{
		UserId:              userId,
		PlanId:              plan.Id,
		Allowance:           plan.Allowance,
		Unlimited:           plan.Unlimited,
		MoneyVersion:        1,
		StartTime:           now.Unix(),
		EndTime:             endUnix,
		Status:              "active",
		Source:              source,
		LastResetTime:       lastReset,
		NextResetTime:       nextReset,
		UpgradeGroup:        upgradeGroup,
		PrevUserGroup:       prevGroup,
		DowngradeGroup:      strings.TrimSpace(plan.DowngradeGroup),
		AllowWalletOverflow: allowWalletOverflow,
		CreatedAt:           common.GetTimestamp(),
		UpdatedAt:           common.GetTimestamp(),
	}
	if err := tx.Create(sub).Error; err != nil {
		return nil, err
	}
	if err := createSubscriptionPeriodTx(tx, sub, money.Amount{Currency: sub.Allowance.Currency}, "grant"); err != nil {
		return nil, err
	}
	return sub, nil
}

func refreshSubscriptionUserGroupCache(userId int, operation string) {
	if err := RefreshUserGroupCache(userId); err != nil {
		common.SysError(common.LogText("failed to refresh user group cache after %s for user %d: %v", operation, userId, err))
	}
}

// Complete a subscription order (idempotent). Creates a UserSubscription snapshot from the plan.
// expectedPaymentProvider guards against cross-gateway callback attacks (empty skips the check).
// actualPaymentMethod updates the order's PaymentMethod to reflect the real payment type used (empty skips update).
func CompleteSubscriptionOrder(tradeNo string, providerPayload string, expectedPaymentProvider string, actualPaymentMethod string) error {
	if tradeNo == "" {
		return errors.New("tradeNo is empty")
	}
	var logUserId int
	var logPlanTitle string
	var logPaymentMethod string
	var upgradeGroup string
	err := DB.Transaction(func(tx *gorm.DB) error {
		var order SubscriptionOrder
		if err := lockForUpdate(tx).Where(`"trade_no" = ?`, tradeNo).First(&order).Error; err != nil {
			return ErrSubscriptionOrderNotFound
		}
		if expectedPaymentProvider != "" && order.PaymentProvider != expectedPaymentProvider {
			return ErrPaymentMethodMismatch
		}
		if order.Status == common.TopUpStatusSuccess {
			return nil
		}
		if order.Status != common.TopUpStatusPending {
			return ErrSubscriptionOrderStatusInvalid
		}
		var contract SubscriptionOrderMoney
		var plan *SubscriptionPlan
		contractErr := tx.Where("trade_no = ?", order.TradeNo).First(&contract).Error
		if contractErr == nil {
			var err error
			plan, err = contract.grantPlan(order.PlanId)
			if err != nil {
				return err
			}
		} else if !errors.Is(contractErr, gorm.ErrRecordNotFound) {
			return contractErr
		}
		if plan == nil {
			var err error
			plan, err = getSubscriptionPlanByIdTx(tx, order.PlanId)
			if err != nil {
				return err
			}
		}
		if err := lockSubscriptionLifecycleUsersTx(tx, []int{order.UserId}); err != nil {
			return err
		}
		// 锁定用户行：并发完成同一用户的不同订单（包括多实例部署下）时，
		// 使 CreateUserSubscriptionFromPlanTx 的 MaxPurchasePerUser 检查按用户串行。
		var userRow User
		if err := lockForUpdate(tx).Select("id").Where("id = ?", order.UserId).First(&userRow).Error; err != nil {
			return err
		}
		subscription, err := CreateUserSubscriptionFromPlanTx(tx, order.UserId, plan, "order")
		if err != nil {
			return err
		}
		if subscription.PrevUserGroup != "" {
			upgradeGroup = strings.TrimSpace(subscription.UpgradeGroup)
		}
		if err := tx.Model(&SubscriptionOrderMoney{}).Where("trade_no = ?", tradeNo).Update("user_subscription_id", subscription.Id).Error; err != nil {
			return err
		}
		if err := upsertSubscriptionTopUpTx(tx, &order); err != nil {
			return err
		}
		order.Status = common.TopUpStatusSuccess
		order.CompleteTime = common.GetTimestamp()
		if actualPaymentMethod != "" && order.PaymentMethod != actualPaymentMethod {
			order.PaymentMethod = actualPaymentMethod
		}
		if err := tx.Save(&order).Error; err != nil {
			return err
		}
		logUserId = order.UserId
		logPlanTitle = plan.Title
		logPaymentMethod = order.PaymentMethod
		return nil
	})
	if err != nil {
		return err
	}
	if upgradeGroup != "" && logUserId > 0 {
		refreshSubscriptionUserGroupCache(logUserId, common.LogText("subscription payment completion"))
	}
	if logUserId > 0 {
		RecordLog(logUserId, LogTypeTopup, common.NewMessage("Subscription activated, plan: {{plan}}, payment method: {{method}}", map[string]any{"plan": logPlanTitle, "method": logPaymentMethod}))
	}
	return nil
}

func upsertSubscriptionTopUpTx(tx *gorm.DB, order *SubscriptionOrder) error {
	if tx == nil || order == nil {
		return errors.New("invalid subscription order")
	}
	var contracts int64
	if err := tx.Model(&SubscriptionOrderMoney{}).Where("trade_no = ?", order.TradeNo).Count(&contracts).Error; err != nil {
		return err
	}
	now := common.GetTimestamp()
	var topup TopUp
	if err := tx.Where("trade_no = ?", order.TradeNo).First(&topup).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			topup = TopUp{
				UserId:        order.UserId,
				Amount:        0,
				Money:         order.Money,
				TradeNo:       order.TradeNo,
				PaymentMethod: order.PaymentMethod,
				CreateTime:    order.CreateTime,
				CompleteTime:  now,
				Status:        common.TopUpStatusSuccess,
			}
			if contracts > 0 {
				topup.ContractVersion = CheckoutContractVersion
				topup.Purpose = "subscription"
				return tx.Omit("Amount", "Money").Create(&topup).Error
			}
			return tx.Create(&topup).Error
		}
		return err
	}
	topup.Money = order.Money
	if topup.PaymentMethod == "" {
		topup.PaymentMethod = order.PaymentMethod
	} else if topup.PaymentMethod != order.PaymentMethod {
		return ErrPaymentMethodMismatch
	}
	if topup.CreateTime == 0 {
		topup.CreateTime = order.CreateTime
	}
	topup.CompleteTime = now
	topup.Status = common.TopUpStatusSuccess
	return tx.Omit("Amount", "Money").Save(&topup).Error
}

func ExpireSubscriptionOrder(tradeNo string, expectedPaymentProvider string) error {
	if tradeNo == "" {
		return errors.New("tradeNo is empty")
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		var order SubscriptionOrder
		if err := lockForUpdate(tx).Where(`"trade_no" = ?`, tradeNo).First(&order).Error; err != nil {
			return ErrSubscriptionOrderNotFound
		}
		if expectedPaymentProvider != "" && order.PaymentProvider != expectedPaymentProvider {
			return ErrPaymentMethodMismatch
		}
		if order.Status != common.TopUpStatusPending {
			return nil
		}
		order.Status = common.TopUpStatusExpired
		order.CompleteTime = common.GetTimestamp()
		return tx.Save(&order).Error
	})
}

// Admin bind (no payment). Creates a UserSubscription from a plan.
func AdminBindSubscription(userId int, planId int, sourceNote string) (*common.Message, error) {
	if userId <= 0 || planId <= 0 {
		return nil, errors.New("invalid userId or planId")
	}
	var plan *SubscriptionPlan
	groupChanged := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		var err error
		plan, err = getSubscriptionPlanByIdTx(tx, planId)
		if err != nil {
			return err
		}
		if err := lockSubscriptionLifecycleUsersTx(tx, []int{userId}); err != nil {
			return err
		}
		// 与 CompleteSubscriptionOrder 一致：先锁用户行，再做购买次数检查。
		var userRow User
		if err := lockForUpdate(tx).Select("id").Where("id = ?", userId).First(&userRow).Error; err != nil {
			return err
		}
		subscription, err := CreateUserSubscriptionFromPlanTx(tx, userId, plan, "admin")
		if err == nil {
			groupChanged = subscription.PrevUserGroup != ""
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if groupChanged {
		refreshSubscriptionUserGroupCache(userId, common.LogText("admin subscription creation"))
		return common.NewMessage("User group will be upgraded to {{group}}", map[string]any{"group": plan.UpgradeGroup}), nil
	}
	return nil, nil
}

var ErrSubscriptionPurchaseConflict = common.NewMessage("Purchase ID belongs to another subscription plan")
var ErrSubscriptionPurchaseTermsChanged = common.NewMessage("Subscription purchase terms have changed. Review the plan and confirm again.")
var ErrSubscriptionPurchaseRevisionConflict = common.NewMessage("Purchase ID belongs to different subscription terms")

type SubscriptionPurchaseResult struct {
	PurchaseRevision string                     `json:"purchase_revision"`
	OrderID          string                     `json:"order_id"`
	Subscription     *UserSubscription          `json:"subscription"`
	Period           *SubscriptionPeriodSummary `json:"period"`
	ContractPrice    money.Amount               `json:"contract_price"`
	WalletCharge     money.Amount               `json:"wallet_charge"`
	FXRate           decimal.NullDecimal        `json:"fx_rate"`
}

// PurchaseSubscriptionWithBalance atomically exchanges wallet cash for one grant.
// Purchase never recognizes request revenue and never reads legacy quota or floats.
func PurchaseSubscriptionWithBalance(ctx context.Context, userId, planId int, purchaseID, expectedRevision string) (*SubscriptionPurchaseResult, error) {
	id, err := uuid.Parse(purchaseID)
	if userId <= 0 || planId <= 0 || err != nil || len(purchaseID) != 36 || id == uuid.Nil {
		return nil, common.NewMessage("Invalid parameters")
	}
	revisionDigest, revisionErr := hex.DecodeString(strings.TrimPrefix(expectedRevision, "v1:"))
	if !strings.HasPrefix(expectedRevision, "v1:") || revisionErr != nil || len(revisionDigest) != sha256.Size || expectedRevision != strings.ToLower(expectedRevision) {
		return nil, common.NewMessage("Invalid parameters")
	}
	digest := sha256.Sum256([]byte(strconv.Itoa(userId) + ":" + id.String()))
	identity := hex.EncodeToString(digest[:])
	tradeNo := "SUBBAL-" + identity
	result := &SubscriptionPurchaseResult{OrderID: tradeNo}
	var logPlanTitle, upgradeGroup string
	created := false
	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		wallets, err := lockSubscriptionFundingWallets(tx, []int{userId})
		if err != nil {
			return err
		}
		var user User
		if err := lockForUpdate(tx).Where("id = ? AND status = ?", userId, common.UserStatusEnabled).First(&user).Error; err != nil {
			return err
		}
		var previous SubscriptionOrder
		err = tx.Where("trade_no = ?", tradeNo).First(&previous).Error
		if err == nil {
			if previous.UserId != userId || previous.PlanId != planId {
				return ErrSubscriptionPurchaseConflict
			}
			if previous.PaymentProvider != PaymentProviderBalance || previous.Status != common.TopUpStatusSuccess {
				return ErrSubscriptionOrderStatusInvalid
			}
			var contract SubscriptionOrderMoney
			if err := tx.Where("trade_no = ?", tradeNo).First(&contract).Error; err != nil {
				return err
			}
			acceptedPlan, err := contract.grantPlan(planId)
			if err != nil {
				return err
			}
			acceptedRevision, err := SubscriptionPurchaseRevision(acceptedPlan)
			if err != nil {
				return err
			}
			if expectedRevision != acceptedRevision {
				return ErrSubscriptionPurchaseRevisionConflict
			}
			result.PurchaseRevision = acceptedRevision
			result.ContractPrice, result.WalletCharge, result.FXRate = contract.ContractPrice, contract.WalletCharge, contract.FXRate
			result.Subscription = &UserSubscription{}
			return tx.Where("id = ? AND user_id = ?", contract.UserSubscriptionID, userId).First(result.Subscription).Error
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		// Lock the authoritative plan through acceptance: an administrator cannot
		// change the displayed promise between the comparison and its frozen grant.
		plan := &SubscriptionPlan{}
		if err := lockForUpdate(tx).First(plan, planId).Error; err != nil {
			return err
		}
		currentRevision, err := SubscriptionPurchaseRevision(plan)
		if err != nil {
			return err
		}
		if expectedRevision != currentRevision {
			return ErrSubscriptionPurchaseTermsChanged
		}
		result.PurchaseRevision = currentRevision
		if !plan.Enabled {
			return common.NewMessage("Subscription plan is not enabled")
		}
		if err := plan.ValidateMoney(); err != nil {
			return err
		}
		if plan.Price.Currency != SettlementCurrency() {
			return common.NewMessage("Subscription price currency does not match wallet currency")
		}
		if plan.AllowBalancePay != nil && !*plan.AllowBalancePay {
			return common.NewMessage("This plan does not allow balance redemption")
		}

		var reminderBefore *balanceReminderObservation
		if plan.Price.Nanos > 0 {
			wallet := wallets[UserWalletAccount(userId)]
			if wallet == nil || wallet.RequestsPaused || wallet.Balance.Nanos < plan.Price.Nanos {
				return common.NewMessage("Insufficient balance")
			}
			reminderBefore, err = observeBalanceReminder(tx, userId, UserWalletAccount(userId))
			if err != nil {
				return err
			}
			posting := LedgerPosting{Account: UserWalletAccount(userId), Type: LedgerEntryCharge, Amount: money.Amount{Nanos: -plan.Price.Nanos, Currency: plan.Price.Currency}, IdempotencyKey: "subscription:purchase:" + identity, RequestId: identity, Remark: "subscription purchase"}
			if err := validateLedgerPosting(posting); err != nil {
				return err
			}
			if _, err := postLedgerEntries(tx, []LedgerPosting{posting}); err != nil {
				return err
			}
		}

		subscription, err := CreateUserSubscriptionFromPlanTx(tx, userId, plan, PaymentMethodBalance)
		if err != nil {
			return err
		}

		now := common.GetTimestamp()
		order := &SubscriptionOrder{
			UserId:          userId,
			PlanId:          plan.Id,
			TradeNo:         tradeNo,
			PaymentMethod:   PaymentMethodBalance,
			PaymentProvider: PaymentProviderBalance,
			Status:          common.TopUpStatusSuccess,
			CreateTime:      now,
			CompleteTime:    now,
		}
		if err := tx.Create(order).Error; err != nil {
			return err
		}

		contract, err := newSubscriptionOrderMoney(plan, tradeNo, plan.Price, subscription.Id)
		if err != nil {
			return err
		}
		if err := tx.Create(contract).Error; err != nil {
			return err
		}
		if err := enqueueBalanceReminder(tx, reminderBefore, UserWalletAccount(userId), "subscription:purchase:"+identity); err != nil {
			return err
		}
		result.Subscription, result.ContractPrice, result.WalletCharge = subscription, plan.Price, plan.Price
		created = true
		logPlanTitle = plan.Title
		if subscription.PrevUserGroup != "" {
			upgradeGroup = strings.TrimSpace(subscription.UpgradeGroup)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if upgradeGroup != "" {
		refreshSubscriptionUserGroupCache(userId, common.LogText("subscription balance purchase"))
	}
	if created {
		RecordLog(userId, LogTypeTopup, common.NewMessage("Subscription purchased, plan: {{plan}}, payment amount: {{amount}}, payment method: {{method}}", map[string]any{
			"plan": logPlanTitle, "amount": decimal.NewFromInt(result.WalletCharge.Nanos).Shift(-9).String() + " " + string(result.WalletCharge.Currency), "method": PaymentMethodBalance,
		}))
	}
	result.Period, err = GetSubscriptionPeriodSummary(ctx, result.Subscription.Id)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// GetAllActiveUserSubscriptions returns all active subscriptions for a user.
func GetAllActiveUserSubscriptions(userId int) ([]SubscriptionSummary, error) {
	if userId <= 0 {
		return nil, errors.New("invalid userId")
	}
	now := common.GetTimestamp()
	var subs []UserSubscription
	err := DB.Where("user_id = ? AND status = ? AND end_time > ?", userId, "active", now).
		Order("end_time desc, id desc").
		Find(&subs).Error
	if err != nil {
		return nil, err
	}
	return buildSubscriptionSummaries(subs)
}

// HasActiveUserSubscription returns whether the user has any active subscription.
// This is a lightweight existence check to avoid heavy pre-consume transactions.
func HasActiveUserSubscription(userId int) (bool, error) {
	if userId <= 0 {
		return false, errors.New("invalid userId")
	}
	now := common.GetTimestamp()
	var count int64
	if err := DB.Model(&UserSubscription{}).
		Where("user_id = ? AND status = ? AND end_time > ?", userId, "active", now).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// UserActiveSubscriptionsAllowWalletOverflow returns whether wallet balance may be used
// after the user's subscription quota is exhausted. A single active subscription that
// disallows wallet overflow (allow_wallet_overflow = false) blocks the fallback.
func UserActiveSubscriptionsAllowWalletOverflow(userId int) (bool, error) {
	if userId <= 0 {
		return false, errors.New("invalid userId")
	}
	now := common.GetTimestamp()
	var strictCount int64
	if err := DB.Model(&UserSubscription{}).
		Where("user_id = ? AND status = ? AND end_time > ? AND allow_wallet_overflow = ?",
			userId, "active", now, false).
		Count(&strictCount).Error; err != nil {
		return false, err
	}
	return strictCount == 0, nil
}

// GetAllUserSubscriptions returns all subscriptions (active and expired) for a user.
func GetAllUserSubscriptions(userId int) ([]SubscriptionSummary, error) {
	if userId <= 0 {
		return nil, errors.New("invalid userId")
	}
	var subs []UserSubscription
	err := DB.Where("user_id = ?", userId).
		Order("end_time desc, id desc").
		Find(&subs).Error
	if err != nil {
		return nil, err
	}
	return buildSubscriptionSummaries(subs)
}

func buildSubscriptionSummaries(subs []UserSubscription) ([]SubscriptionSummary, error) {
	ids := make([]int, 0, len(subs))
	for _, sub := range subs {
		ids = append(ids, sub.Id)
	}
	pending, err := GetLegacySubscriptionReconciliationPending(DB, ids)
	if err != nil {
		return nil, err
	}
	result := make([]SubscriptionSummary, 0, len(subs))
	for _, sub := range subs {
		subCopy := sub
		period, err := GetSubscriptionPeriodSummary(context.Background(), sub.Id)
		if err != nil {
			return nil, err
		}
		result = append(result, SubscriptionSummary{Subscription: &subCopy, Period: period, LegacyReconciliationPending: pending[sub.Id]})
	}
	return result, nil
}

// All lifecycle writers acquire existing funding accounts before user and grant
// rows, in the same order as gateway settlement and wallet purchases.
func lockSubscriptionLifecycleUsersTx(tx *gorm.DB, userIDs []int) error {
	if len(userIDs) == 0 {
		return nil
	}
	if _, err := lockSubscriptionFundingWallets(tx, userIDs); err != nil {
		return err
	}
	var users []User
	return lockForUpdate(tx.Unscoped()).Select("id").Where("id IN ?", userIDs).Order("id asc").Find(&users).Error
}

func lockSubscriptionLifecycleTx(tx *gorm.DB, id int) (*UserSubscription, error) {
	var owner UserSubscription
	if err := tx.Select("user_id").Where("id = ?", id).First(&owner).Error; err != nil {
		return nil, err
	}
	if err := lockSubscriptionLifecycleUsersTx(tx, []int{owner.UserId}); err != nil {
		return nil, err
	}
	var sub UserSubscription
	if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", id, owner.UserId).First(&sub).Error; err != nil {
		return nil, err
	}
	return &sub, nil
}

// AdminInvalidateUserSubscription marks a user subscription as cancelled and ends it immediately.
func AdminInvalidateUserSubscription(userSubscriptionId int) (*common.Message, error) {
	if userSubscriptionId <= 0 {
		return nil, errors.New("invalid userSubscriptionId")
	}
	now := common.GetTimestamp()
	cacheGroup := ""
	downgradeGroup := ""
	var userId int
	err := DB.Transaction(func(tx *gorm.DB) error {
		sub, err := lockSubscriptionLifecycleTx(tx, userSubscriptionId)
		if err != nil {
			return err
		}
		userId = sub.UserId
		if err := tx.Model(&sub).Updates(map[string]any{
			"status":     "cancelled",
			"end_time":   now,
			"updated_at": now,
		}).Error; err != nil {
			return err
		}
		target, err := downgradeUserGroupForSubscriptionTx(tx, sub, now)
		if err != nil {
			return err
		}
		if target != "" {
			cacheGroup = target
			downgradeGroup = target
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if cacheGroup != "" && userId > 0 {
		refreshSubscriptionUserGroupCache(userId, common.LogText("admin subscription update"))
	}
	if downgradeGroup != "" {
		return common.NewMessage("User group will be reverted to {{group}}", map[string]any{"group": downgradeGroup}), nil
	}
	return nil, nil
}

// AdminDeleteUserSubscription hard-deletes a user subscription.
func AdminDeleteUserSubscription(userSubscriptionId int) (*common.Message, error) {
	if userSubscriptionId <= 0 {
		return nil, errors.New("invalid userSubscriptionId")
	}
	now := common.GetTimestamp()
	cacheGroup := ""
	downgradeGroup := ""
	var userId int
	err := DB.Transaction(func(tx *gorm.DB) error {
		sub, err := lockSubscriptionLifecycleTx(tx, userSubscriptionId)
		if err != nil {
			return err
		}
		if sub.MoneyVersion == 1 || sub.CurrentPeriodID != 0 {
			return common.NewMessage("Subscription financial history cannot be deleted")
		}
		userId = sub.UserId
		target, err := downgradeUserGroupForSubscriptionTx(tx, sub, now)
		if err != nil {
			return err
		}
		if target != "" {
			cacheGroup = target
			downgradeGroup = target
		}
		if err := tx.Where("id = ?", userSubscriptionId).Delete(&UserSubscription{}).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if cacheGroup != "" && userId > 0 {
		refreshSubscriptionUserGroupCache(userId, common.LogText("admin subscription deletion"))
	}
	if downgradeGroup != "" {
		return common.NewMessage("User group will be reverted to {{group}}", map[string]any{"group": downgradeGroup}), nil
	}
	return nil, nil
}

func resetUserSubscriptionTx(tx *gorm.DB, sub *UserSubscription, plan *SubscriptionPlan, now int64, advanceResetTime bool) error {
	if tx == nil || sub == nil || plan == nil {
		return errors.New("invalid reset args")
	}
	var err error
	plan, err = subscriptionResetPlanTx(tx, sub, plan)
	if err != nil {
		return err
	}

	if err := createSubscriptionPeriodTx(tx, sub, money.Amount{Currency: sub.Allowance.Currency}, "reset"); err != nil {
		return err
	}
	if advanceResetTime {
		nextReset := calcNextResetTime(time.Unix(now, 0), plan, sub.EndTime)
		sub.NextResetTime = nextReset
		if nextReset > 0 {
			sub.LastResetTime = now
		} else {
			sub.LastResetTime = 0
		}
	}
	return tx.Save(sub).Error
}

func buildSubscriptionResetResult(plan *SubscriptionPlan, subs []UserSubscription, advanceResetTime bool) *SubscriptionResetResult {
	userIds := make([]int, 0, len(subs))
	seenUsers := make(map[int]struct{}, len(subs))
	for _, sub := range subs {
		if _, ok := seenUsers[sub.UserId]; ok {
			continue
		}
		seenUsers[sub.UserId] = struct{}{}
		userIds = append(userIds, sub.UserId)
	}
	return &SubscriptionResetResult{
		PlanId:           plan.Id,
		MatchedCount:     len(subs),
		ResetCount:       len(subs),
		UserCount:        len(userIds),
		AdvanceResetTime: advanceResetTime,
		PlanTitle:        plan.Title,
		AffectedUserIds:  userIds,
	}
}

func adminResetUserSubscriptionsByPlanTx(tx *gorm.DB, userId int, plan *SubscriptionPlan, now int64, advanceResetTime bool) (*SubscriptionResetResult, error) {
	if tx == nil || plan == nil {
		return nil, errors.New("invalid reset args")
	}
	if err := lockSubscriptionLifecycleUsersTx(tx, []int{userId}); err != nil {
		return nil, err
	}
	var subs []UserSubscription
	if err := lockForUpdate(tx).
		Where("user_id = ? AND plan_id = ? AND status = ? AND end_time > ?", userId, plan.Id, "active", now).
		Order("end_time asc, id asc").
		Find(&subs).Error; err != nil {
		return nil, err
	}
	if len(subs) == 0 {
		return nil, common.NewMessage("This user has no active subscription to this plan")
	}
	for i := range subs {
		if err := resetUserSubscriptionTx(tx, &subs[i], plan, now, advanceResetTime); err != nil {
			return nil, err
		}
	}
	return buildSubscriptionResetResult(plan, subs, advanceResetTime), nil
}

func adminResetPlanSubscriptionsTx(tx *gorm.DB, plan *SubscriptionPlan, now int64, advanceResetTime bool) (*SubscriptionResetResult, error) {
	if tx == nil || plan == nil {
		return nil, errors.New("invalid reset args")
	}
	var userIDs []int
	if err := tx.Model(&UserSubscription{}).Where("plan_id = ? AND status = ? AND end_time > ?", plan.Id, "active", now).Distinct("user_id").Pluck("user_id", &userIDs).Error; err != nil {
		return nil, err
	}
	if len(userIDs) == 0 {
		return buildSubscriptionResetResult(plan, nil, advanceResetTime), nil
	}
	if err := lockSubscriptionLifecycleUsersTx(tx, userIDs); err != nil {
		return nil, err
	}
	var subs []UserSubscription
	if err := lockForUpdate(tx).
		Where("plan_id = ? AND status = ? AND end_time > ? AND user_id IN ?", plan.Id, "active", now, userIDs).
		Order("user_id asc, end_time asc, id asc").
		Find(&subs).Error; err != nil {
		return nil, err
	}
	for i := range subs {
		if err := resetUserSubscriptionTx(tx, &subs[i], plan, now, advanceResetTime); err != nil {
			return nil, err
		}
	}
	return buildSubscriptionResetResult(plan, subs, advanceResetTime), nil
}

func AdminResetUserSubscriptionsByPlan(userId int, planId int, advanceResetTime bool) (*SubscriptionResetResult, error) {
	if userId <= 0 || planId <= 0 {
		return nil, errors.New("invalid userId or planId")
	}
	var result *SubscriptionResetResult
	now := GetDBTimestamp()
	err := DB.Transaction(func(tx *gorm.DB) error {
		plan, err := getSubscriptionPlanByIdTx(tx, planId)
		if err != nil {
			return err
		}
		result, err = adminResetUserSubscriptionsByPlanTx(tx, userId, plan, now, advanceResetTime)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func AdminResetPlanSubscriptions(planId int, advanceResetTime bool) (*SubscriptionResetResult, error) {
	if planId <= 0 {
		return nil, errors.New("invalid planId")
	}
	var result *SubscriptionResetResult
	now := GetDBTimestamp()
	err := DB.Transaction(func(tx *gorm.DB) error {
		plan, err := getSubscriptionPlanByIdTx(tx, planId)
		if err != nil {
			return err
		}
		result, err = adminResetPlanSubscriptionsTx(tx, plan, now, advanceResetTime)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

type SubscriptionPreConsumeResult struct {
	UserSubscriptionId int
	PreConsumed        int64
	AmountTotal        int64
	AmountUsedBefore   int64
	AmountUsedAfter    int64
}

// ExpireDueSubscriptions marks expired subscriptions and handles group downgrade.
func ExpireDueSubscriptions(limit int) (int, error) {
	if limit <= 0 {
		limit = 200
	}
	now := GetDBTimestamp()
	var subs []UserSubscription
	if err := DB.Where("status = ? AND end_time > 0 AND end_time <= ?", "active", now).
		Order("end_time asc, id asc").
		Limit(limit).
		Find(&subs).Error; err != nil {
		return 0, err
	}
	if len(subs) == 0 {
		return 0, nil
	}
	expiredCount := 0
	userIds := make(map[int]struct{}, len(subs))
	for _, sub := range subs {
		if sub.UserId > 0 {
			userIds[sub.UserId] = struct{}{}
		}
	}
	for userId := range userIds {
		cacheGroup := ""
		err := DB.Transaction(func(tx *gorm.DB) error {
			if err := lockSubscriptionLifecycleUsersTx(tx, []int{userId}); err != nil {
				return err
			}
			res := tx.Model(&UserSubscription{}).
				Where("user_id = ? AND status = ? AND end_time > 0 AND end_time <= ?", userId, "active", now).
				Updates(map[string]any{
					"status":     "expired",
					"updated_at": common.GetTimestamp(),
				})
			if res.Error != nil {
				return res.Error
			}
			expiredCount += int(res.RowsAffected)

			// If there's an active upgraded subscription, keep current group.
			var activeSub UserSubscription
			activeQuery := tx.Where("user_id = ? AND status = ? AND end_time > ? AND upgrade_group <> ''",
				userId, "active", now).
				Order("end_time desc, id desc").
				Limit(1).
				Find(&activeSub)
			if activeQuery.Error == nil && activeQuery.RowsAffected > 0 {
				return nil
			}

			// Find the most recently expired subscription that defines a group transition
			// (an explicit downgrade target or an upgrade snapshot to revert).
			var lastExpired UserSubscription
			expiredQuery := tx.Where("user_id = ? AND status = ? AND (downgrade_group <> '' OR upgrade_group <> '')",
				userId, "expired").
				Order("end_time desc, id desc").
				Limit(1).
				Find(&lastExpired)
			if expiredQuery.Error != nil || expiredQuery.RowsAffected == 0 {
				return nil
			}
			currentGroup, err := getUserGroupByIdTx(tx, userId)
			if err != nil {
				return err
			}
			// An explicit downgrade group takes precedence; otherwise revert to the
			// group held before purchase (legacy behavior, only when the subscription
			// actually elevated the user).
			target := strings.TrimSpace(lastExpired.DowngradeGroup)
			if target == "" {
				upgradeGroup := strings.TrimSpace(lastExpired.UpgradeGroup)
				prevGroup := strings.TrimSpace(lastExpired.PrevUserGroup)
				if upgradeGroup == "" || prevGroup == "" {
					return nil
				}
				if currentGroup != upgradeGroup {
					return nil
				}
				target = prevGroup
			}
			if target == "" || target == currentGroup {
				return nil
			}
			if err := tx.Model(&User{}).Where("id = ?", userId).
				Update("group", target).Error; err != nil {
				return err
			}
			cacheGroup = target
			return nil
		})
		if err != nil {
			return expiredCount, err
		}
		if cacheGroup != "" {
			refreshSubscriptionUserGroupCache(userId, common.LogText("subscription expiration"))
		}
	}
	return expiredCount, nil
}

// SubscriptionPreConsumeRecord stores idempotent pre-consume operations per request.
type SubscriptionPreConsumeRecord struct {
	Id                 int    `json:"id"`
	RequestId          string `json:"request_id" gorm:"type:varchar(64);uniqueIndex"`
	UserId             int    `json:"user_id" gorm:"index"`
	UserSubscriptionId int    `json:"user_subscription_id" gorm:"index"`
	PreConsumed        int64  `json:"pre_consumed" gorm:"type:bigint;not null;default:0"`
	Status             string `json:"status" gorm:"type:varchar(32);index"` // consumed/refunded
	CreatedAt          int64  `json:"created_at" gorm:"bigint"`
	UpdatedAt          int64  `json:"updated_at" gorm:"bigint;index"`
}

func (r *SubscriptionPreConsumeRecord) BeforeCreate(tx *gorm.DB) error {
	now := common.GetTimestamp()
	r.CreatedAt = now
	r.UpdatedAt = now
	return nil
}

func (r *SubscriptionPreConsumeRecord) BeforeUpdate(tx *gorm.DB) error {
	r.UpdatedAt = common.GetTimestamp()
	return nil
}

func maybeResetUserSubscriptionWithPlanTx(tx *gorm.DB, sub *UserSubscription, plan *SubscriptionPlan, now int64) error {
	if tx == nil || sub == nil || plan == nil {
		return errors.New("invalid reset args")
	}
	var err error
	plan, err = subscriptionResetPlanTx(tx, sub, plan)
	if err != nil {
		return err
	}

	if sub.NextResetTime > 0 && sub.NextResetTime > now {
		return nil
	}
	if NormalizeResetPeriod(plan.QuotaResetPeriod) == SubscriptionResetNever {
		return nil
	}
	baseUnix := sub.LastResetTime
	if baseUnix <= 0 {
		baseUnix = sub.StartTime
	}
	base := time.Unix(baseUnix, 0)
	next := calcNextResetTime(base, plan, sub.EndTime)
	advanced := false
	for next > 0 && next <= now {
		advanced = true
		base = time.Unix(next, 0)
		next = calcNextResetTime(base, plan, sub.EndTime)
	}
	if !advanced {
		if sub.NextResetTime == 0 && next > 0 {
			sub.NextResetTime = next
			sub.LastResetTime = base.Unix()
			return tx.Save(sub).Error
		}
		return nil
	}
	if err := createSubscriptionPeriodTx(tx, sub, money.Amount{Currency: sub.Allowance.Currency}, "reset"); err != nil {
		return err
	}
	sub.LastResetTime = base.Unix()
	sub.NextResetTime = next
	return tx.Save(sub).Error
}

// Legacy quota records are retained as evidence only. New consumption and
// recovery must use the exact funding obligation, never reinterpret old units.
var ErrLegacySubscriptionQuota = common.NewMessage("Legacy subscription quota operations are unavailable")

func PreConsumeUserSubscription(requestId string, userId int, modelName string, quotaType int, amount int64) (*SubscriptionPreConsumeResult, error) {
	return nil, ErrLegacySubscriptionQuota
}

func RefundSubscriptionPreConsume(requestId string) error {
	return ErrLegacySubscriptionQuota
}

// ResetDueSubscriptions resets subscriptions whose next_reset_time has passed.
func ResetDueSubscriptions(limit int) (int, error) {
	if limit <= 0 {
		limit = 200
	}
	now := GetDBTimestamp()
	var subs []UserSubscription
	if err := DB.Where("next_reset_time > 0 AND next_reset_time <= ? AND status = ?", now, "active").
		Order("next_reset_time asc").
		Limit(limit).
		Find(&subs).Error; err != nil {
		return 0, err
	}
	if len(subs) == 0 {
		return 0, nil
	}
	resetCount := 0
	for _, sub := range subs {
		subCopy := sub
		err := DB.Transaction(func(tx *gorm.DB) error {
			locked, err := lockSubscriptionLifecycleTx(tx, subCopy.Id)
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if locked.Status != "active" || locked.NextResetTime <= 0 || locked.NextResetTime > now {
				return nil
			}
			plan, err := getSubscriptionPlanByIdTx(tx, locked.PlanId)
			if err != nil {
				return err
			}
			if err := maybeResetUserSubscriptionWithPlanTx(tx, locked, plan, now); err != nil {
				return err
			}
			resetCount++
			return nil
		})
		if err != nil {
			return resetCount, err
		}
	}
	return resetCount, nil
}

// CleanupSubscriptionPreConsumeRecords preserves archived idempotency evidence.
func CleanupSubscriptionPreConsumeRecords(olderThanSeconds int64) (int64, error) {
	return 0, nil
}

type SubscriptionPlanInfo struct {
	PlanId    int
	PlanTitle string
}

func GetSubscriptionPlanInfoByUserSubscriptionId(userSubscriptionId int) (*SubscriptionPlanInfo, error) {
	if userSubscriptionId <= 0 {
		return nil, errors.New("invalid userSubscriptionId")
	}
	cacheKey := fmt.Sprintf("sub:%d", userSubscriptionId)
	if cached, found, err := getSubscriptionPlanInfoCache().Get(cacheKey); err == nil && found {
		return &cached, nil
	}
	var sub UserSubscription
	if err := DB.Where("id = ?", userSubscriptionId).First(&sub).Error; err != nil {
		return nil, err
	}
	plan, err := getSubscriptionPlanByIdTx(nil, sub.PlanId)
	if err != nil {
		return nil, err
	}
	info := &SubscriptionPlanInfo{
		PlanId:    sub.PlanId,
		PlanTitle: plan.Title,
	}
	_ = getSubscriptionPlanInfoCache().SetWithTTL(cacheKey, *info, subscriptionPlanInfoCacheTTL())
	return info, nil
}

// PostConsumeUserSubscriptionDelta is the retired quota-unit mutation seam.
func PostConsumeUserSubscriptionDelta(userSubscriptionId int, delta int64) error {
	return ErrLegacySubscriptionQuota
}
