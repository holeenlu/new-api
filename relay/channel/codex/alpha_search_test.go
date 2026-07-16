package codex

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSanitizeAlphaSearchBodyRemovesLocalCacheRoutingFields(t *testing.T) {
	body, err := SanitizeAlphaSearchBody([]byte(`{"query":"hello","model":"gpt-5.6-sol","id":"session-1","prompt_cache_key":"local","prompt_cache_retention":"24h"}`))
	require.NoError(t, err)
	require.JSONEq(t, `{"query":"hello","model":"gpt-5.6-sol","id":"session-1"}`, string(body))
}

func TestSanitizeAlphaSearchBodyRejectsInvalidJSON(t *testing.T) {
	_, err := SanitizeAlphaSearchBody([]byte(`{"query":`))
	require.Error(t, err)
}

func TestDoAlphaSearchUsesCodexOAuthAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalInterval := CodexOAuthMinRequestInterval
	CodexOAuthMinRequestInterval = 0
	t.Cleanup(func() { CodexOAuthMinRequestInterval = originalInterval })
	service.InitHttpClient()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/backend-api/codex/alpha/search", r.URL.Path)
		require.Equal(t, "Bearer access-token", r.Header.Get("Authorization"))
		require.Equal(t, "account-id", r.Header.Get("ChatGPT-Account-ID"))
		require.Equal(t, CodexOAuthOriginator, r.Header.Get("Originator"))
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.JSONEq(t, `{"model":"gpt-5.6-sol","query":"hello"}`, string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/alpha/search", nil)
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
		ChannelId:      980003,
		ChannelType:    constant.ChannelTypeCodex,
		ChannelBaseUrl: server.URL,
		ApiKey:         `{"access_token":"access-token","account_id":"account-id"}`,
		ChannelSetting: dto.ChannelSettings{},
	}}

	resp, err := DoAlphaSearch(c, info, []byte(`{"model":"gpt-5.6-sol","query":"hello"}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	payload, err := ReadAlphaSearchResponse(resp)
	require.NoError(t, err)
	require.JSONEq(t, `{"results":[]}`, string(payload))
}
