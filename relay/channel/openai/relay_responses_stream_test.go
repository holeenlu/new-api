package openai

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type responsesStreamErrorReader struct {
	err error
}

func (r responsesStreamErrorReader) Read([]byte) (int, error) {
	return 0, r.err
}

func newCodexResponsesStreamTest(t *testing.T, events ...string) (*gin.Context, *httptest.ResponseRecorder, *http.Response, *relaycommon.RelayInfo) {
	t.Helper()
	previousStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousStreamingTimeout })
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":       []string{"text/event-stream"},
			"X-Codex-Turn-State": []string{"turn-state"},
		},
		Body: io.NopCloser(strings.NewReader(strings.Join(events, "\n") + "\n")),
	}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeCodex,
			UpstreamModelName: "gpt-test",
		},
		IsStream:    true,
		DisablePing: true,
	}
	return c, recorder, resp, info
}

func TestCodexResponsesStreamRetriesModelCapacityBeforeOutput(t *testing.T) {
	c, recorder, resp, info := newCodexResponsesStreamTest(t,
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"type":"server_error","code":"model_at_capacity","message":"Selected model is at capacity. Please try a different model."}}}`,
		`data: [DONE]`,
	)

	usage, apiError := OaiResponsesStreamHandler(c, info, resp)

	require.Nil(t, usage)
	require.NotNil(t, apiError)
	require.Equal(t, types.ErrorCodeModelAtCapacity, apiError.GetErrorCode())
	require.Equal(t, http.StatusTooManyRequests, apiError.GetUpstreamStatusCode())
	require.True(t, info.HasUpstreamFailureResponse())
	require.Empty(t, recorder.Body.String())
	require.Empty(t, recorder.Header().Get("Content-Type"))
	require.Empty(t, recorder.Header().Get("X-Codex-Turn-State"))
}

func TestCodexResponsesStreamRetriesServerOverloadBeforeOutput(t *testing.T) {
	c, recorder, resp, info := newCodexResponsesStreamTest(t,
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded"}}}`,
		`data: [DONE]`,
	)

	usage, apiError := OaiResponsesStreamHandler(c, info, resp)

	require.Nil(t, usage)
	require.NotNil(t, apiError)
	require.Equal(t, types.ErrorCodeModelAtCapacity, apiError.GetErrorCode())
	require.Equal(t, http.StatusServiceUnavailable, apiError.GetUpstreamStatusCode())
	require.True(t, info.HasUpstreamFailureResponse())
	require.Empty(t, recorder.Body.String())
	failureEvent, exists := relaycommon.GetResponsesStreamPreflightFailureEvent(c)
	require.True(t, exists)
	require.Contains(t, failureEvent, `"type":"response.failed"`)
}

func TestCodexResponsesStreamPreservesExplicitAndUnknownFailureStatus(t *testing.T) {
	tests := []struct {
		name      string
		errorJSON string
		want      int
	}{
		{name: "explicit status", errorJSON: `{"type":"provider_error","message":"failed","status_code":418}`, want: http.StatusTeapot},
		{name: "unknown failure", errorJSON: `{"type":"provider_error","message":"failed"}`, want: http.StatusBadGateway},
		{name: "authentication failure", errorJSON: `{"type":"authentication_error","message":"expired"}`, want: http.StatusUnauthorized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, _, resp, info := newCodexResponsesStreamTest(t,
				`data: {"type":"response.failed","response":{"status":"failed","error":`+test.errorJSON+`}}`,
			)

			_, apiError := OaiResponsesStreamHandler(c, info, resp)

			require.NotNil(t, apiError)
			require.Equal(t, test.want, apiError.GetUpstreamStatusCode())
		})
	}
}

