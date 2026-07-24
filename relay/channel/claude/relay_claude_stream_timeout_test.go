package claude

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// blockingStreamBody blocks Read until Close is called, simulating an upstream
// that accepts the connection (HTTP 200) but never emits an SSE event — the
// exact shape of a usage-exhausted subscription account's silent stream.
type blockingStreamBody struct {
	done      chan struct{}
	closeOnce sync.Once
}

func newBlockingStreamBody() *blockingStreamBody {
	return &blockingStreamBody{done: make(chan struct{})}
}

func (b *blockingStreamBody) Read([]byte) (int, error) {
	<-b.done
	return 0, io.EOF
}

func (b *blockingStreamBody) Close() error {
	b.closeOnce.Do(func() { close(b.done) })
	return nil
}

type closeTrackingStreamBody struct {
	io.Reader
	done      chan struct{}
	closeOnce sync.Once
}

func newCloseTrackingStreamBody(data string) *closeTrackingStreamBody {
	return &closeTrackingStreamBody{
		Reader: strings.NewReader(data),
		done:   make(chan struct{}),
	}
}

func (b *closeTrackingStreamBody) Close() error {
	b.closeOnce.Do(func() { close(b.done) })
	return nil
}

func newClaudeSubscriptionStreamTest(
	t *testing.T,
	header http.Header,
	body io.ReadCloser,
) (*gin.Context, *httptest.ResponseRecorder, *http.Response, *relaycommon.RelayInfo) {
	t.Helper()
	previousStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	previousFirstEventTimeout := ClaudeCodeOAuthStreamFirstEventTimeout
	ClaudeCodeOAuthStreamFirstEventTimeout = 100 * time.Millisecond
	t.Cleanup(func() {
		constant.StreamingTimeout = previousStreamingTimeout
		ClaudeCodeOAuthStreamFirstEventTimeout = previousFirstEventTimeout
	})

	if header == nil {
		header = http.Header{}
	}
	header.Set("Content-Type", "text/event-stream")
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: header, Body: body}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeClaudeCode,
			UpstreamModelName: "claude-fable-5",
		},
		IsStream:    true,
		DisablePing: true,
		RelayFormat: types.RelayFormatClaude,
	}
	return c, recorder, resp, info
}

// A silent subscription stream must fail over quickly with a retryable error
// instead of idling for the full streaming timeout, and must not commit any
// downstream output.
func TestClaudeSubscriptionStreamFailsOverOnSilentStream(t *testing.T) {
	c, recorder, resp, info := newClaudeSubscriptionStreamTest(t, nil, newBlockingStreamBody())

	usage, apiError := ClaudeStreamHandler(c, resp, info)

	require.Nil(t, usage)
	require.NotNil(t, apiError)
	require.Equal(t, types.ErrorCodeDoRequestFailed, apiError.GetErrorCode())
	require.Equal(t, http.StatusBadGateway, apiError.StatusCode)
	require.Equal(t, relaycommon.StreamEndReasonTimeout, info.StreamStatus.EndReason)
	require.Empty(t, recorder.Body.String())
	require.Empty(t, recorder.Header().Get("Content-Type"))
}

// When the silent stream's rate-limit headers prove the window is exhausted, the
// failover error carries the usage-limit classification and the parsed reset so
// the credential is cooled and the client learns when it recovers.
func TestClaudeSubscriptionStreamFailsFastOnUsageLimitHeaders(t *testing.T) {
	header := http.Header{}
	header.Set("anthropic-ratelimit-unified-status", "rejected")
	header.Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(time.Now().Add(3*time.Hour).Unix(), 10))
	body := newBlockingStreamBody()
	c, recorder, resp, info := newClaudeSubscriptionStreamTest(t, header, body)

	usage, apiError := ClaudeStreamHandler(c, resp, info)

	require.Nil(t, usage)
	require.NotNil(t, apiError)
	require.Equal(t, types.ErrorCodeUpstreamUsageLimit, apiError.GetErrorCode())
	require.Equal(t, http.StatusTooManyRequests, apiError.GetUpstreamStatusCode())
	require.InDelta(t, (3 * time.Hour).Seconds(), apiError.RetryAfter.Seconds(), 5)
	require.Empty(t, recorder.Body.String())
	select {
	case <-body.done:
	default:
		require.FailNow(t, "usage-limit response body was not closed")
	}
}

