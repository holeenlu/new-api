package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRetryParamCapacityFailureDoesNotConsumeUpstreamRetry(t *testing.T) {
	retryParam := &RetryParam{Retry: common.GetPointer(2)}
	retryParam.ResetRetryNextTry()
	retryParam.IncreaseRetry()
	require.Equal(t, 2, retryParam.GetRetry())
	retryParam.RecordAttempt()
	require.Equal(t, 0, retryParam.AttemptIndex())
	retryParam.RecordAttempt()
	require.Equal(t, 1, retryParam.AttemptIndex())
}

func TestSubscriptionOAuthRequestAttemptBudgetStopsAmplification(t *testing.T) {
	retryParam := &RetryParam{}
	require.True(t, retryParam.SetSubscriptionOAuthAttempt(1, 0, "credential-a"))
	for range maximumSubscriptionOAuthRequestAttempts {
		retryParam.RecordAttempt()
	}

	err := types.NewErrorWithStatusCode(
		errors.New("temporary upstream failure"),
		types.ErrorCodeDoRequestFailed,
		http.StatusServiceUnavailable,
	)
	decision, _ := retryParam.DecideSubscriptionOAuthContinuation(SubscriptionOAuthRetryObservation{
		ChannelType: constant.ChannelTypeCodex,
		Error:       err,
		Retryable:   true,
	})

	require.Equal(t, SubscriptionOAuthRetryStop, decision)
	require.False(t, retryParam.SetSubscriptionOAuthAttempt(2, 0, "credential-b"))
}

func TestSubscriptionOAuthCredentialRefreshIsClaimedOncePerRequest(t *testing.T) {
	retryParam := &RetryParam{}
	require.True(t, retryParam.SetSubscriptionOAuthAttempt(1, 0, "credential-a"))

	require.True(t, retryParam.ClaimSubscriptionOAuthCredentialRefresh())
	require.False(t, retryParam.ClaimSubscriptionOAuthCredentialRefresh())
}

func TestNewRetryParamFreezesAutoEffectiveGroup(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(c, constant.ContextKeyAutoGroup, "vip")

	retryParam := NewRetryParam(c, "auto", "gpt-test", "/v1/responses")
	require.Equal(t, "vip", retryParam.EffectiveGroup)

	common.SetContextKey(c, constant.ContextKeyAutoGroup, "default")
	require.Equal(t, "vip", retryParam.EffectiveGroup)
}

func TestRetryParamCapacityCycleBackoffIsBounded(t *testing.T) {
	retryParam := &RetryParam{}

	originalCycles := common.SubscriptionOAuthCapacityCycleTimes
	originalWait := common.SubscriptionOAuthCapacityWaitSeconds
	common.SubscriptionOAuthCapacityCycleTimes = 5
	common.SubscriptionOAuthCapacityWaitSeconds = 5
	t.Cleanup(func() {
		common.SubscriptionOAuthCapacityCycleTimes = originalCycles
		common.SubscriptionOAuthCapacityWaitSeconds = originalWait
	})

	first, ok := retryParam.NextCapacityCycleBackoff()
	require.True(t, ok)
	second, ok := retryParam.NextCapacityCycleBackoff()
	require.True(t, ok)
	third, ok := retryParam.NextCapacityCycleBackoff()
	require.True(t, ok)
	fourth, ok := retryParam.NextCapacityCycleBackoff()
	require.True(t, ok)

	require.GreaterOrEqual(t, first, 250*time.Millisecond)
	require.LessOrEqual(t, first, 350*time.Millisecond)
	require.GreaterOrEqual(t, second, 500*time.Millisecond)
	require.LessOrEqual(t, second, 600*time.Millisecond)
	require.GreaterOrEqual(t, third, time.Second)
	require.LessOrEqual(t, third, 1100*time.Millisecond)
	require.GreaterOrEqual(t, fourth, 2*time.Second)
	require.LessOrEqual(t, fourth, 2100*time.Millisecond)
	_, ok = retryParam.NextCapacityCycleBackoff()
	require.False(t, ok)
}

