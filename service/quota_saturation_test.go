package service

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestAttachQuotaSaturationNestsUnderAdminInfo verifies the saturation marker
// is nested under other.admin_info.quota_saturation so it is admin-only (the
// log formatter strips admin_info for non-admin viewers).
func TestAttachQuotaSaturationNestsUnderAdminInfo(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(nil)

	relayInfo := &relaycommon.RelayInfo{
		UserId:          7,
		OriginModelName: "gpt-image-1",
		QuotaClamp: &common.QuotaClamp{
			Op:       "QuotaFromDecimal",
			Kind:     common.QuotaClampOverflow,
			Original: 1.8e19,
			Clamped:  common.MaxQuota,
		},
	}

	other := model.NewLogOther()
	other.SetPublic("model_price", 0.004)
	attachQuotaSaturation(ctx, relayInfo, other)

	adminInfo, ok := other.Snapshot()["admin_info"].(map[string]any)
	require.True(t, ok, "admin_info should be created")
	sat, ok := adminInfo["quota_saturation"].(map[string]any)
	require.True(t, ok, "quota_saturation should be nested under admin_info")
	require.Equal(t, "QuotaFromDecimal", sat["op"])
	require.Equal(t, common.QuotaClampOverflow, sat["kind"])
	require.Equal(t, common.MaxQuota, sat["clamped"])
}

// TestAttachQuotaSaturationPreservesExistingAdminInfo verifies the marker is
// merged into a pre-existing admin_info map without clobbering it.
func TestAttachQuotaSaturationPreservesExistingAdminInfo(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(nil)

	relayInfo := &relaycommon.RelayInfo{
		QuotaClamp: &common.QuotaClamp{Op: "QuotaFromFloat", Kind: common.QuotaClampUnderflow, Clamped: common.MinQuota},
	}
	other := model.NewLogOther()
	other.SetAdmin("admin_username", "root")
	attachQuotaSaturation(ctx, relayInfo, other)

	adminInfo := other.Snapshot()["admin_info"].(map[string]any)
	require.Equal(t, "root", adminInfo["admin_username"], "existing admin_info fields preserved")
	require.NotNil(t, adminInfo["quota_saturation"])
}

// TestAttachQuotaSaturationNoClampNoMarker verifies the common case (no
// saturation) leaves the log untouched.
func TestAttachQuotaSaturationNoClampNoMarker(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(nil)

	relayInfo := &relaycommon.RelayInfo{QuotaClamp: nil}
	other := model.NewLogOther()
	other.SetPublic("model_price", 0.004)
	attachQuotaSaturation(ctx, relayInfo, other)

	_, hasAdmin := other.Snapshot()["admin_info"]
	require.False(t, hasAdmin, "no admin_info should be added when there is no clamp")
}

func TestPreConsumeBillingRejectsSaturatedQuotaBeforeDeduction(t *testing.T) {
	seedSubscriptionTaskMoney(t, 0)
	price := model.PriceVersion{Currency: "USD", Expression: `tier("overflow", fixed(1000000000000000000))`}
	_, _, trace, err := GatewayPrice(&price, &dto.Usage{}, billingexpr.RequestInput{}, time.Now(), decimal.NewFromInt(1), nil)
	require.ErrorIs(t, err, money.ErrOverflow)
	require.NotEmpty(t, trace.UnroundedAmount, "overflow keeps the exact diagnostic amount, not a clamp")
	wallet, err := model.GetUserWallet(77)
	require.NoError(t, err)
	assert.EqualValues(t, 8888, wallet.Balance.Nanos)
	assert.Equal(t, 8000, getTokenRemainQuota(t, 77))
	var reservations int64
	require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("type = ?", model.LedgerEntryPreConsume).Count(&reservations).Error)
	assert.Zero(t, reservations)
}

func TestPreConsumeBillingRejectsNegativeQuotaBeforeDeduction(t *testing.T) {
	seedSubscriptionTaskMoney(t, 0)
	request := model.GatewayRequest{ID: "negative-reservation", UserID: 77}
	amount := money.Amount{Nanos: -1, Currency: "USD"}
	require.Error(t, model.ReserveGateway(&request, amount, amount))
	var rows int64
	require.NoError(t, model.DB.Model(&model.GatewayBilling{}).Where("request_id = ?", request.ID).Count(&rows).Error)
	assert.Zero(t, rows)
	wallet, err := model.GetUserWallet(77)
	require.NoError(t, err)
	assert.EqualValues(t, 8888, wallet.Balance.Nanos)
	assert.Equal(t, 8000, getTokenRemainQuota(t, 77))
}
