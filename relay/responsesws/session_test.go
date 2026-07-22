package responsesws

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sessionTestDriver struct {
	upstreamURL   string
	acquire       func(*gin.Context, *relaycommon.RelayInfo) (*service.SubscriptionOAuthLease, error)
	fallbackBody  chan []byte
	dialCalls     atomic.Int32
	fallbackCalls atomic.Int32
}

func (d *sessionTestDriver) DialUpstream(_ *gin.Context, _ *relaycommon.RelayInfo) (string, http.Header, error) {
	d.dialCalls.Add(1)
	return "ws" + strings.TrimPrefix(d.upstreamURL, "http"), nil, nil
}

func (d *sessionTestDriver) AcquireCapacity(c *gin.Context, info *relaycommon.RelayInfo) (*service.SubscriptionOAuthLease, error) {
	if d.acquire == nil {
		return nil, nil
	}
	return d.acquire(c, info)
}

func (d *sessionTestDriver) DoHTTPFallback(
	_ *gin.Context,
	_ *relaycommon.RelayInfo,
	body []byte,
	_ *service.SubscriptionOAuthLease,
) (*http.Response, error) {
	d.fallbackCalls.Add(1)
	if d.fallbackBody != nil {
		d.fallbackBody <- append([]byte(nil), body...)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(
			"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n",
		)),
	}, nil
}

func (d *sessionTestDriver) OnUpstreamConnected(_ *gin.Context, _ *relaycommon.RelayInfo) {}

func newSessionTestContext() *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return c
}

func newSessionTestRelayInfo(channelID, keyIndex int, key string) *relaycommon.RelayInfo {
	return &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
		ChannelId:            channelID,
		ChannelType:          constant.ChannelTypeOpenAI,
		ChannelMultiKeyIndex: keyIndex,
		ApiKey:               key,
		UpstreamModelName:    "gpt-test",
	}}
}

func TestResetChannelForRetryClearsSessionBinding(t *testing.T) {
	session := &Session{
		channelID:             41,
		keyIndex:              2,
		credentialFingerprint: "credential-a",
		model:                 "gpt-test",
		httpFallback:          true,
		pendingID:             42,
		pendingKeyIndex:       3,
		pendingFingerprint:    "credential-b",
		pendingModel:          "gpt-backup",
	}

	session.ResetChannelForRetry()

	require.Zero(t, session.ChannelID())
	_, bound := session.Binding()
	assert.False(t, bound)
	assert.Zero(t, session.keyIndex)
	assert.Empty(t, session.credentialFingerprint)
	assert.Empty(t, session.model)
	assert.False(t, session.httpFallback)
	assert.Zero(t, session.pendingID)
	assert.Zero(t, session.pendingKeyIndex)
	assert.Empty(t, session.pendingFingerprint)
	assert.Empty(t, session.pendingModel)
}

func TestConfirmHTTPFallbackSuccessPinsCredentialBinding(t *testing.T) {
	session := &Session{
		httpFallback:       true,
		pendingID:          42,
		pendingKeyIndex:    3,
		pendingFingerprint: "credential-b",
		pendingModel:       "gpt-backup",
	}

	session.ConfirmHTTPFallbackSuccess()

	binding, bound := session.Binding()
	require.True(t, bound)
	assert.Equal(t, CredentialBinding{
		ChannelID:   42,
		KeyIndex:    3,
		Fingerprint: "credential-b",
	}, binding)
	assert.Equal(t, "gpt-backup", session.model)
	assert.Zero(t, session.pendingID)
	assert.Zero(t, session.pendingKeyIndex)
	assert.Empty(t, session.pendingFingerprint)
	assert.Empty(t, session.pendingModel)
}

func TestResponseBodyCloseHandlesNilFields(t *testing.T) {
	require.NoError(t, (*responseBody)(nil).Close())
	require.NoError(t, (&responseBody{}).Close())
}

