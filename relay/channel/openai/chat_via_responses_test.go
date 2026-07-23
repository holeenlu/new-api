package openai

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newResponsesChatTestContext(t *testing.T, body string, isStream bool) (*gin.Context, *httptest.ResponseRecorder, *http.Response, *relaycommon.RelayInfo) {
	t.Helper()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set(common.RequestIdKey, "responses-test")

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}
	info := &relaycommon.RelayInfo{
		ChannelMeta:        &relaycommon.ChannelMeta{UpstreamModelName: "gpt-test"},
		IsStream:           isStream,
		RelayFormat:        types.RelayFormatOpenAI,
		ShouldIncludeUsage: true,
		DisablePing:        true,
	}
	return c, recorder, resp, info
}

func TestOaiResponsesToChatStreamHandlerConvertsSSEOrderAndUsage(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-test","created_at":1710000000}}`,
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup"}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"q\":\"x\"}"}`,
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`,
		`data: [DONE]`,
		``,
	}, "\n")

	c, recorder, resp, info := newResponsesChatTestContext(t, body, true)

	usage, err := OaiResponsesToChatStreamHandler(c, info, resp)
	require.Nil(t, err)
	require.NotNil(t, usage)
	require.Equal(t, 2, usage.PromptTokens)
	require.Equal(t, 3, usage.CompletionTokens)
	require.Equal(t, 5, usage.TotalTokens)

	got := recorder.Body.String()
	require.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
	require.Contains(t, got, `"role":"assistant"`)
	require.Contains(t, got, `"content":"hello"`)
	require.Contains(t, got, `"name":"lookup"`)
	require.Contains(t, got, `"arguments":"{\"q\":\"x\"}"`)
	require.Contains(t, got, `"finish_reason":"tool_calls"`)
	require.Contains(t, got, `"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5`)
	require.Contains(t, got, `data: [DONE]`)
	requireOrderedSubstrings(t, got,
		`"role":"assistant"`,
		`"content":"hello"`,
		`"name":"lookup"`,
		`"arguments":"{\"q\":\"x\"}"`,
		`"finish_reason":"tool_calls"`,
		`"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5`,
		`data: [DONE]`,
	)
}

func TestOaiResponsesToChatBufferedStreamHandlerReturnsJSONFromSSE(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	body := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"buffered text"}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup"}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"q\":\"x\"}"}`,
		`data: {"type":"response.done","response":{"model":"gpt-test","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
		`data: [DONE]`,
		``,
	}, "\n")

	c, recorder, resp, info := newResponsesChatTestContext(t, body, false)

	usage, err := OaiResponsesToChatBufferedStreamHandler(c, info, resp)
	require.Nil(t, err)
	require.NotNil(t, usage)
	require.Equal(t, 3, usage.TotalTokens)

	got := recorder.Body.String()
	require.NotContains(t, got, `data:`)
	require.Contains(t, got, `"object":"chat.completion"`)
	require.Contains(t, got, `"content":"buffered text"`)
	require.Contains(t, got, `"name":"lookup"`)
	require.Contains(t, got, `"arguments":"{\"q\":\"x\"}"`)
	require.Contains(t, got, `"finish_reason":"tool_calls"`)
}

