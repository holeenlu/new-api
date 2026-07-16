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
