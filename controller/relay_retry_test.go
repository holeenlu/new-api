package controller

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestShouldRetrySubscriptionOAuthTransientError(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("channel_type", constant.ChannelTypeCodex)
	err := types.NewOpenAIError(errors.New("upstream timed out"), types.ErrorCodeBadResponseStatusCode, 524)

	require.True(t, shouldRetry(c, err, 1))
	require.True(t, shouldRetry(c, err, 0))
}

func TestShouldRetryKeepsTimeoutRetryDisabledForOtherChannels(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("channel_type", constant.ChannelTypeOpenAI)
	err := types.NewOpenAIError(errors.New("upstream timed out"), types.ErrorCodeBadResponseStatusCode, http.StatusGatewayTimeout)

	require.False(t, shouldRetry(c, err, 1))
}

func TestShouldRetryCodexLocalConcurrencyLimit(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("channel_type", constant.ChannelTypeCodex)
	err := types.NewErrorWithStatusCode(
		errors.New("codex OAuth channel concurrency limit reached; retry later"),
		types.ErrorCodeOAuthChannelConcurrencyLimit,
		http.StatusServiceUnavailable,
		types.ErrOptionWithNoRecordErrorLog(),
	)

	require.True(t, shouldRetry(c, err, 1))
	require.False(t, types.IsRecordErrorLog(err))
}

func TestClearStaleRetryAfter(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Header("Retry-After", "1")

	clearStaleRetryAfter(c)

	require.Empty(t, recorder.Header().Get("Retry-After"))
}

func TestCodexCapacityFailoverExcludesOnlySaturatedCredential(t *testing.T) {
	initial := &model.Channel{
		Id:      1,
		Type:    constant.ChannelTypeCodex,
		Status:  common.ChannelStatusEnabled,
		BaseURL: common.GetPointer("https://chatgpt.com"),
		Key:     "key-1\nkey-2",
	}
	initial.ChannelInfo.IsMultiKey = true
	initial.SetTag("openai-vip")
	initial.SetOtherSettings(dto.ChannelOtherSettings{})
	alternate := &model.Channel{
		Id:      2,
		Type:    constant.ChannelTypeCodex,
		Status:  common.ChannelStatusEnabled,
		BaseURL: common.GetPointer("https://chatgpt.com"),
		Key:     "key-3",
	}
	alternate.SetTag("openai-vip")
	alternate.SetOtherSettings(dto.ChannelOtherSettings{})
	boundary := service.NewRetryBoundary(initial)
	require.NotNil(t, boundary)
	boundary.MarkAttempt(initial, 0)
	err := types.NewErrorWithStatusCode(
		errors.New("codex OAuth channel concurrency limit reached; retry later"),
		types.ErrorCodeOAuthChannelConcurrencyLimit,
		http.StatusServiceUnavailable,
		types.ErrOptionWithNoRecordErrorLog(),
	)

	retryParam := &service.RetryParam{Retry: common.GetPointer(0), Boundary: boundary}
	retryParam.SetSubscriptionOAuthAttempt(
		initial.Id,
		0,
		service.SubscriptionOAuthCredentialFingerprint(initial.Type, initial.Id, 0, "key-1"),
	)
	applySubscriptionOAuthCapacityFailover(initial, err, retryParam)

	require.True(t, boundary.Allows(initial))
	require.True(t, boundary.Allows(alternate))
	require.Contains(t, boundary.ExcludedKeyIndexes(initial), 0)
}

func TestSubscriptionOAuthModelUnavailableSwitchesWithinRetryBoundary(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("channel_type", constant.ChannelTypeCodex)
	initial := &model.Channel{
		Id: 31, Type: constant.ChannelTypeCodex, Status: common.ChannelStatusEnabled,
		BaseURL: common.GetPointer("https://chatgpt.com"),
		Key:     `{"access_token":"token-a","account_id":"model-limited"}`,
	}
	initial.SetTag("openai-vip")
	initial.SetOtherSettings(dto.ChannelOtherSettings{})
	backup := &model.Channel{
		Id: 32, Type: constant.ChannelTypeCodex, Status: common.ChannelStatusEnabled,
		BaseURL: common.GetPointer("https://chatgpt.com"),
		Key:     `{"access_token":"token-b","account_id":"model-enabled"}`,
	}
	backup.SetTag("openai-vip")
	backup.SetOtherSettings(dto.ChannelOtherSettings{})

	boundary := service.NewRetryBoundary(initial)
	fingerprint := service.SubscriptionOAuthCredentialFingerprint(initial.Type, initial.Id, 0, initial.Key)
	retryParam := &service.RetryParam{Retry: common.GetPointer(0), Boundary: boundary}
	retryParam.SetSubscriptionOAuthAttempt(initial.Id, 0, fingerprint)
	apiError := service.ApplyChannelErrorPolicy(
		initial.Type,
		types.NewOpenAIError(errors.New("model gpt-missing is not supported"), types.ErrorCodeBadResponseStatusCode, http.StatusBadRequest),
	)

	require.True(t, shouldContinueSubscriptionOAuthRetry(c, &relaycommon.RelayInfo{}, retryParam, apiError))
	require.False(t, boundary.Allows(initial))
	require.True(t, boundary.Allows(backup))
}

