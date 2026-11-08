package model

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/money"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// GatewayUsage contains measured quantities only. Provider extensions and raw
// response data never enter request records.
type GatewayUsage struct {
	PromptTokens           int64 `json:"prompt_tokens"`
	CompletionTokens       int64 `json:"completion_tokens"`
	TotalTokens            int64 `json:"total_tokens"`
	CachedTokens           int64 `json:"cached_tokens,omitempty"`
	CacheCreationTokens    int64 `json:"cache_creation_tokens,omitempty"`
	CacheCreation12hTokens int64 `json:"cache_creation_12h_tokens,omitempty"`
	ImageInputTokens       int64 `json:"image_input_tokens,omitempty"`
	AudioInputTokens       int64 `json:"audio_input_tokens,omitempty"`
	ImageOutputTokens      int64 `json:"image_output_tokens,omitempty"`
	AudioOutputTokens      int64 `json:"audio_output_tokens,omitempty"`
}

type GatewayRequest struct {
	SalePending        bool           `json:"sale_pending,omitempty" gorm:"-"`
	ID                 string         `json:"id" gorm:"type:varchar(64);primaryKey"`
	UserID             int            `json:"user_id" gorm:"not null;index"`
	TokenID            int            `json:"token_id" gorm:"not null;index"`
	ModelOfferingID    int            `json:"model_offering_id" gorm:"not null;index"`
	OfferingName       string         `json:"offering_name" gorm:"type:text;not null"`
	RequestedName      string         `json:"requested_name" gorm:"type:text;not null"`
	ChoiceCount        int            `json:"choice_count" gorm:"not null;default:1"`
	SalePriceVersionID int            `json:"sale_price_version_id" gorm:"not null"`
	SaleExpression     string         `json:"sale_expression" gorm:"type:text;not null"`
	SaleCurrency       money.Currency `json:"sale_currency" gorm:"type:char(3);not null"`
	SaleFXRate         string         `json:"sale_fx_rate" gorm:"type:text;not null"`
	Usage              GatewayUsage   `json:"usage" gorm:"serializer:json;type:text;not null"`
	UsageSource        string         `json:"usage_source" gorm:"type:varchar(16);not null"`
	GroupRatio         string         `json:"group_ratio" gorm:"type:text;not null"`
	Receivable         money.Amount   `json:"receivable" gorm:"embedded;embeddedPrefix:receivable_"`
	LedgerEntryIDs     []string       `json:"ledger_entry_ids" gorm:"serializer:json;type:text;not null"`
	StartedAt          int64          `json:"started_at" gorm:"not null;index"`
	EndedAt            int64          `json:"ended_at" gorm:"not null"`
	Status             string         `json:"status" gorm:"type:varchar(32);not null;index"`
	ErrorCode          string         `json:"error_code,omitempty" gorm:"type:varchar(64);not null"`
}

func (GatewayRequest) TableName() string { return "gateway_requests" }

type GatewayAttempt struct {
	CostPending            bool           `json:"cost_pending,omitempty" gorm:"-"`
	ConvertedCostPending   bool           `json:"converted_cost_pending,omitempty" gorm:"-"`
	ID                     string         `json:"id" gorm:"type:varchar(64);primaryKey"`
	RequestID              string         `json:"request_id" gorm:"type:varchar(64);not null;index"`
	ChannelID              int            `json:"channel_id" gorm:"not null"`
	ProviderID             int            `json:"provider_id" gorm:"not null"`
	ModelID                int            `json:"model_id" gorm:"not null"`
	MemberTier             int            `json:"member_tier" gorm:"not null"`
	StartedAt              int64          `json:"started_at" gorm:"not null"`
	EndedAt                int64          `json:"ended_at" gorm:"not null"`
	Status                 string         `json:"status" gorm:"type:varchar(32);not null"`
	ErrorClass             string         `json:"error_class" gorm:"type:varchar(64);not null"`
	FailoverDecision       string         `json:"failover_decision" gorm:"type:varchar(32);not null"`
	FailoverReason         string         `json:"failover_reason" gorm:"type:varchar(64);not null"`
	CircuitAction          string         `json:"circuit_action" gorm:"type:varchar(32);not null"`
	RouteDiagnostics       map[string]any `json:"route_diagnostics,omitempty" gorm:"serializer:json;type:text;not null"`
	Usage                  GatewayUsage   `json:"usage" gorm:"serializer:json;type:text;not null"`
	UsageSource            string         `json:"usage_source" gorm:"type:varchar(16);not null"`
	PurchasePriceVersionID int            `json:"purchase_price_version_id" gorm:"not null"`
	PurchaseExpression     string         `json:"purchase_expression" gorm:"type:text;not null"`
	Cost                   money.Amount   `json:"cost" gorm:"embedded;embeddedPrefix:cost_"`
	AccountingFXRate       string         `json:"accounting_fx_rate" gorm:"type:text;not null"`
	ConvertedCost          money.Amount   `json:"converted_cost" gorm:"embedded;embeddedPrefix:converted_cost_"`
	ConversionLosses       []string       `json:"conversion_losses" gorm:"serializer:json;type:text;not null"`
}

