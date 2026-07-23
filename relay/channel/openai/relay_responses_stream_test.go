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
	"github.com/QuantumNous/new-api/dto"
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

func TestOaiResponsesStreamStopsAtCleanTerminalWhileHTTPUpstreamStaysOpen(t *testing.T) {
	for _, eventType := range []string{"response.completed", "response.incomplete"} {
		t.Run(eventType, func(t *testing.T) {
			c, recorder, resp, info := newCodexResponsesStreamTest(t)
			reader, writer := io.Pipe()
			resp.Body = reader
			result := make(chan *types.NewAPIError, 1)
			go func() {
				_, apiError := OaiResponsesStreamHandler(c, info, resp)
				result <- apiError
			}()

			_, writeErr := io.WriteString(writer,
				`data: {"type":"`+eventType+`","response":{"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`+"\n\n",
			)
			require.NoError(t, writeErr)

			select {
			case apiError := <-result:
				require.Nil(t, apiError)
			case <-time.After(time.Second):
				_ = writer.Close()
				<-result
				t.Fatal("Responses handler waited for HTTP upstream EOF after a terminal event")
			}
			require.NoError(t, writer.Close())
			require.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
			require.Contains(t, recorder.Body.String(), eventType)
		})
	}
}

func TestOaiResponsesStreamIgnoresEventsAfterCleanTerminal(t *testing.T) {
	c, recorder, resp, info := newCodexResponsesStreamTest(t,
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`,
		`data: {"type":"response.output_text.delta","delta":"must-not-be-forwarded"}`,
	)

	usage, apiError := OaiResponsesStreamHandler(c, info, resp)

	require.Nil(t, apiError)
	require.Equal(t, 3, usage.TotalTokens)
	require.Contains(t, recorder.Body.String(), "response.completed")
	require.NotContains(t, recorder.Body.String(), "must-not-be-forwarded")
	require.True(t, info.StreamStatus.IsNormalEnd())
}

func TestCodexResponsesStreamRejectsEOFBeforeTerminalDuringPreflight(t *testing.T) {
	c, recorder, resp, info := newCodexResponsesStreamTest(t,
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
	)

	usage, apiError := OaiResponsesStreamHandler(c, info, resp)

	require.Nil(t, usage)
	require.NotNil(t, apiError)
	require.Equal(t, types.ErrorCodeBadResponseBody, apiError.GetErrorCode())
	require.Equal(t, http.StatusBadGateway, apiError.StatusCode)
	require.Nil(t, info.CommittedUpstreamError())
	require.False(t, relaycommon.IsResponsesStreamFailureEmitted(c))
	require.Empty(t, recorder.Body.String())
	require.Empty(t, recorder.Header().Get("Content-Type"))
}

func TestOaiResponsesStreamSynthesizesFailureWhenCommittedStreamEndsWithoutTerminal(t *testing.T) {
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
			`data: {"type":"response.created","response":{"id":"resp_partial"}}` + "\n",
		)),
	}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenAI,
			UpstreamModelName: "gpt-test",
		},
		IsStream:    true,
		DisablePing: true,
	}

	usage, apiError := OaiResponsesStreamHandler(c, info, resp)

	require.Nil(t, apiError)
	require.NotNil(t, usage)
	require.NotNil(t, info.CommittedUpstreamError())
	require.Equal(t, types.ErrorCodeBadResponseBody, info.CommittedUpstreamError().GetErrorCode())
	require.True(t, relaycommon.IsResponsesStreamFailureEmitted(c))
	require.Contains(t, recorder.Body.String(), `"id":"resp_partial"`)
	require.Contains(t, recorder.Body.String(), "event: error")
	require.Contains(t, recorder.Body.String(), "ended before a terminal event")
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
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"Retry-After":  []string{"17"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-test"}}

	usage, apiError := OaiResponsesHandler(c, info, resp)

	require.Nil(t, usage)
	require.NotNil(t, apiError)
	require.Equal(t, http.StatusTooManyRequests, apiError.StatusCode)
	require.Equal(t, http.StatusTooManyRequests, apiError.UpstreamStatusCode)
	require.Equal(t, 17*time.Second, apiError.RetryAfter)
	require.True(t, info.HasUpstreamFailureResponse())
}

func TestOaiResponsesHandlerNormalizesAuthoritativeUsageDetails(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := `{"id":"resp_1","usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18,"input_tokens_details":{"cached_tokens":3,"cache_write_tokens":2,"text_tokens":4,"image_tokens":1,"audio_tokens":1},"output_tokens_details":{"reasoning_tokens":5,"text_tokens":2}}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-test"}}

	usage, apiError := OaiResponsesHandler(c, info, resp)

	require.Nil(t, apiError)
	require.NotNil(t, usage)
	require.Equal(t, 11, usage.PromptTokens)
	require.Equal(t, 7, usage.CompletionTokens)
	require.Equal(t, 18, usage.TotalTokens)
	require.Equal(t, dto.BillingUsageSourceOAIResponses, usage.UsageSource)
	require.Equal(t, dto.BillingUsageSemanticOpenAI, usage.UsageSemantic)
	require.Equal(t, 3, usage.PromptTokensDetails.CachedTokens)
	require.Equal(t, 2, usage.PromptTokensDetails.CacheWriteTokens)
	require.Equal(t, 4, usage.PromptTokensDetails.TextTokens)
	require.Equal(t, 1, usage.PromptTokensDetails.ImageTokens)
	require.Equal(t, 1, usage.PromptTokensDetails.AudioTokens)
	require.Equal(t, 5, usage.CompletionTokenDetails.ReasoningTokens)
	require.Equal(t, 2, usage.CompletionTokenDetails.TextTokens)
	require.NotNil(t, usage.BillingUsage)
	require.Equal(t, dto.BillingUsageSourceOAIResponses, usage.BillingUsage.Source)
	require.Equal(t, dto.BillingUsageSemanticOpenAI, usage.BillingUsage.Semantic)
	require.NotNil(t, usage.BillingUsage.OpenAIUsage)
	require.NotNil(t, usage.BillingUsage.OpenAIUsage.InputTokensDetails)
	require.NotNil(t, usage.BillingUsage.OpenAIUsage.OutputTokensDetails)
	require.Equal(t, 2, usage.BillingUsage.OpenAIUsage.InputTokensDetails.CacheWriteTokens)
	require.Equal(t, 5, usage.BillingUsage.OpenAIUsage.OutputTokensDetails.ReasoningTokens)
	require.JSONEq(t, body, recorder.Body.String())
}

