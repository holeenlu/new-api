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

func TestApplyChannelErrorPolicyClassifiesClaudeRateLimitWithoutUsageLookup(t *testing.T) {
	tests := []struct {
		name     string
		message  string
		errType  string
		wantCode types.ErrorCode
	}{
		{
			name:     "account rate limit",
			message:  "This request would exceed your account's rate limit. Please try again later.",
			errType:  "rate_limit_error",
			wantCode: types.ErrorCodeUpstreamRateLimited,
		},
		{
			name:     "explicit subscription usage limit",
			message:  "You have reached your usage limit. Please try again after your plan resets.",
			errType:  "rate_limit_error",
			wantCode: types.ErrorCodeUpstreamUsageLimit,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := types.WithClaudeError(
				types.ClaudeError{Type: test.errType, Message: test.message},
				http.StatusTooManyRequests,
			)
			err.UpstreamStatusCode = http.StatusTooManyRequests

			got := ApplyChannelErrorPolicy(constant.ChannelTypeClaudeCode, err)

			require.Equal(t, test.wantCode, got.GetErrorCode())
		})
	}
}

func TestApplyChannelErrorPolicyUsesClaudeStructuredCode(t *testing.T) {
	tests := []struct {
		name     string
		code     types.ErrorCode
		wantCode types.ErrorCode
	}{
		{name: "usage window", code: "usage_limit_reached", wantCode: types.ErrorCodeUpstreamUsageLimit},
		{name: "permanent quota", code: "insufficient_quota", wantCode: types.ErrorCodeUpstreamQuotaExhausted},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := types.WithClaudeError(types.ClaudeError{
				Type:    "rate_limit_error",
				Message: "request rejected",
				Code:    test.code,
			}, http.StatusTooManyRequests)
			err.UpstreamStatusCode = http.StatusTooManyRequests

			classified := ApplyChannelErrorPolicy(constant.ChannelTypeClaudeCode, err)

			require.Equal(t, test.wantCode, classified.GetErrorCode())
		})
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

func TestIsSubscriptionOAuthUsageLimit(t *testing.T) {
	newErr := func(message string, upstreamStatus int) *types.NewAPIError {
		err := types.NewErrorWithStatusCode(errors.New(message), types.ErrorCodeBadResponseStatusCode, http.StatusTooManyRequests)
		err.UpstreamStatusCode = upstreamStatus
		return err
	}
	tests := []struct {
		name        string
		message     string
		upstream    int
		channelType int
		want        bool
	}{
		{"english usage limit on 429", "You've reached your usage limit for this plan", http.StatusTooManyRequests, constant.ChannelTypeCodex, true},
		{"provider usage limit code on 429", "request rejected", http.StatusTooManyRequests, constant.ChannelTypeCodex, true},
		{"chinese usage limit on 429", "你已达到使用上限，请稍后再试", http.StatusTooManyRequests, constant.ChannelTypeCodex, true},
		{"weekly limit on 429", "weekly limit exceeded", http.StatusTooManyRequests, constant.ChannelTypeClaudeCode, true},
		{"generic burst rate limit is not a usage limit", "rate limit exceeded, retry shortly", http.StatusTooManyRequests, constant.ChannelTypeCodex, false},
		{"usage limit text but not a 429", "usage limit reached", http.StatusInternalServerError, constant.ChannelTypeCodex, false},
		{"usage limit on non-subscription channel", "usage limit reached", http.StatusTooManyRequests, constant.ChannelTypeOpenAI, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := newErr(test.message, test.upstream)
			if test.name == "provider usage limit code on 429" {
				err.RelayError = types.OpenAIError{Type: "usage_limit_reached", Message: test.message}
			}
			require.Equal(t, test.want, IsSubscriptionOAuthUsageLimit(test.channelType, err))
		})
	}
	require.False(t, IsSubscriptionOAuthUsageLimit(constant.ChannelTypeCodex, nil))
	localCooldown := types.NewErrorWithStatusCode(
		errors.New("subscription OAuth usage window is exhausted"),
		types.ErrorCodeUpstreamUsageLimit,
		http.StatusServiceUnavailable,
	)
	require.True(t, IsSubscriptionOAuthUsageLimit(constant.ChannelTypeClaudeCode, localCooldown))
}

