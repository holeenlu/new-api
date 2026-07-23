package controller

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

type ordinaryResponsesSessionTestDriver struct {
	upstreamURL string
}

func (d *ordinaryResponsesSessionTestDriver) DialUpstream(_ *gin.Context, _ *relaycommon.RelayInfo) (string, http.Header, error) {
	return "ws" + strings.TrimPrefix(d.upstreamURL, "http"), nil, nil
}

func (d *ordinaryResponsesSessionTestDriver) AcquireCapacity(_ *gin.Context, _ *relaycommon.RelayInfo) (*service.SubscriptionOAuthLease, error) {
	return nil, nil
}

func (d *ordinaryResponsesSessionTestDriver) DoHTTPFallback(
	_ *gin.Context,
	_ *relaycommon.RelayInfo,
	_ []byte,
	_ *service.SubscriptionOAuthLease,
) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
}

func (d *ordinaryResponsesSessionTestDriver) OnUpstreamConnected(_ *gin.Context, _ *relaycommon.RelayInfo) {
}

func TestShouldRetrySubscriptionOAuthTransientError(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("channel_type", constant.ChannelTypeCodex)
	err := types.NewOpenAIError(errors.New("upstream timed out"), types.ErrorCodeBadResponseStatusCode, 524)

	require.False(t, shouldRetry(c, err, 1, true))
	require.False(t, shouldRetry(c, err, 0, true))
}

func TestShouldRetrySubscriptionOAuthTimeoutMappingRequiresExplicitOverride(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("channel_type", constant.ChannelTypeCodex)
	err := types.NewOpenAIError(errors.New("upstream timed out"), types.ErrorCodeBadResponseStatusCode, 503)
	err.UpstreamStatusCode = 524

	require.True(t, shouldRetry(c, err, 1, true))

	err.StatusCode = http.StatusBadRequest
	require.False(t, shouldRetry(c, err, 1, true))
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

	require.False(t, shouldRetry(c, err, 1, true))
}

