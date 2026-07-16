package service

import (
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
	channel := &model.Channel{Id: id, Type: channelType, BaseURL: common.GetPointer(baseURL)}
	channel.SetOtherSettings(dto.ChannelOtherSettings{DataPolicy: policy})
	return channel
}

func TestRetryBoundaryDefaultsSubscriptionOAuthToCurrentChannel(t *testing.T) {
	initial := retryPolicyChannel(1, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	alternate := retryPolicyChannel(2, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	boundary := NewRetryBoundary(initial)
	require.NotNil(t, boundary)

	boundary.MarkAttempt(initial)
	assert.False(t, boundary.Allows(initial))
	assert.False(t, boundary.Allows(alternate))
}

func TestRetryBoundaryAllowsAnotherKeyOnlyWithinCurrentChannel(t *testing.T) {
	initial := retryPolicyChannel(1, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	initial.ChannelInfo.IsMultiKey = true
	initial.Key = "key-1\nkey-2"
	alternate := retryPolicyChannel(2, constant.ChannelTypeCodex, "https://chatgpt.com", nil)
	boundary := NewRetryBoundary(initial)
	require.NotNil(t, boundary)

	boundary.MarkAttempt(initial, 0)
	assert.True(t, boundary.Allows(initial))
	assert.Equal(t, map[int]struct{}{0: {}}, boundary.UsedMultiKeyIndexes(initial.Id))
	boundary.MarkAttempt(initial, 1)
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
	boundary := NewRetryBoundary(initial)
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
	boundary := NewRetryBoundary(initial)
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
	boundary := NewRetryBoundary(initial)
	require.NotNil(t, boundary)
	boundary.MarkAttempt(initial)

	assert.True(t, boundary.Allows(matching))
	assert.False(t, boundary.Allows(mismatched))
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
