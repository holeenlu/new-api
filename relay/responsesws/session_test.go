package responsesws

import (
	"bufio"
	"errors"
	"fmt"
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
	dialErr       error
	dialCalls     atomic.Int32
	fallbackCalls atomic.Int32
}

func (d *sessionTestDriver) DialUpstream(_ *gin.Context, _ *relaycommon.RelayInfo) (string, http.Header, error) {
	d.dialCalls.Add(1)
	if d.dialErr != nil {
		return "", nil, d.dialErr
	}
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

func TestResetChannelForRetryAllowsReplaySafeWebSocketRedial(t *testing.T) {
	redialErr := errors.New("websocket redial reached")
	driver := &sessionTestDriver{dialErr: redialErr}
	session := &Session{
		channelID:             41,
		keyIndex:              2,
		credentialFingerprint: "credential-a",
		model:                 "gpt-test",
		httpFallback:          true,
	}

	// The relay calls ResetChannelForRetry only after proving that the current
	// self-contained turn was not written upstream. Releasing the old binding at
	// that boundary must also release persistent HTTP fallback so the retry may
	// establish a fresh WebSocket on the newly selected credential.
	session.ResetChannelForRetry()
	response, err := session.DoRequest(
		newSessionTestContext(),
		driver,
		newSessionTestRelayInfo(42, 0, "credential-b"),
		strings.NewReader(`{"model":"gpt-test","input":"retry"}`),
	)

	require.Nil(t, response)
	require.ErrorIs(t, err, redialErr)
	assert.Equal(t, int32(1), driver.dialCalls.Load())
	assert.Zero(t, driver.fallbackCalls.Load())
}

func TestResponseIDTrackingEvictsOldestEntryAtBound(t *testing.T) {
	conn := &websocket.Conn{}
	session := &Session{conn: conn}

	for index := 0; index < maxTrackedResponseIDsPerConnection+2; index++ {
		require.NoError(t, session.rememberResponseID(conn, fmt.Sprintf("resp_%d", index)))
	}

	assert.False(t, session.CanContinue("resp_0"))
	assert.False(t, session.CanContinue("resp_1"))
	assert.True(t, session.CanContinue("resp_2"))
	assert.True(t, session.CanContinue(fmt.Sprintf("resp_%d", maxTrackedResponseIDsPerConnection+1)))
	session.mu.Lock()
	defer session.mu.Unlock()
	assert.Len(t, session.responseIDs, maxTrackedResponseIDsPerConnection)
	assert.Len(t, session.responseIDOrder, maxTrackedResponseIDsPerConnection)
}

func TestResponseIDTrackingRejectsOversizedIDAndInvalidatesConnection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		payload := fmt.Sprintf(
			`{"type":"response.completed","response":{"id":%q}}`,
			strings.Repeat("x", maxTrackedResponseIDBytes+1),
		)
		_ = conn.WriteMessage(websocket.TextMessage, []byte(payload))
	}))
	defer server.Close()

	driver := &sessionTestDriver{upstreamURL: server.URL}
	session := &Session{}
	defer session.Close()
	response, err := session.DoRequest(
		newSessionTestContext(),
		driver,
		newSessionTestRelayInfo(120, 0, "credential-a"),
		strings.NewReader(`{"model":"gpt-test","input":"first"}`),
	)
	require.NoError(t, err)
	require.NotNil(t, response)
	_, err = io.ReadAll(response.Body)
	require.ErrorContains(t, err, "response id exceeds")
	require.NoError(t, response.Body.Close())
	assert.False(t, session.HasLiveConnection())
	assert.False(t, session.CanContinue(strings.Repeat("x", maxTrackedResponseIDBytes+1)))
}

func TestResponseIDTrackingRejectsCumulativeByteOverflow(t *testing.T) {
	session := &Session{}
	responseID := strings.Repeat("x", maxTrackedResponseIDBytes-len("resp_0000_"))
	entryCountAtLimit := maxTrackedResponseIDTotalBytes / maxTrackedResponseIDBytes
	for index := 0; index < entryCountAtLimit; index++ {
		id := fmt.Sprintf("resp_%04d_%s", index, responseID)
		require.NoError(t, session.rememberResponseID(nil, id))
	}
	overflowID := fmt.Sprintf("resp_%04d_%s", entryCountAtLimit, responseID)
	require.ErrorContains(t, session.rememberResponseID(nil, overflowID), "cumulative limit")

	assert.Nil(t, session.responseIDs)
	assert.Nil(t, session.responseIDOrder)
	assert.Zero(t, session.responseIDBytes)
}

func TestContinuationAdmissionInvalidatesExpiredConnection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	connectionClosed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				close(connectionClosed)
				return
			}
			if err := conn.WriteMessage(websocket.TextMessage, []byte(
				`{"type":"response.completed","response":{"id":"resp_expired"}}`,
			)); err != nil {
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
		newSessionTestRelayInfo(121, 0, "credential-a"),
		strings.NewReader(`{"model":"gpt-test","input":"first"}`),
	)
	require.NoError(t, err)
	_, err = io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.True(t, session.CanContinue("resp_expired"))

	session.mu.Lock()
	session.connectedAt = time.Now().Add(-MaxConnectionLifetime - time.Second)
	session.mu.Unlock()
	assert.False(t, session.CanContinue("resp_expired"))
	assert.False(t, session.HasLiveConnection())
	select {
	case <-connectionClosed:
	case <-time.After(time.Second):
		require.FailNow(t, "continuation admission did not close the expired upstream connection")
	}
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

