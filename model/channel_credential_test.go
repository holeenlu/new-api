package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestSubscriptionOAuthCredentialFingerprintUsesAccountIdentity(t *testing.T) {
	first := SubscriptionOAuthCredentialFingerprint(
		constant.ChannelTypeCodex,
		1,
		0,
		`{"access_token":"token-a","account_id":"shared-account"}`,
	)
	second := SubscriptionOAuthCredentialFingerprint(
		constant.ChannelTypeCodex,
		2,
		3,
		`{"access_token":"token-b","account_id":"shared-account"}`,
	)
	require.Equal(t, first, second)

	claudeExport := SubscriptionOAuthCredentialFingerprint(
		constant.ChannelTypeClaudeCode,
		3,
		0,
		`export CLAUDE_CODE_OAUTH_TOKEN="oauth-token"`,
	)
	claudeRaw := SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeClaudeCode, 4, 1, "oauth-token")
	require.Equal(t, claudeExport, claudeRaw)
	claudeSpaced := SubscriptionOAuthCredentialFingerprint(
		constant.ChannelTypeClaudeCode,
		5,
		2,
		`CLAUDE_CODE_OAUTH_TOKEN= "oauth-token"`,
	)
	require.Equal(t, claudeRaw, claudeSpaced)
}

func TestQuarantineSubscriptionOAuthCredentialDisablesEveryReference(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Channel{}, &Ability{}))

	previousDB := DB
	previousMemoryCache := common.MemoryCacheEnabled
	DB = db
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		DB = previousDB
		common.MemoryCacheEnabled = previousMemoryCache
	})

	sharedA := `{"access_token":"token-a","account_id":"shared-account"}`
	sharedB := `{"access_token":"token-b","account_id":"shared-account"}`
	other := `{"access_token":"token-c","account_id":"other-account"}`
	channels := []*Channel{
		{
			Type: constant.ChannelTypeCodex, Name: "single", Key: sharedA,
			Status: common.ChannelStatusEnabled, Group: "default", Models: "gpt-test",
		},
		{
			Type: constant.ChannelTypeCodex, Name: "multi", Key: other + "\n" + sharedB,
			Status: common.ChannelStatusEnabled, Group: "vip", Models: "gpt-test",
			ChannelInfo: ChannelInfo{IsMultiKey: true, MultiKeySize: 2},
		},
		{
			Type: constant.ChannelTypeCodex, Name: "unrelated", Key: other,
			Status: common.ChannelStatusEnabled, Group: "default", Models: "gpt-test",
		},
	}
	for _, channel := range channels {
		channel.SetOtherSettings(dto.ChannelOtherSettings{
			UpstreamModelMetadata: map[string]dto.UpstreamModelMetadata{
				"gpt-test": {ContextWindow: 1_000_000, MaxContextWindow: 1_000_000, Complete: true},
			},
			UpstreamModelMetadataUpdatedTime: 123,
		})
		require.NoError(t, db.Create(channel).Error)
		require.NoError(t, db.Create(&Ability{
			Group: channel.Group, Model: "gpt-test", ChannelId: channel.Id, Enabled: true,
		}).Error)
	}

	fingerprint := SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeCodex, channels[0].Id, 0, sharedA)
	affected, err := QuarantineSubscriptionOAuthCredential(
		constant.ChannelTypeCodex,
		fingerprint,
		"oauth_unauthorized: expired",
	)
	require.NoError(t, err)
	require.Len(t, affected, 2)

	var single Channel
	require.NoError(t, db.First(&single, channels[0].Id).Error)
	require.Equal(t, common.ChannelStatusManuallyDisabled, single.Status)
	require.Empty(t, single.GetOtherSettings().UpstreamModelMetadata)

	var multi Channel
	require.NoError(t, db.First(&multi, channels[1].Id).Error)
	require.Equal(t, common.ChannelStatusEnabled, multi.Status)
	require.Equal(t, common.ChannelStatusManuallyDisabled, multi.ChannelInfo.MultiKeyStatusList[1])
	require.Empty(t, multi.GetOtherSettings().UpstreamModelMetadata)
	_, disabledOtherKey := multi.ChannelInfo.MultiKeyStatusList[0]
	require.False(t, disabledOtherKey)

	var unrelated Channel
	require.NoError(t, db.First(&unrelated, channels[2].Id).Error)
	require.Equal(t, common.ChannelStatusEnabled, unrelated.Status)
	require.NotEmpty(t, unrelated.GetOtherSettings().UpstreamModelMetadata)

	var singleAbility Ability
	require.NoError(t, db.Where("channel_id = ?", channels[0].Id).First(&singleAbility).Error)
	require.False(t, singleAbility.Enabled)
	var multiAbility Ability
	require.NoError(t, db.Where("channel_id = ?", channels[1].Id).First(&multiAbility).Error)
	require.True(t, multiAbility.Enabled)
}
