package model

import (
	"database/sql"
	"fmt"

	"gorm.io/gorm"
)

type RankingQuotaTotal struct {
	ModelName   string `json:"model_name"`
	TotalTokens int64  `json:"total_tokens"`
}

type RankingQuotaBucket struct {
	ModelName string `json:"model_name"`
	Bucket    int64  `json:"bucket"`
	Tokens    int64  `json:"tokens"`
}

type RankingModelPublisher struct {
	Name string
	Icon string
}

// GetRankingModelPublishers decorates historical model names without consulting
// current routing, offerings or prices. Display defaults never create records.
func GetRankingModelPublishers(modelNames []string) (map[string]RankingModelPublisher, error) {
	publishers := make(map[string]RankingModelPublisher)
	if len(modelNames) == 0 {
		return publishers, nil
	}
	var metadata []Model
	var vendors []Vendor
	if err := DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Select("id", "model_name", "name_rule", "vendor_id", "status").Order("id").Find(&metadata).Error; err != nil {
			return err
		}
		return tx.Select("id", "name", "icon").Order("id").Find(&vendors).Error
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}); err != nil {
		return nil, err
	}
	vendorByID := make(map[int]*Vendor, len(vendors))
	for i := range vendors {
		vendorByID[vendors[i].Id] = &vendors[i]
	}
	resolved := resolveModelMetadata(metadata, modelNames)
	initDefaultVendorMapping(resolved, vendorByID, modelNames)
	for _, name := range modelNames {
		item := resolved[name]
		if item == nil || item.Status != 1 {
			continue
		}
		if vendor := vendorByID[item.VendorID]; vendor != nil {
			publishers[name] = RankingModelPublisher{Name: vendor.Name, Icon: vendor.Icon}
		}
	}
	return publishers, nil
}

func GetRankingQuotaTotals(startTime int64, endTime int64) ([]RankingQuotaTotal, error) {
	var rows []RankingQuotaTotal
	query := DB.Table("quota_data").
		Select("model_name, sum(token_used) as total_tokens").
		Where("model_name <> ''").
		Group("model_name").
		Having("sum(token_used) > 0").
		Order("total_tokens DESC")
	query = applyRankingQuotaTimeRange(query, startTime, endTime)
	err := query.Find(&rows).Error
	return rows, err
}

func GetRankingQuotaBuckets(startTime int64, endTime int64, bucketSize int64) ([]RankingQuotaBucket, error) {
	if bucketSize <= 0 {
		bucketSize = 3600
	}
	bucketExpr := fmt.Sprintf("(created_at / %d) * %d", bucketSize, bucketSize)
	var rows []RankingQuotaBucket
	query := DB.Table("quota_data").
		Select(fmt.Sprintf("model_name, %s as bucket, sum(token_used) as tokens", bucketExpr)).
		Where("model_name <> ''").
		Group(fmt.Sprintf("model_name, %s", bucketExpr)).
		Having("sum(token_used) > 0").
		Order("bucket ASC")
	query = applyRankingQuotaTimeRange(query, startTime, endTime)
	err := query.Find(&rows).Error
	return rows, err
}

func applyRankingQuotaTimeRange(query *gorm.DB, startTime int64, endTime int64) *gorm.DB {
	if startTime > 0 {
		query = query.Where("created_at >= ?", startTime)
	}
	if endTime > 0 {
		query = query.Where("created_at <= ?", endTime)
	}
	return query
}