func TestDoRequestHandlesNilCapacityLeaseWhenDialSetupFails(t *testing.T) {
	dialErr := errors.New("cannot build upstream websocket URL")
	driver := &sessionTestDriver{dialErr: dialErr}
	session := &Session{}

	response, err := session.DoRequest(
		newSessionTestContext(),
		driver,
		newSessionTestRelayInfo(101, 0, "standard-api-key"),
		strings.NewReader(`{"model":"gpt-test","input":"hello"}`),
	)

	require.Nil(t, response)
	require.ErrorIs(t, err, dialErr)
	assert.Equal(t, int32(1), driver.dialCalls.Load())
	assert.Zero(t, driver.fallbackCalls.Load())
}

func TestDoRequestModelAffinityUsesExactOutboundPayload(t *testing.T) {
	const channelID = 102
	fingerprint := service.SubscriptionOAuthCredentialFingerprint(
		constant.ChannelTypeOpenAI,
		channelID,
		0,
		"standard-api-key",
	)
	tests := []struct {
		name      string
		bodyModel string
		wantDial  bool
	}{
		{
			name:      "raw passthrough model matches despite mapped relay model",
			bodyModel: "raw-model-a",
			wantDial:  true,
		},
		{
			// The exact-comparison contract survives the self-contained rebind rule:
			// a differing raw model must never silently reuse the old binding — it
			// rebinds (re-dials) instead.
			name:      "different raw model rebinds instead of reusing the binding",
			bodyModel: "raw-model-b",
			wantDial:  true,
		},
		{
			name:      "model affinity comparison is exact so case difference rebinds",
			bodyModel: "RAW-MODEL-A",
			wantDial:  true,
		},
		{
			name:      "model affinity preserves outbound whitespace so it rebinds",
			bodyModel: " raw-model-a ",
			wantDial:  true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dialErr := errors.New("dial reached after affinity validation")
			driver := &sessionTestDriver{dialErr: dialErr}
			session := &Session{
				channelID:             channelID,
				credentialFingerprint: fingerprint,
				model:                 "raw-model-a",
			}
			info := newSessionTestRelayInfo(channelID, 0, "standard-api-key")
			info.UpstreamModelName = "shared-mapped-model"

			response, err := session.DoRequest(
				newSessionTestContext(),
				driver,
				info,
				strings.NewReader(`{"model":"`+test.bodyModel+`","input":"hello"}`),
			)

			require.Nil(t, response)
			require.ErrorIs(t, err, dialErr)
			assert.Equal(t, int32(1), driver.dialCalls.Load())
		})
	}
}

func TestNormalizeResponseDoneEventUsesNestedTerminalState(t *testing.T) {
	tests := []struct {
		name         string
		payload      string
		wantType     string
		wantError    string
		wantMetadata string
	}{
		{
			name:         "completed",
			payload:      `{"type":"response.done","response":{"status":"completed","metadata":{"request_id":"req_completed"}},"metadata":{"exact_integer":9007199254740993}}`,
			wantType:     "response.completed",
			wantMetadata: `"exact_integer":9007199254740993`,
		},
		{
			name:         "incomplete",
			payload:      `{"type":"response.done","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"metadata":{"request_id":"req_incomplete"}}}`,
			wantType:     "response.incomplete",
			wantMetadata: `"request_id":"req_incomplete"`,
		},
		{
			name:         "failed",
			payload:      `{"type":"response.done","response":{"status":"failed","error":{"type":"server_error","code":"upstream_failure","message":"boom"},"metadata":{"request_id":"req_failed"}}}`,
			wantType:     "response.failed",
			wantError:    `"code":"upstream_failure"`,
			wantMetadata: `"request_id":"req_failed"`,
		},
		{
			name:         "error overrides completed status",
			payload:      `{"type":"response.done","response":{"status":"completed","error":{"type":"server_error","message":"late failure"},"metadata":{"request_id":"req_error"}}}`,
			wantType:     "response.failed",
			wantError:    `"message":"late failure"`,
			wantMetadata: `"request_id":"req_error"`,
		},
		{
			name:         "top-level error overrides completed status",
			payload:      `{"type":"response.done","error":{"type":"server_error","message":"transport failure"},"response":{"status":"completed","metadata":{"request_id":"req_top_error"}}}`,
			wantType:     "response.failed",
			wantError:    `"message":"transport failure"`,
			wantMetadata: `"request_id":"req_top_error"`,
		},
		{
			name:         "unknown status fails closed",
			payload:      `{"type":"response.done","response":{"status":"mystery","metadata":{"request_id":"req_unknown"}}}`,
			wantType:     "response.failed",
			wantMetadata: `"request_id":"req_unknown"`,
		},
		{
			name:         "missing response fails closed",
			payload:      `{"type":"response.done","metadata":{"request_id":"req_missing"}}`,
			wantType:     "response.failed",
			wantMetadata: `"request_id":"req_missing"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			normalized, eventType, err := NormalizeResponseDoneEvent([]byte(test.payload))
			require.NoError(t, err)
			assert.Equal(t, test.wantType, eventType)
			assert.Contains(t, string(normalized), `"type":"`+test.wantType+`"`)
			assert.Contains(t, string(normalized), test.wantMetadata)
			if test.wantError != "" {
				assert.Contains(t, string(normalized), test.wantError)
			}
		})
	}
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
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.done","response":{"status":"completed","metadata":{"exact_integer":9007199254740993}}}`))
	}))
	defer server.Close()

	driver := &sessionTestDriver{upstreamURL: server.URL}
	session := &Session{}
	defer session.Close()
	response, err := session.DoRequest(
		newSessionTestContext(),
		driver,
		newSessionTestRelayInfo(101, 4, "credential-a"),
		strings.NewReader(`{"model":"gpt-test","input":"hello","metadata":{"exact_integer":9007199254740993},"stream":true,"stream_options":{"include_usage":true},"background":false}`),
	)
	require.NoError(t, err)
	responsePayload, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Contains(t, string(responsePayload), `"type":"response.completed"`)
	assert.Contains(t, string(responsePayload), `"exact_integer":9007199254740993`)
	upstreamRequestPayload := <-requestPayload
	assert.Contains(t, string(upstreamRequestPayload), `"exact_integer":9007199254740993`)

	var event map[string]any
	require.NoError(t, common.UnmarshalWithNumber(upstreamRequestPayload, &event))
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