func TestOaiResponsesCompactionHandlerPreservesWrappedErrorMetadata(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
	body := `{"error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"overloaded"}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"Retry-After":  []string{"9"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-test"}}

	usage, apiError := OaiResponsesCompactionHandler(c, info, resp)

	require.Nil(t, usage)
	require.NotNil(t, apiError)
	require.Equal(t, http.StatusServiceUnavailable, apiError.StatusCode)
	require.Equal(t, http.StatusServiceUnavailable, apiError.UpstreamStatusCode)
	require.Equal(t, 9*time.Second, apiError.RetryAfter)
	require.True(t, info.HasUpstreamFailureResponse())
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
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"Retry-After":  []string{"7"},
		},
		Body: io.NopCloser(strings.NewReader(
			`data: {"type":"response.failed","response":{"error":{"type":"server_error","message":"boom"}}}` + "\n" +
				`data: {"type":"response.output_text.delta","delta":"must-not-be-forwarded"}` + "\n" +
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
	require.True(t, info.StreamStatus.HasErrors())
	require.NotNil(t, info.CommittedUpstreamError())
	require.Equal(t, 7*time.Second, info.CommittedUpstreamError().RetryAfter)
	require.Contains(t, recorder.Body.String(), "response.failed")
	require.NotContains(t, recorder.Body.String(), "must-not-be-forwarded")
}

func TestOaiResponsesStreamStopsOnMalformedProtocolEvent(t *testing.T) {
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
			"data: {\"type\":\"response.done\",\"response\":" + "\n" +
				`data: {"type":"response.output_text.delta","delta":"must-not-be-forwarded"}` + "\n",
		)),
	}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenAI,
			UpstreamModelName: "gpt-test",
		},
		IsStream:    true,
		DisablePing: true,
	}

	usage, apiError := OaiResponsesStreamHandler(c, info, resp)

	require.Nil(t, apiError)
	require.NotNil(t, usage)
	require.True(t, info.HasUpstreamFailureResponse())
	require.NotNil(t, info.CommittedUpstreamError())
	require.Equal(t, types.ErrorCodeBadResponseBody, info.CommittedUpstreamError().GetErrorCode())
	require.True(t, info.StreamStatus.HasErrors())
	require.True(t, relaycommon.IsResponsesStreamFailureEmitted(c))
	require.Contains(t, recorder.Body.String(), "event: error")
	require.Contains(t, recorder.Body.String(), string(types.ErrorCodeBadResponseBody))
	require.NotContains(t, recorder.Body.String(), "must-not-be-forwarded")
}

func TestOaiResponsesStreamUsesAuthoritativeUsageAndImageFromEveryTerminalEvent(t *testing.T) {
	for _, eventType := range []string{
		"response.completed",
		"response.incomplete",
		"response.done",
		"response.failed",
		"response.error",
		"error",
	} {
		t.Run(eventType, func(t *testing.T) {
			previousStreamingTimeout := constant.StreamingTimeout
			constant.StreamingTimeout = 30
			t.Cleanup(func() { constant.StreamingTimeout = previousStreamingTimeout })

			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			responseStatus := ""
			if eventType == "response.done" {
				responseStatus = `"status":"completed",`
			}
			body := strings.Join([]string{
				`data: {"type":"response.output_text.delta","delta":"must not become estimated completion usage"}`,
				`data: {"type":"` + eventType + `","response":{` + responseStatus + `"usage":{"input_tokens":7,"output_tokens":0,"total_tokens":7,"input_tokens_details":{"cached_tokens":3,"cache_write_tokens":2,"text_tokens":1,"image_tokens":1,"audio_tokens":1},"output_tokens_details":{"reasoning_tokens":0,"text_tokens":0}},"output":[{"type":"image_generation_call","quality":"high","size":"1024x1024"}]}}`,
				`data: [DONE]`,
			}, "\n") + "\n"
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}
			info := &relaycommon.RelayInfo{
				ChannelMeta: &relaycommon.ChannelMeta{
					ChannelType:       constant.ChannelTypeOpenAI,
					UpstreamModelName: "gpt-test",
				},
				IsStream:    true,
				DisablePing: true,
			}

			usage, apiError := OaiResponsesStreamHandler(c, info, resp)

			require.Nil(t, apiError)
			require.Equal(t, 7, usage.PromptTokens)
			require.Zero(t, usage.CompletionTokens)
			require.Equal(t, 7, usage.TotalTokens)
			require.Equal(t, 3, usage.PromptTokensDetails.CachedTokens)
			require.Equal(t, 2, usage.PromptTokensDetails.CacheWriteTokens)
			require.Equal(t, 1, usage.PromptTokensDetails.TextTokens)
			require.Equal(t, 1, usage.PromptTokensDetails.ImageTokens)
			require.Equal(t, 1, usage.PromptTokensDetails.AudioTokens)
			require.NotNil(t, usage.OutputTokensDetails)
			require.NotNil(t, usage.BillingUsage)
			require.Equal(t, dto.BillingUsageSourceOAIResponses, usage.BillingUsage.Source)
			require.NotNil(t, usage.BillingUsage.OpenAIUsage)
			require.NotNil(t, usage.BillingUsage.OpenAIUsage.InputTokensDetails)
			require.NotNil(t, usage.BillingUsage.OpenAIUsage.OutputTokensDetails)
			require.True(t, c.GetBool("image_generation_call"))
			require.Equal(t, "high", c.GetString("image_generation_call_quality"))
			require.Equal(t, "1024x1024", c.GetString("image_generation_call_size"))
		})
	}
}

func TestCodexResponsesStreamNormalizesFailedResponseDoneDuringPreflight(t *testing.T) {
	c, recorder, resp, info := newCodexResponsesStreamTest(t,
		`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
		`data: {"type":"response.done","response":{"id":"resp_1","status":"failed","error":{"type":"server_error","code":"server_error","message":"retry elsewhere"}}}`,
		`data: [DONE]`,
	)

	usage, apiError := OaiResponsesStreamHandler(c, info, resp)

	require.Nil(t, usage)
	require.NotNil(t, apiError)
	require.Equal(t, http.StatusInternalServerError, apiError.GetUpstreamStatusCode())
	require.True(t, info.HasUpstreamFailureResponse())
	require.Empty(t, recorder.Body.String())
	require.False(t, recorder.Flushed)
	failureEvent, exists := relaycommon.GetResponsesStreamPreflightFailureEvent(c)
	require.True(t, exists)
	require.Contains(t, failureEvent, `"type":"response.failed"`)
	require.NotContains(t, failureEvent, `"type":"response.done"`)
}

