package service

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/types"

	"github.com/stretchr/testify/require"
)

func TestApplyChannelErrorPolicySkipsSubscriptionOAuthRetries(t *testing.T) {
	originalRetry429 := common.SubscriptionOAuthRetry429
	common.SubscriptionOAuthRetry429 = false
	t.Cleanup(func() { common.SubscriptionOAuthRetry429 = originalRetry429 })
	for _, channelType := range []int{constant.ChannelTypeClaudeCode, constant.ChannelTypeCodex} {
		err := types.NewOpenAIError(errors.New("rate limited"), types.ErrorCodeBadResponseStatusCode, http.StatusTooManyRequests)

		got := ApplyChannelErrorPolicy(channelType, err)

		require.NotSame(t, err, got)
		require.Equal(t, types.ErrorCodeUpstreamRateLimited, got.GetErrorCode())
		require.True(t, types.IsSkipRetryError(got))
	}
}

func TestApplyChannelErrorPolicyAllowsConfiguredOAuth429Retry(t *testing.T) {
	originalRetry429 := common.SubscriptionOAuthRetry429
	common.SubscriptionOAuthRetry429 = true
	t.Cleanup(func() { common.SubscriptionOAuthRetry429 = originalRetry429 })
	err := types.NewOpenAIError(errors.New("rate limited"), types.ErrorCodeBadResponseStatusCode, http.StatusTooManyRequests)

	got := ApplyChannelErrorPolicy(constant.ChannelTypeCodex, err)

	require.False(t, types.IsSkipRetryError(got))
	require.True(t, IsSubscriptionOAuthTransientError(constant.ChannelTypeCodex, got))
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

func TestIsSubscriptionOAuthConcurrencyLimit(t *testing.T) {
	err := types.NewErrorWithStatusCode(
		errors.New("subscription OAuth channel concurrency limit reached; retry later"),
		types.ErrorCodeOAuthChannelConcurrencyLimit,
		http.StatusServiceUnavailable,
	)

	for _, channelType := range []int{constant.ChannelTypeCodex, constant.ChannelTypeClaudeCode} {
		require.True(t, IsSubscriptionOAuthConcurrencyLimit(channelType, err))
	}
	require.False(t, IsSubscriptionOAuthConcurrencyLimit(constant.ChannelTypeOpenAI, err))
}

func TestSubscriptionOAuthTransientErrorDoesNotDisableChannel(t *testing.T) {
	for _, channelType := range []int{constant.ChannelTypeClaudeCode, constant.ChannelTypeCodex} {
		err := types.NewErrorWithStatusCode(
			errors.New("upstream connection closed before response headers"),
			types.ErrorCodeDoRequestFailed,
			http.StatusBadGateway,
		)

		require.False(t, ShouldDisableChannelForType(channelType, err))
	}
}

func TestSubscriptionOAuth429NeverAutoDisablesCredential(t *testing.T) {
	originalRetry429 := common.SubscriptionOAuthRetry429
	common.SubscriptionOAuthRetry429 = false
	t.Cleanup(func() { common.SubscriptionOAuthRetry429 = originalRetry429 })

	err := types.NewErrorWithStatusCode(errors.New("rate limited"), types.ErrorCodeBadResponseStatusCode, http.StatusServiceUnavailable)
	err.UpstreamStatusCode = http.StatusTooManyRequests
	for _, channelType := range []int{constant.ChannelTypeClaudeCode, constant.ChannelTypeCodex} {
		require.False(t, ShouldDisableChannelForType(channelType, err))
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
		err.UpstreamStatusCode = test.statusCode
		err.RetryAfter = 3 * time.Second

		got := ApplyChannelErrorPolicy(constant.ChannelTypeCodex, err)

		require.NotSame(t, err, got)
		require.Equal(t, test.wantCode, got.GetErrorCode())
		require.Equal(t, test.statusCode, got.StatusCode)
		require.Equal(t, test.statusCode, got.GetUpstreamStatusCode())
		require.Equal(t, 3*time.Second, got.RetryAfter)
		require.True(t, types.IsSkipRetryError(got))
	}
}

func TestApplyChannelErrorPolicyClassifiesOAuthAccountFailures(t *testing.T) {
	tests := []struct {
		name       string
		message    string
		code       types.ErrorCode
		statusCode int
		wantCode   types.ErrorCode
	}{
		{name: "organization disabled", message: "This organization has been disabled.", statusCode: http.StatusForbidden, wantCode: types.ErrorCodeUpstreamAccountDisabled},
		{name: "credit balance", message: "Your credit balance is too low", statusCode: http.StatusTooManyRequests, wantCode: types.ErrorCodeUpstreamQuotaExhausted},
		{name: "current quota", message: "You exceeded your current quota", statusCode: http.StatusTooManyRequests, wantCode: types.ErrorCodeUpstreamQuotaExhausted},
		{name: "chinese insufficient balance", message: "余额不足", statusCode: http.StatusForbidden, wantCode: types.ErrorCodeUpstreamQuotaExhausted},
		{name: "chinese quota exhausted", message: "额度已用完", statusCode: http.StatusForbidden, wantCode: types.ErrorCodeUpstreamQuotaExhausted},
		{name: "chinese call limit", message: "调用次数已达上限", statusCode: http.StatusTooManyRequests, wantCode: types.ErrorCodeUpstreamQuotaExhausted},
		{name: "provider error code", message: "quota rejected", code: "insufficient_quota", statusCode: http.StatusTooManyRequests, wantCode: types.ErrorCodeUpstreamQuotaExhausted},
		{name: "credit exhausted", message: "credit exhausted", statusCode: http.StatusTooManyRequests, wantCode: types.ErrorCodeUpstreamQuotaExhausted},
		{name: "out of quota", message: "out of quota", statusCode: http.StatusTooManyRequests, wantCode: types.ErrorCodeUpstreamQuotaExhausted},
		{name: "no quota available", message: "no quota available", statusCode: http.StatusTooManyRequests, wantCode: types.ErrorCodeUpstreamQuotaExhausted},
		{name: "ordinary rate limit", message: "too many requests", code: "rate_limit_error", statusCode: http.StatusTooManyRequests, wantCode: types.ErrorCodeUpstreamRateLimited},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := types.NewOpenAIError(errors.New(test.message), test.code, test.statusCode)
			got := ApplyChannelErrorPolicy(constant.ChannelTypeCodex, err)

			require.Equal(t, test.wantCode, got.GetErrorCode())
			require.Equal(t, test.statusCode, got.GetUpstreamStatusCode())
			require.True(t, types.IsSkipRetryError(got))
			require.Equal(t, test.wantCode != types.ErrorCodeUpstreamRateLimited, IsSubscriptionOAuthAccountUnavailable(constant.ChannelTypeCodex, got))
		})
	}
}

