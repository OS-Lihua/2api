package model

import "github.com/QuantumNous/new-api/pkg/protocol"

// ChannelProbeResult is an observation, never a capability declaration. Only
// bounded, gateway-owned reason codes are stored; no upstream bodies or URLs.
type ChannelProbeResult struct {
	ID         int64         `json:"id" gorm:"primaryKey"`
	ChannelID  int           `json:"channel_id" gorm:"not null;index:idx_channel_probe_latest,priority:1"`
	Channel    Channel       `json:"-" gorm:"constraint:OnDelete:CASCADE"`
	Capability string        `json:"capability" gorm:"type:varchar(64);not null;index:idx_channel_probe_latest,priority:2"`
	Protocol   protocol.Type `json:"protocol" gorm:"type:varchar(80);not null"`
	StartedAt  int64         `json:"started_at" gorm:"not null"`
	FinishedAt int64         `json:"finished_at" gorm:"not null;index:idx_channel_probe_latest,priority:3"`
	Status     string        `json:"status" gorm:"type:varchar(16);not null;check:channel_probe_status,status IN ('running','passed','failed','inconclusive')"`
	Reason     string        `json:"reason" gorm:"type:varchar(64);not null"`
	ActorID    int           `json:"actor_id" gorm:"not null"`
}