func TestShouldRetryKeepsTimeoutRetryDisabledForOtherChannels(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("channel_type", constant.ChannelTypeOpenAI)
	err := types.NewOpenAIError(errors.New("upstream timed out"), types.ErrorCodeBadResponseStatusCode, http.StatusGatewayTimeout)

	require.False(t, shouldRetry(c, err, 1, true))
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

func TestOrdinaryRelayRetriesRejectedWebSocketHandshakeBeforeApplicationWrite(t *testing.T) {
	original := operation_setting.AutomaticRetryStatusCodeRanges
	operation_setting.AutomaticRetryStatusCodeRanges = []operation_setting.StatusCodeRange{
		{Start: http.StatusTooManyRequests, End: http.StatusTooManyRequests},
		{Start: http.StatusInternalServerError, End: http.StatusServiceUnavailable},
	}
	t.Cleanup(func() { operation_setting.AutomaticRetryStatusCodeRanges = original })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUpgradeRequired)
	}))
	t.Cleanup(server.Close)
	driver := &ordinaryResponsesSessionTestDriver{upstreamURL: server.URL}

	tests := []struct {
		name                   string
		statusCode             int
		internalPin            bool
		preexistingSpecificPin bool
		requestWritten         bool
		responseStarted        bool
		wantRetry              bool
		wantChannelID          int
	}{
		{
			name:          "internal pin rate limited during handshake",
			statusCode:    http.StatusTooManyRequests,
			internalPin:   true,
			wantRetry:     true,
			wantChannelID: 0,
		},
		{
			name:          "internal pin unavailable during handshake",
			statusCode:    http.StatusServiceUnavailable,
			internalPin:   true,
			wantRetry:     true,
			wantChannelID: 0,
		},
		{
			name:           "application frame may have been written",
			statusCode:     http.StatusServiceUnavailable,
			internalPin:    true,
			requestWritten: true,
			wantChannelID:  41,
		},
		{
			name:            "application response already started",
			statusCode:      http.StatusServiceUnavailable,
			internalPin:     true,
			responseStarted: true,
			wantChannelID:   41,
		},
		{
			name:                   "pre-existing explicit pin stays fixed",
			statusCode:             http.StatusServiceUnavailable,
			preexistingSpecificPin: true,
			wantChannelID:          41,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := &responsesws.Session{}
			defer session.Close()
			bindContext, _ := gin.CreateTestContext(httptest.NewRecorder())
			bindContext.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			bindInfo := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
				ChannelId:         41,
				ChannelType:       constant.ChannelTypeOpenAI,
				ApiKey:            "standard-api-key",
				UpstreamModelName: "gpt-test",
			}}
			response, err := session.DoRequest(
				bindContext,
				driver,
				bindInfo,
				strings.NewReader(`{"model":"gpt-test","input":"bind"}`),
			)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			session.ConfirmHTTPFallbackSuccess()
			require.Equal(t, 41, session.ChannelID())

			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Set("channel_type", constant.ChannelTypeOpenAI)
			if test.preexistingSpecificPin {
				common.SetContextKey(c, constant.ContextKeyTokenSpecificChannelId, "99")
				applyResponsesWebSocketTurnPin(c, 41, false)
				require.Equal(t, "41", common.GetContextKeyString(c, constant.ContextKeyTokenSpecificChannelId))
				require.False(t, c.GetBool(responsesWebSocketInternalPinKey))
			} else {
				common.SetContextKey(c, constant.ContextKeyTokenSpecificChannelId, "41")
			}
			if test.internalPin {
				c.Set(responsesWebSocketInternalPinKey, true)
			}
			responsesws.SetSession(c, session)

			apiError := types.NewOpenAIError(
				errors.New("websocket handshake rejected"),
				types.ErrorCodeBadResponseStatusCode,
				test.statusCode,
			)
			apiError.UpstreamStatusCode = test.statusCode
			info := &relaycommon.RelayInfo{}
			info.MarkUpstreamFailureResponse()
			if test.requestWritten {
				info.MarkUpstreamRequestWritten()
			}
			if test.responseStarted {
				info.MarkUpstreamResponseStarted()
			}

			require.Equal(t, test.wantRetry, shouldRetryOrdinaryRelay(c, info, apiError, 1))
			require.Equal(t, test.wantChannelID, session.ChannelID())
		})
	}
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

	require.True(t, shouldRetry(c, err, 1, true))
	require.False(t, types.IsRecordErrorLog(err))
}

func TestShouldRetryCanIgnoreInternalWebSocketChannelPin(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("channel_type", constant.ChannelTypeCodex)
	c.Set("specific_channel_id", "41")
	apiError := types.NewErrorWithStatusCode(
		errors.New("upstream unavailable before output"),
		types.ErrorCodeDoRequestFailed,
		http.StatusServiceUnavailable,
	)
	apiError.UpstreamStatusCode = http.StatusServiceUnavailable

	require.False(t, shouldRetry(c, apiError, 1, true))
	require.True(t, shouldRetry(c, apiError, 1, false))
}

