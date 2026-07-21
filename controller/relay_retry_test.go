package controller

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/responsesws"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestShouldRetrySubscriptionOAuthTransientError(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("channel_type", constant.ChannelTypeCodex)
	err := types.NewOpenAIError(errors.New("upstream timed out"), types.ErrorCodeBadResponseStatusCode, 524)

	require.False(t, shouldRetry(c, err, 1))
	require.False(t, shouldRetry(c, err, 0))
}

func TestShouldRetrySubscriptionOAuthTimeoutMappingRequiresExplicitOverride(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("channel_type", constant.ChannelTypeCodex)
	err := types.NewOpenAIError(errors.New("upstream timed out"), types.ErrorCodeBadResponseStatusCode, 503)
	err.UpstreamStatusCode = 524

	require.True(t, shouldRetry(c, err, 1))

	err.StatusCode = http.StatusBadRequest
	require.False(t, shouldRetry(c, err, 1))
}

func TestShouldRetrySubscriptionOAuthBadResponseBodyNeverReplays(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("channel_type", constant.ChannelTypeCodex)
	err := types.NewOpenAIError(
		errors.New("invalid upstream response body"),
		types.ErrorCodeBadResponseBody,
		http.StatusInternalServerError,
	)
	err.UpstreamStatusCode = http.StatusInternalServerError

	require.False(t, shouldRetry(c, err, 1))
}

func TestShouldRetryKeepsTimeoutRetryDisabledForOtherChannels(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("channel_type", constant.ChannelTypeOpenAI)
	err := types.NewOpenAIError(errors.New("upstream timed out"), types.ErrorCodeBadResponseStatusCode, http.StatusGatewayTimeout)

	require.False(t, shouldRetry(c, err, 1))
}

func TestOrdinaryRelayRetryStopsAfterUpstreamWrite(t *testing.T) {
	original := operation_setting.AutomaticRetryStatusCodeRanges
	operation_setting.AutomaticRetryStatusCodeRanges = []operation_setting.StatusCodeRange{{Start: 500, End: 503}}
	t.Cleanup(func() { operation_setting.AutomaticRetryStatusCodeRanges = original })

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("channel_type", constant.ChannelTypeOpenAI)
	apiError := types.NewOpenAIError(errors.New("connection reset"), types.ErrorCodeDoRequestFailed, http.StatusBadGateway)
	info := &relaycommon.RelayInfo{}

	require.True(t, shouldRetryOrdinaryRelay(c, info, apiError, 1))
	info.MarkUpstreamRequestWritten()
	require.False(t, shouldRetryOrdinaryRelay(c, info, apiError, 1))
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

func TestBoundSubscriptionOAuthWebSocketStopsTransientRetry(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("channel_type", constant.ChannelTypeCodex)
	service.DisableSubscriptionOAuthRetry(c)
	retryParam := &service.RetryParam{}
	retryParam.SetSubscriptionOAuthAttempt(1, 0, "credential-a")
	apiError := types.NewOpenAIError(
		errors.New("upstream websocket closed"),
		types.ErrorCodeDoRequestFailed,
		http.StatusBadGateway,
	)

	require.False(t, shouldContinueSubscriptionOAuthRetry(c, &relaycommon.RelayInfo{}, retryParam, apiError))
}

func TestBoundSubscriptionOAuthWebSocketSwitchesOnUsageLimitBeforeOutput(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("channel_type", constant.ChannelTypeCodex)
	c.Set(responsesWebSocketInternalPinKey, true)
	c.Set("specific_channel_id", "41")
	service.DisableSubscriptionOAuthRetry(c)
	responsesws.SetSession(c, &responsesws.Session{})

	initial := &model.Channel{
		Id: 41, Type: constant.ChannelTypeCodex, Status: common.ChannelStatusEnabled,
		BaseURL: common.GetPointer("https://chatgpt.com"),
		Key:     `{"access_token":"token-a","account_id":"usage-limited"}`,
		Group:   "default",
	}
	initial.SetOtherSettings(dto.ChannelOtherSettings{})
	backup := &model.Channel{
		Id: 42, Type: constant.ChannelTypeCodex, Status: common.ChannelStatusEnabled,
		BaseURL: common.GetPointer("https://chatgpt.com"),
		Key:     `{"access_token":"token-b","account_id":"usage-available"}`,
		Group:   "default",
	}
	backup.SetOtherSettings(dto.ChannelOtherSettings{})

	retryParam := &service.RetryParam{Boundary: service.NewRetryBoundary(initial, "default")}
	fingerprint := service.SubscriptionOAuthCredentialFingerprint(initial.Type, initial.Id, 0, initial.Key)
	require.True(t, retryParam.SetSubscriptionOAuthAttempt(initial.Id, 0, fingerprint))
	apiError := types.NewErrorWithStatusCode(
		errors.New("You've reached your usage limit"),
		types.ErrorCodeUpstreamUsageLimit,
		http.StatusTooManyRequests,
	)
	apiError.UpstreamStatusCode = http.StatusTooManyRequests
	apiError.RetryAfter = 5 * time.Hour

	require.True(t, shouldContinueSubscriptionOAuthRetry(c, &relaycommon.RelayInfo{}, retryParam, apiError))
	require.False(t, retryParam.Boundary.Allows(initial))
	require.True(t, retryParam.Boundary.Allows(backup))
}

func TestCorrelateCodexOAuthUsageLimitUsesCurrentCredentialWhamSnapshot(t *testing.T) {
	service.InitHttpClient()
	resetAt := time.Now().Add(5 * time.Hour).Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/backend-api/wham/usage", r.URL.Path)
		require.Equal(t, "Bearer token-a", r.Header.Get("Authorization"))
		require.Equal(t, "account-a", r.Header.Get("chatgpt-account-id"))
		_, _ = w.Write([]byte(`{"rate_limit":{"allowed":false,"limit_reached":true,"primary_window":{"used_percent":100,"reset_at":` +
			fmt.Sprint(resetAt) + `}}}`))
	}))
	t.Cleanup(server.Close)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	key := `{"access_token":"token-a","account_id":"account-a"}`
	common.SetContextKey(c, constant.ContextKeyChannelKey, key)
	channel := &model.Channel{
		Id:      31,
		Type:    constant.ChannelTypeCodex,
		Key:     key,
		BaseURL: common.GetPointer(server.URL),
	}
	apiError := types.NewOpenAIError(
		errors.New("exceeded retry limit, last status: 429 Too Many Requests"),
		types.ErrorCodeBadResponseStatusCode,
		http.StatusTooManyRequests,
	)
	apiError.UpstreamStatusCode = http.StatusTooManyRequests

	correlated := correlateCodexOAuthUsageLimit(c, channel, apiError)

	require.Equal(t, types.ErrorCodeUpstreamUsageLimit, correlated.GetErrorCode())
	require.Greater(t, correlated.RetryAfter, 4*time.Hour)
}

