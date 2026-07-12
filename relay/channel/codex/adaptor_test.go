package codex

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSetupRequestHeaderForwardsCodexClientHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("X-Codex-Beta-Features", "remote_compaction_v2")
	c.Request.Header.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")
	c.Request.Header.Set("Session-Id", "session-123")
	c.Request.Header.Set("User-Agent", "untrusted-client")
	c.Request.Header.Set("Originator", "untrusted-originator")

	headers := make(http.Header)
	err := (&Adaptor{}).SetupRequestHeader(c, &headers, &relaycommon.RelayInfo{
		IsStream: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			ApiKey: `{"access_token":"access-token","account_id":"account-id"}`,
		},
	})

	require.NoError(t, err)
	require.Equal(t, "remote_compaction_v2", headers.Get("X-Codex-Beta-Features"))
	require.Equal(t, "true", headers.Get("X-OpenAI-Internal-Codex-Responses-Lite"))
	require.Equal(t, "session-123", headers.Get("Session-Id"))
	require.Equal(t, "Bearer access-token", headers.Get("Authorization"))
	require.Equal(t, "account-id", headers.Get("Chatgpt-Account-Id"))
	require.Equal(t, CodexOAuthUserAgent, headers.Get("User-Agent"))
	require.Equal(t, CodexOAuthOriginator, headers.Get("Originator"))
}

func TestCodexOAuthPacingHonorsContextCancellation(t *testing.T) {
	originalInterval := CodexOAuthMinRequestInterval
	CodexOAuthMinRequestInterval = time.Hour
	t.Cleanup(func() { CodexOAuthMinRequestInterval = originalInterval })

	channelID := 910003
	require.NoError(t, waitForCodexOAuthTurn(context.Background(), channelID))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, waitForCodexOAuthTurn(ctx, channelID), context.Canceled)
}

func TestCodexOAuthConcurrencySlots(t *testing.T) {
	originalMaxConcurrency := CodexOAuthMaxConcurrency
	CodexOAuthMaxConcurrency = 2
	t.Cleanup(func() { CodexOAuthMaxConcurrency = originalMaxConcurrency })

	channelID := 910001
	releaseFirst, ok := acquireCodexOAuthSlot(channelID)
	require.True(t, ok)
	releaseSecond, ok := acquireCodexOAuthSlot(channelID)
	require.True(t, ok)

	_, ok = acquireCodexOAuthSlot(channelID)
	require.False(t, ok)

	releaseFirst()
	releaseThird, ok := acquireCodexOAuthSlot(channelID)
	require.True(t, ok)
	releaseSecond()
	releaseThird()
}

func TestCodexOAuthResponseBodyReleasesSlotOnce(t *testing.T) {
	originalMaxConcurrency := CodexOAuthMaxConcurrency
	CodexOAuthMaxConcurrency = 2
	t.Cleanup(func() { CodexOAuthMaxConcurrency = originalMaxConcurrency })

	channelID := 910002
	release, ok := acquireCodexOAuthSlot(channelID)
	require.True(t, ok)
	body := &codexOAuthResponseBody{
		ReadCloser: io.NopCloser(strings.NewReader("ok")),
		release:    release,
	}

	require.NoError(t, body.Close())
	require.NoError(t, body.Close())

	first, ok := acquireCodexOAuthSlot(channelID)
	require.True(t, ok)
	second, ok := acquireCodexOAuthSlot(channelID)
	require.True(t, ok)
	_, ok = acquireCodexOAuthSlot(channelID)
	require.False(t, ok)
	first()
	second()
}