func TestParamOverrideContinuationCannotUseInternalWebSocketRetryPin(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("channel_type", constant.ChannelTypeCodex)
	c.Set("specific_channel_id", "41")
	c.Set(responsesWebSocketInternalPinKey, true)
	service.DisableSubscriptionOAuthRetry(c)
	responsesws.MarkContinuationRequired(c)

	apiError := types.NewErrorWithStatusCode(
		errors.New("credential capacity exhausted before continuation write"),
		types.ErrorCodeOAuthChannelConcurrencyLimit,
		http.StatusServiceUnavailable,
	)
	retryParam := &service.RetryParam{}
	require.True(t, retryParam.SetSubscriptionOAuthAttempt(41, 0, "credential-a"))

	require.False(t, hasReplaySafeResponsesWebSocketInternalPin(c))
	require.False(t, shouldContinueSubscriptionOAuthRetry(c, &relaycommon.RelayInfo{}, retryParam, apiError))
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

func TestSubscriptionOAuthWebSocketFirstEventTimeoutDoesNotReplayWrittenRequest(t *testing.T) {
	original := operation_setting.AutomaticRetryStatusCodeRanges
	operation_setting.AutomaticRetryStatusCodeRanges = []operation_setting.StatusCodeRange{{Start: http.StatusBadGateway, End: http.StatusBadGateway}}
	t.Cleanup(func() { operation_setting.AutomaticRetryStatusCodeRanges = original })

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("channel_type", constant.ChannelTypeCodex)
	c.Set(responsesWebSocketInternalPinKey, true)
	retryParam := &service.RetryParam{}
	require.True(t, retryParam.SetSubscriptionOAuthAttempt(1, 0, "credential-a"))
	info := &relaycommon.RelayInfo{}
	info.MarkUpstreamRequestWritten()
	apiError := types.NewOpenAIError(
		errors.New("responses websocket upstream produced no event within 30s"),
		types.ErrorCodeDoRequestFailed,
		http.StatusBadGateway,
	)

	require.False(t, shouldContinueSubscriptionOAuthRetry(c, info, retryParam, apiError))
}

func TestRefreshCodexCredentialHonorsStatefulWebSocketRetryDisable(t *testing.T) {
	tests := []struct {
		name                 string
		internalPin          bool
		continuationRequired bool
		wantRefreshAvailable bool
	}{
		{name: "stateful continuation", wantRefreshAvailable: true},
		{name: "self-contained internal pin", internalPin: true, wantRefreshAvailable: false},
		{name: "param override continuation", internalPin: true, continuationRequired: true, wantRefreshAvailable: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Set("channel_type", constant.ChannelTypeCodex)
			common.SetContextKey(c, constant.ContextKeyChannelKey, "invalid-oauth-key")
			service.DisableSubscriptionOAuthRetry(c)
			if test.internalPin {
				c.Set(responsesWebSocketInternalPinKey, true)
			}
			if test.continuationRequired {
				responsesws.MarkContinuationRequired(c)
			}

			retryParam := &service.RetryParam{}
			require.True(t, retryParam.SetSubscriptionOAuthAttempt(41, 0, "credential-a"))
			apiError := types.NewErrorWithStatusCode(
				errors.New("access token expired"),
				types.ErrorCodeOAuthUnauthorized,
				http.StatusUnauthorized,
			)
			apiError.UpstreamStatusCode = http.StatusUnauthorized

			got, retry := refreshCodexCredentialForRetry(
				c,
				&relaycommon.RelayInfo{},
				retryParam,
				&model.Channel{Id: 41, Type: constant.ChannelTypeCodex},
				apiError,
			)

			require.Same(t, apiError, got)
			require.False(t, retry)
			require.Equal(t, test.wantRefreshAvailable, retryParam.ClaimSubscriptionOAuthCredentialRefresh())
		})
	}
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

func TestSetRelayRetryAfterHeaderRoundsUp(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	setRelayRetryAfterHeader(c, 1500*time.Millisecond)

	require.Equal(t, "2", recorder.Header().Get("Retry-After"))
}

func TestEmitResponsesStreamPreflightFailurePreservesUpstreamEventType(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	data := `{"type":"response.error","error":{"type":"server_error","message":"boom"}}`
	relaycommon.SetResponsesStreamPreflightFailureEvent(c, data)

	emitted := emitResponsesStreamPreflightFailure(c)

	require.True(t, emitted)
	require.True(t, relaycommon.IsResponsesStreamFailureEmitted(c))
	require.Contains(t, recorder.Header().Get("Content-Type"), "text/event-stream")
	require.Contains(t, recorder.Body.String(), "event: response.error")
	require.NotContains(t, recorder.Body.String(), "event: response.failed")
	require.Contains(t, recorder.Body.String(), data)
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