func TestOaiResponsesStreamNormalizesFailedResponseDoneBeforeAccounting(t *testing.T) {
	previousStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousStreamingTimeout })

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"do not estimate this"}`,
		`data: {"type":"response.done","response":{"status":"failed","error":{"type":"server_error","message":"boom"},"usage":{"input_tokens":5,"output_tokens":0,"total_tokens":5}}}`,
		`data: [DONE]`,
	}, "\n") + "\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenAI,
			UpstreamModelName: "gpt-test",
		},
		IsStream:    true,
		DisablePing: true,
	}

	usage, apiError := OaiResponsesStreamHandler(c, info, resp)

	require.Nil(t, apiError)
	require.Equal(t, 5, usage.PromptTokens)
	require.Zero(t, usage.CompletionTokens)
	require.Equal(t, 5, usage.TotalTokens)
	require.True(t, info.HasUpstreamFailureResponse())
	require.True(t, info.StreamStatus.HasErrors())
	require.Contains(t, recorder.Body.String(), `"type":"response.failed"`)
	require.NotContains(t, recorder.Body.String(), `"type":"response.done"`)
}

// codex.rate_limits arrives FIRST on every Codex turn (production evidence from
// the WebSocket protocol logs). It is connection-scoped metadata, not semantic
// output, so it must NOT commit the preflight: an explicit capacity failure
// right after it must still fail over to a backup credential with nothing
// leaked downstream.
func TestCodexResponsesStreamMetadataDoesNotCommitPreflight(t *testing.T) {
	c, recorder, resp, info := newCodexResponsesStreamTest(t,
		`data: {"type":"codex.rate_limits","rate_limits":{"primary_used_percent":12}}`,
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"type":"server_error","code":"model_at_capacity","message":"Selected model is at capacity."}}}`,
		`data: [DONE]`,
	)

	usage, apiError := OaiResponsesStreamHandler(c, info, resp)

	require.Nil(t, usage)
	require.NotNil(t, apiError)
	require.Equal(t, types.ErrorCodeModelAtCapacity, apiError.GetErrorCode())
	require.Empty(t, recorder.Body.String(), "metadata must not commit the downstream before failover")
	require.Empty(t, recorder.Header().Get("X-Codex-Turn-State"))
}