func TestSubscriptionOAuthUsageLimitFromResetWindowWithoutMarker(t *testing.T) {
	newBare429 := func(retryAfter time.Duration) *types.NewAPIError {
		err := types.WithClaudeError(
			types.ClaudeError{Type: "rate_limit_error", Message: "This request would exceed your account's rate limit."},
			http.StatusTooManyRequests,
		)
		err.UpstreamStatusCode = http.StatusTooManyRequests
		err.RetryAfter = retryAfter
		return err
	}

	// A subscription usage window resets in hours, well past the 15-minute burst
	// cap: even without any usage-limit wording it must classify as a usage limit
	// so routing cools the exhausted credential for its whole window.
	window := newBare429(3 * time.Hour)
	require.True(t, IsSubscriptionOAuthUsageLimit(constant.ChannelTypeClaudeCode, window))
	require.Equal(t, types.ErrorCodeUpstreamUsageLimit,
		ApplyChannelErrorPolicy(constant.ChannelTypeClaudeCode, window).GetErrorCode())

	// A genuine seconds-level burst stays a transient rate limit.
	burst := newBare429(5 * time.Second)
	require.False(t, IsSubscriptionOAuthUsageLimit(constant.ChannelTypeClaudeCode, burst))
	require.Equal(t, types.ErrorCodeUpstreamRateLimited,
		ApplyChannelErrorPolicy(constant.ChannelTypeClaudeCode, burst).GetErrorCode())
}

