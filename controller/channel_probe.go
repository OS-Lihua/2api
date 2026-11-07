package controller

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
	"net/http"
	"slices"
	"time"
)

const channelProbeTimeout = 90 * time.Second
const channelProbeSaveTimeout = 5 * time.Second

// RunChannelProbe is an explicit admin operation, including for pending or
// disabled Channels. It never runs the user billing or automatic health path.
func RunChannelProbe(c *gin.Context) {
	id, ok := providerIDParam(c)
	if !ok {
		return
	}
	var fields map[string]json.RawMessage
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1024)
	if err := common.DecodeJson(c.Request.Body, &fields); err != nil || len(fields) != 1 {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
		return
	}
	var feature string
	if common.Unmarshal(fields["capability"], &feature) != nil || !slices.Contains(channelProbeCapabilities, feature) {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
		return
	}
	var record model.Channel
	if err := model.DB.WithContext(c.Request.Context()).First(&record, id).Error; err != nil {
		singleChannelError(c, err)
		return
	}
	provider, err := model.GetProviderById(record.ProviderID, true)
	if err != nil {
		singleChannelError(c, err)
		return
	}
	if err := model.RecoverInterruptedChannelProbes(c.Request.Context()); err != nil {
		singleChannelError(c, err)
		return
	}
	startedAt := clock.Now().Unix()
	result := model.ChannelProbeResult{ConfigurationHash: record.ProbeConfigurationHash(), ChannelID: id, Capability: feature, Protocol: record.Protocol, StartedAt: startedAt, DeadlineAt: startedAt + int64((channelProbeTimeout+channelProbeSaveTimeout)/time.Second), Status: "running", Reason: "", ActorID: c.GetInt("id")}
	if err := model.DB.Create(&result).Error; err != nil {
		var conflict *pgconn.PgError
		if errors.As(err, &conflict) && conflict.Code == "23505" {
			common.ApiErrorStatus(c, http.StatusConflict, common.NewMessage("A capability probe is already running for this Channel"))
			return
		}
		singleChannelError(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), channelProbeTimeout)
	defer cancel()
	result.Status, result.Reason = probeChannel(ctx, record, provider, &result)
	result.FinishedAt = clock.Now().Unix()
	saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), channelProbeSaveTimeout)
	defer saveCancel()
	if err := model.DB.WithContext(saveCtx).Model(&result).Select("status", "reason", "finished_at", "tested_value").Updates(&result).Error; err != nil {
		singleChannelError(c, err)
		return
	}
	recordManageAudit(c, "channel.probe", map[string]any{"id": id, "probe_id": result.ID, "capability": feature, "status": result.Status, "reason": result.Reason})
	common.ApiSuccess(c, result)
}

// ListChannelProbes returns the latest observation of each individual capability.
func ListChannelProbes(c *gin.Context) {
	id, ok := providerIDParam(c)
	if !ok {
		return
	}
	var record model.Channel
	if err := model.DB.First(&record, id).Error; err != nil {
		singleChannelError(c, err)
		return
	}
	if err := model.RecoverInterruptedChannelProbes(c.Request.Context()); err != nil {
		singleChannelError(c, err)
		return
	}
	results := []model.ChannelProbeResult{}
	latestIDs := model.DB.Model(&model.ChannelProbeResult{}).Select("MAX(id)").Where("channel_id = ? AND configuration_hash = ?", id, record.ProbeConfigurationHash()).Group("capability")
	if err := model.DB.Where("id IN (?)", latestIDs).Order("capability").Find(&results).Error; err != nil {
		singleChannelError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"capabilities": channelProbeCapabilities, "results": results})
}
