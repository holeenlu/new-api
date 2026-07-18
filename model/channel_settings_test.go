package model

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
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

func TestSubscriptionOAuthLegacyTagIsolationDoesNotRequireTag(t *testing.T) {
	channel := &Channel{Type: constant.ChannelTypeCodex}
	channel.SetOtherSettings(dto.ChannelOtherSettings{DataPolicy: &dto.ChannelDataPolicy{
		Provider:       "OpenAI",
		Region:         "us",
		Retention:      "zero",
		Training:       dto.DataTrainingDisabled,
		RetryIsolation: dto.RetryIsolationTag,
	}})

	require.NoError(t, channel.ValidateSettings())
}

func TestAdvancedCustomChannelRequiresModelListRouteWhenUpdateChecksEnabled(t *testing.T) {
	inferenceRoute := dto.AdvancedCustomRoute{
		IncomingPath: "/v1/chat/completions",
		UpstreamPath: "/v1/chat/completions",
		Converter:    "none",
	}
	channel := &Channel{Type: constant.ChannelTypeAdvancedCustom}
	channel.SetOtherSettings(dto.ChannelOtherSettings{
		UpstreamModelUpdateCheckEnabled: true,
		AdvancedCustom: &dto.AdvancedCustomConfig{
			Routes: []dto.AdvancedCustomRoute{inferenceRoute},
		},
	})
	require.ErrorContains(t, channel.ValidateSettings(), dto.AdvancedCustomModelListPath)

	channel.SetOtherSettings(dto.ChannelOtherSettings{
		UpstreamModelUpdateCheckEnabled: true,
		AdvancedCustom: &dto.AdvancedCustomConfig{
			Routes: []dto.AdvancedCustomRoute{
				inferenceRoute,
				{
					IncomingPath: dto.AdvancedCustomModelListPath,
					UpstreamPath: dto.AdvancedCustomModelListPath,
					Converter:    "none",
				},
			},
		},
	})
	require.NoError(t, channel.ValidateSettings())
}
