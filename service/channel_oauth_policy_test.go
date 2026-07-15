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
	for _, statusCode := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 524} {
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

func TestApplyChannelErrorPolicyClassifiesOAuthAuthorizationErrors(t *testing.T) {
	tests := []struct {
		statusCode int
		wantCode   types.ErrorCode
	}{
		{statusCode: http.StatusUnauthorized, wantCode: types.ErrorCodeOAuthUnauthorized},
		{statusCode: http.StatusForbidden, wantCode: types.ErrorCodeOAuthForbidden},
	}

	for _, test := range tests {
		err := types.NewOpenAIError(errors.New("upstream rejected request"), types.ErrorCodeBadResponseStatusCode, test.statusCode)

		got := ApplyChannelErrorPolicy(constant.ChannelTypeCodex, err)

		require.Equal(t, test.wantCode, got.GetErrorCode())
		require.Equal(t, test.statusCode, got.StatusCode)
		require.True(t, types.IsSkipRetryError(got))
	}
}

func TestApplyChannelErrorPolicyClassifiesUnsupportedModel(t *testing.T) {
	err := types.NewOpenAIError(errors.New("model gpt-missing is not supported"), types.ErrorCodeBadResponseStatusCode, http.StatusBadRequest)

	got := ApplyChannelErrorPolicy(constant.ChannelTypeClaudeCode, err)

	require.Equal(t, types.ErrorCodeModelNotSupported, got.GetErrorCode())
	require.Equal(t, http.StatusBadRequest, got.StatusCode)
	require.True(t, types.IsSkipRetryError(got))
}
