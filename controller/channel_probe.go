package controller

import (
	"context"
	"encoding/json"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/gin-gonic/gin"
	"net/http"
	"slices"
	"time"
)

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
	result := model.ChannelProbeResult{ChannelID: id, Capability: feature, Protocol: record.Protocol, StartedAt: clock.Now().Unix(), Status: "running", Reason: "", ActorID: c.GetInt("id")}
	if err := model.DB.Create(&result).Error; err != nil {
		singleChannelError(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	result.Status, result.Reason = probeChannel(ctx, record, provider, feature)
	result.FinishedAt = clock.Now().Unix()
	saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 5*time.Second)
	defer saveCancel()
	if err := model.DB.WithContext(saveCtx).Model(&result).Select("status", "reason", "finished_at").Updates(&result).Error; err != nil {
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
	results := []model.ChannelProbeResult{}
	latestIDs := model.DB.Model(&model.ChannelProbeResult{}).Select("MAX(id)").Where("channel_id = ?", id).Group("capability")
	if err := model.DB.Where("id IN (?)", latestIDs).Order("capability").Find(&results).Error; err != nil {
		singleChannelError(c, err)
		return
	}
	for i := range results {
		if results[i].Status == "running" && clock.Now().Unix()-results[i].StartedAt > 25 {
			results[i].Status = "inconclusive"
			results[i].Reason = "probe_interrupted"
		}
	}
	common.ApiSuccess(c, gin.H{"capabilities": channelProbeCapabilities, "results": results})
}
