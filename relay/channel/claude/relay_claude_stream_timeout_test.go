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
func TestClaudeSubscriptionStreamClassifiesUsageLimitFromHeadersOnSilentStream(t *testing.T) {
	header := http.Header{}
	header.Set("anthropic-ratelimit-unified-status", "rejected")
	header.Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(time.Now().Add(3*time.Hour).Unix(), 10))
	c, recorder, resp, info := newClaudeSubscriptionStreamTest(t, header, newBlockingStreamBody())

	usage, apiError := ClaudeStreamHandler(c, resp, info)

	require.Nil(t, usage)
	require.NotNil(t, apiError)
	require.Equal(t, types.ErrorCodeUpstreamUsageLimit, apiError.GetErrorCode())
	require.Equal(t, http.StatusTooManyRequests, apiError.GetUpstreamStatusCode())
	require.InDelta(t, (3 * time.Hour).Seconds(), apiError.RetryAfter.Seconds(), 5)
	require.Empty(t, recorder.Body.String())
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