func TestApplyChannelErrorPolicyClassifiesUnsupportedModel(t *testing.T) {
	err := types.NewOpenAIError(errors.New("model gpt-missing is not supported"), types.ErrorCodeBadResponseStatusCode, http.StatusBadRequest)

	got := ApplyChannelErrorPolicy(constant.ChannelTypeClaudeCode, err)

	require.Equal(t, types.ErrorCodeModelNotSupported, got.GetErrorCode())
	require.Equal(t, http.StatusBadRequest, got.StatusCode)
	require.True(t, types.IsSkipRetryError(got))
}

func TestApplyChannelErrorPolicyClassifiesModelCapacitySeparatelyFromRateLimit(t *testing.T) {
	err := types.NewOpenAIError(
		errors.New("Selected model is at capacity. Please try a different model."),
		types.ErrorCodeBadResponseStatusCode,
		http.StatusTooManyRequests,
	)

	got := ApplyChannelErrorPolicy(constant.ChannelTypeCodex, err)

	require.Equal(t, types.ErrorCodeModelAtCapacity, got.GetErrorCode())
	require.True(t, IsSubscriptionOAuthModelAtCapacity(constant.ChannelTypeCodex, got))
	require.False(t, IsSubscriptionOAuthAccountUnavailable(constant.ChannelTypeCodex, got))
	require.True(t, types.IsSkipRetryError(got))
}

func TestApplyChannelErrorPolicyClassifiesResponsesStreamOverloadAsCapacity(t *testing.T) {
	err := types.NewOpenAIError(
		errors.New("Our servers are currently overloaded"),
		types.ErrorCodeBadResponseStatusCode,
		http.StatusTooManyRequests,
	)
	err.RelayError = &types.OpenAIError{Type: "service_unavailable_error", Code: "server_is_overloaded"}

	got := ApplyChannelErrorPolicy(constant.ChannelTypeCodex, err)

	require.Equal(t, types.ErrorCodeModelAtCapacity, got.GetErrorCode())
	require.True(t, IsSubscriptionOAuthModelAtCapacity(constant.ChannelTypeCodex, got))
}

func TestApplyChannelErrorPolicyClassifiesModelErrorCodeWithoutMessageHint(t *testing.T) {
	err := types.NewOpenAIError(errors.New("resource unavailable"), types.ErrorCodeModelNotFound, http.StatusNotFound)

	got := ApplyChannelErrorPolicy(constant.ChannelTypeCodex, err)

	require.Equal(t, types.ErrorCodeModelNotSupported, got.GetErrorCode())
	require.True(t, IsSubscriptionOAuthModelUnavailable(constant.ChannelTypeCodex, got))
}

func TestSubscriptionOAuthCredentialFailureScopes(t *testing.T) {
	unauthorized := ApplyChannelErrorPolicy(
		constant.ChannelTypeCodex,
		types.NewOpenAIError(errors.New("unauthorized"), types.ErrorCodeBadResponseStatusCode, http.StatusUnauthorized),
	)
	unsupportedModel := ApplyChannelErrorPolicy(
		constant.ChannelTypeCodex,
		types.NewOpenAIError(errors.New("model gpt-missing is not supported"), types.ErrorCodeBadResponseStatusCode, http.StatusBadRequest),
	)

	require.True(t, IsSubscriptionOAuthAccountUnavailable(constant.ChannelTypeCodex, unauthorized))
	require.False(t, IsSubscriptionOAuthModelUnavailable(constant.ChannelTypeCodex, unauthorized))
	require.True(t, IsSubscriptionOAuthModelUnavailable(constant.ChannelTypeCodex, unsupportedModel))
	require.False(t, IsSubscriptionOAuthAccountUnavailable(constant.ChannelTypeCodex, unsupportedModel))
	require.False(t, IsSubscriptionOAuthAccountUnavailable(constant.ChannelTypeOpenAI, unauthorized))
}
