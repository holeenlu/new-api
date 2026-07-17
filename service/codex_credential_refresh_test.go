package service

import (
	"context"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRefreshCodexChannelCredentialSkipsRotationWhenCredentialAlreadyChanged(t *testing.T) {
	truncate(t)

	credentialJSON, err := common.Marshal(dto.CodexOAuthCredential{
		AccessToken:  "new-access-token",
		RefreshToken: "new-refresh-token",
		AccountID:    "account-id",
		Type:         "codex",
	})
	require.NoError(t, err)
	require.NoError(t, model.DB.Create(&model.Channel{
		Id:   1,
		Type: constant.ChannelTypeCodex,
		Key:  string(credentialJSON),
	}).Error)

	credential, channel, err := RefreshCodexChannelCredential(
		context.Background(),
		1,
		CodexCredentialRefreshOptions{ExpectedAccessToken: "stale-access-token"},
	)

	require.NoError(t, err)
	require.NotNil(t, credential)
	require.NotNil(t, channel)
	assert.Equal(t, "new-access-token", credential.AccessToken)
	assert.Equal(t, string(credentialJSON), channel.Key)
}

func TestReplaceCodexChannelCredentialDoesNotOverwriteConcurrentUpdate(t *testing.T) {
	truncate(t)
	require.NoError(t, model.DB.Create(&model.Channel{
		Id:   2,
		Type: constant.ChannelTypeCodex,
		Key:  "credential-before-refresh",
	}).Error)
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 2).Update("key", "credential-from-reauthorization").Error)

	err := replaceCodexChannelCredential(2, "credential-before-refresh", "credential-from-refresh")

	require.ErrorIs(t, err, errCodexCredentialChanged)
	channel, err := model.GetChannelById(2, true)
	require.NoError(t, err)
	assert.Equal(t, "credential-from-reauthorization", channel.Key)
}

func TestCodexCredentialRefreshLockIsRemovedAfterLastCaller(t *testing.T) {
	const channelID = 987654
	release := acquireCodexChannelCredentialRefreshLock(channelID)

	codexChannelCredentialRefreshLocks.Lock()
	lock := codexChannelCredentialRefreshLocks.locks[channelID]
	require.NotNil(t, lock)
	require.Equal(t, 1, lock.refs)
	codexChannelCredentialRefreshLocks.Unlock()

	release()
	codexChannelCredentialRefreshLocks.Lock()
	_, exists := codexChannelCredentialRefreshLocks.locks[channelID]
	codexChannelCredentialRefreshLocks.Unlock()
	require.False(t, exists)
}