func (GatewayAttempt) TableName() string { return "gateway_attempts" }

// MigrateGatewayRecords uses text JSON columns on both supported log databases.
// ClickHouse retries append identical finalized rows, and FINAL deduplicates them.
func MigrateGatewayRecords() error {
	if !common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		return LOG_DB.AutoMigrate(&GatewayRequest{}, &GatewayAttempt{})
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS gateway_requests (
		id String, user_id Int64, token_id Int64, model_offering_id Int64,
		offering_name String, requested_name String, choice_count Int64 DEFAULT 1, sale_price_version_id Int64,
		sale_expression String, sale_currency String, sale_fx_rate String,
		usage String, usage_source String, group_ratio String,
		receivable_nanos Int64, receivable_currency String, ledger_entry_ids String,
		started_at Int64, ended_at Int64, status String, error_code String
		) ENGINE = ReplacingMergeTree() ORDER BY id`,
		`ALTER TABLE gateway_requests ADD COLUMN IF NOT EXISTS choice_count Int64 DEFAULT 1`,
		`CREATE TABLE IF NOT EXISTS gateway_attempts (
		id String, request_id String, channel_id Int64, provider_id Int64,
		model_id Int64, member_tier Int64, started_at Int64, ended_at Int64,
		status String, error_class String, failover_decision String,
		failover_reason String, circuit_action String, route_diagnostics String,
		usage String, usage_source String, purchase_price_version_id Int64,
		purchase_expression String, cost_nanos Int64, cost_currency String,
		accounting_fx_rate String, converted_cost_nanos Int64,
		converted_cost_currency String, conversion_losses String
		) ENGINE = ReplacingMergeTree() ORDER BY id`,
	} {
		if err := LOG_DB.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}

func gatewayRecordQuery(ctx context.Context, record any, table string) *gorm.DB {
	query := LOG_DB.WithContext(ctx).Model(record)
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		query = query.Table(table + " FINAL")
	}
	return query
}

type GatewayRequestFilter struct {
	UserID          int
	TokenID         int
	ModelOfferingID int
	Status          string
	StartedAfter    int64
	StartedBefore   int64
}

func (filter GatewayRequestFilter) apply(query *gorm.DB, pending bool) *gorm.DB {
	for _, field := range []struct {
		name     string
		value    int64
		operator string
	}{
		{"user_id", int64(filter.UserID), "="},
		{"token_id", int64(filter.TokenID), "="},
		{"model_offering_id", int64(filter.ModelOfferingID), "="},
		{"started_at", filter.StartedAfter, ">="},
		{"started_at", filter.StartedBefore, "<="},
	} {
		if field.value <= 0 {
			continue
		}
		column := `"` + field.name + `"`
		if pending && field.name != "user_id" {
			column = fmt.Sprintf(`(NULLIF("settlement", '')::jsonb -> 'request' ->> '%s')::bigint`, field.name)
		}
		query = query.Where(column+" "+field.operator+" ?", field.value)
	}
	if filter.Status == "" {
		return query
	}
	if !pending {
		return query.Where(`"status" = ?`, filter.Status)
	}
	if filter.Status != "settlement_pending" {
		return query.Where("1 = 0")
	}
	return query
}

func ListGatewayRequests(ctx context.Context, filter GatewayRequestFilter, offset, limit int) ([]GatewayRequest, int64, error) {
	query := filter.apply(gatewayRecordQuery(ctx, &GatewayRequest{}, "gateway_requests"), false)
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	pendingQuery := filter.apply(gatewayPendingAccounting(DB.WithContext(ctx)), true)
	var pendingTotal int64
	if err := pendingQuery.Count(&pendingTotal).Error; err != nil {
		return nil, 0, err
	}
	items := []GatewayRequest{}
	if pendingTotal == 0 {
		err := query.Order("started_at DESC, id DESC").Offset(offset).Limit(limit).Find(&items).Error
		return items, total, err
	}
	if err := query.Order("started_at DESC, id DESC").Limit(offset + limit).Find(&items).Error; err != nil {
		return nil, 0, err
	}
	var pendingRows []struct{ RequestData string }
	if err := pendingQuery.Select(`(NULLIF("settlement", '')::jsonb -> 'request')::text AS "request_data"`).Order(`(NULLIF("settlement", '')::jsonb -> 'request' ->> 'started_at')::bigint DESC, "request_id" DESC`).Limit(offset + limit).Scan(&pendingRows).Error; err != nil {
		return nil, 0, err
	}
	for _, row := range pendingRows {
		var request GatewayRequest
		if err := common.UnmarshalJsonStr(row.RequestData, &request); err != nil {
			return nil, 0, err
		}
		request.Status = "settlement_pending"
		items = append(items, request)
	}
	slices.SortFunc(items, func(a, b GatewayRequest) int {
		if order := cmp.Compare(b.StartedAt, a.StartedAt); order != 0 {
			return order
		}
		return cmp.Compare(b.ID, a.ID)
	})
	total += pendingTotal
	return items[min(offset, len(items)):min(offset+limit, len(items))], total, nil
}

// GetGatewayRequest applies the ownership predicate in the database, including
// for an attempts read. A cross-user identifier is indistinguishable from missing.
func GetGatewayRequest(ctx context.Context, id string, ownerID int) (*GatewayRequest, error) {
	intent, pendingErr := gatewayPendingSettlement(ctx, id, ownerID)
	if pendingErr == nil {
		return &intent.Request, nil
	}
	if !errors.Is(pendingErr, gorm.ErrRecordNotFound) {
		return nil, pendingErr
	}
	query := gatewayRecordQuery(ctx, &GatewayRequest{}, "gateway_requests").Where("id = ?", id)
	if ownerID > 0 {
		query = query.Where("user_id = ?", ownerID)
	}
	var request GatewayRequest
	err := query.Take(&request).Error
	return &request, err
}

func ListGatewayAttempts(ctx context.Context, requestID string, offset, limit int) ([]GatewayAttempt, int64, error) {
	intent, pendingErr := gatewayPendingSettlement(ctx, requestID, 0)
	if pendingErr == nil {
		attempts := intent.Attempts
		if intent.Attempt != nil {
			attempts = []GatewayAttempt{*intent.Attempt}
		}
		return attempts[min(offset, len(attempts)):min(offset+limit, len(attempts))], int64(len(attempts)), nil
	}
	if !errors.Is(pendingErr, gorm.ErrRecordNotFound) {
		return nil, 0, pendingErr
	}
	query := gatewayRecordQuery(ctx, &GatewayAttempt{}, "gateway_attempts").Where("request_id = ?", requestID)
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	items := []GatewayAttempt{}
	err := query.Order("started_at, id").Offset(offset).Limit(limit).Find(&items).Error
	return items, total, err
}

// gatewayPendingSettlement exposes unsettled quantitative facts through the
// existing record APIs without publishing estimates as finalized log records.
func gatewayPendingSettlement(ctx context.Context, id string, ownerID int) (*GatewaySettlement, error) {
	query := gatewayPendingAccounting(DB.WithContext(ctx)).Where(`"request_id" = ?`, id)
	if ownerID > 0 {
		query = query.Where("user_id = ?", ownerID)
	}
	var row GatewayBilling
	if err := query.Take(&row).Error; err != nil {
		return nil, err
	}
	var intent GatewaySettlement
	if err := common.UnmarshalJsonStr(row.Settlement, &intent); err != nil {
		return nil, err
	}
	intent.Request.Status = "settlement_pending"
	return &intent, nil
}

func appendGatewayRecord(record any) error {
	if !common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		return LOG_DB.Clauses(clause.OnConflict{DoNothing: true}).Create(record).Error
	}
	// ClickHouse's native String binder does not accept defined string types.
	// Override just the storage fields; quantities remain exact int64 nanos.
	switch record := record.(type) {
	case *GatewayRequest:
		row := struct {
			GatewayRequest
			SaleCurrency       string `gorm:"column:sale_currency"`
			ReceivableCurrency string `gorm:"column:receivable_currency"`
		}{*record, string(record.SaleCurrency), string(record.Receivable.Currency)}
		return LOG_DB.Table(record.TableName()).Create(&row).Error
	case *GatewayAttempt:
		row := struct {
			GatewayAttempt
			CostCurrency          string `gorm:"column:cost_currency"`
			ConvertedCostCurrency string `gorm:"column:converted_cost_currency"`
		}{*record, string(record.Cost.Currency), string(record.ConvertedCost.Currency)}
		return LOG_DB.Table(record.TableName()).Create(&row).Error
	default:
		return fmt.Errorf("unsupported gateway record")
	}
}

// RecentOfferingOutputTokens uses at most the latest 1000 successful requests
// in the preceding seven days, ordered deterministically. The mean rounds up.
// Cold starts reserve 1024 output tokens, bounded by the selected Model limit.
func RecentOfferingOutputTokens(ctx context.Context, offeringID int, at int64) (int64, error) {
	var recent []GatewayRequest
	err := gatewayRecordQuery(ctx, &GatewayRequest{}, "gateway_requests").
		Where("model_offering_id = ? AND status = ? AND ended_at >= ? AND ended_at <= ?", offeringID, "success", at-7*24*60*60*1000, at).
		Order("ended_at DESC, id DESC").Limit(1000).Find(&recent).Error
	if err != nil {
		return 0, err
	}
	if len(recent) == 0 {
		return 1024, nil
	}
	total := new(big.Rat)
	for _, record := range recent {
		if record.Usage.CompletionTokens < 0 || record.Usage.CompletionTokens > 1<<31-1 || record.ChoiceCount < 1 {
			return 0, fmt.Errorf("invalid recorded output usage")
		}
		total.Add(total, big.NewRat(record.Usage.CompletionTokens, int64(record.ChoiceCount)))
	}
	mean := total.Quo(total, big.NewRat(int64(len(recent)), 1))
	whole, remainder := new(big.Int), new(big.Int)
	whole.QuoRem(mean.Num(), mean.Denom(), remainder)
	if remainder.Sign() > 0 {
		whole.Add(whole, big.NewInt(1))
	}
	return whole.Int64(), nil
}
