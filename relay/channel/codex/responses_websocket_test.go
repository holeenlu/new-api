package codex

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestResponsesWebSocketSessionUsesCodexUpstreamProtocol(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalInterval := CodexOAuthMinRequestInterval
	CodexOAuthMinRequestInterval = 0
	t.Cleanup(func() { CodexOAuthMinRequestInterval = originalInterval })
	service.InitHttpClient()

	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("OpenAI-Beta") != codexResponsesWebSocketBeta {
			t.Errorf("OpenAI-Beta = %q", r.Header.Get("OpenAI-Beta"))
		}
		if r.Header.Get("Authorization") != "Bearer access-token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer conn.Close()
		_, request, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read websocket request: %v", err)
			return
		}
		if !strings.Contains(string(request), `"type":"response.create"`) {
			t.Errorf("request does not contain response.create: %s", request)
		}
		err = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`))
		if err != nil {
			t.Errorf("write websocket response: %v", err)
		}
	}))
	defer server.Close()

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("Content-Type", "application/json")
	setCodexResponsesLiteEnabled(c, true)
	info := &relaycommon.RelayInfo{
		IsStream:  true,
		RelayMode: relayconstant.RelayModeResponses,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId:         980001,
			ChannelType:       constant.ChannelTypeCodex,
			ChannelBaseUrl:    server.URL,
			ApiKey:            `{"access_token":"access-token","account_id":"account-id"}`,
			UpstreamModelName: "gpt-5.6-sol",
			ChannelSetting:    dto.ChannelSettings{},
		},
	}
	session := &ResponsesWebSocketSession{}
	defer session.Close()
	resp, err := session.doRequest(c, &Adaptor{}, info, strings.NewReader(`{"model":"gpt-5.6-sol","stream":true}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "event: response.completed")
	require.Contains(t, string(body), `"total_tokens":3`)
}

func TestResponsesWebSocketSessionFallsBackToHTTPOnUpgradeRequired(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalInterval := CodexOAuthMinRequestInterval
	CodexOAuthMinRequestInterval = 0
	t.Cleanup(func() { CodexOAuthMinRequestInterval = originalInterval })
	service.InitHttpClient()

	var websocketAttempts int
	var httpAttempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			websocketAttempts++
			w.WriteHeader(http.StatusUpgradeRequired)
		case http.MethodPost:
			httpAttempts++
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			require.NotContains(t, string(body), `"type":"response.create"`)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("Content-Type", "application/json")
	setCodexResponsesLiteEnabled(c, true)
	info := &relaycommon.RelayInfo{
		IsStream:  true,
		RelayMode: relayconstant.RelayModeResponses,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId:         980002,
			ChannelType:       constant.ChannelTypeCodex,
			ChannelBaseUrl:    server.URL,
			ApiKey:            `{"access_token":"access-token","account_id":"account-id"}`,
			UpstreamModelName: "gpt-5.6-sol",
			ChannelSetting:    dto.ChannelSettings{},
		},
	}
	session := &ResponsesWebSocketSession{}
	defer session.Close()
	resp, err := session.doRequest(c, &Adaptor{}, info, strings.NewReader(`{"model":"gpt-5.6-sol","stream":true}`))
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Contains(t, string(body), "response.completed")

	resp, err = session.doRequest(c, &Adaptor{}, info, strings.NewReader(`{"model":"gpt-5.6-sol","stream":true}`))
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, 1, websocketAttempts)
	require.Equal(t, 2, httpAttempts)
}