func TestDoRequestLeavesApplicationAttemptReplaySafeAfterRejectedHandshake(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
	}{
		{name: "rate limited", statusCode: http.StatusTooManyRequests},
		{name: "service unavailable", statusCode: http.StatusServiceUnavailable},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(test.statusCode)
				_, _ = w.Write([]byte(`{"error":{"type":"server_error","message":"try another channel"}}`))
			}))
			defer server.Close()

			driver := &sessionTestDriver{upstreamURL: server.URL}
			session := &Session{}
			info := newSessionTestRelayInfo(110, 0, "standard-api-key")
			response, err := session.DoRequest(
				newSessionTestContext(),
				driver,
				info,
				strings.NewReader(`{"model":"gpt-test","input":"hello"}`),
			)

			require.Nil(t, response)
			require.Error(t, err)
			var apiError *types.NewAPIError
			require.ErrorAs(t, err, &apiError)
			assert.Equal(t, test.statusCode, apiError.StatusCode)
			assert.Equal(t, test.statusCode, apiError.UpstreamStatusCode)
			assert.Equal(t, 7*time.Second, apiError.RetryAfter)
			written, responseStarted := info.UpstreamAttemptState()
			assert.False(t, written)
			assert.False(t, responseStarted)
			assert.True(t, info.HasUpstreamFailureResponse())
			assert.Zero(t, session.ChannelID())
			assert.Equal(t, int32(1), driver.dialCalls.Load())
			assert.Zero(t, driver.fallbackCalls.Load())
		})
	}
}

func TestDoRequestFallsBackToHTTPForNonErrorHandshakeResponse(t *testing.T) {
	for _, statusCode := range []int{http.StatusOK, http.StatusFound, http.StatusUpgradeRequired} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(statusCode)
			}))
			defer server.Close()

			fallbackBody := make(chan []byte, 1)
			driver := &sessionTestDriver{upstreamURL: server.URL, fallbackBody: fallbackBody}
			session := &Session{}
			defer session.Close()
			info := newSessionTestRelayInfo(111, 0, "standard-api-key")
			response, err := session.DoRequest(
				newSessionTestContext(),
				driver,
				info,
				strings.NewReader(`{"model":"gpt-test","input":"hello","stream":true}`),
			)

			require.NoError(t, err)
			require.NotNil(t, response)
			require.NoError(t, response.Body.Close())
			assert.Equal(t, int32(1), driver.dialCalls.Load())
			assert.Equal(t, int32(1), driver.fallbackCalls.Load())
			assert.Contains(t, string(<-fallbackBody), `"stream":true`)
			written, responseStarted := info.UpstreamAttemptState()
			assert.False(t, written)
			assert.False(t, responseStarted)
			assert.False(t, info.HasUpstreamFailureResponse())
		})
	}
}

func TestDoRequestOnCurrentTransportNeverRedialsAfterLiveConnectionExpires(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
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
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed"}`)); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	driver := &sessionTestDriver{upstreamURL: server.URL}
	session := &Session{}
	defer session.Close()
	info := newSessionTestRelayInfo(112, 0, "standard-api-key")
	response, err := session.DoRequest(
		newSessionTestContext(),
		driver,
		info,
		strings.NewReader(`{"model":"gpt-test","input":"first"}`),
	)
	require.NoError(t, err)
	_, err = io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.True(t, session.HasLiveConnection())

	session.mu.Lock()
	session.connectedAt = time.Now().Add(-MaxConnectionLifetime - time.Second)
	session.mu.Unlock()
	response, err = session.DoRequestOnCurrentTransport(
		newSessionTestContext(),
		driver,
		info,
		strings.NewReader(`{"model":"gpt-test","input":"second"}`),
	)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.False(t, session.HasLiveConnection())
	assert.Equal(t, int32(1), driver.dialCalls.Load())
	assert.Equal(t, int32(1), driver.fallbackCalls.Load())

	response, err = session.DoRequestOnCurrentTransport(
		newSessionTestContext(),
		driver,
		info,
		strings.NewReader(`{"model":"gpt-test","previous_response_id":"resp_from_websocket","input":"continue"}`),
	)
	require.Nil(t, response)
	require.Error(t, err)
	var apiError *types.NewAPIError
	require.ErrorAs(t, err, &apiError)
	assert.Equal(t, types.ErrorCodeWebSocketConnectionLimitReached, apiError.GetErrorCode())
	assert.True(t, types.IsSkipRetryError(apiError))
	assert.Equal(t, int32(1), driver.dialCalls.Load())
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

func TestDoRequestReturnsConflictForSessionAffinitySwitch(t *testing.T) {
	tests := []struct {
		name    string
		session *Session
		info    *relaycommon.RelayInfo
	}{
		{
			name:    "channel",
			session: &Session{channelID: 107},
			info:    newSessionTestRelayInfo(108, 0, "credential-a"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			driver := &sessionTestDriver{}
			response, err := test.session.DoRequest(
				newSessionTestContext(),
				driver,
				test.info,
				strings.NewReader(`{"model":"gpt-test","input":"hello"}`),
			)
			require.Nil(t, response)
			require.Error(t, err)
			var apiError *types.NewAPIError
			require.ErrorAs(t, err, &apiError)
			assert.Equal(t, http.StatusConflict, apiError.StatusCode)
			assert.Equal(t, types.ErrorCodeInvalidRequest, apiError.GetErrorCode())
			assert.True(t, types.IsSkipRetryError(apiError))
			assert.Zero(t, driver.dialCalls.Load())
			assert.Zero(t, driver.fallbackCalls.Load())
		})
	}
}

func TestDoRequestRejectsContinuationWithoutLiveUpstreamConnection(t *testing.T) {
	const channelID = 108
	fingerprint := service.SubscriptionOAuthCredentialFingerprint(
		constant.ChannelTypeOpenAI,
		channelID,
		0,
		"credential-a",
	)
	tests := []struct {
		name    string
		session *Session
	}{
		{
			name:    "first turn continuation",
			session: &Session{},
		},
		{
			name: "detected disconnected session",
			session: &Session{
				channelID:             channelID,
				credentialFingerprint: fingerprint,
				model:                 "gpt-test",
			},
		},
		{
			name:    "http fallback session",
			session: &Session{httpFallback: true},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			acquireCalls := atomic.Int32{}
			driver := &sessionTestDriver{
				acquire: func(_ *gin.Context, _ *relaycommon.RelayInfo) (*service.SubscriptionOAuthLease, error) {
					acquireCalls.Add(1)
					return nil, nil
				},
			}
			response, err := test.session.DoRequest(
				newSessionTestContext(),
				driver,
				newSessionTestRelayInfo(channelID, 0, "credential-a"),
				strings.NewReader(`{"model":"gpt-test","previous_response_id":"resp_1","input":"continue"}`),
			)
			require.Nil(t, response)
			require.Error(t, err)
			var apiError *types.NewAPIError
			require.ErrorAs(t, err, &apiError)
			assert.Equal(t, http.StatusBadRequest, apiError.StatusCode)
			assert.Equal(t, types.ErrorCodeWebSocketConnectionLimitReached, apiError.GetErrorCode())
			assert.True(t, types.IsSkipRetryError(apiError))
			assert.Zero(t, acquireCalls.Load())
			assert.Zero(t, driver.dialCalls.Load())
			assert.Zero(t, driver.fallbackCalls.Load())
		})
	}
}

