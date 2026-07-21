package codex

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/responsesws"
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
	session := &responsesws.Session{}
	defer session.Close()
	resp, err := session.DoRequest(c, &Adaptor{}, info, strings.NewReader(`{"model":"gpt-5.6-sol","stream":true}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "event: response.completed")
	require.Contains(t, string(body), `"total_tokens":3`)
}

func TestResponsesWebSocketSessionStopsAfterTerminalFailureEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalInterval := CodexOAuthMinRequestInterval
	CodexOAuthMinRequestInterval = 0
	t.Cleanup(func() { CodexOAuthMinRequestInterval = originalInterval })
	service.InitHttpClient()

	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()
		_, _, err = conn.ReadMessage()
		require.NoError(t, err)
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.failed","response":{"status":"failed","error":{"type":"server_error","code":"upstream_failure","message":"upstream failed"}}}`)))
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
			ChannelId:         980003,
			ChannelType:       constant.ChannelTypeCodex,
			ChannelBaseUrl:    server.URL,
			ApiKey:            `{"access_token":"access-token","account_id":"account-id"}`,
			UpstreamModelName: "gpt-5.6-luna",
			ChannelSetting:    dto.ChannelSettings{},
		},
	}
	session := &responsesws.Session{}
	defer session.Close()
	resp, err := session.DoRequest(c, &Adaptor{}, info, strings.NewReader(`{"model":"gpt-5.6-luna","stream":true}`))
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "event: response.failed")
	require.Contains(t, string(body), "upstream failed")
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
	session := &responsesws.Session{}
	defer session.Close()
	resp, err := session.DoRequest(c, &Adaptor{}, info, strings.NewReader(`{"model":"gpt-5.6-sol","stream":true}`))
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Contains(t, string(body), "response.completed")

	resp, err = session.DoRequest(c, &Adaptor{}, info, strings.NewReader(`{"model":"gpt-5.6-sol","stream":true}`))
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

	session := &responsesws.Session{}
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
	resp, err := session.DoRequest(firstContext, &Adaptor{}, firstInfo, strings.NewReader(`{"model":"gpt-5.6-sol","stream":true}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
	require.Zero(t, session.ChannelID())
	_, pinned := firstContext.Get("specific_channel_id")
	require.False(t, pinned)

	secondContext, secondInfo := newAttempt(980011, "fallback-success-account")
	resp, err = session.DoRequest(secondContext, &Adaptor{}, secondInfo, strings.NewReader(`{"model":"gpt-5.6-sol","stream":true}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
	require.Zero(t, session.ChannelID())
	session.ConfirmHTTPFallbackSuccess()
	require.Equal(t, 980011, session.ChannelID())
}

func TestResponsesWebSocketSessionFallsBackToHTTPOnMalformedHandshake(t *testing.T) {
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
			// Reproduce an upstream served over HTTP/2 (or otherwise not a WebSocket
			// endpoint): answer the upgrade with non-HTTP/1.1 bytes so the dialer
			// reports "malformed HTTP response" and returns a nil *http.Response.
			websocketAttempts++
			hijacker, ok := w.(http.Hijacker)
			require.True(t, ok)
			conn, _, err := hijacker.Hijack()
			require.NoError(t, err)
			_, _ = conn.Write([]byte("\x00\x00\x12\x04\x00\x00\x00\x00\x00\x00\x03\x00\x00\x00d"))
			_ = conn.Close()
		case http.MethodPost:
			httpAttempts++
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			require.NotContains(t, string(body), `"type":"response.create"`)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"total_tokens\":2}}}\n\n"))
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
			ChannelId:         980004,
			ChannelType:       constant.ChannelTypeCodex,
			ChannelBaseUrl:    server.URL,
			ApiKey:            `{"access_token":"access-token","account_id":"malformed-handshake-account"}`,
			UpstreamModelName: "gpt-5.6-sol",
			ChannelSetting:    dto.ChannelSettings{},
		},
	}
	session := &responsesws.Session{}
	defer session.Close()
	resp, err := session.DoRequest(c, &Adaptor{}, info, strings.NewReader(`{"model":"gpt-5.6-sol","stream":true}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Contains(t, string(body), "response.completed")

	// The failed WebSocket handshake must degrade to HTTP for the rest of the
	// session instead of surfacing a hard 502; the follow-up turn reuses HTTP.
	resp, err = session.DoRequest(c, &Adaptor{}, info, strings.NewReader(`{"model":"gpt-5.6-sol","stream":true}`))
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, 1, websocketAttempts)
	require.Equal(t, 2, httpAttempts)
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
	session := &responsesws.Session{}
	_, err := session.DoRequest(c, &Adaptor{}, &relaycommon.RelayInfo{
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
	require.Equal(t, types.ErrorCode("unknown_error"), apiErr.GetErrorCode())
	require.Equal(t, 9*time.Second, apiErr.RetryAfter)
	require.Zero(t, session.ChannelID())
}

func TestResponsesWebSocketHandshakePreservesUsageResetMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalInterval := CodexOAuthMinRequestInterval
	CodexOAuthMinRequestInterval = 0
	t.Cleanup(func() { CodexOAuthMinRequestInterval = originalInterval })
	service.InitHttpClient()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"usage_limit_reached","message":"usage limit reached","resets_in_seconds":18000}}`))
	}))
	defer server.Close()

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	_, err := (&responsesws.Session{}).DoRequest(c, &Adaptor{}, &relaycommon.RelayInfo{
		IsStream:  true,
		RelayMode: relayconstant.RelayModeResponses,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId:         980013,
			ChannelType:       constant.ChannelTypeCodex,
			ChannelBaseUrl:    server.URL,
			ApiKey:            `{"access_token":"access-token","account_id":"handshake-usage-limit"}`,
			UpstreamModelName: "gpt-5.6-sol",
		},
	}, strings.NewReader(`{"model":"gpt-5.6-sol","stream":true}`))

	require.Error(t, err)
	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, 5*time.Hour, apiErr.RetryAfter)
	classified := service.ApplyChannelErrorPolicy(constant.ChannelTypeCodex, apiErr)
	require.Equal(t, types.ErrorCodeUpstreamUsageLimit, classified.GetErrorCode())
}