func TestClearStaleRetryAfter(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Header("Retry-After", "1")

	clearStaleRetryAfter(c)

	require.Empty(t, recorder.Header().Get("Retry-After"))
}

func TestShouldRetryTaskRelayUsesConfiguredStatusCodes(t *testing.T) {
	original := operation_setting.AutomaticRetryStatusCodeRanges
	operation_setting.AutomaticRetryStatusCodeRanges = []operation_setting.StatusCodeRange{
		{Start: http.StatusTooManyRequests, End: http.StatusTooManyRequests},
	}
	t.Cleanup(func() {
		operation_setting.AutomaticRetryStatusCodeRanges = original
	})

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	require.True(t, shouldRetryTaskRelay(c, 1, &dto.TaskError{StatusCode: http.StatusTooManyRequests}, 1))
	require.False(t, shouldRetryTaskRelay(c, 1, &dto.TaskError{StatusCode: http.StatusInternalServerError}, 1))
	require.False(t, shouldRetryTaskRelay(c, 1, &dto.TaskError{StatusCode: http.StatusGatewayTimeout}, 1))
	require.False(t, shouldRetryTaskRelay(c, 1, &dto.TaskError{StatusCode: 524}, 1))
	require.False(t, shouldRetryTaskRelay(c, 1, &dto.TaskError{StatusCode: 0}, 1))
	require.False(t, shouldRetryTaskRelay(c, 1, &dto.TaskError{StatusCode: 600}, 1))
	require.False(t, shouldRetryTaskRelay(c, 1, &dto.TaskError{StatusCode: http.StatusTooManyRequests, LocalError: true}, 1))
}

func TestCodexCapacityFailoverExcludesOnlySaturatedCredential(t *testing.T) {
	initial := &model.Channel{
		Id:      1,
		Type:    constant.ChannelTypeCodex,
		Status:  common.ChannelStatusEnabled,
		BaseURL: common.GetPointer("https://chatgpt.com"),
		Key:     "key-1\nkey-2",
		Group:   "default",
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
		Group:   "default",
	}
	alternate.SetTag("openai-vip")
	alternate.SetOtherSettings(dto.ChannelOtherSettings{})
	boundary := service.NewRetryBoundary(initial, "default")
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
	decision, _ := retryParam.DecideSubscriptionOAuthContinuation(service.SubscriptionOAuthRetryObservation{
		ChannelType: initial.Type,
		Error:       err,
		Retryable:   true,
	})

	require.Equal(t, service.SubscriptionOAuthSwitchCredential, decision)
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
		Group:   "default",
	}
	initial.SetTag("openai-vip")
	initial.SetOtherSettings(dto.ChannelOtherSettings{})
	backup := &model.Channel{
		Id: 32, Type: constant.ChannelTypeCodex, Status: common.ChannelStatusEnabled,
		BaseURL: common.GetPointer("https://chatgpt.com"),
		Key:     `{"access_token":"token-b","account_id":"model-enabled"}`,
		Group:   "default",
	}
	backup.SetTag("openai-vip")
	backup.SetOtherSettings(dto.ChannelOtherSettings{})

	boundary := service.NewRetryBoundary(initial, "default")
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
		Group:   "default",
	}
	initial.SetTag("openai-vip")
	initial.SetOtherSettings(dto.ChannelOtherSettings{})

	boundary := service.NewRetryBoundary(initial, "default")
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
		Group:   "default",
	}
	initial.SetTag("openai-vip")
	initial.SetOtherSettings(dto.ChannelOtherSettings{})
	backup := &model.Channel{
		Id: 52, Type: constant.ChannelTypeCodex, Status: common.ChannelStatusEnabled,
		BaseURL: common.GetPointer("https://chatgpt.com"),
		Key:     `{"access_token":"token-b","account_id":"quota-available"}`,
		Group:   "default",
	}
	backup.SetTag("openai-vip")
	backup.SetOtherSettings(dto.ChannelOtherSettings{})

	boundary := service.NewRetryBoundary(initial, "default")
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