func TestSubscriptionOAuthRetrySwitchesAfterFiveCredentialFailures(t *testing.T) {
	originalRetries := common.SubscriptionOAuthUpstreamRetryTimes
	common.SubscriptionOAuthUpstreamRetryTimes = 5
	t.Cleanup(func() { common.SubscriptionOAuthUpstreamRetryTimes = originalRetries })

	retryParam := &RetryParam{}
	retryParam.SetSubscriptionOAuthAttempt(1, 0, "credential-a")
	for attempt := 1; attempt < 5; attempt++ {
		require.Equal(
			t,
			SubscriptionOAuthRetryCurrentCredential,
			retryParam.decideSubscriptionOAuthRetry(false, true, true, false, http.StatusServiceUnavailable, 0),
		)
	}
	require.Equal(
		t,
		SubscriptionOAuthSwitchCredential,
		retryParam.decideSubscriptionOAuthRetry(false, true, true, false, http.StatusServiceUnavailable, 0),
	)
	require.Nil(t, retryParam.SubscriptionOAuthAttemptTarget())
}

func TestSubscriptionOAuthRetryStopsAmbiguousPostWriteFailure(t *testing.T) {
	retryParam := &RetryParam{}
	retryParam.SetSubscriptionOAuthAttempt(1, 0, "credential-a")
	require.Equal(
		t,
		SubscriptionOAuthRetryStop,
		retryParam.decideSubscriptionOAuthRetry(true, false, false, false, http.StatusBadGateway, 0),
	)
}

func TestSubscriptionOAuthWrittenRequestStopsBeforeFailureBudget(t *testing.T) {
	originalRetries := common.SubscriptionOAuthUpstreamRetryTimes
	common.SubscriptionOAuthUpstreamRetryTimes = 5
	t.Cleanup(func() { common.SubscriptionOAuthUpstreamRetryTimes = originalRetries })

	retryParam := &RetryParam{}
	retryParam.SetSubscriptionOAuthAttempt(1, 0, "credential-budget")
	for range 3 {
		require.Equal(
			t,
			SubscriptionOAuthRetryCurrentCredential,
			retryParam.decideSubscriptionOAuthRetry(false, true, true, false, http.StatusServiceUnavailable, 0),
		)
	}
	require.Equal(t, SubscriptionOAuthRetryStop,
		retryParam.decideSubscriptionOAuthRetry(true, false, false, false, http.StatusBadGateway, 0))
}

func TestSubscriptionOAuthRetryZeroDisablesAmbiguousRetry(t *testing.T) {
	originalRetries := common.SubscriptionOAuthUpstreamRetryTimes
	common.SubscriptionOAuthUpstreamRetryTimes = 0
	t.Cleanup(func() { common.SubscriptionOAuthUpstreamRetryTimes = originalRetries })

	retryParam := &RetryParam{}
	retryParam.SetSubscriptionOAuthAttempt(1, 0, "credential-no-retry")
	require.Equal(
		t,
		SubscriptionOAuthRetryStop,
		retryParam.decideSubscriptionOAuthRetry(true, false, false, false, http.StatusBadGateway, 0),
	)
}

func TestSubscriptionOAuth429AlwaysCoolsCredentialAndSwitchesOnlyWhenEnabled(t *testing.T) {
	fingerprint := SubscriptionOAuthCredentialFingerprint(
		constant.ChannelTypeCodex,
		1,
		0,
		`{"account_id":"rate-limited-account"}`,
	)
	state := replaceSubscriptionOAuthStateForTest(t, fingerprint)
	originalRetry429 := common.SubscriptionOAuthRetry429
	t.Cleanup(func() { common.SubscriptionOAuthRetry429 = originalRetry429 })

	common.SubscriptionOAuthRetry429 = false
	retryParam := &RetryParam{}
	retryParam.SetSubscriptionOAuthAttempt(1, 0, fingerprint)
	require.Equal(
		t,
		SubscriptionOAuthRetryStop,
		retryParam.decideSubscriptionOAuthRetry(false, true, true, false, http.StatusTooManyRequests, 12*time.Second),
	)
	state.mu.Lock()
	require.True(t, state.recoveryPending)
	require.Equal(t, 12*time.Second, state.cooldownDuration)
	state.mu.Unlock()

	state.mu.Lock()
	state.generation = 1
	state.recoveryPending = false
	state.cooldownUntil = time.Time{}
	state.mu.Unlock()
	common.SubscriptionOAuthRetry429 = true
	retryParam.SetSubscriptionOAuthAttempt(1, 0, fingerprint)
	require.Equal(
		t,
		SubscriptionOAuthSwitchCredential,
		retryParam.decideSubscriptionOAuthRetry(false, true, true, false, http.StatusTooManyRequests, time.Second),
	)
}

