package model

import (
	"strconv"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLegacyAutoGroupOptionReloadRetainsOnlyRawData(t *testing.T) {
	common.OptionMapRWMutex.Lock()
	previousOptions := common.OptionMap
	common.OptionMap = map[string]string{}
	common.OptionMapRWMutex.Unlock()
	previousGroups := setting.AutoGroups2JsonString()
	previousDefault, previousMax := setting.DefaultUseAutoGroup, setting.GetMaxTokenAutoGroups()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptions
		common.OptionMapRWMutex.Unlock()
		setting.DefaultUseAutoGroup = previousDefault
		require.NoError(t, setting.UpdateAutoGroupsByJsonString(previousGroups))
		require.NoError(t, setting.UpdateMaxTokenAutoGroups(strconv.Itoa(previousMax)))
	})

	for _, source := range []Option{
		{Key: "AutoGroups", Value: `["archived"]`},
		{Key: "AutoGroups", Value: `malformed`},
		{Key: "DefaultUseAutoGroup", Value: strconv.FormatBool(!previousDefault)},
		{Key: "MaxTokenAutoGroups", Value: "999999"},
		{Key: "MaxTokenAutoGroups", Value: ""},
		{Key: "MaxTokenAutoGroups", Value: "0"},
		{Key: "MaxTokenAutoGroups", Value: "-1"},
		{Key: "MaxTokenAutoGroups", Value: "1.5"},
		{Key: "MaxTokenAutoGroups", Value: "invalid"},
	} {
		require.NoError(t, updateOptionMap(source.Key, source.Value))
		value, exists := common.OptionMap[source.Key]
		assert.True(t, exists, source.Key)
		assert.Equal(t, source.Value, value, source.Key)
		assert.Equal(t, previousGroups, setting.AutoGroups2JsonString())
		assert.Equal(t, previousDefault, setting.DefaultUseAutoGroup)
		assert.Equal(t, previousMax, setting.GetMaxTokenAutoGroups())
	}
}
