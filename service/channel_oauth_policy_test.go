package service

import (
	"errors"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/types"

	"github.com/stretchr/testify/require"
)

func TestApplyChannelErrorPolicySkipsSubscriptionOAuthRetries(t *testing.T) {
	for _, channelType := range []int{constant.ChannelTypeClaudeCode, constant.ChannelTypeCodex} {
		err := types.NewOpenAIError(errors.New("rate limited"), types.ErrorCodeBadResponseStatusCode, http.StatusTooManyRequests)

		got := ApplyChannelErrorPolicy(channelType, err)

		require.Same(t, err, got)
		require.True(t, types.IsSkipRetryError(got))
	}
}

func TestApplyChannelErrorPolicyLeavesSubscriptionOAuthServerErrorsRetryable(t *testing.T) {
	for _, statusCode := range []int{http.StatusInternalServerError, http.StatusGatewayTimeout, 524} {
		for _, channelType := range []int{constant.ChannelTypeClaudeCode, constant.ChannelTypeCodex} {
			err := types.NewOpenAIError(errors.New("transient upstream failure"), types.ErrorCodeBadResponseStatusCode, statusCode)

			got := ApplyChannelErrorPolicy(channelType, err)

			require.Same(t, err, got)
			require.False(t, types.IsSkipRetryError(got))
			require.True(t, IsSubscriptionOAuthTransientError(channelType, got))
		}
	}
}

func TestApplyChannelErrorPolicyLeavesOtherChannelsUnchanged(t *testing.T) {
	err := types.NewOpenAIError(errors.New("rate limited"), types.ErrorCodeBadResponseStatusCode, http.StatusTooManyRequests)

	got := ApplyChannelErrorPolicy(constant.ChannelTypeAnthropic, err)

	require.Same(t, err, got)
	require.False(t, types.IsSkipRetryError(got))
}
