package codex

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCodexChannelTestMetadataIsStablePerRequestAndUniqueAcrossRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	firstContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	firstContext.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	secondContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	secondContext.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{
		IsChannelTest: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			ApiKey:            `{"access_token":"access-token","account_id":"account-id"}`,
			UpstreamModelName: "gpt-5.6-sol",
		},
	}

	converted, err := (&Adaptor{}).ConvertOpenAIResponsesRequest(firstContext, info, dto.OpenAIResponsesRequest{})
	require.NoError(t, err)
	request := converted.(dto.OpenAIResponsesRequest)
	var clientMetadata map[string]any
	require.NoError(t, common.Unmarshal(request.ClientMetadata, &clientMetadata))

	headers := make(http.Header)
	require.NoError(t, (&Adaptor{}).SetupRequestHeader(firstContext, &headers, info))
	require.Equal(t, clientMetadata["session_id"], headers.Get("session-id"))
	require.Equal(t, clientMetadata["thread_id"], headers.Get("thread-id"))
	require.Equal(t, clientMetadata["x-codex-window-id"], headers.Get("x-codex-window-id"))

	firstMetadata, err := getOrCreateCodexChannelTestMetadata(firstContext)
	require.NoError(t, err)
	secondMetadata, err := getOrCreateCodexChannelTestMetadata(secondContext)
	require.NoError(t, err)
	require.NotEqual(t, firstMetadata.SessionID, secondMetadata.SessionID)
	require.NotEqual(t, firstMetadata.TurnID, secondMetadata.TurnID)
	require.NotEqual(t, firstMetadata.InstallationID, secondMetadata.InstallationID)
}

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
	require.Empty(t, headers.Get("X-OpenAI-Internal-Codex-Responses-Lite"))
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

func TestInitOAuthRuntimeSettingsReadsLoadedEnvironment(t *testing.T) {
	originalMaxConcurrency := CodexOAuthMaxConcurrency
	originalInterval := CodexOAuthMinRequestInterval
	t.Cleanup(func() {
		CodexOAuthMaxConcurrency = originalMaxConcurrency
		CodexOAuthMinRequestInterval = originalInterval
	})
	t.Setenv("CODEX_OAUTH_MAX_CONCURRENCY", "7")
	t.Setenv("CODEX_OAUTH_MIN_REQUEST_INTERVAL_MS", "125")

	InitOAuthRuntimeSettings()

	require.Equal(t, 7, CodexOAuthMaxConcurrency)
	require.Equal(t, 125*time.Millisecond, CodexOAuthMinRequestInterval)
}

func TestCodexLocalConcurrencyLimitRemainsRetryable(t *testing.T) {
	originalMaxConcurrency := CodexOAuthMaxConcurrency
	CodexOAuthMaxConcurrency = 1
	t.Cleanup(func() { CodexOAuthMaxConcurrency = originalMaxConcurrency })

	channelID := 910004
	release, ok := acquireCodexOAuthSlot(channelID)
	require.True(t, ok)
	defer release()

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	_, err := (&Adaptor{}).DoRequest(c, &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: channelID},
	}, http.NoBody)
	require.Error(t, err)
	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusServiceUnavailable, apiErr.StatusCode)
	require.False(t, types.IsSkipRetryError(apiErr))
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
