package codex

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"

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