func TestSubscriptionOAuthCredentialCooldownForError(t *testing.T) {
	newErr := func(message string, retryAfter time.Duration) *types.NewAPIError {
		err := types.NewErrorWithStatusCode(errors.New(message), types.ErrorCodeBadResponseStatusCode, http.StatusTooManyRequests)
		err.UpstreamStatusCode = http.StatusTooManyRequests
		err.RetryAfter = retryAfter
		return err
	}
	usageLimit := func(retryAfter time.Duration) *types.NewAPIError {
		return newErr("You've reached your usage limit", retryAfter)
	}
	burst := func(retryAfter time.Duration) *types.NewAPIError {
		return newErr("rate limit exceeded", retryAfter)
	}

	// Usage-limit exhaustion: use the long default only when no reset window is
	// available, honor exact short and weekly resets, and cap a huge window so it cannot
	// sideline the credential indefinitely.
	require.Equal(t, subscriptionOAuthUsageLimitCooldown, SubscriptionOAuthCredentialCooldownForError(constant.ChannelTypeCodex, usageLimit(0)))
	require.Equal(t, 30*time.Second, SubscriptionOAuthCredentialCooldownForError(constant.ChannelTypeCodex, usageLimit(30*time.Second)))
	require.Equal(t, 2*time.Hour, SubscriptionOAuthCredentialCooldownForError(constant.ChannelTypeCodex, usageLimit(2*time.Hour)))
	require.Equal(t, 6*24*time.Hour, SubscriptionOAuthCredentialCooldownForError(constant.ChannelTypeCodex, usageLimit(6*24*time.Hour)))
	require.Equal(t, maximumSubscriptionOAuthUsageLimitCooldown, SubscriptionOAuthCredentialCooldownForError(constant.ChannelTypeCodex, usageLimit(30*24*time.Hour)))

	// A transient burst 429 whose reset stays within the 15m cap keeps its short
	// transient cooldown.
	require.Equal(t, 5*time.Second, SubscriptionOAuthCredentialCooldownForError(constant.ChannelTypeCodex, burst(5*time.Second)))
	require.Equal(t, 10*time.Minute, SubscriptionOAuthCredentialCooldownForError(constant.ChannelTypeCodex, burst(10*time.Minute)))

	// A bare 429 whose reset exceeds the 15m burst cap is a usage window, not a
	// burst: it is cooled for its whole reset (honored from err.RetryAfter)
	// instead of being re-probed after the transient cap, even without any
	// usage-limit wording.
	require.Equal(t, 1*time.Hour, SubscriptionOAuthCredentialCooldownForError(constant.ChannelTypeCodex, burst(1*time.Hour)))
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

func TestSubscriptionOAuthAuthorizationStatusRequiresExplicitEvidenceForQuarantine(t *testing.T) {
	for _, test := range []struct {
		name       string
		message    string
		statusCode int
		persistent bool
	}{
		{name: "bare unauthorized", message: "upstream rejected request", statusCode: http.StatusUnauthorized},
		{name: "bare forbidden", message: "request is not permitted", statusCode: http.StatusForbidden},
		{name: "expired access token", message: "access token expired", statusCode: http.StatusUnauthorized},
		{name: "generic forbidden marker", message: "oauth_forbidden", statusCode: http.StatusForbidden},
		{name: "revoked refresh token", message: "refresh token has been revoked", statusCode: http.StatusUnauthorized, persistent: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := types.NewOpenAIError(errors.New(test.message), types.ErrorCodeBadResponseStatusCode, test.statusCode)
			err.UpstreamStatusCode = test.statusCode
			classified := ApplyChannelErrorPolicy(constant.ChannelTypeClaudeCode, err)

			require.True(t, IsSubscriptionOAuthAccountUnavailable(constant.ChannelTypeClaudeCode, classified))
			require.Equal(t, test.persistent, IsSubscriptionOAuthPersistentAccountFailure(constant.ChannelTypeClaudeCode, classified))
			require.Equal(t, test.persistent, ShouldDisableChannelForType(constant.ChannelTypeClaudeCode, classified))
			if !test.persistent {
				require.False(t, QuarantineSubscriptionOAuthCredential(types.ChannelError{
					ChannelId:   1,
					ChannelType: constant.ChannelTypeClaudeCode,
					UsingKey:    "still-valid-token",
				}, classified))
			}
		})
	}
}

func TestSubscriptionOAuthAuthorizationClassificationIsIdempotent(t *testing.T) {
	bare := types.NewOpenAIError(
		errors.New("upstream rejected request"),
		types.ErrorCodeBadResponseStatusCode,
		http.StatusUnauthorized,
	)
	bare.UpstreamStatusCode = http.StatusUnauthorized
	classified := ApplyChannelErrorPolicy(constant.ChannelTypeClaudeCode, bare)
	classified = ApplyChannelErrorPolicy(constant.ChannelTypeClaudeCode, classified)
	require.False(t, IsSubscriptionOAuthPersistentAccountFailure(constant.ChannelTypeClaudeCode, classified))

	explicit := types.NewOpenAIError(
		errors.New("authentication failed"),
		types.ErrorCodeBadResponseStatusCode,
		http.StatusTooManyRequests,
	)
	explicit.RelayError = types.OpenAIError{
		Message: "authentication failed",
		Code:    types.ErrorCodeOAuthUnauthorized,
	}
	classified = ApplyChannelErrorPolicy(constant.ChannelTypeClaudeCode, explicit)
	require.False(t, IsSubscriptionOAuthPersistentAccountFailure(constant.ChannelTypeClaudeCode, classified))
}

func TestCodexExpiredAccessTokenRefreshesBeforeQuarantine(t *testing.T) {
	err := types.NewOpenAIError(errors.New("access token expired"), types.ErrorCodeBadResponseStatusCode, http.StatusUnauthorized)
	err.UpstreamStatusCode = http.StatusUnauthorized
	classified := ApplyChannelErrorPolicy(constant.ChannelTypeCodex, err)

	require.True(t, ShouldRefreshCodexOAuthCredential(constant.ChannelTypeCodex, classified))
	require.False(t, ShouldRefreshCodexOAuthCredential(constant.ChannelTypeClaudeCode, classified))
	require.False(t, IsSubscriptionOAuthPersistentAccountFailure(constant.ChannelTypeCodex, classified))
}

