package model

import (
	"context"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/protocol"
)

// RecoverInterruptedChannelProbes finalizes observations left by a process
// interruption. Each row's deadline includes its request and final-save budget.
func RecoverInterruptedChannelProbes(ctx context.Context) error {
	now := clock.Now().Unix()
	return DB.WithContext(ctx).Model(&ChannelProbeResult{}).Where("status = ? AND deadline_at < ?", "running", now).Updates(map[string]any{"status": "inconclusive", "reason": "probe_interrupted", "finished_at": now}).Error
}

// ChannelProbeResult is an observation, never a capability declaration. Only
// bounded, gateway-owned reason codes are stored; no upstream bodies or URLs.
type ChannelProbeResult struct {
	ID          int64         `json:"id" gorm:"primaryKey;index:idx_channel_probe_latest,priority:3,sort:desc"`
	ChannelID   int           `json:"channel_id" gorm:"not null;index:idx_channel_probe_latest,priority:1;uniqueIndex:idx_channel_probe_active,where:status = 'running'"`
	Channel     Channel       `json:"-" gorm:"constraint:OnDelete:CASCADE"`
	Capability  string        `json:"capability" gorm:"type:varchar(64);not null;index:idx_channel_probe_latest,priority:2"`
	Protocol    protocol.Type `json:"protocol" gorm:"type:varchar(80);not null"`
	StartedAt   int64         `json:"started_at" gorm:"not null"`
	DeadlineAt  int64         `json:"deadline_at" gorm:"not null;index:idx_channel_probe_running,where:status = 'running'"`
	FinishedAt  int64         `json:"finished_at" gorm:"not null"`
	Status      string        `json:"status" gorm:"type:varchar(16);not null;check:channel_probe_status,status IN ('running','passed','failed','inconclusive')"`
	Reason      string        `json:"reason" gorm:"type:varchar(64);not null"`
	TestedValue int64         `json:"tested_value,omitempty" gorm:"not null"`
	ActorID     int           `json:"actor_id" gorm:"not null"`
}
