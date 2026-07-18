package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"

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
			retryParam.DecideSubscriptionOAuthRetry(false, true, true, false, http.StatusServiceUnavailable, 0),
		)
	}
	require.Equal(
		t,
		SubscriptionOAuthSwitchCredential,
		retryParam.DecideSubscriptionOAuthRetry(false, true, true, false, http.StatusServiceUnavailable, 0),
	)
	require.Nil(t, retryParam.SubscriptionOAuthAttemptTarget())
}

func TestSubscriptionOAuthRetryLimitsAmbiguousPostWriteFailure(t *testing.T) {
	retryParam := &RetryParam{}
	retryParam.SetSubscriptionOAuthAttempt(1, 0, "credential-a")
	require.Equal(
		t,
		SubscriptionOAuthRetryCurrentCredential,
		retryParam.DecideSubscriptionOAuthRetry(true, false, false, false, http.StatusBadGateway, 0),
	)
	require.Equal(
		t,
		SubscriptionOAuthSwitchCredential,
		retryParam.DecideSubscriptionOAuthRetry(true, false, false, false, http.StatusBadGateway, 0),
	)
}

func TestSubscriptionOAuthAmbiguousFailureCannotExceedFailureBudget(t *testing.T) {
	originalRetries := common.SubscriptionOAuthUpstreamRetryTimes
	common.SubscriptionOAuthUpstreamRetryTimes = 5
	t.Cleanup(func() { common.SubscriptionOAuthUpstreamRetryTimes = originalRetries })

	retryParam := &RetryParam{}
	retryParam.SetSubscriptionOAuthAttempt(1, 0, "credential-budget")
	for range 4 {
		require.Equal(
			t,
			SubscriptionOAuthRetryCurrentCredential,
			retryParam.DecideSubscriptionOAuthRetry(false, true, true, false, http.StatusServiceUnavailable, 0),
		)
	}
	require.Equal(
		t,
		SubscriptionOAuthSwitchCredential,
		retryParam.DecideSubscriptionOAuthRetry(true, false, false, false, http.StatusBadGateway, 0),
	)
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
		retryParam.DecideSubscriptionOAuthRetry(true, false, false, false, http.StatusBadGateway, 0),
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
		retryParam.DecideSubscriptionOAuthRetry(false, true, true, false, http.StatusTooManyRequests, 12*time.Second),
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
		retryParam.DecideSubscriptionOAuthRetry(false, true, true, false, http.StatusTooManyRequests, time.Second),
	)
}

func TestSubscriptionOAuthCapacityReplayPreservesCredentialOrder(t *testing.T) {
	retryParam := &RetryParam{}
	for index, fingerprint := range []string{"credential-a", "credential-b", "credential-c"} {
		retryParam.SetSubscriptionOAuthAttempt(index+1, 0, fingerprint)
		retryParam.HandleSubscriptionOAuthCapacityFailure()
	}

	require.True(t, retryParam.StartCapacityReplay())
	for index, fingerprint := range []string{"credential-a", "credential-b", "credential-c"} {
		target := retryParam.SubscriptionOAuthAttemptTarget()
		require.NotNil(t, target)
		require.Equal(t, index+1, target.ChannelID)
		require.Equal(t, fingerprint, target.Fingerprint)
		retryParam.HandleSubscriptionOAuthCapacityFailure()
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
	BindSubscriptionOAuthLease(c, lease)
	retryParam := &RetryParam{}
	retryParam.SetSubscriptionOAuthAttempt(1, 0, fingerprint)
	retryParam.CaptureSubscriptionOAuthAttemptMetadata(c)
	lease.Release()

	require.Equal(
		t,
		SubscriptionOAuthSwitchCredential,
		retryParam.DecideSubscriptionOAuthRetry(false, true, true, false, http.StatusServiceUnavailable, 0),
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
		retryParam.DecideSubscriptionOAuthRetry(true, true, true, true, http.StatusTooManyRequests, 0),
	)
}
