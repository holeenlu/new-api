package codex

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

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
		require.Empty(t, r.Header.Get("X-OpenAI-Actor-Authorization"))
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.JSONEq(t, `{"model":"gpt-5.6-sol","query":"hello"}`, string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/alpha/search", nil)
	c.Request.Header.Set("X-OpenAI-Actor-Authorization", "Bearer client-controlled-token")
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

func TestDoAlphaSearchReturnsTypedLocalConcurrencyLimit(t *testing.T) {
	originalMaxConcurrency := CodexOAuthMaxConcurrency
	CodexOAuthMaxConcurrency = 1
	t.Cleanup(func() { CodexOAuthMaxConcurrency = originalMaxConcurrency })

	channelID := 980004
	key := `{"access_token":"access-token","account_id":"limited-alpha-account"}`
	fingerprint := service.SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeCodex, channelID, 0, key)
	lease, err := service.AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 1, 0)
	require.NoError(t, err)
	defer lease.Release()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/alpha/search", nil)
	_, err = DoAlphaSearch(c, &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
		ChannelId:   channelID,
		ChannelType: constant.ChannelTypeCodex,
		ApiKey:      key,
	}}, []byte(`{"model":"gpt-5.6-sol","query":"hello"}`))
	require.Error(t, err)
	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusServiceUnavailable, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeOAuthChannelConcurrencyLimit, apiErr.GetErrorCode())
	require.False(t, types.IsRecordErrorLog(apiErr))
	require.Equal(t, time.Second, apiErr.RetryAfter)
	require.Equal(t, "1", recorder.Header().Get("Retry-After"))
}