func TestOaiChatToResponsesStreamHandlerConvertsSSEOrderAndUsage(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := strings.Join([]string{
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":1710000000,"model":"gpt-test","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":1710000000,"model":"gpt-test","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":1710000000,"model":"gpt-test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup"}}]},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":1710000000,"model":"gpt-test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":\"x\"}"}}]},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":1710000000,"model":"gpt-test","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":1710000000,"model":"gpt-test","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`,
		`data: [DONE]`,
		``,
	}, "\n")

	c, recorder, resp, info := newResponsesChatTestContext(t, body, true)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	usage, err := OaiChatToResponsesStreamHandler(c, info, resp)
	require.Nil(t, err)
	require.NotNil(t, usage)
	require.Equal(t, 2, usage.PromptTokens)
	require.Equal(t, 3, usage.CompletionTokens)
	require.Equal(t, 5, usage.TotalTokens)

	got := recorder.Body.String()
	require.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
	require.Contains(t, got, `event: response.created`)
	require.Contains(t, got, `event: response.output_text.delta`)
	require.Contains(t, got, `"delta":"hello"`)
	require.Contains(t, got, `event: response.function_call_arguments.delta`)
	require.Contains(t, got, `"delta":"{\"q\":\"x\"}"`)
	require.Contains(t, got, `event: response.completed`)
	require.Contains(t, got, `"input_tokens":2`)
	require.Contains(t, got, `"output_tokens":3`)
	requireOrderedSubstrings(t, got,
		`event: response.created`,
		`event: response.output_item.added`,
		`event: response.output_text.delta`,
		`event: response.output_item.added`,
		`event: response.function_call_arguments.delta`,
		`event: response.output_text.done`,
		`event: response.function_call_arguments.done`,
		`event: response.completed`,
	)
}

func requireOrderedSubstrings(t *testing.T, s string, parts ...string) {
	t.Helper()

	offset := 0
	for _, part := range parts {
		idx := strings.Index(s[offset:], part)
		require.NotEqualf(t, -1, idx, "missing %q after byte offset %d", part, offset)
		offset += idx + len(part)
	}
}

// A truncated upstream stream (EOF before any terminal event) must fail the
// buffered conversion instead of fabricating a "completed" chat response that
// hides the upstream failure and settles billing on partial output.
func TestOaiResponsesToChatBufferedStreamHandlerFailsWithoutTerminal(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	body := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		`data: {"type":"response.output_text.delta","delta":"partial"}`,
		``, // upstream dies here: no terminal, no [DONE]
	}, "\n")

	c, recorder, resp, info := newResponsesChatTestContext(t, body, false)

	usage, err := OaiResponsesToChatBufferedStreamHandler(c, info, resp)
	require.Nil(t, usage)
	require.Error(t, err)
	require.Equal(t, http.StatusBadGateway, err.StatusCode)
	require.NotContains(t, recorder.Body.String(), `"chat.completion"`)
}

// A top-level `{"type":"error"}` event is a terminal failure for the buffered
// conversion, carrying the upstream's classification.
func TestOaiResponsesToChatBufferedStreamHandlerTreatsTopLevelErrorAsFailure(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	body := strings.Join([]string{
		`data: {"type":"error","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"slow down"}}`,
		`data: [DONE]`,
		``,
	}, "\n")

	c, _, resp, info := newResponsesChatTestContext(t, body, false)

	usage, err := OaiResponsesToChatBufferedStreamHandler(c, info, resp)
	require.Nil(t, usage)
	require.Error(t, err)
	require.Equal(t, http.StatusTooManyRequests, err.StatusCode)
	require.Contains(t, err.Error(), "slow down")
}

// A truncated upstream stream must not end with a fabricated normal chat
// termination: no finalize chunk, no [DONE]; an in-stream error chunk tells the
// OpenAI-format client the turn failed.
func TestOaiResponsesToChatStreamHandlerFailsWithoutTerminal(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-test","created_at":1710000000}}`,
		`data: {"type":"response.output_text.delta","delta":"partial"}`,
		``, // upstream dies here: no terminal, no [DONE]
	}, "\n")

	c, recorder, resp, info := newResponsesChatTestContext(t, body, true)

	usage, err := OaiResponsesToChatStreamHandler(c, info, resp)
	require.Nil(t, err) // committed stream: failure is recorded, usage settles on received output
	require.NotNil(t, usage)

	got := recorder.Body.String()
	require.NotContains(t, got, `data: [DONE]`)
	require.Contains(t, got, `"error"`)
	require.Contains(t, got, "before a terminal event")
	require.NotNil(t, info.CommittedUpstreamError())
}

// A top-level `{"type":"error"}` after the stream has committed must surface as
// the protocol's in-stream error chunk and suppress the normal termination —
// not as a JSON error appended to a live SSE stream.
func TestOaiResponsesToChatStreamHandlerTreatsTopLevelErrorAsFailure(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-test","created_at":1710000000}}`,
		`data: {"type":"response.output_text.delta","delta":"partial"}`,
		`data: {"type":"error","error":{"type":"server_error","code":"internal_error","message":"upstream exploded"}}`,
		`data: [DONE]`,
		``,
	}, "\n")

	c, recorder, resp, info := newResponsesChatTestContext(t, body, true)

	usage, err := OaiResponsesToChatStreamHandler(c, info, resp)
	// Committed stream: the failure is surfaced in-stream and recorded; usage
	// settles on received output.
	require.Nil(t, err)
	require.NotNil(t, usage)
	require.NotNil(t, info.CommittedUpstreamError())

	got := recorder.Body.String()
	require.NotContains(t, got, `data: [DONE]`)
	require.Contains(t, got, `"error"`)
	require.Contains(t, got, "upstream exploded")
}