// Buffered connection metadata is replayed to the client once real output
// commits the preflight — buffering must not drop it.
func TestCodexResponsesStreamReplaysBufferedMetadataAfterCommit(t *testing.T) {
	c, recorder, resp, info := newCodexResponsesStreamTest(t,
		`data: {"type":"codex.rate_limits","rate_limits":{"primary_used_percent":12}}`,
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		`data: {"type":"response.output_text.delta","delta":"hi"}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
		`data: [DONE]`,
	)

	usage, apiError := OaiResponsesStreamHandler(c, info, resp)

	require.Nil(t, apiError)
	require.NotNil(t, usage)
	require.Equal(t, 3, usage.TotalTokens)
	got := recorder.Body.String()
	require.Contains(t, got, "codex.rate_limits")
	require.Contains(t, got, "response.completed")
}

// A typeless stream event cannot be classified; before anything is committed it
// fails the preflight so the relay can fail over safely.
func TestCodexResponsesStreamTypelessEventFailsPreflight(t *testing.T) {
	c, recorder, resp, info := newCodexResponsesStreamTest(t,
		`data: {"foo":"bar"}`,
		`data: [DONE]`,
	)

	usage, apiError := OaiResponsesStreamHandler(c, info, resp)

	require.Nil(t, usage)
	require.NotNil(t, apiError)
	require.Equal(t, http.StatusBadGateway, apiError.GetUpstreamStatusCode())
	require.Empty(t, recorder.Body.String())
}

// fillReader yields n bytes of filler — an oversized upstream body without
// allocating the payload up front.
type fillReader struct{ remaining int }

func (r *fillReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > r.remaining {
		n = r.remaining
	}
	for i := 0; i < n; i++ {
		p[i] = 'a'
	}
	r.remaining -= n
	return n, nil
}

// An oversized non-stream "success" body must fail bounded instead of being
// buffered into gateway memory without limit.
func TestOaiResponsesHandlerRejectsOversizedBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(&fillReader{remaining: maxResponsesBodyBytes + 1}),
	}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-test"}}

	usage, apiError := OaiResponsesHandler(c, info, resp)
	require.Nil(t, usage)
	require.NotNil(t, apiError)
	require.Equal(t, http.StatusBadGateway, apiError.StatusCode)
	require.Contains(t, apiError.Error(), "exceeds")
}

// A 200 body with no identity, status, or output (e.g. `{}`) is not a Responses
// result: forwarding it would fake success on zero usage.
func TestOaiResponsesHandlerRejectsEmptySuccessObject(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{}`)),
	}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-test"}}

	usage, apiError := OaiResponsesHandler(c, info, resp)
	require.Nil(t, usage)
	require.NotNil(t, apiError)
	require.Equal(t, http.StatusBadGateway, apiError.StatusCode)
	require.Empty(t, recorder.Body.String())
}

// When buffered metadata overflows the preflight limit, the commit replays the
// buffer (the overflowing event included); the handler must not send that event
// a second time through the common post-commit path.
func TestCodexResponsesStreamOversizedMetadataCommitDoesNotDuplicate(t *testing.T) {
	oversized := `{"type":"codex.rate_limits","filler":"` + strings.Repeat("x", maxResponsesStreamPreflightBytes) + `"}`
	c, recorder, resp, info := newCodexResponsesStreamTest(t,
		"data: "+oversized,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
		`data: [DONE]`,
	)

	usage, apiError := OaiResponsesStreamHandler(c, info, resp)

	require.Nil(t, apiError)
	require.NotNil(t, usage)
	// One SSE emission produces "codex.rate_limits" twice (event: line + data
	// type field), so count the payload-only filler marker instead.
	require.Equal(t, 1, strings.Count(recorder.Body.String(), `"filler"`))
}