func TestSubscriptionOAuthExplicitChannelDoesNotSwitchOnModelUnavailable(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("channel_type", constant.ChannelTypeCodex)
	c.Set("specific_channel_id", "41")
	initial := &model.Channel{
		Id: 41, Type: constant.ChannelTypeCodex, Status: common.ChannelStatusEnabled,
		BaseURL: common.GetPointer("https://chatgpt.com"),
		Key:     `{"access_token":"token-a","account_id":"pinned-model-limited"}`,
	}
	initial.SetTag("openai-vip")
	initial.SetOtherSettings(dto.ChannelOtherSettings{})

	boundary := service.NewRetryBoundary(initial)
	retryParam := &service.RetryParam{Retry: common.GetPointer(0), Boundary: boundary}
	retryParam.SetSubscriptionOAuthAttempt(
		initial.Id,
		0,
		service.SubscriptionOAuthCredentialFingerprint(initial.Type, initial.Id, 0, initial.Key),
	)
	apiError := service.ApplyChannelErrorPolicy(
		initial.Type,
		types.NewOpenAIError(errors.New("model gpt-missing is not supported"), types.ErrorCodeBadResponseStatusCode, http.StatusBadRequest),
	)

	require.False(t, shouldContinueSubscriptionOAuthRetry(c, &relaycommon.RelayInfo{}, retryParam, apiError))
	require.False(t, boundary.Allows(initial))
}

func TestSubscriptionOAuthQuotaExhaustionSwitchesEvenWhen429RetryIsDisabled(t *testing.T) {
	originalRetry429 := common.SubscriptionOAuthRetry429
	common.SubscriptionOAuthRetry429 = false
	t.Cleanup(func() { common.SubscriptionOAuthRetry429 = originalRetry429 })

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("channel_type", constant.ChannelTypeCodex)
	initial := &model.Channel{
		Id: 51, Type: constant.ChannelTypeCodex, Status: common.ChannelStatusEnabled,
		BaseURL: common.GetPointer("https://chatgpt.com"),
		Key:     `{"access_token":"token-a","account_id":"quota-exhausted"}`,
	}
	initial.SetTag("openai-vip")
	initial.SetOtherSettings(dto.ChannelOtherSettings{})
	backup := &model.Channel{
		Id: 52, Type: constant.ChannelTypeCodex, Status: common.ChannelStatusEnabled,
		BaseURL: common.GetPointer("https://chatgpt.com"),
		Key:     `{"access_token":"token-b","account_id":"quota-available"}`,
	}
	backup.SetTag("openai-vip")
	backup.SetOtherSettings(dto.ChannelOtherSettings{})

	boundary := service.NewRetryBoundary(initial)
	fingerprint := service.SubscriptionOAuthCredentialFingerprint(initial.Type, initial.Id, 0, initial.Key)
	retryParam := &service.RetryParam{Retry: common.GetPointer(0), Boundary: boundary}
	retryParam.SetSubscriptionOAuthAttempt(initial.Id, 0, fingerprint)
	apiError := service.ApplyChannelErrorPolicy(
		initial.Type,
		types.NewOpenAIError(errors.New("You exceeded your current quota"), "insufficient_quota", http.StatusTooManyRequests),
	)

	require.Equal(t, types.ErrorCodeUpstreamQuotaExhausted, apiError.GetErrorCode())
	require.True(t, shouldContinueSubscriptionOAuthRetry(c, &relaycommon.RelayInfo{}, retryParam, apiError))
	require.False(t, boundary.Allows(initial))
	require.True(t, boundary.Allows(backup))
}