func TestCodexResponsesStreamTreatsFailureWithoutErrorDetailsAsBadGateway(t *testing.T) {
	c, recorder, resp, info := newCodexResponsesStreamTest(t,
		`data: {"type":"response.failed","response":{"status":"failed"}}`,
	)

	_, apiError := OaiResponsesStreamHandler(c, info, resp)

	require.NotNil(t, apiError)
	require.Equal(t, http.StatusBadGateway, apiError.GetUpstreamStatusCode())
	require.True(t, info.HasUpstreamFailureResponse())
	require.Empty(t, recorder.Body.String())
}

func TestCodexResponsesStreamRetriesUsageLimitBeforeOutput(t *testing.T) {
	c, recorder, resp, info := newCodexResponsesStreamTest(t,
		`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
		`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"type":"usage_limit_reached","code":"usage_limit_reached","message":"You've reached your usage limit","resets_in_seconds":18000}}}`,
	)

	usage, apiError := OaiResponsesStreamHandler(c, info, resp)

	require.Nil(t, usage)
	require.NotNil(t, apiError)
	require.Equal(t, types.ErrorCodeUpstreamUsageLimit, apiError.GetErrorCode())
	require.Equal(t, 5*time.Hour, apiError.RetryAfter)
	require.True(t, info.HasUpstreamFailureResponse())
	require.False(t, recorder.Flushed)
	require.Empty(t, recorder.Body.String())
}

func TestCodexResponsesStreamRetriesPermanentCredentialFailuresBeforeOutput(t *testing.T) {
	tests := []struct {
		name      string
		errorJSON string
		wantCode  types.ErrorCode
	}{
		{
			name:      "unauthorized",
			errorJSON: `{"type":"authentication_error","code":"oauth_unauthorized","message":"OAuth credential is invalid or expired"}`,
			wantCode:  types.ErrorCodeOAuthUnauthorized,
		},
		{
			name:      "forbidden",
			errorJSON: `{"type":"permission_error","code":"oauth_forbidden","message":"OAuth account is not permitted to access this resource"}`,
			wantCode:  types.ErrorCodeOAuthForbidden,
		},
		{
			name:      "account disabled",
			errorJSON: `{"type":"account_error","code":"account_disabled","message":"This organization has been disabled"}`,
			wantCode:  types.ErrorCodeUpstreamAccountDisabled,
		},
		{
			name:      "quota exhausted",
			errorJSON: `{"type":"insufficient_quota","code":"insufficient_quota","message":"You exceeded your current quota"}`,
			wantCode:  types.ErrorCodeUpstreamQuotaExhausted,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, recorder, resp, info := newCodexResponsesStreamTest(t,
				`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
				`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":`+test.errorJSON+`}}`,
			)

			usage, apiError := OaiResponsesStreamHandler(c, info, resp)

			require.Nil(t, usage)
			require.NotNil(t, apiError)
			require.Equal(t, test.wantCode, apiError.GetErrorCode())
			require.True(t, info.HasUpstreamFailureResponse())
			require.False(t, recorder.Flushed)
			require.Empty(t, recorder.Body.String())
		})
	}
}

func TestCodexResponsesStreamFlushesPreflightEventsInOrder(t *testing.T) {
	c, recorder, resp, info := newCodexResponsesStreamTest(t,
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		`data: {"type":"response.in_progress","response":{"id":"resp_1"}}`,
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`,
		`data: [DONE]`,
	)

	usage, apiError := OaiResponsesStreamHandler(c, info, resp)

	require.Nil(t, apiError)
	require.Equal(t, 3, usage.TotalTokens)
	body := recorder.Body.String()
	require.Equal(t, "turn-state", recorder.Header().Get("X-Codex-Turn-State"))
	require.Contains(t, body, `event: response.created`)
	require.Contains(t, body, `event: response.in_progress`)
	require.Contains(t, body, `event: response.output_text.delta`)
	require.Less(t, strings.Index(body, `event: response.created`), strings.Index(body, `event: response.in_progress`))
	require.Less(t, strings.Index(body, `event: response.in_progress`), strings.Index(body, `event: response.output_text.delta`))
}

func TestCodexResponsesStreamDoesNotRetryCapacityAfterOutput(t *testing.T) {
	c, recorder, resp, info := newCodexResponsesStreamTest(t,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"call_1","name":"lookup"}}`,
		`data: {"type":"error","error":{"code":"model_at_capacity","message":"Selected model is at capacity. Please try a different model."}}`,
		`data: [DONE]`,
	)

	usage, apiError := OaiResponsesStreamHandler(c, info, resp)

	require.Nil(t, apiError)
	require.NotNil(t, usage)
	require.Contains(t, recorder.Body.String(), `Selected model is at capacity`)
	require.True(t, info.StreamStatus.HasErrors())
}

