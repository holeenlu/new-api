package claude

import (
	"net/http"
	"net/http/httptest"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestClaudeStreamErrorPreservesProviderStatus(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	info := &relaycommon.RelayInfo{RelayFormat: types.RelayFormatClaude}

	err := HandleStreamResponseData(c, info, &ClaudeResponseInfo{}, `{"type":"error","error":{"type":"rate_limit_error","message":"temporary limit"}}`)

	require.NotNil(t, err)
	require.Equal(t, http.StatusTooManyRequests, err.GetUpstreamStatusCode())
	require.Equal(t, "rate_limit_error", err.ToClaudeError().Type)
}

func TestClaudeStreamErrorUsesStructuredCodeForStatus(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	info := &relaycommon.RelayInfo{RelayFormat: types.RelayFormatClaude}

	err := HandleStreamResponseData(c, info, &ClaudeResponseInfo{}, `{"type":"error","error":{"type":"api_error","code":"usage_limit_reached","message":"request rejected"}}`)

	require.NotNil(t, err)
	require.Equal(t, http.StatusTooManyRequests, err.GetUpstreamStatusCode())
	relayError, ok := err.RelayError.(types.ClaudeError)
	require.True(t, ok)
	require.Equal(t, "usage_limit_reached", relayError.Code)
}