func TestSubscriptionOAuthUsageLimitAlwaysSwitchesToBackupCredential(t *testing.T) {
	credential := `{"account_id":"usage-limited-account"}`
	fingerprint := SubscriptionOAuthCredentialFingerprint(
		constant.ChannelTypeClaudeCode,
		7,
		0,
		credential,
	)
	state := replaceSubscriptionOAuthStateForTest(t, fingerprint)
	originalRetry429 := common.SubscriptionOAuthRetry429
	common.SubscriptionOAuthRetry429 = false
	t.Cleanup(func() { common.SubscriptionOAuthRetry429 = originalRetry429 })

	initial := &model.Channel{
		Id: 7, Type: constant.ChannelTypeClaudeCode, Status: common.ChannelStatusEnabled,
		Key: credential, Group: "default",
	}
	backup := &model.Channel{
		Id: 8, Type: constant.ChannelTypeClaudeCode, Status: common.ChannelStatusEnabled,
		Key: `{"account_id":"usage-available-account"}`, Group: "default",
	}
	retryParam := &RetryParam{Boundary: NewRetryBoundary(initial, "default")}
	retryParam.SetSubscriptionOAuthAttempt(7, 0, fingerprint)
	err := types.NewErrorWithStatusCode(
		errors.New("You've reached your usage limit"),
		types.ErrorCodeUpstreamUsageLimit,
		http.StatusTooManyRequests,
	)
	err.UpstreamStatusCode = http.StatusTooManyRequests
	err.RetryAfter = 5 * time.Hour

	decision, cooldown := retryParam.DecideSubscriptionOAuthContinuation(SubscriptionOAuthRetryObservation{
		ChannelType: constant.ChannelTypeClaudeCode,
		Error:       err,
		Retryable:   true,
	})

	require.Equal(t, SubscriptionOAuthSwitchCredential, decision)
	require.Equal(t, 5*time.Hour, cooldown)
	require.Nil(t, retryParam.SubscriptionOAuthAttemptTarget())
	require.False(t, retryParam.Boundary.Allows(initial))
	require.True(t, retryParam.Boundary.Allows(backup))
	state.mu.Lock()
	recoveryPending := state.recoveryPending
	cooldownDuration := state.cooldownDuration
	state.mu.Unlock()
	require.True(t, recoveryPending)
	require.Equal(t, 5*time.Hour, cooldownDuration)
}

func TestSubscriptionOAuthUsageLimitDoesNotReplayUnsafeRequests(t *testing.T) {
	for _, observation := range []SubscriptionOAuthRetryObservation{
		{DownstreamStarted: true},
		{RetryDisabled: true},
		{SpecificChannel: true},
	} {
		retryParam := &RetryParam{}
		retryParam.SetSubscriptionOAuthAttempt(8, 0, "usage-limited-pinned")
		err := types.NewErrorWithStatusCode(
			errors.New("usage_limit_reached"),
			types.ErrorCodeUpstreamUsageLimit,
			http.StatusTooManyRequests,
		)
		err.UpstreamStatusCode = http.StatusTooManyRequests
		observation.ChannelType = constant.ChannelTypeCodex
		observation.Error = err
		observation.Retryable = true

		decision, _ := retryParam.DecideSubscriptionOAuthContinuation(observation)

		require.Equal(t, SubscriptionOAuthRetryStop, decision)
		require.Nil(t, retryParam.SubscriptionOAuthAttemptTarget())
	}
}