func TestClaudeSubscriptionStreamFailsFastOnWindowHeaderWithoutReset(t *testing.T) {
	header := http.Header{}
	header.Set("anthropic-ratelimit-unified-5h-status", "rejected")
	body := newBlockingStreamBody()
	c, recorder, resp, info := newClaudeSubscriptionStreamTest(t, header, body)

	usage, apiError := ClaudeStreamHandler(c, resp, info)

	require.Nil(t, usage)
	require.NotNil(t, apiError)
	require.Equal(t, types.ErrorCodeUpstreamUsageLimit, apiError.GetErrorCode())
	require.True(t, apiError.UsageWindows.FiveHourExhausted)
	require.Zero(t, apiError.RetryAfter)
	require.Empty(t, recorder.Body.String())
	select {
	case <-body.done:
	default:
		require.FailNow(t, "usage-limit response body was not closed")
	}
}

func TestClaudeSubscriptionStreamEmitsTerminalSSEErrorAfterCommittedUsageLimit(t *testing.T) {
	body := newCloseTrackingStreamBody(strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_usage_limit","model":"claude-fable-5","usage":{"input_tokens":1,"output_tokens":0}}}`,
		`data: {"type":"error","error":{"type":"rate_limit_error","code":"usage_limit_reached","message":"subscription usage limit reached"}}`,
	}, "\n"))
	c, recorder, resp, info := newClaudeSubscriptionStreamTest(t, nil, body)

	usage, apiError := ClaudeStreamHandler(c, resp, info)

	require.NotNil(t, usage)
	require.Nil(t, apiError)
	committed := info.CommittedUpstreamError()
	require.NotNil(t, committed)
	require.Equal(t, types.ErrorCodeUpstreamUsageLimit, committed.GetErrorCode())
	require.Equal(t, "anthropic", usage.UsageSemantic)
	require.NotNil(t, usage.BillingUsage)
	require.Contains(t, recorder.Body.String(), "event: message_start")
	require.Contains(t, recorder.Body.String(), "event: error")
	require.Contains(t, recorder.Body.String(), `"code":"upstream_usage_limit"`)
	select {
	case <-body.done:
	default:
		require.FailNow(t, "committed usage-limit stream body was not closed")
	}
}

// A subscription stream that closes immediately with no event (HTTP 200 then
// EOF, rather than idling until the first-event bound) is the same silent
// failure and must also fail over without committing downstream output.
func TestClaudeSubscriptionStreamFailsOverOnImmediateEmptyStream(t *testing.T) {
	body := io.NopCloser(strings.NewReader(""))
	c, recorder, resp, info := newClaudeSubscriptionStreamTest(t, nil, body)

	usage, apiError := ClaudeStreamHandler(c, resp, info)

	require.Nil(t, usage)
	require.NotNil(t, apiError)
	require.Equal(t, types.ErrorCodeDoRequestFailed, apiError.GetErrorCode())
	require.Equal(t, http.StatusBadGateway, apiError.StatusCode)
	require.Equal(t, relaycommon.StreamEndReasonEOF, info.StreamStatus.EndReason)
	require.Empty(t, recorder.Body.String())
	require.Empty(t, recorder.Header().Get("Content-Type"))
}

// A subscription stream that emits only [DONE] with no preceding event ends in
// the "done" reason yet produced no output, so it is the same silent failure and
// must fail over rather than be delivered as a successful empty stream.
func TestClaudeSubscriptionStreamFailsOverOnDoneOnlyStream(t *testing.T) {
	body := io.NopCloser(strings.NewReader("data: [DONE]\n"))
	c, recorder, resp, info := newClaudeSubscriptionStreamTest(t, nil, body)

	usage, apiError := ClaudeStreamHandler(c, resp, info)

	require.Nil(t, usage)
	require.NotNil(t, apiError)
	require.Equal(t, types.ErrorCodeDoRequestFailed, apiError.GetErrorCode())
	require.Equal(t, http.StatusBadGateway, apiError.StatusCode)
	require.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
	require.Empty(t, recorder.Body.String())
	require.Empty(t, recorder.Header().Get("Content-Type"))
}

// Once the first event arrives the first-event bound no longer applies: a stream
// that produces output is delivered normally and is never aborted as silent.
func TestClaudeSubscriptionStreamDeliversOutputAfterFirstEvent(t *testing.T) {
	body := io.NopCloser(strings.NewReader("data: {\"type\":\"ping\"}\ndata: [DONE]\n"))
	c, _, resp, info := newClaudeSubscriptionStreamTest(t, nil, body)

	usage, apiError := ClaudeStreamHandler(c, resp, info)

	require.Nil(t, apiError)
	require.NotNil(t, usage)
	require.NotEqual(t, relaycommon.StreamEndReasonTimeout, info.StreamStatus.EndReason)
}