func TestDoRequestOmitsHTTPOnlyFieldsFromWebSocketEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	requestPayload := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		requestPayload <- payload
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed"}`))
	}))
	defer server.Close()

	driver := &sessionTestDriver{upstreamURL: server.URL}
	session := &Session{}
	defer session.Close()
	response, err := session.DoRequest(
		newSessionTestContext(),
		driver,
		newSessionTestRelayInfo(101, 4, "credential-a"),
		strings.NewReader(`{"model":"gpt-test","input":"hello","stream":true,"stream_options":{"include_usage":true},"background":false}`),
	)
	require.NoError(t, err)
	_, err = io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())

	var event map[string]any
	require.NoError(t, common.Unmarshal(<-requestPayload, &event))
	assert.Equal(t, "response.create", event["type"])
	assert.Equal(t, "gpt-test", event["model"])
	assert.Equal(t, "hello", event["input"])
	assert.NotContains(t, event, "stream")
	assert.NotContains(t, event, "stream_options")
	assert.NotContains(t, event, "background")

	binding, bound := session.Binding()
	require.True(t, bound)
	assert.Equal(t, 101, binding.ChannelID)
	assert.Equal(t, 4, binding.KeyIndex)
	assert.Equal(t, service.SubscriptionOAuthCredentialFingerprint(
		constant.ChannelTypeOpenAI,
		101,
		4,
		"credential-a",
	), binding.Fingerprint)
}

func TestDoRequestPreservesTransportFieldsForHTTPFallback(t *testing.T) {
	fallbackBody := make(chan []byte, 1)
	driver := &sessionTestDriver{fallbackBody: fallbackBody}
	session := &Session{httpFallback: true}
	response, err := session.DoRequest(
		newSessionTestContext(),
		driver,
		newSessionTestRelayInfo(106, 0, "credential-a"),
		strings.NewReader(`{"model":"gpt-test","stream":true,"stream_options":{"include_usage":true},"background":false}`),
	)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())

	var request map[string]any
	require.NoError(t, common.Unmarshal(<-fallbackBody, &request))
	assert.Equal(t, true, request["stream"])
	assert.Equal(t, false, request["background"])
	assert.Contains(t, request, "stream_options")
	assert.NotContains(t, request, "type")
	assert.Zero(t, driver.dialCalls.Load())
	assert.Equal(t, int32(1), driver.fallbackCalls.Load())
}

func TestDoRequestRejectsCredentialSwitchOnPersistentConnection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	requestCount := atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			requestCount.Add(1)
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed"}`)); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	driver := &sessionTestDriver{upstreamURL: server.URL}
	session := &Session{}
	defer session.Close()
	response, err := session.DoRequest(
		newSessionTestContext(),
		driver,
		newSessionTestRelayInfo(102, 0, "credential-a"),
		strings.NewReader(`{"model":"gpt-test","input":"first"}`),
	)
	require.NoError(t, err)
	_, err = io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())

	response, err = session.DoRequest(
		newSessionTestContext(),
		driver,
		newSessionTestRelayInfo(102, 1, "credential-b"),
		strings.NewReader(`{"model":"gpt-test","input":"second"}`),
	)
	require.Nil(t, response)
	require.Error(t, err)
	var apiError *types.NewAPIError
	require.ErrorAs(t, err, &apiError)
	assert.Equal(t, http.StatusConflict, apiError.StatusCode)
	assert.Equal(t, types.ErrorCodeInvalidRequest, apiError.GetErrorCode())
	assert.True(t, types.IsSkipRetryError(apiError))
	assert.Equal(t, int32(1), driver.dialCalls.Load())
	assert.Equal(t, int32(1), requestCount.Load())
}

func TestDoRequestDoesNotReplayAfterWebSocketWriteFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	requestCount := atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			requestCount.Add(1)
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed"}`)); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	driver := &sessionTestDriver{upstreamURL: server.URL}
	session := &Session{}
	defer session.Close()
	response, err := session.DoRequest(
		newSessionTestContext(),
		driver,
		newSessionTestRelayInfo(103, 0, "credential-a"),
		strings.NewReader(`{"model":"gpt-test","input":"first"}`),
	)
	require.NoError(t, err)
	_, err = io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())

	session.mu.Lock()
	require.NotNil(t, session.conn)
	require.NoError(t, session.conn.Close())
	session.mu.Unlock()

	secondInfo := newSessionTestRelayInfo(103, 0, "credential-a")
	response, err = session.DoRequest(
		newSessionTestContext(),
		driver,
		secondInfo,
		strings.NewReader(`{"model":"gpt-test","input":"second"}`),
	)
	require.Nil(t, response)
	require.Error(t, err)
	var apiError *types.NewAPIError
	require.ErrorAs(t, err, &apiError)
	assert.Equal(t, http.StatusBadGateway, apiError.StatusCode)
	assert.Equal(t, types.ErrorCodeDoRequestFailed, apiError.GetErrorCode())
	assert.True(t, types.IsSkipRetryError(apiError))
	written, _ := secondInfo.UpstreamAttemptState()
	assert.True(t, written)
	assert.Equal(t, int32(1), driver.dialCalls.Load())
	assert.Zero(t, driver.fallbackCalls.Load())
	assert.Equal(t, int32(1), requestCount.Load())
}