// A committed Claude-format conversion must terminate a truncated stream with
// the Claude protocol's `event: error`, not a bare truncation.
func TestOaiResponsesToChatStreamHandlerEmitsClaudeErrorEvent(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-test","created_at":1710000000}}`,
		`data: {"type":"response.output_text.delta","delta":"partial"}`,
		``, // truncated: no terminal
	}, "\n")

	c, recorder, resp, info := newResponsesChatTestContext(t, body, true)
	info.RelayFormat = types.RelayFormatClaude

	usage, err := OaiResponsesToChatStreamHandler(c, info, resp)
	require.Nil(t, err)
	require.NotNil(t, usage)
	require.NotNil(t, info.CommittedUpstreamError())

	got := recorder.Body.String()
	require.Contains(t, got, "event: error")
	require.Contains(t, got, `"type":"error"`)
	// Inner error type must be a Claude-protocol type (via ToClaudeError), not a
	// raw gateway type like new_api_error.
	require.Contains(t, got, `"type":"api_error"`)
	require.NotContains(t, got, "new_api_error")
	require.Contains(t, got, "before a terminal event")
}

// A 200-wrapped upstream error on the non-stream conversion path must keep its
// real classification: 429 stays retryable with its Retry-After, and the
// upstream failure is marked — not a dead-end StatusCode=200 error.
func TestOaiResponsesToChatHandlerDerivesWrappedErrorStatus(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	c, _, resp, info := newResponsesChatTestContext(t,
		`{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"slow down"}}`, false)
	resp.Header.Set("Retry-After", "30")

	usage, err := OaiResponsesToChatHandler(c, info, resp)
	require.Nil(t, usage)
	require.Error(t, err)
	require.Equal(t, http.StatusTooManyRequests, err.StatusCode)
	require.Equal(t, 30*time.Second, err.RetryAfter)
	require.True(t, info.HasUpstreamFailureResponse())
}

// A terminal failure event that carries authoritative usage must settle billing
// on it — the upstream already consumed those input tokens — not on a text
// estimate.
func TestOaiResponsesToChatStreamHandlerSettlesFailureUsage(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-test","created_at":1710000000}}`,
		`data: {"type":"response.output_text.delta","delta":"partial"}`,
		`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","usage":{"input_tokens":41,"output_tokens":2,"total_tokens":43},"error":{"type":"server_error","message":"boom"}}}`,
		``,
	}, "\n")

	c, _, resp, info := newResponsesChatTestContext(t, body, true)

	usage, err := OaiResponsesToChatStreamHandler(c, info, resp)
	require.Nil(t, err) // committed: surfaced in-stream
	require.NotNil(t, usage)
	require.Equal(t, 41, usage.PromptTokens)
	require.Equal(t, 43, usage.TotalTokens)
	require.True(t, info.HasUpstreamFailureResponse())
}

// JSON-semantic integrity: `"status":null` must not pass as a real status.
func TestOaiResponsesToChatHandlerRejectsNullStatusBody(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	c, _, resp, info := newResponsesChatTestContext(t, `{"id":"resp_1","status":null,"output":null}`, false)

	usage, err := OaiResponsesToChatHandler(c, info, resp)
	require.Nil(t, usage)
	require.Error(t, err)
	require.Equal(t, http.StatusBadGateway, err.StatusCode)
}

