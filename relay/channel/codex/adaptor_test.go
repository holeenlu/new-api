package codex

import (
	"net/http"
	"net/http/httptest"
	"testing"

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

	headers := make(http.Header)
	err := (&Adaptor{}).SetupRequestHeader(c, &headers, &relaycommon.RelayInfo{
		ApiKey:   `{"access_token":"access-token","account_id":"account-id"}`,
		IsStream: true,
	})

	require.NoError(t, err)
	require.Equal(t, "remote_compaction_v2", headers.Get("X-Codex-Beta-Features"))
	require.Equal(t, "true", headers.Get("X-OpenAI-Internal-Codex-Responses-Lite"))
	require.Equal(t, "session-123", headers.Get("Session-Id"))
	require.Equal(t, "Bearer access-token", headers.Get("Authorization"))
	require.Equal(t, "account-id", headers.Get("Chatgpt-Account-Id"))
}
