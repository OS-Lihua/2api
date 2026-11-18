package service

import (
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func configureRequestAutoGroupsTest(t *testing.T) {
	t.Helper()
	originalMax := setting.GetMaxTokenAutoGroups()
	originalAutoGroups := setting.AutoGroups2JsonString()
	originalUsableGroups := setting.UserUsableGroups2JSONString()
	originalRatios := ratio_setting.GroupRatio2JSONString()
	require.NoError(t, setting.UpdateMaxTokenAutoGroups("2"))
	require.NoError(t, setting.UpdateAutoGroupsByJsonString(`["vip","default","svip"]`))
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"Default","vip":"VIP","svip":"SVIP"}`))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"vip":1,"svip":1}`))
	t.Cleanup(func() {
		require.NoError(t, setting.UpdateMaxTokenAutoGroups(fmt.Sprint(originalMax)))
		require.NoError(t, setting.UpdateAutoGroupsByJsonString(originalAutoGroups))
		require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(originalUsableGroups))
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(originalRatios))
	})
}

func seedAutoGroupOfferings(t *testing.T, db *gorm.DB) []model.ModelOffering {
	t.Helper()
	offerings := []model.ModelOffering{}
	for _, name := range []string{"vip", "default", "svip"} {
		offering := model.ModelOffering{Name: name, Visibility: "all", Groups: []string{}, Enabled: true, RouteMethod: "cost", AcceptedConversionLosses: []string{}}
		require.NoError(t, db.Create(&offering).Error)
		offerings = append(offerings, offering)
	}
	return offerings
}

func allowedAutoGroupOfferingNames(t *testing.T) []string {
	t.Helper()
	names := []string{}
	require.NoError(t, model.AllowedOfferingsQuery(t.Context(), 7, 7).Order("id").Pluck("name", &names).Error)
	return names
}

func TestGetRequestAutoGroupsInheritedListIsNotLimited(t *testing.T) {
	configureRequestAutoGroupsTest(t)
	db := setupChannelSelectAutoGroupsTest(t)
	seedAutoGroupOfferings(t, db)
	assert.Equal(t, []string{"vip", "default", "svip"}, allowedAutoGroupOfferingNames(t), "live Offering permission is not truncated by retired Auto-group settings")
	require.NoError(t, setting.UpdateAutoGroupsByJsonString(`[]`))
	assert.Equal(t, []string{"vip", "default", "svip"}, allowedAutoGroupOfferingNames(t))
}

func TestGetRequestAutoGroupsFiltersBeforeApplyingCurrentLimit(t *testing.T) {
	configureRequestAutoGroupsTest(t)
	db := setupChannelSelectAutoGroupsTest(t)
	offerings := seedAutoGroupOfferings(t, db)
	require.NoError(t, db.Create(&model.TokenOfferingPolicy{TokenID: 7, DefaultAllow: false}).Error)
	for _, offering := range offerings[:2] {
		require.NoError(t, db.Create(&model.TokenOfferingException{TokenID: 7, OfferingID: offering.ID}).Error)
	}
	assert.Equal(t, []string{"vip", "default"}, allowedAutoGroupOfferingNames(t))
	require.NoError(t, setting.UpdateMaxTokenAutoGroups("1"))
	assert.Equal(t, []string{"vip", "default"}, allowedAutoGroupOfferingNames(t), "a retired setting cannot revoke an authorized Offering")
	require.NoError(t, db.Where("token_id = ? AND offering_id = ?", 7, offerings[1].ID).Delete(&model.TokenOfferingException{}).Error)
	assert.Equal(t, []string{"vip"}, allowedAutoGroupOfferingNames(t), "current Key permission changes apply immediately")
}

func TestGetRequestAutoGroupsDoesNotFallBackAfterPermissionChange(t *testing.T) {
	configureRequestAutoGroupsTest(t)
	db := setupChannelSelectAutoGroupsTest(t)
	offerings := seedAutoGroupOfferings(t, db)
	require.NoError(t, db.Model(&model.User{}).Where("id = ?", 7).Update("group", "vip").Error)
	require.NoError(t, db.Model(&offerings[0]).Updates(map[string]any{"visibility": "groups", "groups": `["vip"]`}).Error)
	require.NoError(t, db.Create(&model.TokenOfferingPolicy{TokenID: 7, DefaultAllow: false}).Error)
	require.NoError(t, db.Create(&model.TokenOfferingException{TokenID: 7, OfferingID: offerings[0].ID}).Error)
	assert.Equal(t, []string{"vip"}, allowedAutoGroupOfferingNames(t))
	require.NoError(t, db.Model(&model.User{}).Where("id = ?", 7).Update("group", "default").Error)
	assert.Empty(t, allowedAutoGroupOfferingNames(t), "old group snapshots cannot preserve revoked visibility")
	require.NoError(t, db.Model(&model.User{}).Where("id = ?", 7).Updates(map[string]any{"group": "vip", "status": common.UserStatusDisabled}).Error)
	assert.Empty(t, allowedAutoGroupOfferingNames(t), "disabled users cannot use inherited permissions")
}