func TestDoRequestAllowsContinuationOnLiveUpstreamConnection(t *testing.T) {
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
	for _, body := range []string{
		`{"model":"gpt-test","input":"first"}`,
		`{"model":"gpt-test","previous_response_id":"resp_1","input":"continue"}`,
	} {
		response, err := session.DoRequest(
			newSessionTestContext(),
			driver,
			newSessionTestRelayInfo(109, 0, "credential-a"),
			strings.NewReader(body),
		)
		require.NoError(t, err)
		_, err = io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
	}

	assert.Equal(t, int32(1), driver.dialCalls.Load())
	assert.Zero(t, driver.fallbackCalls.Load())
	assert.Equal(t, int32(2), requestCount.Load())
}

func TestDoRequestRejectsResponseIDFromReplacedConnection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	connectionCount := atomic.Int32{}
	requestCount := atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connectionNumber := connectionCount.Add(1)
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			requestCount.Add(1)
			payload := fmt.Sprintf(
				`{"type":"response.completed","response":{"id":"resp_connection_%d"}}`,
				connectionNumber,
			)
			if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	driver := &sessionTestDriver{upstreamURL: server.URL}
	session := &Session{}
	defer session.Close()
	info := newSessionTestRelayInfo(119, 0, "credential-a")
	for _, body := range []string{
		`{"model":"gpt-test","input":"first"}`,
		`{"model":"gpt-test","input":"replacement"}`,
	} {
		response, err := session.DoRequest(
			newSessionTestContext(),
			driver,
			info,
			strings.NewReader(body),
		)
		require.NoError(t, err)
		_, err = io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())

		if connectionCount.Load() == 1 {
			session.mu.Lock()
			session.invalidateLocked(session.conn)
			session.mu.Unlock()
		}
	}

	response, err := session.DoRequest(
		newSessionTestContext(),
		driver,
		info,
		strings.NewReader(`{"model":"gpt-test","previous_response_id":"resp_connection_1","input":"stale"}`),
	)
	require.Nil(t, response)
	var apiError *types.NewAPIError
	require.ErrorAs(t, err, &apiError)
	assert.Equal(t, types.ErrorCodeWebSocketConnectionLimitReached, apiError.GetErrorCode())
	assert.True(t, types.IsSkipRetryError(apiError))
	assert.Equal(t, int32(2), connectionCount.Load())
	assert.Equal(t, int32(2), requestCount.Load())

	response, err = session.DoRequest(
		newSessionTestContext(),
		driver,
		info,
		strings.NewReader(`{"model":"gpt-test","previous_response_id":"resp_connection_2","input":"current"}`),
	)
	require.NoError(t, err)
	_, err = io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, int32(3), requestCount.Load())
}

// A connection that dies while idle is detected by the permanent reader and
// invalidated, so the next self-contained turn reconnects before writing its
// frame — the written-request replay invariant holds because nothing was
// written to the dead socket. (Under the pre-reader model this scenario was a
// post-write failure; the reader converts it into a replay-safe reconnect.)
func TestDoRequestReconnectsSafelyAfterIdleConnectionDeath(t *testing.T) {
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

	// Permanent-reader model: the reader observes the closed socket while idle and
	// invalidates the binding, so the next self-contained turn reconnects BEFORE
	// its frame is written — replay-safe by construction. Wait for that cleanup
	// deterministically instead of racing it.
	require.Eventually(t, func() bool {
		return !session.HasLiveConnection()
	}, 2*time.Second, 5*time.Millisecond, "reader did not invalidate the closed connection")

	secondInfo := newSessionTestRelayInfo(103, 0, "credential-a")
	response, err = session.DoRequest(
		newSessionTestContext(),
		driver,
		secondInfo,
		strings.NewReader(`{"model":"gpt-test","input":"second"}`),
	)
	require.NoError(t, err)
	_, err = io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, int32(2), driver.dialCalls.Load()) // pre-write reconnect, not a replay
	assert.Zero(t, driver.fallbackCalls.Load())
	assert.Equal(t, int32(2), requestCount.Load())
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