// Authoritative-usage boundaries: a failure usage without total_tokens keeps
// its input/output split (total derived, not re-estimated), and an explicit
// all-zero usage stays zero instead of becoming a billed prompt estimate.
func TestOaiResponsesToChatStreamHandlerAuthoritativeUsageBoundaries(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	t.Run("missing total_tokens keeps split", func(t *testing.T) {
		body := strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-test","created_at":1710000000}}`,
			`data: {"type":"response.output_text.delta","delta":"partial"}`,
			`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","usage":{"input_tokens":41,"output_tokens":2},"error":{"type":"server_error","message":"boom"}}}`,
			``,
		}, "\n")
		c, _, resp, info := newResponsesChatTestContext(t, body, true)

		usage, err := OaiResponsesToChatStreamHandler(c, info, resp)
		require.Nil(t, err)
		require.NotNil(t, usage)
		require.Equal(t, 41, usage.PromptTokens)
		require.Equal(t, 2, usage.CompletionTokens)
		require.Equal(t, 43, usage.TotalTokens)
		// The nested billing snapshot must carry the same completed total: the
		// upstream copy is completed BEFORE normalization takes its snapshot.
		require.NotNil(t, usage.BillingUsage)
		require.NotNil(t, usage.BillingUsage.OpenAIUsage)
		require.Equal(t, 43, usage.BillingUsage.OpenAIUsage.TotalTokens)
	})

	t.Run("explicit zero stays zero", func(t *testing.T) {
		body := strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-test","created_at":1710000000}}`,
			`data: {"type":"response.output_text.delta","delta":"partial"}`,
			`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0},"error":{"type":"server_error","message":"boom"}}}`,
			``,
		}, "\n")
		c, _, resp, info := newResponsesChatTestContext(t, body, true)

		usage, err := OaiResponsesToChatStreamHandler(c, info, resp)
		require.Nil(t, err)
		require.NotNil(t, usage)
		require.Zero(t, usage.PromptTokens, "explicit upstream zero must not become a billed estimate")
		require.Zero(t, usage.TotalTokens)
	})
}

// A clean completion carrying explicit zero usage is authoritative too: it must
// not be replaced by a billed prompt estimate.
func TestOaiResponsesToChatStreamHandlerKeepsZeroUsageOnCleanCompletion(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-test","created_at":1710000000}}`,
		`data: {"type":"response.output_text.delta","delta":"cached answer"}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`,
		`data: [DONE]`,
		``,
	}, "\n")

	c, _, resp, info := newResponsesChatTestContext(t, body, true)

	usage, err := OaiResponsesToChatStreamHandler(c, info, resp)
	require.Nil(t, err)
	require.NotNil(t, usage)
	require.Zero(t, usage.PromptTokens, "explicit upstream zero must not become a billed estimate")
	require.Zero(t, usage.TotalTokens)
}

// terminalCancelWriter lets pre-terminal chunks land in the recorder, then on
// the terminal chunk (exact `"finish_reason":"stop"` — every chunk serializes
// `"finish_reason":null`, so a bare substring match would trip on the first
// chunk) records the attempt, cancels the request context (the client
// disconnecting right at the terminal), and fails the write. The context
// cancellation is what the handler actually observes: CustomEvent.Render
// swallows raw write errors, but FlushWriter reports a done context.
type terminalCancelWriter struct {
	*httptest.ResponseRecorder
	cancel          context.CancelFunc
	terminalAttempt bool
}

func (w *terminalCancelWriter) Write(b []byte) (int, error) {
	if strings.Contains(string(b), `"finish_reason":"stop"`) {
		w.terminalAttempt = true
		w.cancel()
		return 0, fmt.Errorf("downstream write failed at terminal")
	}
	return w.ResponseRecorder.Write(b)
}

// A downstream write failure at the clean terminal must not lose the terminal's
// authoritative usage: the capture happens after conversion but before the
// writes, so an explicit zero stays zero instead of becoming a billed estimate.
func TestOaiResponsesToChatStreamHandlerKeepsZeroUsageWhenTerminalWriteFails(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-test","created_at":1710000000}}`,
		`data: {"type":"response.output_text.delta","delta":"cached answer"}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`,
		`data: [DONE]`,
		``,
	}, "\n")

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	recorder := httptest.NewRecorder()
	writer := &terminalCancelWriter{ResponseRecorder: recorder, cancel: cancelRequest}
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(requestCtx)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-test"},
		IsStream:    true,
		RelayFormat: types.RelayFormatOpenAI,
		DisablePing: true,
	}

	usage, err := OaiResponsesToChatStreamHandler(c, info, resp)
	require.Nil(t, err) // committed stream: failure recorded, usage settles
	require.NotNil(t, usage)
	require.True(t, writer.terminalAttempt, "the terminal write must actually have been attempted")
	require.Contains(t, recorder.Body.String(), "cached answer", "pre-terminal chunks must have reached the client")
	require.Zero(t, usage.PromptTokens, "authoritative zero must survive a failed terminal write")
	require.Zero(t, usage.TotalTokens)
}
