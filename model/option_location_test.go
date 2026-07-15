package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
)

func TestUpdateOptionMapAppliesValidatedUpstreamLocationMode(t *testing.T) {
	originalMode := common.GetUpstreamLocationMode()
	common.OptionMapRWMutex.Lock()
	originalOptionMap := common.OptionMap
	common.OptionMap = make(map[string]string)
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		require.NoError(t, common.SetUpstreamLocationMode(originalMode))
		common.OptionMapRWMutex.Lock()
		common.OptionMap = originalOptionMap
		common.OptionMapRWMutex.Unlock()
	})

	require.NoError(t, updateOptionMap("UpstreamLocationMode", common.UpstreamLocationModeAuto))
	require.Equal(t, common.UpstreamLocationModeAuto, common.GetUpstreamLocationMode())
	require.Equal(t, common.UpstreamLocationModeAuto, common.OptionMap["UpstreamLocationMode"])

	require.Error(t, updateOptionMap("UpstreamLocationMode", "unexpected"))
	require.Error(t, updateOptionMap("UpstreamLocationMode", "AUTO"))
	require.Equal(t, common.UpstreamLocationModeAuto, common.GetUpstreamLocationMode())
	require.Equal(t, common.UpstreamLocationModeAuto, common.OptionMap["UpstreamLocationMode"])
}
