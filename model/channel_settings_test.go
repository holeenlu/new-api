package model

import (
	"testing"

	"github.com/QuantumNous/new-api/dto"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelValidateSettingsRequiresTagForTagRetryIsolation(t *testing.T) {
	channel := &Channel{}
	channel.SetOtherSettings(dto.ChannelOtherSettings{DataPolicy: &dto.ChannelDataPolicy{
		Provider:       "OpenAI",
		Region:         "us",
		Retention:      "zero",
		Training:       dto.DataTrainingDisabled,
		RetryIsolation: dto.RetryIsolationTag,
	}})

	err := channel.ValidateSettings()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tag is required")

	channel.SetTag("openai-vip")
	require.NoError(t, channel.ValidateSettings())
}