func TestPermanentCodexOAuthRefreshFailureRequiresTokenEndpointEvidence(t *testing.T) {
	require.True(t, IsPermanentCodexOAuthRefreshFailure(&CodexOAuthUpstreamError{
		StatusCode: http.StatusBadRequest,
		Message:    "OAuth credential refresh was rejected; reauthorize the account",
	}))
	require.False(t, IsPermanentCodexOAuthRefreshFailure(&CodexOAuthUpstreamError{
		StatusCode: http.StatusServiceUnavailable,
		Message:    "OAuth authorization service is temporarily unavailable",
	}))
	require.False(t, IsPermanentCodexOAuthRefreshFailure(errors.New("context deadline exceeded")))
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
		{name: "ambiguous current quota", message: "You exceeded your current quota", statusCode: http.StatusTooManyRequests, wantCode: types.ErrorCodeUpstreamRateLimited},
		{name: "chinese insufficient balance", message: "余额不足", statusCode: http.StatusForbidden, wantCode: types.ErrorCodeUpstreamQuotaExhausted},
		{name: "ambiguous chinese quota", message: "额度已用完", statusCode: http.StatusForbidden, wantCode: types.ErrorCodeOAuthForbidden},
		{name: "ambiguous chinese call limit", message: "调用次数已达上限", statusCode: http.StatusTooManyRequests, wantCode: types.ErrorCodeUpstreamRateLimited},
		{name: "provider error code", message: "quota rejected", code: "insufficient_quota", statusCode: http.StatusTooManyRequests, wantCode: types.ErrorCodeUpstreamQuotaExhausted},
		{name: "credit exhausted", message: "credit exhausted", statusCode: http.StatusTooManyRequests, wantCode: types.ErrorCodeUpstreamQuotaExhausted},
		{name: "ambiguous out of quota", message: "out of quota", statusCode: http.StatusTooManyRequests, wantCode: types.ErrorCodeUpstreamRateLimited},
		{name: "ambiguous no quota available", message: "no quota available", statusCode: http.StatusTooManyRequests, wantCode: types.ErrorCodeUpstreamRateLimited},
		{name: "stream unauthorized code", message: "authentication failed", code: types.ErrorCodeOAuthUnauthorized, statusCode: http.StatusTooManyRequests, wantCode: types.ErrorCodeOAuthUnauthorized},
		{name: "stream forbidden code", message: "permission denied", code: types.ErrorCodeOAuthForbidden, statusCode: http.StatusTooManyRequests, wantCode: types.ErrorCodeOAuthForbidden},
		{name: "ordinary rate limit", message: "too many requests", code: "rate_limit_error", statusCode: http.StatusTooManyRequests, wantCode: types.ErrorCodeUpstreamRateLimited},
		{name: "usage window exhausted", message: "You've reached your usage limit", code: "usage_limit_reached", statusCode: http.StatusTooManyRequests, wantCode: types.ErrorCodeUpstreamUsageLimit},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := types.NewOpenAIError(errors.New(test.message), test.code, test.statusCode)
			got := ApplyChannelErrorPolicy(constant.ChannelTypeCodex, err)

			require.Equal(t, test.wantCode, got.GetErrorCode())
			require.Equal(t, test.statusCode, got.GetUpstreamStatusCode())
			require.True(t, types.IsSkipRetryError(got))
			wantUnavailable := test.wantCode != types.ErrorCodeUpstreamRateLimited && test.wantCode != types.ErrorCodeUpstreamUsageLimit
			require.Equal(t, wantUnavailable, IsSubscriptionOAuthAccountUnavailable(constant.ChannelTypeCodex, got))
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