func TestResponsesWebSocketHTTPFallbackPinsOnlySuccessfulChannel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalInterval := CodexOAuthMinRequestInterval
	CodexOAuthMinRequestInterval = 0
	t.Cleanup(func() { CodexOAuthMinRequestInterval = originalInterval })
	service.InitHttpClient()

	postAttempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusUpgradeRequired)
			return
		}
		postAttempts++
		if postAttempts == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"))
	}))
	defer server.Close()

	session := &ResponsesWebSocketSession{}
	defer session.Close()
	newAttempt := func(channelID int, accountID string) (*gin.Context, *relaycommon.RelayInfo) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		return c, &relaycommon.RelayInfo{
			IsStream:  true,
			RelayMode: relayconstant.RelayModeResponses,
			ChannelMeta: &relaycommon.ChannelMeta{
				ChannelId:         channelID,
				ChannelType:       constant.ChannelTypeCodex,
				ChannelBaseUrl:    server.URL,
				ApiKey:            `{"access_token":"access-token","account_id":"` + accountID + `"}`,
				UpstreamModelName: "gpt-5.6-sol",
			},
		}
	}

	firstContext, firstInfo := newAttempt(980010, "fallback-failed-account")
	resp, err := session.doRequest(firstContext, &Adaptor{}, firstInfo, strings.NewReader(`{"model":"gpt-5.6-sol","stream":true}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
	require.Zero(t, session.ChannelID())
	_, pinned := firstContext.Get("specific_channel_id")
	require.False(t, pinned)

	secondContext, secondInfo := newAttempt(980011, "fallback-success-account")
	resp, err = session.doRequest(secondContext, &Adaptor{}, secondInfo, strings.NewReader(`{"model":"gpt-5.6-sol","stream":true}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
	require.Zero(t, session.ChannelID())
	session.ConfirmHTTPFallbackSuccess()
	require.Equal(t, 980011, session.ChannelID())
}

func TestResponsesWebSocketHandshakePreservesUpstream429(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalInterval := CodexOAuthMinRequestInterval
	CodexOAuthMinRequestInterval = 0
	t.Cleanup(func() { CodexOAuthMinRequestInterval = originalInterval })
	service.InitHttpClient()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "9")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer server.Close()

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	session := &ResponsesWebSocketSession{}
	_, err := session.doRequest(c, &Adaptor{}, &relaycommon.RelayInfo{
		IsStream:  true,
		RelayMode: relayconstant.RelayModeResponses,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId:         980012,
			ChannelType:       constant.ChannelTypeCodex,
			ChannelBaseUrl:    server.URL,
			ApiKey:            `{"access_token":"access-token","account_id":"handshake-429-account"}`,
			UpstreamModelName: "gpt-5.6-sol",
		},
	}, strings.NewReader(`{"model":"gpt-5.6-sol","stream":true}`))
	require.Error(t, err)
	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusTooManyRequests, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeBadResponseStatusCode, apiErr.GetErrorCode())
	require.Equal(t, 9*time.Second, apiErr.RetryAfter)
	require.Zero(t, session.ChannelID())
}

func TestResponsesWebSocketSessionUsesConfiguredRequestBodyLimit(t *testing.T) {
	originalLimit := constant.MaxRequestBodyMB
	constant.MaxRequestBodyMB = 1
	t.Cleanup(func() { constant.MaxRequestBodyMB = originalLimit })

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	_, err := (&ResponsesWebSocketSession{}).doRequest(
		c,
		&Adaptor{},
		&relaycommon.RelayInfo{},
		strings.NewReader(strings.Repeat("x", (1<<20)+1)),
	)
	require.Error(t, err)
	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusRequestEntityTooLarge, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeReadRequestBodyFailed, apiErr.GetErrorCode())
}

func TestResponsesWebSocketSessionReturnsTypedLocalConcurrencyLimit(t *testing.T) {
	originalMaxConcurrency := CodexOAuthMaxConcurrency
	CodexOAuthMaxConcurrency = 1
	t.Cleanup(func() { CodexOAuthMaxConcurrency = originalMaxConcurrency })

	channelID := 980005
	key := `{"access_token":"access-token","account_id":"limited-websocket-account"}`
	fingerprint := service.SubscriptionOAuthCredentialFingerprint(constant.ChannelTypeCodex, channelID, 0, key)
	lease, err := service.AcquireSubscriptionOAuthCapacity(context.Background(), fingerprint, 1, 0)
	require.NoError(t, err)
	defer lease.Release()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	_, err = (&ResponsesWebSocketSession{}).doRequest(
		c,
		&Adaptor{},
		&relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId:         channelID,
			ChannelType:       constant.ChannelTypeCodex,
			ApiKey:            key,
			UpstreamModelName: "gpt-5.6-sol",
		}},
		strings.NewReader(`{"model":"gpt-5.6-sol","stream":true}`),
	)
	require.Error(t, err)
	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusServiceUnavailable, apiErr.StatusCode)
	require.Equal(t, types.ErrorCodeOAuthChannelConcurrencyLimit, apiErr.GetErrorCode())
	require.False(t, types.IsRecordErrorLog(apiErr))
	require.Equal(t, time.Second, apiErr.RetryAfter)
	require.Equal(t, "1", recorder.Header().Get("Retry-After"))
}
