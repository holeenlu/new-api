package openai

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	require.True(t, info.HasUpstreamFailureResponse())
	require.Empty(t, recorder.Body.String())
	failureEvent, exists := relaycommon.GetResponsesStreamPreflightFailureEvent(c)
	require.True(t, exists)
	require.Contains(t, failureEvent, `"type":"response.failed"`)
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
