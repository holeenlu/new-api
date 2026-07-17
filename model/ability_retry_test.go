package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"

	"github.com/stretchr/testify/require"
)

func TestGetChannelWithCandidateFilterKeepsHighestRemainingPriority(t *testing.T) {
	clearPreferredOwnerTables(t)
	t.Cleanup(func() { clearPreferredOwnerTables(t) })

	const (
		group     = "retry-priority"
		modelName = "gpt-5.6-sol"
	)
	insertPreferredOwnerCandidate(t, 1, modelName, group, constant.ChannelTypeCodex, 2, 1, common.ChannelStatusEnabled, true)
	insertPreferredOwnerCandidate(t, 2, modelName, group, constant.ChannelTypeCodex, 2, 1, common.ChannelStatusEnabled, true)
	insertPreferredOwnerCandidate(t, 3, modelName, group, constant.ChannelTypeCodex, 1, 1, common.ChannelStatusEnabled, true)

	channel, err := GetChannel(group, modelName, 1, "", func(candidate *Channel) bool {
		return candidate.Id != 1
	})

	require.NoError(t, err)
	require.NotNil(t, channel)
	require.Equal(t, 2, channel.Id)
}

func TestCachedChannelSelectionWithCandidateFilterKeepsHighestRemainingPriority(t *testing.T) {
	clearPreferredOwnerTables(t)
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() {
		clearPreferredOwnerTables(t)
		InitChannelCache()
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
	})

	const (
		group     = "cached-retry-priority"
		modelName = "gpt-5.6-sol"
	)
	insertPreferredOwnerCandidate(t, 1, modelName, group, constant.ChannelTypeCodex, 3, 1, common.ChannelStatusEnabled, true)
	insertPreferredOwnerCandidate(t, 2, modelName, group, constant.ChannelTypeCodex, 2, 1, common.ChannelStatusEnabled, true)
	insertPreferredOwnerCandidate(t, 3, modelName, group, constant.ChannelTypeCodex, 1, 1, common.ChannelStatusEnabled, true)
	for channelID, priority := range map[int]int64{1: 3, 2: 2, 3: 1} {
		require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channelID).Updates(map[string]any{
			"group":    group,
			"models":   modelName,
			"priority": priority,
			"weight":   1,
		}).Error)
	}
	InitChannelCache()

	channel, err := GetRandomSatisfiedChannel(group, modelName, 2, "", func(candidate *Channel) bool {
		return candidate.Id != 1
	})

	require.NoError(t, err)
	require.NotNil(t, channel)
	require.Equal(t, 2, channel.Id)
}