func TestSubscriptionOAuthRateLimitCooldownSwitchesCredential(t *testing.T) {
	initial := &model.Channel{
		Id: 12, Type: constant.ChannelTypeClaudeCode, Status: common.ChannelStatusEnabled,
		Key: "rate-limited-key", Group: "default",
	}
	backup := &model.Channel{
		Id: 13, Type: constant.ChannelTypeClaudeCode, Status: common.ChannelStatusEnabled,
		Key: "backup-key", Group: "default",
	}
	fingerprint := SubscriptionOAuthCredentialFingerprint(initial.Type, initial.Id, 0, initial.Key)
	retryParam := &RetryParam{Boundary: NewRetryBoundary(initial, "default")}
	retryParam.SetSubscriptionOAuthAttempt(initial.Id, 0, fingerprint)
	apiError := types.NewErrorWithStatusCode(
		&subscriptionOAuthCapacityError{
			cause:          errSubscriptionOAuthCredentialCool,
			retryAfter:     15 * time.Minute,
			cooldownReason: subscriptionOAuthCooldownRateLimit,
		},
		types.ErrorCodeUpstreamRateLimited,
		http.StatusTooManyRequests,
	)
	apiError.RetryAfter = 15 * time.Minute

	decision, retryAfter := retryParam.DecideSubscriptionOAuthContinuation(SubscriptionOAuthRetryObservation{
		ChannelType: initial.Type,
		Error:       apiError,
		Retryable:   true,
	})

	require.Equal(t, SubscriptionOAuthSwitchCredential, decision)
	require.Equal(t, 15*time.Minute, retryAfter)
	require.Nil(t, retryParam.SubscriptionOAuthAttemptTarget())
	require.False(t, retryParam.Boundary.Allows(initial))
	require.True(t, retryParam.Boundary.Allows(backup))
	require.False(t, retryParam.Boundary.HasCapacityExclusions())
}

func TestSubscriptionOAuthCapacityReplayPreservesCredentialOrder(t *testing.T) {
	boundary := NewRetryBoundary(&model.Channel{
		Id:     1,
		Type:   constant.ChannelTypeCodex,
		Status: common.ChannelStatusEnabled,
		Group:  "default",
	}, "default")
	require.NotNil(t, boundary)
	retryParam := &RetryParam{Boundary: boundary}
	for index, fingerprint := range []string{"credential-a", "credential-b", "credential-c"} {
		retryParam.SetSubscriptionOAuthAttempt(index+1, 0, fingerprint)
		retryParam.handleSubscriptionOAuthCapacityFailure()
	}

	require.True(t, retryParam.RestartSubscriptionOAuthCapacityCycle())
	for index, fingerprint := range []string{"credential-a", "credential-b", "credential-c"} {
		target := retryParam.SubscriptionOAuthAttemptTarget()
		require.NotNil(t, target)
		require.Equal(t, index+1, target.ChannelID)
		require.Equal(t, fingerprint, target.Fingerprint)
		retryParam.handleSubscriptionOAuthCapacityFailure()
	}
	require.Nil(t, retryParam.SubscriptionOAuthAttemptTarget())
}

func TestSubscriptionOAuthFailedRecoveryProbeSwitchesImmediately(t *testing.T) {
	fingerprint := SubscriptionOAuthCredentialFingerprint(
		constant.ChannelTypeCodex,
		1,
		0,
		`{"account_id":"retry-recovery-account"}`,
	)
	state := replaceSubscriptionOAuthStateForTest(t, fingerprint)
	require.True(t, CooldownSubscriptionOAuthCredential(fingerprint, 1, time.Minute))
	state.mu.Lock()
	state.cooldownUntil = time.Now().Add(-time.Second)
	state.mu.Unlock()

	lease, err := AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 10, 0)
	require.NoError(t, err)
	require.True(t, lease.IsRecoveryProbe())
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	retryParam := NewRetryParam(c, "default", "gpt-test", "/v1/responses")
	retryParam.SetSubscriptionOAuthAttempt(1, 0, fingerprint)
	BindSubscriptionOAuthLease(c, lease)
	retryParam.CaptureSubscriptionOAuthAttemptMetadata(c)
	lease.Release()

	require.Equal(
		t,
		SubscriptionOAuthSwitchCredential,
		retryParam.decideSubscriptionOAuthRetry(false, true, true, false, http.StatusServiceUnavailable, 0),
	)
}

func TestSubscriptionOAuthRetry429CannotBypassStartedResponse(t *testing.T) {
	originalRetry429 := common.SubscriptionOAuthRetry429
	common.SubscriptionOAuthRetry429 = true
	t.Cleanup(func() { common.SubscriptionOAuthRetry429 = originalRetry429 })

	retryParam := &RetryParam{}
	retryParam.SetSubscriptionOAuthAttempt(1, 0, "credential-429")
	require.Equal(
		t,
		SubscriptionOAuthRetryStop,
		retryParam.decideSubscriptionOAuthRetry(true, true, true, true, http.StatusTooManyRequests, 0),
	)
}