// D: a connection idle beyond ReuseIdleReconnectThreshold is recycled — the next
// turn reconnects (a fresh handshake) rather than reusing a possibly-dead socket.
func TestDoRequestReconnectsAfterIdleThreshold(t *testing.T) {
	gin.SetMode(gin.TestMode)
	original := ReuseIdleReconnectThreshold
	ReuseIdleReconnectThreshold = 20 * time.Millisecond
	defer func() { ReuseIdleReconnectThreshold = original }()

	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
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
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_1"}}`)); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	driver := &sessionTestDriver{upstreamURL: server.URL}
	session := &Session{}
	defer session.Close()

	// Turn 1: self-contained; establishes the connection.
	resp1, err := session.DoRequest(newSessionTestContext(), driver, newSessionTestRelayInfo(110, 0, "credential-a"), strings.NewReader(`{"model":"gpt-test","input":"one"}`))
	require.NoError(t, err)
	_, err = io.ReadAll(resp1.Body)
	require.NoError(t, err)
	require.NoError(t, resp1.Body.Close())

	// Drive the idle baseline into the past instead of sleeping, so the threshold
	// fires deterministically without depending on scheduler timing.
	session.mu.Lock()
	session.lastUsedAt = time.Now().Add(-time.Minute)
	session.mu.Unlock()

	// Turn 2: self-contained; idle beyond the threshold recycles and re-dials.
	resp2, err := session.DoRequest(newSessionTestContext(), driver, newSessionTestRelayInfo(110, 0, "credential-a"), strings.NewReader(`{"model":"gpt-test","input":"two"}`))
	require.NoError(t, err)
	_, err = io.ReadAll(resp2.Body)
	require.NoError(t, err)
	require.NoError(t, resp2.Body.Close())

	// The second turn recycled the idle connection and re-dialed.
	assert.Equal(t, int32(2), driver.dialCalls.Load())
}

// H: within the idle threshold a reused connection can still be silently dead.
// The turn's request is already written (no replay), but it must fail in
// ~FirstEventTimeout, not block for the full idleTimeout (minutes).
func TestDoRequestReusedTurnFailsFastOnSilentConnection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalFirstEvent := FirstEventTimeout
	FirstEventTimeout = 100 * time.Millisecond
	defer func() { FirstEventTimeout = originalFirstEvent }()
	originalThreshold := ReuseIdleReconnectThreshold
	ReuseIdleReconnectThreshold = time.Hour // keep D from firing so we exercise H
	defer func() { ReuseIdleReconnectThreshold = originalThreshold }()

	release := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_1"}}`)); err != nil {
			return
		}
		// Second turn: stay silent (never read or reply) while keeping the socket
		// open — a half-open / idle-dead connection.
		<-release
	}))
	defer server.Close()
	defer close(release)

	driver := &sessionTestDriver{upstreamURL: server.URL}
	session := &Session{}
	defer session.Close()

	resp1, err := session.DoRequest(newSessionTestContext(), driver, newSessionTestRelayInfo(111, 0, "credential-a"), strings.NewReader(`{"model":"gpt-test","input":"one"}`))
	require.NoError(t, err)
	_, err = io.ReadAll(resp1.Body)
	require.NoError(t, err)
	require.NoError(t, resp1.Body.Close())

	start := time.Now()
	resp2, err := session.DoRequest(newSessionTestContext(), driver, newSessionTestRelayInfo(111, 0, "credential-a"), strings.NewReader(`{"model":"gpt-test","input":"two"}`))
	require.NoError(t, err)
	_, readErr := io.ReadAll(resp2.Body)
	elapsed := time.Since(start)
	_ = resp2.Body.Close()

	require.Error(t, readErr) // first-event timeout surfaces as a stream read error
	assert.Less(t, elapsed, 2*time.Second, "reused dead connection must fail fast, not block idleTimeout")
	assert.Equal(t, int32(1), driver.dialCalls.Load()) // reused (D did not fire), then failed fast
}

// D must NOT recycle a continuation: previous_response_id is owned by this exact
// connection, so an idle-over-threshold continuation keeps its socket (no second
// handshake) rather than moving to a replacement. A dead connection would be
// failed fast by H instead, never reconnected-and-resubmitted.
func TestDoRequestKeepsContinuationOnIdleConnection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
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
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_1"}}`)); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	driver := &sessionTestDriver{upstreamURL: server.URL}
	session := &Session{}
	defer session.Close()

	// Turn 1: self-contained; emits response id resp_1 owned by this connection.
	resp1, err := session.DoRequest(newSessionTestContext(), driver, newSessionTestRelayInfo(112, 0, "credential-a"), strings.NewReader(`{"model":"gpt-test","input":"one"}`))
	require.NoError(t, err)
	_, err = io.ReadAll(resp1.Body)
	require.NoError(t, err)
	require.NoError(t, resp1.Body.Close())

	// Idle far beyond the threshold.
	session.mu.Lock()
	session.lastUsedAt = time.Now().Add(-time.Minute)
	session.mu.Unlock()

	// Turn 2: continuation on resp_1. Must reuse the same connection despite the
	// idle gap — never a second handshake, never a fallback.
	resp2, err := session.DoRequest(newSessionTestContext(), driver, newSessionTestRelayInfo(112, 0, "credential-a"), strings.NewReader(`{"model":"gpt-test","previous_response_id":"resp_1","input":"two"}`))
	require.NoError(t, err)
	_, err = io.ReadAll(resp2.Body)
	require.NoError(t, err)
	require.NoError(t, resp2.Body.Close())

	assert.Equal(t, int32(1), driver.dialCalls.Load()) // continuation kept its connection
	assert.Zero(t, driver.fallbackCalls.Load())
}

