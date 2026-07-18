package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"

	"github.com/stretchr/testify/require"
)

func setChannelModelMetadata(t *testing.T, channelID int, metadata map[string]dto.UpstreamModelMetadata, mapping string) {
	t.Helper()
	channel := &Channel{Id: channelID}
	channel.SetOtherSettings(dto.ChannelOtherSettings{UpstreamModelMetadata: metadata})
	updates := map[string]any{"settings": channel.OtherSettings}
	if mapping != "" {
		updates["model_mapping"] = mapping
	}
	require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channelID).Updates(updates).Error)
}

func TestGetEnabledModelUpstreamMetadataUsesMinimumAcrossGroup(t *testing.T) {
	clearPreferredOwnerTables(t)
	t.Cleanup(func() { clearPreferredOwnerTables(t) })

	const modelName = "gpt-5.6-sol"
	insertPreferredOwnerCandidate(t, 1, modelName, "svip", constant.ChannelTypeCodex, 2, 1, common.ChannelStatusEnabled, true)
	insertPreferredOwnerCandidate(t, 2, modelName, "svip", constant.ChannelTypeCodex, 1, 1, common.ChannelStatusEnabled, true)
	insertPreferredOwnerCandidate(t, 3, modelName, "other", constant.ChannelTypeCodex, 3, 1, common.ChannelStatusEnabled, true)
	setChannelModelMetadata(t, 1, map[string]dto.UpstreamModelMetadata{
		modelName: {ContextWindow: 1_000_000, MaxContextWindow: 1_000_000, Complete: true},
	}, "")
	setChannelModelMetadata(t, 2, map[string]dto.UpstreamModelMetadata{
		modelName: {ContextWindow: 800_000, MaxContextWindow: 900_000, Complete: true},
	}, "")
	setChannelModelMetadata(t, 3, map[string]dto.UpstreamModelMetadata{
		modelName: {ContextWindow: 200_000, MaxContextWindow: 200_000, Complete: true},
	}, "")

	profiles, err := GetEnabledModelUpstreamMetadata([]string{modelName}, []string{"svip"}, constant.ChannelTypeCodex)

	require.NoError(t, err)
	require.Equal(t, dto.UpstreamModelMetadata{
		ContextWindow:    800_000,
		MaxContextWindow: 900_000,
		Complete:         true,
	}, profiles[modelName])
}

func TestGetEnabledModelUpstreamMetadataFailsClosedForMissingMetadata(t *testing.T) {
	clearPreferredOwnerTables(t)
	t.Cleanup(func() { clearPreferredOwnerTables(t) })

	const modelName = "gpt-5.6-sol"
	insertPreferredOwnerCandidate(t, 1, modelName, "svip", constant.ChannelTypeCodex, 1, 1, common.ChannelStatusEnabled, true)
	insertPreferredOwnerCandidate(t, 2, modelName, "svip", constant.ChannelTypeCodex, 1, 1, common.ChannelStatusEnabled, true)
	setChannelModelMetadata(t, 1, map[string]dto.UpstreamModelMetadata{
		modelName: {ContextWindow: 1_000_000, MaxContextWindow: 1_000_000, Complete: true},
	}, "")

	profiles, err := GetEnabledModelUpstreamMetadata([]string{modelName}, []string{"svip"}, constant.ChannelTypeCodex)

	require.NoError(t, err)
	require.False(t, profiles[modelName].Valid())
}

func TestGetEnabledModelUpstreamMetadataAppliesModelMapping(t *testing.T) {
	clearPreferredOwnerTables(t)
	t.Cleanup(func() { clearPreferredOwnerTables(t) })

	insertPreferredOwnerCandidate(t, 1, "codex-sol", "svip", constant.ChannelTypeCodex, 1, 1, common.ChannelStatusEnabled, true)
	setChannelModelMetadata(t, 1, map[string]dto.UpstreamModelMetadata{
		"gpt-5.6-sol": {ContextWindow: 1_000_000, MaxContextWindow: 1_000_000, Complete: true},
	}, `{"codex-sol":"gpt-5.6-sol"}`)

	profiles, err := GetEnabledModelUpstreamMetadata([]string{"codex-sol"}, []string{"svip"}, constant.ChannelTypeCodex)

	require.NoError(t, err)
	require.True(t, profiles["codex-sol"].Valid())
	require.Equal(t, 1_000_000, profiles["codex-sol"].ContextWindow)
}

func TestGetEnabledModelUpstreamMetadataRequiresEveryEligibleChannel(t *testing.T) {
	clearPreferredOwnerTables(t)
	t.Cleanup(func() { clearPreferredOwnerTables(t) })

	const modelName = "gpt-5.6-sol"
	insertPreferredOwnerCandidate(t, 1, modelName, "svip", constant.ChannelTypeCodex, 1, 1, common.ChannelStatusEnabled, true)
	insertPreferredOwnerCandidate(t, 2, modelName, "svip", constant.ChannelTypeClaudeCode, 1, 1, common.ChannelStatusEnabled, true)
	setChannelModelMetadata(t, 1, map[string]dto.UpstreamModelMetadata{
		modelName: {ContextWindow: 1_000_000, MaxContextWindow: 1_000_000, Complete: true},
	}, "")

	profiles, err := GetEnabledModelUpstreamMetadata([]string{modelName}, []string{"svip"})

	require.NoError(t, err)
	require.False(t, profiles[modelName].Valid())
}