func TestSubscriptionOAuthModelCapacitySwitchesEvenWhen429RetryIsDisabled(t *testing.T) {
	originalRetry429 := common.SubscriptionOAuthRetry429
	common.SubscriptionOAuthRetry429 = false
	t.Cleanup(func() { common.SubscriptionOAuthRetry429 = originalRetry429 })

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("channel_type", constant.ChannelTypeCodex)
	initial := &model.Channel{
		Id: 71, Type: constant.ChannelTypeCodex, Status: common.ChannelStatusEnabled,
		BaseURL: common.GetPointer("https://chatgpt.com"),
		Key:     `{"access_token":"token-a","account_id":"capacity-a"}`,
		Group:   "default",
	}
	initial.SetOtherSettings(dto.ChannelOtherSettings{})
	backup := &model.Channel{
		Id: 72, Type: constant.ChannelTypeCodex, Status: common.ChannelStatusEnabled,
		BaseURL: common.GetPointer("https://chatgpt.com"),
		Key:     `{"access_token":"token-b","account_id":"capacity-b"}`,
		Group:   "default",
	}
	backup.SetOtherSettings(dto.ChannelOtherSettings{})

	boundary := service.NewRetryBoundary(initial, "default")
	fingerprint := service.SubscriptionOAuthCredentialFingerprint(initial.Type, initial.Id, 0, initial.Key)
	retryParam := &service.RetryParam{Retry: common.GetPointer(0), Boundary: boundary}
	retryParam.SetSubscriptionOAuthAttempt(initial.Id, 0, fingerprint)
	apiError := service.ApplyChannelErrorPolicy(
		initial.Type,
		types.NewOpenAIError(
			errors.New("Selected model is at capacity. Please try a different model."),
			types.ErrorCodeBadResponseStatusCode,
			http.StatusTooManyRequests,
		),
	)

	require.Equal(t, types.ErrorCodeModelAtCapacity, apiError.GetErrorCode())
	require.True(t, shouldContinueSubscriptionOAuthRetry(c, &relaycommon.RelayInfo{}, retryParam, apiError))
	require.False(t, boundary.Allows(initial))
	require.True(t, boundary.Allows(backup))
}

func TestTrackRetryAttemptStopsSelectingCredentialAfterFiveAttempts(t *testing.T) {
	originalRetries := common.SubscriptionOAuthUpstreamRetryTimes
	common.SubscriptionOAuthUpstreamRetryTimes = 5
	t.Cleanup(func() { common.SubscriptionOAuthUpstreamRetryTimes = originalRetries })

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	initial := &model.Channel{
		Id: 61, Type: constant.ChannelTypeCodex, Status: common.ChannelStatusEnabled,
		BaseURL: common.GetPointer("https://chatgpt.com"),
		Key:     `{"access_token":"token-a","account_id":"attempt-guard"}`,
		Group:   "default",
	}
	initial.SetTag("openai-vip")
	initial.SetOtherSettings(dto.ChannelOtherSettings{})
	backup := &model.Channel{
		Id: 62, Type: constant.ChannelTypeCodex, Status: common.ChannelStatusEnabled,
		BaseURL: common.GetPointer("https://chatgpt.com"),
		Key:     `{"access_token":"token-b","account_id":"attempt-backup"}`,
		Group:   "default",
	}
	backup.SetTag("openai-vip")
	backup.SetOtherSettings(dto.ChannelOtherSettings{})
	common.SetContextKey(c, constant.ContextKeyChannelKey, initial.Key)
	common.SetContextKey(c, constant.ContextKeyChannelMultiKeyIndex, 0)

	retryParam := &service.RetryParam{Retry: common.GetPointer(0)}
	for range 5 {
		require.True(t, trackRetryAttempt(c, retryParam, initial))
	}
	require.False(t, trackRetryAttempt(c, retryParam, initial))
	require.False(t, retryParam.Boundary.Allows(initial))
	require.True(t, retryParam.Boundary.Allows(backup))
}