func TestCodexResponsesStreamReturnsScannerErrorBeforeOutput(t *testing.T) {
	c, recorder, resp, info := newCodexResponsesStreamTest(t)
	resp.Body = io.NopCloser(responsesStreamErrorReader{err: errors.New("stream ID 99; INTERNAL_ERROR; received from peer")})

	usage, apiError := OaiResponsesStreamHandler(c, info, resp)

	require.Nil(t, usage)
	require.NotNil(t, apiError)
	require.Equal(t, types.ErrorCodeDoRequestFailed, apiError.GetErrorCode())
	require.Equal(t, http.StatusBadGateway, apiError.StatusCode)
	require.Empty(t, recorder.Body.String())
	require.Empty(t, recorder.Header().Get("Content-Type"))
}

// ② A non-stream Responses reply that wraps an error in a 200 must derive a
// routable status from the error content, otherwise shouldRetry treats it as
// success (2xx) and never retries or fails over.
func TestOaiResponsesHandlerDerivesStatusFromWrapped200Error(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := `{"error":{"type":"rate_limit_exceeded","code":"rate_limit_exceeded","message":"Rate limit reached"}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-test"}}

	usage, apiError := OaiResponsesHandler(c, info, resp)

	require.Nil(t, usage)
	require.NotNil(t, apiError)
	require.Equal(t, http.StatusTooManyRequests, apiError.StatusCode)
}

// ③ A preflight-stage terminal failure of type response.error (not only
// response.failed) must be stored so the client still gets a structured terminal
// event after retries are exhausted.
func TestCodexResponsesStreamStoresPreflightFailureEventForErrorType(t *testing.T) {
	c, _, resp, info := newCodexResponsesStreamTest(t,
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		`data: {"type":"response.error","error":{"type":"server_error","code":"server_error","message":"boom"}}`,
		`data: [DONE]`,
	)

	usage, apiError := OaiResponsesStreamHandler(c, info, resp)

	require.Nil(t, usage)
	require.NotNil(t, apiError)
	require.True(t, info.HasUpstreamFailureResponse())
	failureEvent, exists := relaycommon.GetResponsesStreamPreflightFailureEvent(c)
	require.True(t, exists)
	require.Contains(t, failureEvent, `"type":"response.error"`)
}

// ④ A non-subscription channel treats the downstream as committed from the first
// event, so an in-stream terminal failure cannot be retried — but it must still be
// marked as an upstream failure (observability) and forwarded to the client.
func TestOaiResponsesStreamNonSubscriptionMarksUpstreamFailure(t *testing.T) {
	previousStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousStreamingTimeout })

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			`data: {"type":"response.failed","response":{"error":{"type":"server_error","message":"boom"}}}` + "\n" +
				`data: [DONE]` + "\n")),
	}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, UpstreamModelName: "gpt-test"},
		IsStream:    true,
		DisablePing: true,
	}

	usage, apiError := OaiResponsesStreamHandler(c, info, resp)

	require.Nil(t, apiError) // already committed downstream: forwarded, not retried
	require.NotNil(t, usage)
	require.True(t, info.HasUpstreamFailureResponse())
	require.Contains(t, recorder.Body.String(), "response.failed")
}
