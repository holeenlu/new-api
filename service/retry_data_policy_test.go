package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func retryPolicyChannel(id int, channelType int, baseURL string, policy *dto.ChannelDataPolicy) *model.Channel {
	channel := &model.Channel{Id: id, Type: channelType, Status: common.ChannelStatusEnabled, BaseURL: common.GetPointer(baseURL), Key: fmt.Sprintf("key-%d", id), Group: "default"}
	channel.SetOtherSettings(dto.ChannelOtherSettings{DataPolicy: policy})
	return channel
}

func TestSubscriptionOAuthRetryBoundaryUsesFrozenGroupAndIgnoresTag(t *testing.T) {
	initial := retryPolicyChannel(1, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	initial.SetTag("openai-vip")
	alternate := retryPolicyChannel(2, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	alternate.SetTag("openai-vip")
	differentTag := retryPolicyChannel(3, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	differentTag.SetTag("openai-default")
	differentGroup := retryPolicyChannel(4, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	differentGroup.Group = "vip"
	differentGroup.SetTag("openai-vip")
	boundary := NewRetryBoundary(initial, "default")
	require.NotNil(t, boundary)
	assert.Equal(t, dto.RetryIsolationGroup, boundary.policy.RetryIsolation)

	boundary.MarkAttempt(initial)
	assert.True(t, boundary.Allows(initial))
	assert.True(t, boundary.Allows(alternate))
	assert.True(t, boundary.Allows(differentTag))
	assert.False(t, boundary.Allows(differentGroup))
}

func TestSubscriptionOAuthRetryBoundaryDoesNotRequireTag(t *testing.T) {
	initial := retryPolicyChannel(1, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	alternate := retryPolicyChannel(2, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	boundary := NewRetryBoundary(initial, "default")
	require.NotNil(t, boundary)
	assert.Equal(t, dto.RetryIsolationGroup, boundary.policy.RetryIsolation)

	boundary.MarkAttempt(initial)
	assert.True(t, boundary.Allows(alternate))
}

func TestSubscriptionOAuthRetryBoundaryMatchesMultiGroupExactly(t *testing.T) {
	initial := retryPolicyChannel(1, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	initial.Group = "default,vip"
	matching := retryPolicyChannel(2, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	matching.Group = "vip,enterprise"
	prefixOnly := retryPolicyChannel(3, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	prefixOnly.Group = "vip-plus"

	boundary := NewRetryBoundary(initial, "vip")
	require.True(t, boundary.Allows(matching))
	require.False(t, boundary.Allows(prefixOnly))
}

func TestSubscriptionOAuthRetryBoundarySharesCredentialIdentityAcrossGroups(t *testing.T) {
	initial := retryPolicyChannel(1, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	initial.Key = `{"access_token":"token-a","account_id":"shared-account"}`
	duplicate := retryPolicyChannel(2, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	duplicate.Group = "vip"
	duplicate.Key = `{"access_token":"token-b","account_id":"shared-account"}`

	boundary := NewRetryBoundary(initial, "default")
	boundary.FailCredential(SubscriptionOAuthCredentialFingerprint(initial.Type, initial.Id, 0, initial.Key))

	require.False(t, boundary.Allows(duplicate), "different groups must not create a second credential identity")
}

func TestSubscriptionOAuthExplicitProviderPolicyCannotCrossFrozenGroup(t *testing.T) {
	policy := &dto.ChannelDataPolicy{
		Provider:       "OpenAI",
		Region:         "us",
		Retention:      "zero",
		Training:       dto.DataTrainingDisabled,
		RetryIsolation: dto.RetryIsolationProvider,
	}
	initial := retryPolicyChannel(1, constant.ChannelTypeCodex, "https://chatgpt.com", policy)
	sameGroup := retryPolicyChannel(2, constant.ChannelTypeCodex, "https://chatgpt.com", policy)
	otherGroup := retryPolicyChannel(3, constant.ChannelTypeCodex, "https://chatgpt.com", policy)
	otherGroup.Group = "vip"

	boundary := NewRetryBoundary(initial, "default")
	require.True(t, boundary.Allows(sameGroup))
	require.False(t, boundary.Allows(otherGroup))
}

func TestRetryBoundaryDoesNotOverrideExplicitSubscriptionIsolation(t *testing.T) {
	policy := &dto.ChannelDataPolicy{RetryIsolation: dto.RetryIsolationChannel}
	initial := retryPolicyChannel(1, constant.ChannelTypeCodex, "https://chatgpt.com", policy)
	alternate := retryPolicyChannel(2, constant.ChannelTypeCodex, "https://chatgpt.com", policy)
	boundary := NewRetryBoundary(initial, "default")
	require.NotNil(t, boundary)

	boundary.MarkAttempt(initial)

	assert.True(t, boundary.Allows(initial))
	assert.False(t, boundary.Allows(alternate))
}

func TestSubscriptionOAuthGroupRetryDoesNotEnterChannelIsolatedCandidate(t *testing.T) {
	initial := retryPolicyChannel(1, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	isolated := retryPolicyChannel(2, constant.ChannelTypeCodex, "https://chatgpt.com", &dto.ChannelDataPolicy{
		RetryIsolation: dto.RetryIsolationChannel,
	})

	boundary := NewRetryBoundary(initial, "default")
	require.False(t, boundary.Allows(isolated))
}

func TestRetryBoundaryExcludesOnlySaturatedMultiKeyCredential(t *testing.T) {
	initial := retryPolicyChannel(1, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	initial.SetTag("openai-vip")
	initial.ChannelInfo.IsMultiKey = true
	initial.Key = "key-1\nkey-2"
	alternate := retryPolicyChannel(2, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	alternate.SetTag("openai-vip")
	boundary := NewRetryBoundary(initial, "default")
	require.NotNil(t, boundary)

	boundary.MarkAttempt(initial, 0)
	assert.True(t, boundary.Allows(initial))
	fingerprint := SubscriptionOAuthCredentialFingerprint(initial.Type, initial.Id, 0, "key-1")
	boundary.ExcludeCredential(fingerprint, initial.Id, 0)

	assert.True(t, boundary.Allows(initial))
	assert.True(t, boundary.Allows(alternate))
}

func TestRetryBoundaryAllowsAnotherKeyOnlyWithinCurrentChannel(t *testing.T) {
	initial := retryPolicyChannel(1, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	initial.ChannelInfo.IsMultiKey = true
	initial.Key = "key-1\nkey-2"
	alternate := retryPolicyChannel(2, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	boundary := NewRetryBoundary(initial, "default")
	require.NotNil(t, boundary)

	boundary.MarkAttempt(initial, 0)
	boundary.FailCredential(SubscriptionOAuthCredentialFingerprint(initial.Type, initial.Id, 0, "key-1"))
	assert.True(t, boundary.Allows(initial))
	assert.Equal(t, map[int]struct{}{0: {}}, boundary.UsedMultiKeyIndexes(initial.Id))
	boundary.MarkAttempt(initial, 1)
	boundary.FailCredential(SubscriptionOAuthCredentialFingerprint(initial.Type, initial.Id, 1, "key-2"))
	assert.False(t, boundary.Allows(initial))
	assert.False(t, boundary.Allows(alternate))
}

func TestRetryBoundaryFailsClosedForIncompleteBroaderPolicy(t *testing.T) {
	incomplete := &dto.ChannelDataPolicy{
		Region:         "us",
		Retention:      "zero",
		Training:       dto.DataTrainingDisabled,
		RetryIsolation: dto.RetryIsolationProvider,
	}
	initial := retryPolicyChannel(1, constant.ChannelTypeOpenAI, "https://api.openai.com/v1", incomplete)
	alternate := retryPolicyChannel(2, constant.ChannelTypeOpenAI, "https://api.openai.com/v1", incomplete)
	boundary := NewRetryBoundary(initial, "default")
	require.NotNil(t, boundary)

	boundary.MarkAttempt(initial)
	assert.False(t, boundary.Allows(alternate))
}

func TestRetryBoundaryAllowsOnlyMatchingProviderDataBoundary(t *testing.T) {
	policy := &dto.ChannelDataPolicy{
		Provider:       "OpenAI",
		Region:         "us",
		Retention:      "30-days",
		Training:       dto.DataTrainingDisabled,
		RetryIsolation: dto.RetryIsolationProvider,
	}
	initial := retryPolicyChannel(1, constant.ChannelTypeOpenAI, "https://api.openai.com/v1", policy)
	matching := retryPolicyChannel(2, constant.ChannelTypeOpenAI, "https://api.openai.com/v1/", policy)
	differentRegion := retryPolicyChannel(3, constant.ChannelTypeOpenAI, "https://api.openai.com/v1", &dto.ChannelDataPolicy{
		Provider:       "OpenAI",
		Region:         "eu",
		Retention:      "30-days",
		Training:       dto.DataTrainingDisabled,
		RetryIsolation: dto.RetryIsolationProvider,
	})
	differentEndpoint := retryPolicyChannel(4, constant.ChannelTypeOpenAI, "https://other.example/v1", policy)
	differentProvider := retryPolicyChannel(5, constant.ChannelTypeOpenAI, "https://api.openai.com/v1", &dto.ChannelDataPolicy{
		Provider:       "Other Provider",
		Region:         "us",
		Retention:      "30-days",
		Training:       dto.DataTrainingDisabled,
		RetryIsolation: dto.RetryIsolationProvider,
	})
	boundary := NewRetryBoundary(initial, "default")
	require.NotNil(t, boundary)
	boundary.MarkAttempt(initial)

	assert.True(t, boundary.Allows(matching))
	assert.False(t, boundary.Allows(differentRegion))
	assert.False(t, boundary.Allows(differentEndpoint))
	assert.False(t, boundary.Allows(differentProvider))

	boundary.MarkAttempt(matching)
	assert.False(t, boundary.Allows(matching))
}

func TestRetryBoundaryPolicyGroupRequiresMatchingDisclosure(t *testing.T) {
	policy := &dto.ChannelDataPolicy{
		Provider:         "Example Provider",
		Region:           "us",
		Retention:        "zero",
		Training:         dto.DataTrainingDisabled,
		RetryIsolation:   dto.RetryIsolationPolicyGroup,
		RetryPolicyGroup: "enterprise-us",
	}
	initial := retryPolicyChannel(1, constant.ChannelTypeOpenAI, "https://primary.example/v1", policy)
	matching := retryPolicyChannel(2, constant.ChannelTypeOpenAI, "https://backup.example/v1", policy)
	mismatched := retryPolicyChannel(3, constant.ChannelTypeOpenAI, "https://backup.example/v1", &dto.ChannelDataPolicy{
		Provider:         "Example Provider",
		Region:           "us",
		Retention:        "30-days",
		Training:         dto.DataTrainingDisabled,
		RetryIsolation:   dto.RetryIsolationPolicyGroup,
		RetryPolicyGroup: "enterprise-us",
	})
	boundary := NewRetryBoundary(initial, "default")
	require.NotNil(t, boundary)
	boundary.MarkAttempt(initial)

	assert.True(t, boundary.Allows(matching))
	assert.False(t, boundary.Allows(mismatched))
}

func TestNonSubscriptionRetryBoundaryTagIsolationRequiresMatchingTagAndDataPolicy(t *testing.T) {
	policy := &dto.ChannelDataPolicy{
		Provider:       "OpenAI",
		Region:         "us",
		Retention:      "zero",
		Training:       dto.DataTrainingDisabled,
		RetryIsolation: dto.RetryIsolationTag,
	}
	initial := retryPolicyChannel(1, constant.ChannelTypeOpenAI, "https://api.openai.com", policy)
	initial.SetTag("openai-vip")
	matching := retryPolicyChannel(2, constant.ChannelTypeOpenAI, "https://api.openai.com", policy)
	matching.SetTag("openai-vip")
	differentTag := retryPolicyChannel(3, constant.ChannelTypeOpenAI, "https://api.openai.com", policy)
	differentTag.SetTag("openai-default")
	differentPolicy := retryPolicyChannel(4, constant.ChannelTypeOpenAI, "https://api.openai.com", &dto.ChannelDataPolicy{
		Provider:       "OpenAI",
		Region:         "eu",
		Retention:      "zero",
		Training:       dto.DataTrainingDisabled,
		RetryIsolation: dto.RetryIsolationTag,
	})
	differentPolicy.SetTag("openai-vip")
	differentType := retryPolicyChannel(5, constant.ChannelTypeAnthropic, "https://api.anthropic.com", policy)
	differentType.SetTag("openai-vip")
	boundary := NewRetryBoundary(initial, "default")
	require.NotNil(t, boundary)
	boundary.MarkAttempt(initial)

	assert.True(t, boundary.Allows(matching))
	assert.False(t, boundary.Allows(differentTag))
	assert.False(t, boundary.Allows(differentPolicy))
	assert.False(t, boundary.Allows(differentType))
	matching.Status = common.ChannelStatusManuallyDisabled
	assert.False(t, boundary.Allows(matching))
}

func TestRetryBoundaryExcludesDuplicateChannelsUsingFailedCredential(t *testing.T) {
	initial := retryPolicyChannel(1, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	initial.Key = `{"access_token":"token-a","account_id":"shared-account"}`
	initial.SetTag("openai-vip")
	duplicate := retryPolicyChannel(2, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	duplicate.Key = `{"access_token":"token-b","account_id":"shared-account"}`
	duplicate.SetTag("openai-vip")
	alternate := retryPolicyChannel(3, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	alternate.Key = `{"access_token":"token-c","account_id":"other-account"}`
	alternate.SetTag("openai-vip")

	boundary := NewRetryBoundary(initial, "default")
	fingerprint := SubscriptionOAuthCredentialFingerprint(initial.Type, initial.Id, 0, initial.Key)
	boundary.FailCredential(fingerprint)

	assert.False(t, boundary.Allows(initial))
	assert.False(t, boundary.Allows(duplicate))
	assert.True(t, boundary.Allows(alternate))
}

func TestSubscriptionOAuthFiveFailuresSwitchWithinFrozenGroup(t *testing.T) {
	originalRetries := common.SubscriptionOAuthUpstreamRetryTimes
	common.SubscriptionOAuthUpstreamRetryTimes = 5
	t.Cleanup(func() { common.SubscriptionOAuthUpstreamRetryTimes = originalRetries })

	initial := retryPolicyChannel(1, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	initial.Key = `{"access_token":"token-a","account_id":"account-a"}`
	initial.SetTag("openai-vip")
	backup := retryPolicyChannel(2, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	backup.Key = `{"access_token":"token-b","account_id":"account-b"}`
	backup.SetTag("openai-vip")

	boundary := NewRetryBoundary(initial, "default")
	fingerprint := SubscriptionOAuthCredentialFingerprint(initial.Type, initial.Id, 0, initial.Key)
	retryParam := &RetryParam{Boundary: boundary}
	retryParam.SetSubscriptionOAuthAttempt(initial.Id, 0, fingerprint)
	for attempt := 1; attempt < 5; attempt++ {
		assert.Equal(
			t,
			SubscriptionOAuthRetryCurrentCredential,
			retryParam.DecideSubscriptionOAuthRetry(false, true, true, false, http.StatusServiceUnavailable, 0),
		)
	}
	assert.Equal(
		t,
		SubscriptionOAuthSwitchCredential,
		retryParam.DecideSubscriptionOAuthRetry(false, true, true, false, http.StatusServiceUnavailable, 0),
	)
	assert.False(t, boundary.Allows(initial))
	assert.True(t, boundary.Allows(backup))
}

func TestSubscriptionOAuthAttemptGuardExcludesCredentialAfterConfiguredLimit(t *testing.T) {
	originalRetries := common.SubscriptionOAuthUpstreamRetryTimes
	common.SubscriptionOAuthUpstreamRetryTimes = 5
	t.Cleanup(func() { common.SubscriptionOAuthUpstreamRetryTimes = originalRetries })

	initial := retryPolicyChannel(6, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	initial.Key = `{"access_token":"token-a","account_id":"guarded-account"}`
	initial.SetTag("openai-vip")
	backup := retryPolicyChannel(7, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	backup.Key = `{"access_token":"token-b","account_id":"backup-account"}`
	backup.SetTag("openai-vip")

	boundary := NewRetryBoundary(initial, "default")
	fingerprint := SubscriptionOAuthCredentialFingerprint(initial.Type, initial.Id, 0, initial.Key)
	retryParam := &RetryParam{Boundary: boundary}
	for range 5 {
		require.True(t, retryParam.SetSubscriptionOAuthAttempt(initial.Id, 0, fingerprint))
	}
	require.False(t, retryParam.SetSubscriptionOAuthAttempt(initial.Id, 0, fingerprint))
	require.False(t, boundary.Allows(initial))
	require.True(t, boundary.Allows(backup))
}

func TestSubscriptionOAuthAccountUnavailableSwitchesAndCoolsCredential(t *testing.T) {
	initial := retryPolicyChannel(11, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	initial.Key = `{"access_token":"token-a","account_id":"unauthorized-account"}`
	initial.SetTag("openai-vip")
	backup := retryPolicyChannel(12, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	backup.Key = `{"access_token":"token-b","account_id":"available-account"}`
	backup.SetTag("openai-vip")

	boundary := NewRetryBoundary(initial, "default")
	fingerprint := SubscriptionOAuthCredentialFingerprint(initial.Type, initial.Id, 0, initial.Key)
	replaceSubscriptionOAuthStateForTest(t, fingerprint)
	retryParam := &RetryParam{Boundary: boundary}
	retryParam.SetSubscriptionOAuthAttempt(initial.Id, 0, fingerprint)
	retryParam.HandleSubscriptionOAuthAccountUnavailable()

	require.False(t, boundary.Allows(initial))
	require.True(t, boundary.Allows(backup))
	_, err := AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 10, 0)
	require.ErrorIs(t, err, errSubscriptionOAuthCredentialCool)
}

func TestSubscriptionOAuthModelUnavailableSwitchesWithoutCoolingAccount(t *testing.T) {
	initial := retryPolicyChannel(21, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	initial.Key = `{"access_token":"token-a","account_id":"model-limited-account"}`
	initial.SetTag("openai-vip")
	backup := retryPolicyChannel(22, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	backup.Key = `{"access_token":"token-b","account_id":"model-enabled-account"}`
	backup.SetTag("openai-vip")

	boundary := NewRetryBoundary(initial, "default")
	fingerprint := SubscriptionOAuthCredentialFingerprint(initial.Type, initial.Id, 0, initial.Key)
	replaceSubscriptionOAuthStateForTest(t, fingerprint)
	retryParam := &RetryParam{Boundary: boundary}
	retryParam.SetSubscriptionOAuthAttempt(initial.Id, 0, fingerprint)
	retryParam.HandleSubscriptionOAuthModelUnavailable()

	require.False(t, boundary.Allows(initial))
	require.True(t, boundary.Allows(backup))
	lease, err := AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 10, 0)
	require.NoError(t, err)
	lease.Release()
}

func TestRetryBoundaryRestartsOnlyCapacityExcludedGroupChannels(t *testing.T) {
	policy := &dto.ChannelDataPolicy{}
	initial := retryPolicyChannel(1, constant.ChannelTypeCodex, "https://chatgpt.com", policy)
	initial.SetTag("openai-vip")
	backup := retryPolicyChannel(2, constant.ChannelTypeCodex, "https://chatgpt.com", policy)
	backup.SetTag("openai-vip")
	boundary := NewRetryBoundary(initial, "default")
	require.NotNil(t, boundary)

	boundary.MarkAttempt(initial)
	boundary.ExcludeCredential(
		SubscriptionOAuthCredentialFingerprint(initial.Type, initial.Id, 0, initial.Key),
		initial.Id,
		0,
	)
	boundary.MarkAttempt(backup)
	boundary.ExcludeCredential(
		SubscriptionOAuthCredentialFingerprint(backup.Type, backup.Id, 0, backup.Key),
		backup.Id,
		0,
	)
	assert.False(t, boundary.Allows(initial))
	assert.False(t, boundary.Allows(backup))

	assert.True(t, boundary.RestartCapacityCycle())
	assert.True(t, boundary.Allows(initial))
	assert.True(t, boundary.Allows(backup))
	assert.False(t, boundary.RestartCapacityCycle())
}

func TestApplyRelayDataPolicyHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	channel := retryPolicyChannel(1, constant.ChannelTypeAnthropic, "https://api.anthropic.com", &dto.ChannelDataPolicy{
		Provider:       "Anthropic",
		Region:         "us",
		Retention:      "30-days",
		Training:       dto.DataTrainingDisabled,
		RetryIsolation: dto.RetryIsolationProvider,
	})

	ApplyRelayDataPolicyHeaders(ctx, channel, 2)

	assert.Equal(t, "Anthropic", recorder.Header().Get("X-Relay-Upstream-Provider"))
	assert.Equal(t, "2", recorder.Header().Get("X-Relay-Attempt"))
	assert.Equal(t, "1", recorder.Header().Get("X-Relay-Retry-Count"))
	assert.Equal(t, "provider", recorder.Header().Get("X-Relay-Retry-Isolation"))
	assert.Equal(t, "us", recorder.Header().Get("X-Relay-Data-Region"))
	assert.Equal(t, "30-days", recorder.Header().Get("X-Relay-Data-Retention"))
	assert.Equal(t, "disabled", recorder.Header().Get("X-Relay-Data-Training"))
}
