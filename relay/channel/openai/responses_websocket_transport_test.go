package openai

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
	"github.com/QuantumNous/new-api/relay/responsesws"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// A standard OpenAI-compatible channel with ResponsesWebSocketEnabled routes a
// Responses turn over the upstream WebSocket (Bearer auth, standard /v1/responses
// path) and streams the events back as SSE.
func TestResponsesWebSocketOpenAIAdaptorUsesUpstreamWebSocket(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service.InitHttpClient()

	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
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
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)); err != nil {
			t.Errorf("write websocket response: %v", err)
		}
	}))
	defer server.Close()

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("Content-Type", "application/json")
	session := &responsesws.Session{}
	defer session.Close()
	responsesws.SetSession(c, session)

	info := &relaycommon.RelayInfo{
		IsStream:       true,
		RelayMode:      relayconstant.RelayModeResponses,
		RequestURLPath: "/v1/responses",
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId:            990001,
			ChannelType:          constant.ChannelTypeOpenAI,
			ChannelBaseUrl:       server.URL,
			ApiKey:               "test-key",
			UpstreamModelName:    "gpt-5",
			ChannelSetting:       dto.ChannelSettings{},
			ChannelOtherSettings: dto.ChannelOtherSettings{ResponsesWebSocketEnabled: true},
		},
	}

	respAny, err := (&Adaptor{}).DoRequest(c, info, strings.NewReader(`{"model":"gpt-5","stream":true}`))
	require.NoError(t, err)
	resp, ok := respAny.(*http.Response)
	require.True(t, ok)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "event: response.completed")
	require.Contains(t, string(body), `"total_tokens":3`)
}

// A response.incomplete event (e.g. max output tokens or a content filter) is a
// clean terminal state. The session must stop the read loop and close the SSE
// stream even though the persistent upstream keeps the connection open afterward;
// otherwise the turn would hang on the idle read until the idle timeout. The
// upstream here deliberately holds the connection open after the terminal event.
func TestResponsesWebSocketOpenAIAdaptorStopsOnIncompleteEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service.InitHttpClient()

	release := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read websocket request: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.incomplete","response":{"status":"incomplete","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)); err != nil {
			t.Errorf("write websocket response: %v", err)
			return
		}
		// Emulate the persistent upstream: keep the connection open awaiting the
		// next response.create instead of closing it. Without the fix the session
		// would block here until idleTimeout.
		<-release
	}))
	defer server.Close()
	defer close(release)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("Content-Type", "application/json")
	session := &responsesws.Session{}
	defer session.Close()
	responsesws.SetSession(c, session)

	info := &relaycommon.RelayInfo{
		IsStream:       true,
		RelayMode:      relayconstant.RelayModeResponses,
		RequestURLPath: "/v1/responses",
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId:            990003,
			ChannelType:          constant.ChannelTypeOpenAI,
			ChannelBaseUrl:       server.URL,
			ApiKey:               "test-key",
			UpstreamModelName:    "gpt-5",
			ChannelSetting:       dto.ChannelSettings{},
			ChannelOtherSettings: dto.ChannelOtherSettings{ResponsesWebSocketEnabled: true},
		},
	}

	respAny, err := (&Adaptor{}).DoRequest(c, info, strings.NewReader(`{"model":"gpt-5","stream":true}`))
	require.NoError(t, err)
	resp, ok := respAny.(*http.Response)
	require.True(t, ok)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "event: response.incomplete")
	require.Contains(t, string(body), `"total_tokens":3`)
}

// Without the flag the adaptor must not open an upstream WebSocket even when a
// session is bound; it takes the ordinary HTTP path instead.
func TestResponsesWebSocketOpenAIAdaptorSkipsWebSocketWithoutFlag(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service.InitHttpClient()

	var upgradeAttempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upgradeAttempts++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response"}`))
	}))
	defer server.Close()

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("Content-Type", "application/json")
	session := &responsesws.Session{}
	defer session.Close()
	responsesws.SetSession(c, session)

	info := &relaycommon.RelayInfo{
		IsStream:       false,
		RelayMode:      relayconstant.RelayModeResponses,
		RequestURLPath: "/v1/responses",
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId:            990002,
			ChannelType:          constant.ChannelTypeOpenAI,
			ChannelBaseUrl:       server.URL,
			ApiKey:               "test-key",
			UpstreamModelName:    "gpt-5",
			ChannelSetting:       dto.ChannelSettings{},
			ChannelOtherSettings: dto.ChannelOtherSettings{ResponsesWebSocketEnabled: false},
		},
	}

	respAny, err := (&Adaptor{}).DoRequest(c, info, strings.NewReader(`{"model":"gpt-5"}`))
	require.NoError(t, err)
	resp, ok := respAny.(*http.Response)
	require.True(t, ok)
	require.NoError(t, resp.Body.Close())
	// Zero WebSocket upgrades: the plain HTTP handler served the request.
	require.Zero(t, session.ChannelID())
}