func TestResponsesWebSocketFallsBackToHTTPWhenUpstreamSilent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalInterval := CodexOAuthMinRequestInterval
	CodexOAuthMinRequestInterval = 0
	t.Cleanup(func() { CodexOAuthMinRequestInterval = originalInterval })
	originalFirstEvent := responsesws.FirstEventTimeout
	responsesws.FirstEventTimeout = 100 * time.Millisecond
	t.Cleanup(func() { responsesws.FirstEventTimeout = originalFirstEvent })
	service.InitHttpClient()

	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	var httpRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			// Accept the upgrade but never send a Responses event, simulating an
			// upstream that does not actually speak the WebSocket protocol.
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			_, _, _ = conn.ReadMessage()
			time.Sleep(500 * time.Millisecond)
			return
		}
		// HTTP fallback transport.
		httpRequests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"total_tokens\":5}}}\n\n"))
	}))
	defer server.Close()

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("Content-Type", "application/json")
	info := &relaycommon.RelayInfo{
		IsStream:  true,
		RelayMode: relayconstant.RelayModeResponses,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId:         980020,
			ChannelType:       constant.ChannelTypeCodex,
			ChannelBaseUrl:    server.URL,
			ApiKey:            `{"access_token":"access-token","account_id":"silent-account"}`,
			UpstreamModelName: "gpt-5.6-sol",
			ChannelSetting:    dto.ChannelSettings{},
		},
	}
	session := &responsesws.Session{}
	defer session.Close()
	resp, err := session.DoRequest(c, &Adaptor{}, info, strings.NewReader(`{"model":"gpt-5.6-sol","stream":true}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Contains(t, string(body), "event: response.completed")
	// The silent WebSocket produced no event, so the response must have come from
	// the HTTP fallback transport.
	require.Equal(t, int32(1), httpRequests.Load())
}

func TestResponsesWebSocketSessionUsesConfiguredRequestBodyLimit(t *testing.T) {
	originalLimit := constant.MaxRequestBodyMB
	constant.MaxRequestBodyMB = 1
	t.Cleanup(func() { constant.MaxRequestBodyMB = originalLimit })

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	_, err := (&responsesws.Session{}).DoRequest(
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
	_, err = (&responsesws.Session{}).DoRequest(
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

func TestResponsesWebSocketSessionRecyclesConnectionAtLifetime(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalInterval := CodexOAuthMinRequestInterval
	CodexOAuthMinRequestInterval = 0
	t.Cleanup(func() { CodexOAuthMinRequestInterval = originalInterval })
	originalLifetime := responsesws.MaxConnectionLifetime
	responsesws.MaxConnectionLifetime = 0
	t.Cleanup(func() { responsesws.MaxConnectionLifetime = originalLifetime })
	service.InitHttpClient()

	var upgrades atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		upgrades.Add(1)
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"usage":{"total_tokens":3}}}`)); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	newTurn := func() (*gin.Context, *relaycommon.RelayInfo) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		c.Request.Header.Set("Content-Type", "application/json")
		info := &relaycommon.RelayInfo{
			IsStream:  true,
			RelayMode: relayconstant.RelayModeResponses,
			ChannelMeta: &relaycommon.ChannelMeta{
				ChannelId:         970001,
				ChannelType:       constant.ChannelTypeCodex,
				ChannelBaseUrl:    server.URL,
				ApiKey:            `{"access_token":"access-token","account_id":"lifetime-account"}`,
				UpstreamModelName: "gpt-5.6-sol",
				ChannelSetting:    dto.ChannelSettings{},
			},
		}
		return c, info
	}

	session := &responsesws.Session{}
	defer session.Close()

	for turn := 0; turn < 2; turn++ {
		c, info := newTurn()
		resp, err := session.DoRequest(c, &Adaptor{}, info, strings.NewReader(`{"model":"gpt-5.6-sol","stream":true}`))
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Contains(t, string(body), "event: response.completed")
		require.NoError(t, resp.Body.Close())
	}

	// The second turn must reconnect because the connection exceeded its lifetime.
	require.Equal(t, int32(2), upgrades.Load())
}