func TestDoRequestRepliesToUpstreamPing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	pongObserved := make(chan string, 1)
	serverErr := make(chan error, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			serverErr <- err
			return
		}
		conn.SetPongHandler(func(message string) error {
			pongObserved <- message
			return nil
		})
		if err := conn.WriteControl(websocket.PingMessage, []byte("keepalive-check"), time.Now().Add(time.Second)); err != nil {
			serverErr <- err
			return
		}
		go func() {
			select {
			case message := <-pongObserved:
				if message != "keepalive-check" {
					serverErr <- fmt.Errorf("unexpected pong payload %q", message)
					return
				}
				serverErr <- conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed"}`))
			case <-time.After(2 * time.Second):
				serverErr <- errors.New("timed out waiting for upstream pong")
			}
		}()
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()

	driver := &sessionTestDriver{upstreamURL: server.URL}
	session := &Session{}
	defer session.Close()
	resp, err := session.DoRequest(
		newSessionTestContext(),
		driver,
		newSessionTestRelayInfo(113, 0, "credential-a"),
		strings.NewReader(`{"model":"gpt-test","input":"ping"}`),
	)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Contains(t, string(body), "response.completed")
	require.NoError(t, <-serverErr)
}

func TestEmitEventAndMaybeStopReturnsProtocolAndPipeErrors(t *testing.T) {
	session := &Session{}

	reader, writer := io.Pipe()
	_, stop, err := session.emitEventAndMaybeStop(nil, writer, []byte(`{"type":`))
	require.True(t, stop)
	require.Error(t, err)
	_ = reader.Close()

	reader, writer = io.Pipe()
	require.NoError(t, reader.Close())
	eventType, stop, err := session.emitEventAndMaybeStop(nil, writer, []byte(`{"type":"response.created"}`))
	require.Equal(t, "response.created", eventType)
	require.True(t, stop)
	require.ErrorIs(t, err, io.ErrClosedPipe)
}

// A self-contained turn may switch models within one downstream session: the
// old binding (connection, model, response ids) is dropped and the new model
// establishes a fresh upstream connection instead of failing with 409.
func TestDoRequestSelfContainedModelSwitchReconnects(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
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
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_1"}}`)); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	driver := &sessionTestDriver{upstreamURL: server.URL}
	session := &Session{}
	defer session.Close()

	for i, body := range []string{
		`{"model":"gpt-model-a","input":"one"}`,
		`{"model":"gpt-model-b","input":"two"}`,
	} {
		resp, err := session.DoRequest(newSessionTestContext(), driver, newSessionTestRelayInfo(130, 0, "credential-a"), strings.NewReader(body))
		require.NoError(t, err, "turn %d", i)
		_, err = io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}

	// The model switch dropped the old connection and re-dialed for the new model.
	assert.Equal(t, int32(2), driver.dialCalls.Load())
	assert.Zero(t, driver.fallbackCalls.Load())
}

// A continuation must never follow a model switch: previous_response_id is owned
// by the old model's connection, so a cross-model continuation stays fail-closed
// (409) without reconnecting or moving to another channel.
func TestDoRequestCrossModelContinuationRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
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
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_1"}}`)); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	driver := &sessionTestDriver{upstreamURL: server.URL}
	session := &Session{}
	defer session.Close()

	resp1, err := session.DoRequest(newSessionTestContext(), driver, newSessionTestRelayInfo(131, 0, "credential-a"), strings.NewReader(`{"model":"gpt-model-a","input":"one"}`))
	require.NoError(t, err)
	_, err = io.ReadAll(resp1.Body)
	require.NoError(t, err)
	require.NoError(t, resp1.Body.Close())

	resp2, err := session.DoRequest(newSessionTestContext(), driver, newSessionTestRelayInfo(131, 0, "credential-a"), strings.NewReader(`{"model":"gpt-model-b","previous_response_id":"resp_1","input":"two"}`))
	require.Nil(t, resp2)
	require.Error(t, err)
	var apiError *types.NewAPIError
	require.ErrorAs(t, err, &apiError)
	assert.Equal(t, http.StatusConflict, apiError.StatusCode)
	assert.True(t, types.IsSkipRetryError(apiError))
	// No reconnect, no fallback: the continuation was refused fail-closed.
	assert.Equal(t, int32(1), driver.dialCalls.Load())
	assert.Zero(t, driver.fallbackCalls.Load())
}