func TestDoRequestRejectsConnectionLocalContinuationAfterLifetime(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	requestCount := atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			requestCount.Add(1)
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_1"}}`)); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	driver := &sessionTestDriver{upstreamURL: server.URL}
	session := &Session{}
	defer session.Close()
	response, err := session.DoRequest(
		newSessionTestContext(),
		driver,
		newSessionTestRelayInfo(104, 0, "credential-a"),
		strings.NewReader(`{"model":"gpt-test","input":"first"}`),
	)
	require.NoError(t, err)
	_, err = io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())

	session.mu.Lock()
	session.connectedAt = time.Now().Add(-MaxConnectionLifetime - time.Second)
	session.mu.Unlock()
	response, err = session.DoRequest(
		newSessionTestContext(),
		driver,
		newSessionTestRelayInfo(104, 0, "credential-a"),
		strings.NewReader(`{"model":"gpt-test","previous_response_id":"resp_1","input":"second"}`),
	)
	require.Nil(t, response)
	require.Error(t, err)
	var apiError *types.NewAPIError
	require.ErrorAs(t, err, &apiError)
	assert.Equal(t, http.StatusBadRequest, apiError.StatusCode)
	assert.Equal(t, types.ErrorCodeWebSocketConnectionLimitReached, apiError.GetErrorCode())
	assert.True(t, types.IsSkipRetryError(apiError))
	assert.Equal(t, int32(1), driver.dialCalls.Load())
	assert.Zero(t, driver.fallbackCalls.Load())
	assert.Equal(t, int32(1), requestCount.Load())
}

func TestClosingResponseBodyStopsUpstreamReaderBeforeNextTurn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	firstConnectionClosed := make(chan struct{})
	connectionCount := atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		connectionNumber := connectionCount.Add(1)
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if connectionNumber == 1 {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created"}`)); err != nil {
				return
			}
			_, _, _ = conn.ReadMessage()
			close(firstConnectionClosed)
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed"}`))
	}))
	defer server.Close()

	const leaseFingerprint = "responsesws-test-early-close"
	driver := &sessionTestDriver{
		upstreamURL: server.URL,
		acquire: func(c *gin.Context, _ *relaycommon.RelayInfo) (*service.SubscriptionOAuthLease, error) {
			return service.AcquireSubscriptionOAuthCapacity(c.Request.Context(), leaseFingerprint, 1, 0)
		},
	}
	session := &Session{}
	defer session.Close()
	response, err := session.DoRequest(
		newSessionTestContext(),
		driver,
		newSessionTestRelayInfo(105, 0, "credential-a"),
		strings.NewReader(`{"model":"gpt-test","input":"first"}`),
	)
	require.NoError(t, err)

	reader := bufio.NewReader(response.Body)
	var firstEvent strings.Builder
	for {
		line, readErr := reader.ReadString('\n')
		require.NoError(t, readErr)
		firstEvent.WriteString(line)
		if line == "\n" {
			break
		}
	}
	require.Contains(t, firstEvent.String(), "event: response.created")
	require.NoError(t, response.Body.Close())
	select {
	case <-firstConnectionClosed:
	case <-time.After(time.Second):
		require.FailNow(t, "closing the response body did not close the upstream reader")
	}

	response, err = session.DoRequest(
		newSessionTestContext(),
		driver,
		newSessionTestRelayInfo(105, 0, "credential-a"),
		strings.NewReader(`{"model":"gpt-test","input":"second"}`),
	)
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Contains(t, string(body), "event: response.completed")
	assert.Equal(t, int32(2), connectionCount.Load())
}