func TestIdleReaderAnswersUpstreamPingAndKeepsContinuation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	pongObserved := make(chan struct{}, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetPongHandler(func(message string) error {
			if message == "idle-check" {
				select {
				case pongObserved <- struct{}{}:
				default:
				}
			}
			return nil
		})
		turn := 0
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			turn++
			responseID := fmt.Sprintf("resp_%d", turn)
			if err := conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q}}`, responseID))); err != nil {
				return
			}
			if turn == 1 {
				if err := conn.WriteControl(websocket.PingMessage, []byte("idle-check"), time.Now().Add(time.Second)); err != nil {
					return
				}
			}
		}
	}))
	defer server.Close()

	driver := &sessionTestDriver{upstreamURL: server.URL}
	session := &Session{}
	defer session.Close()
	resp1, err := session.DoRequest(newSessionTestContext(), driver, newSessionTestRelayInfo(140, 0, "credential-a"), strings.NewReader(`{"model":"gpt-test","input":"one"}`))
	require.NoError(t, err)
	_, err = io.ReadAll(resp1.Body)
	require.NoError(t, err)
	require.NoError(t, resp1.Body.Close())
	select {
	case <-pongObserved:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "idle reader did not answer upstream ping")
	}

	resp2, err := session.DoRequest(newSessionTestContext(), driver, newSessionTestRelayInfo(140, 0, "credential-a"), strings.NewReader(`{"model":"gpt-test","previous_response_id":"resp_1","input":"two"}`))
	require.NoError(t, err)
	_, err = io.ReadAll(resp2.Body)
	require.NoError(t, err)
	require.NoError(t, resp2.Body.Close())
	assert.Equal(t, int32(1), driver.dialCalls.Load())
	assert.Zero(t, driver.fallbackCalls.Load())
}

func TestPostTerminalConnectionExtensionsDoNotInvalidateConnection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	metadataConsumed := make(chan struct{}, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetPongHandler(func(message string) error {
			if message == "metadata-drained" {
				select {
				case metadataConsumed <- struct{}{}:
				default:
				}
			}
			return nil
		})
		turn := 0
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			turn++
			if err := conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d"}}`, turn))); err != nil {
				return
			}
			if turn == 1 {
				if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"responsesapi.websocket_timing","elapsed_ms":12}`)); err != nil {
					return
				}
				if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"vendor.future_connection_notice","sequence":1}`)); err != nil {
					return
				}
				if err := conn.WriteControl(websocket.PingMessage, []byte("metadata-drained"), time.Now().Add(time.Second)); err != nil {
					return
				}
			}
		}
	}))
	defer server.Close()

	driver := &sessionTestDriver{upstreamURL: server.URL}
	session := &Session{}
	defer session.Close()
	resp1, err := session.DoRequest(newSessionTestContext(), driver, newSessionTestRelayInfo(141, 0, "credential-a"), strings.NewReader(`{"model":"gpt-test","input":"one"}`))
	require.NoError(t, err)
	_, err = io.ReadAll(resp1.Body)
	require.NoError(t, err)
	require.NoError(t, resp1.Body.Close())
	select {
	case <-metadataConsumed:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "reader did not consume post-terminal metadata")
	}
	require.True(t, session.HasLiveConnection())

	resp2, err := session.DoRequest(newSessionTestContext(), driver, newSessionTestRelayInfo(141, 0, "credential-a"), strings.NewReader(`{"model":"gpt-test","previous_response_id":"resp_1","input":"two"}`))
	require.NoError(t, err)
	_, err = io.ReadAll(resp2.Body)
	require.NoError(t, err)
	require.NoError(t, resp2.Body.Close())
	assert.Equal(t, int32(1), driver.dialCalls.Load())
}

func TestPostTerminalTurnEventInvalidatesConnection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_1"}}`)); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"late"}`))
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()

	driver := &sessionTestDriver{upstreamURL: server.URL}
	session := &Session{}
	defer session.Close()
	resp, err := session.DoRequest(newSessionTestContext(), driver, newSessionTestRelayInfo(145, 0, "credential-a"), strings.NewReader(`{"model":"gpt-test","input":"one"}`))
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Eventually(t, func() bool { return !session.HasLiveConnection() }, 2*time.Second, 5*time.Millisecond)
}

func TestPostTerminalUnclassifiableFrameInvalidatesConnection(t *testing.T) {
	testCases := []struct {
		name    string
		payload string
	}{
		{name: "malformed JSON", payload: `{"type":`},
		{name: "missing type", payload: `{"sequence":1}`},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
				if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_1"}}`)); err != nil {
					return
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(testCase.payload))
				_, _, _ = conn.ReadMessage()
			}))
			defer server.Close()

			driver := &sessionTestDriver{upstreamURL: server.URL}
			session := &Session{}
			defer session.Close()
			resp, err := session.DoRequest(newSessionTestContext(), driver, newSessionTestRelayInfo(149, 0, "credential-a"), strings.NewReader(`{"model":"gpt-test","input":"one"}`))
			require.NoError(t, err)
			_, err = io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Eventually(t, func() bool { return !session.HasLiveConnection() }, 2*time.Second, 5*time.Millisecond)
		})
	}
}

func TestConnectionMetadataDoesNotSatisfyFirstTurnEventTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalFirstEvent := FirstEventTimeout
	FirstEventTimeout = 50 * time.Millisecond
	t.Cleanup(func() { FirstEventTimeout = originalFirstEvent })
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"responsesapi.websocket_timing","elapsed_ms":5}`)); err != nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}))
	defer server.Close()

	driver := &sessionTestDriver{upstreamURL: server.URL}
	session := &Session{}
	defer session.Close()
	resp, err := session.DoRequest(newSessionTestContext(), driver, newSessionTestRelayInfo(147, 0, "credential-a"), strings.NewReader(`{"model":"gpt-test","input":"one"}`))
	require.NoError(t, err)
	_, readErr := io.ReadAll(resp.Body)
	require.Error(t, readErr)
	require.Contains(t, readErr.Error(), "no event within 50ms")
	require.NoError(t, resp.Body.Close())
}

func TestStaleContinuationProbeSuccessKeepsOriginalConnection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalFreshness, originalTimeout := ContinuationLivenessFreshness, ContinuationProbeTimeout
	ContinuationLivenessFreshness = time.Nanosecond
	ContinuationProbeTimeout = time.Second
	t.Cleanup(func() {
		ContinuationLivenessFreshness, ContinuationProbeTimeout = originalFreshness, originalTimeout
	})
	applicationFrames := atomic.Int32{}
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			messageType, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if messageType != websocket.TextMessage {
				continue
			}
			turn := applicationFrames.Add(1)
			if err := conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d"}}`, turn))); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	driver := &sessionTestDriver{upstreamURL: server.URL}
	session := &Session{}
	defer session.Close()
	resp1, err := session.DoRequest(newSessionTestContext(), driver, newSessionTestRelayInfo(146, 0, "credential-a"), strings.NewReader(`{"model":"gpt-test","input":"one"}`))
	require.NoError(t, err)
	_, err = io.ReadAll(resp1.Body)
	require.NoError(t, err)
	require.NoError(t, resp1.Body.Close())
	require.NotNil(t, session.lastActivity)
	session.lastActivity.Store(time.Now().Add(-time.Minute).UnixNano())

	resp2, err := session.DoRequest(newSessionTestContext(), driver, newSessionTestRelayInfo(146, 0, "credential-a"), strings.NewReader(`{"model":"gpt-test","previous_response_id":"resp_1","input":"two"}`))
	require.NoError(t, err)
	_, err = io.ReadAll(resp2.Body)
	require.NoError(t, err)
	require.NoError(t, resp2.Body.Close())
	assert.Equal(t, int32(2), applicationFrames.Load())
	assert.Equal(t, int32(1), driver.dialCalls.Load())
}

func TestReadErrorAfterTerminalInvalidatesConnection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	applicationFrames := atomic.Int32{}
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		applicationFrames.Add(1)
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_1"}}`))
	}))
	defer server.Close()

	driver := &sessionTestDriver{upstreamURL: server.URL}
	session := &Session{}
	defer session.Close()
	resp1, err := session.DoRequest(newSessionTestContext(), driver, newSessionTestRelayInfo(142, 0, "credential-a"), strings.NewReader(`{"model":"gpt-test","input":"one"}`))
	require.NoError(t, err)
	_, err = io.ReadAll(resp1.Body)
	require.NoError(t, err)
	require.NoError(t, resp1.Body.Close())
	require.Eventually(t, func() bool { return !session.HasLiveConnection() }, 2*time.Second, 5*time.Millisecond)

	resp2, err := session.DoRequest(newSessionTestContext(), driver, newSessionTestRelayInfo(142, 0, "credential-a"), strings.NewReader(`{"model":"gpt-test","previous_response_id":"resp_1","input":"two"}`))
	require.Nil(t, resp2)
	require.Error(t, err)
	assert.Equal(t, int32(1), applicationFrames.Load())
	assert.Equal(t, int32(1), driver.dialCalls.Load())
}

func TestReadErrorInvalidatesConnectionBeforeBlockedTurnNotification(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		for range 5 {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"chunk"}`)); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	driver := &sessionTestDriver{upstreamURL: server.URL}
	session := &Session{}
	defer session.Close()
	resp, err := session.DoRequest(newSessionTestContext(), driver, newSessionTestRelayInfo(148, 0, "credential-a"), strings.NewReader(`{"model":"gpt-test","input":"one"}`))
	require.NoError(t, err)
	// Leave the downstream pipe unread so the stream consumer blocks and the
	// four-slot turn queue fills. The reader must still clear the dead generation
	// before it can block notifying that consumer about the read error.
	require.Eventually(t, func() bool { return !session.HasLiveConnection() }, 2*time.Second, 5*time.Millisecond)
	require.NoError(t, resp.Body.Close())
}

func TestStaleContinuationProbeFailureDoesNotWriteApplicationFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalFreshness, originalTimeout := ContinuationLivenessFreshness, ContinuationProbeTimeout
	ContinuationLivenessFreshness = time.Nanosecond
	ContinuationProbeTimeout = 80 * time.Millisecond
	t.Cleanup(func() {
		ContinuationLivenessFreshness, ContinuationProbeTimeout = originalFreshness, originalTimeout
	})
	applicationFrames := atomic.Int32{}
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetPingHandler(func(string) error { return nil })
		for {
			messageType, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if messageType != websocket.TextMessage {
				continue
			}
			if applicationFrames.Add(1) == 1 {
				if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_1"}}`)); err != nil {
					return
				}
			}
		}
	}))
	defer server.Close()

	driver := &sessionTestDriver{upstreamURL: server.URL}
	session := &Session{}
	defer session.Close()
	resp1, err := session.DoRequest(newSessionTestContext(), driver, newSessionTestRelayInfo(143, 0, "credential-a"), strings.NewReader(`{"model":"gpt-test","input":"one"}`))
	require.NoError(t, err)
	_, err = io.ReadAll(resp1.Body)
	require.NoError(t, err)
	require.NoError(t, resp1.Body.Close())
	require.NotNil(t, session.lastActivity)
	session.lastActivity.Store(time.Now().Add(-time.Minute).UnixNano())

	resp2, err := session.DoRequest(newSessionTestContext(), driver, newSessionTestRelayInfo(143, 0, "credential-a"), strings.NewReader(`{"model":"gpt-test","previous_response_id":"resp_1","input":"two"}`))
	require.Nil(t, resp2)
	require.Error(t, err)
	assert.Equal(t, int32(1), applicationFrames.Load())
	assert.Equal(t, int32(1), driver.dialCalls.Load())
}

func TestMaxConnectionLifetimeActivelyClosesIdleConnection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalLifetime := MaxConnectionLifetime
	MaxConnectionLifetime = 100 * time.Millisecond
	t.Cleanup(func() { MaxConnectionLifetime = originalLifetime })
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_1"}}`))
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()

	driver := &sessionTestDriver{upstreamURL: server.URL}
	session := &Session{}
	defer session.Close()
	resp, err := session.DoRequest(newSessionTestContext(), driver, newSessionTestRelayInfo(144, 0, "credential-a"), strings.NewReader(`{"model":"gpt-test","input":"one"}`))
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Eventually(t, func() bool { return !session.HasLiveConnection() }, 2*time.Second, 5*time.Millisecond)
}
