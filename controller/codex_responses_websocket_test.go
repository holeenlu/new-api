package controller

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/responsesws"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResponsesWebSocketSSEWriterForwardsJSONEvents(t *testing.T) {
	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()

		writer := newResponsesWebSocketSSEWriter(context.Background(), conn)
		_, err = writer.Write([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hel"))
		if err != nil {
			serverErr <- err
			return
		}
		_, err = writer.Write([]byte("lo\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"))
		if err != nil {
			serverErr <- err
			return
		}
		serverErr <- writer.Finish()
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	_, first, err := conn.ReadMessage()
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"response.output_text.delta","delta":"hello"}`, string(first))
	_, second, err := conn.ReadMessage()
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"response.completed"}`, string(second))
	require.NoError(t, <-serverErr)
}

func TestNormalizeResponsesWebSocketFrameSupportsAppend(t *testing.T) {
	first, err := normalizeResponsesWebSocketFrame(map[string]any{
		"type":         "response.create",
		"model":        "gpt-5.6-sol",
		"instructions": "answer briefly",
		"input":        []any{map[string]any{"role": "user", "content": "one"}},
	}, nil)
	require.NoError(t, err)
	require.Equal(t, true, first["stream"])

	appendFrame, err := normalizeResponsesWebSocketFrame(map[string]any{
		"type":  "response.append",
		"input": []any{map[string]any{"role": "user", "content": "two"}},
	}, reusableResponsesWebSocketFields(first))
	require.NoError(t, err)
	require.Equal(t, "gpt-5.6-sol", appendFrame["model"])
	require.Equal(t, "answer briefly", appendFrame["instructions"])
	require.Equal(t, true, appendFrame["stream"])
}

func TestReusableResponsesWebSocketFieldsDropsRequestState(t *testing.T) {
	defaults := reusableResponsesWebSocketFields(map[string]any{
		"model":                "gpt-5.6-luna",
		"instructions":         "keep",
		"input":                []any{map[string]any{"content": "large input"}},
		"previous_response_id": "response-id",
		"stream":               true,
		"generate":             true,
	})

	require.Equal(t, map[string]any{
		"model":        "gpt-5.6-luna",
		"instructions": "keep",
	}, defaults)
}

func TestNormalizeResponsesWebSocketFrameRejectsAppendBeforeCreate(t *testing.T) {
	_, err := normalizeResponsesWebSocketFrame(map[string]any{
		"type":  "response.append",
		"input": []any{},
	}, nil)
	require.EqualError(t, err, "response.append received before response.create")
}

func TestResponsesWebSocketDefaultsResetWhenNewCreateStarts(t *testing.T) {
	firstCreate, defaults, err := beginResponsesWebSocketTurn(map[string]any{
		"type":         "response.create",
		"model":        "gpt-5.6-sol",
		"instructions": "first defaults",
		"input":        []any{},
	}, nil)
	require.NoError(t, err)
	require.Nil(t, defaults)

	// Simulate the first create reaching response.completed.
	defaults = reusableResponsesWebSocketFields(firstCreate)
	require.NotNil(t, defaults)

	_, defaults, err = beginResponsesWebSocketTurn(map[string]any{
		"type":         "response.create",
		"model":        "gpt-5.6-sol",
		"instructions": "replacement defaults",
		"input":        []any{},
	}, defaults)
	require.NoError(t, err)
	require.Nil(t, defaults)

	// Simulate the replacement create ending in response.failed: no clean
	// terminal stores new defaults, so append cannot inherit the first create.
	_, defaults, err = beginResponsesWebSocketTurn(map[string]any{
		"type":  "response.append",
		"input": []any{},
	}, defaults)
	require.Nil(t, defaults)
	require.EqualError(t, err, "response.append received before response.create")
}

// An upstream failure surfaced over the client WebSocket must keep its
// classification: a 429 becomes a rate_limit_error carrying the resolved
// retry_after, not a generic invalid_request_error, so the client can wait.
func TestResponsesWebSocketErrorFrameCarriesClassificationAndRetryAfter(t *testing.T) {
	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		writer := newResponsesWebSocketSSEWriter(context.Background(), conn)
		// Mirror the relay setting a status and Retry-After before any output.
		writer.Header().Set("Retry-After", "30")
		writer.WriteHeader(http.StatusTooManyRequests)
		serverErr <- writer.Finish()
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	_, createdFrame, err := conn.ReadMessage()
	require.NoError(t, err)
	require.JSONEq(t,
		`{"type":"response.created","response":{"id":"resp_gateway_failed","object":"response","status":"in_progress"}}`,
		string(createdFrame),
	)
	_, frame, err := conn.ReadMessage()
	require.NoError(t, err)
	require.JSONEq(t,
		`{"type":"response.failed","response":{"id":"resp_gateway_failed","object":"response","status":"failed","error":{"message":"Too Many Requests","type":"rate_limit_error","code":429,"retry_after":30}}}`,
		string(frame),
	)
	require.NoError(t, <-serverErr)
}

func TestResponsesWebSocketContinuationErrorFramePreservesStableCode(t *testing.T) {
	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()

		apiError := responsesWebSocketContinuationError("resp_unavailable", &responsesws.Session{})
		require.NotNil(t, apiError)
		serverErr <- writeResponsesWebSocketErrorWithCode(
			conn,
			apiError.StatusCode,
			apiError.ToOpenAIError().Message,
			apiError.GetErrorCode(),
			"",
		)
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	_, frame, err := conn.ReadMessage()
	require.NoError(t, err)
	var event struct {
		Type  string `json:"type"`
		Error struct {
			Code       any    `json:"code"`
			RetryAfter *int   `json:"retry_after"`
			Message    string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, common.Unmarshal(frame, &event))
	assert.Equal(t, "error", event.Type)
	assert.Equal(t, string(types.ErrorCodeWebSocketConnectionLimitReached), event.Error.Code)
	assert.Nil(t, event.Error.RetryAfter)
	assert.NotEmpty(t, event.Error.Message)
	require.NoError(t, <-serverErr)
}

// SucceededTurn gates response.append inheritance: only a clean terminal
// (response.completed / response.incomplete) counts, so a failed or errored turn
// never seeds the next append.
func TestResponsesWebSocketSSEWriterMarksSuccessOnlyOnCleanTerminal(t *testing.T) {
	cases := []struct {
		name    string
		event   string
		success bool
	}{
		{"completed", `{"type":"response.completed"}`, true},
		{"incomplete", `{"type":"response.incomplete"}`, true},
		{"failed", `{"type":"response.failed"}`, false},
		{"error", `{"type":"error"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := make(chan bool, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
				if err != nil {
					got <- false
					return
				}
				defer conn.Close()
				writer := newResponsesWebSocketSSEWriter(context.Background(), conn)
				_, _ = writer.Write([]byte("data: " + tc.event + "\n\n"))
				_ = writer.Finish()
				got <- writer.SucceededTurn()
			}))
			defer server.Close()

			url := "ws" + strings.TrimPrefix(server.URL, "http")
			conn, _, err := websocket.DefaultDialer.Dial(url, nil)
			require.NoError(t, err)
			defer conn.Close()
			go func() {
				for {
					if _, _, e := conn.ReadMessage(); e != nil {
						return
					}
				}
			}()
			require.Equal(t, tc.success, <-got)
		})
	}
}

func TestResponsesWebSocketTerminalDetectionUsesTopLevelType(t *testing.T) {
	require.True(t, responsesWebSocketPayloadIsTerminal(`{"type":"response.completed"}`))
	require.True(t, responsesWebSocketPayloadIsCleanTerminal(`{"type":"response.done","response":{"status":"completed"}}`))
	require.True(t, responsesWebSocketPayloadIsCleanTerminal(`{"type":"response.done","response":{"status":"incomplete"}}`))
	require.False(t, responsesWebSocketPayloadIsCleanTerminal(`{"type":"response.done","response":{"status":"failed"}}`))
	require.False(t, responsesWebSocketPayloadIsCleanTerminal(`{"type":"response.done","response":{"status":"unknown"}}`))
	require.False(t, responsesWebSocketPayloadIsCleanTerminal(`{"type":"response.done"}`))
	require.True(t, responsesWebSocketPayloadIsCleanTerminal(`{"type": "response.incomplete"}`))
	require.False(t, responsesWebSocketPayloadIsTerminal(
		`{"type":"response.output_text.delta","delta":"quoted event: \"response.completed\""}`,
	))
	require.False(t, responsesWebSocketPayloadIsTerminal(
		`{"type":"response.output_item.done","response":{"type":"response.failed"}}`,
	))
	require.False(t, responsesWebSocketPayloadIsTerminal(`not-json "response.completed"`))
}

func TestResponsesWebSocketSSEWriterNormalizesResponseDone(t *testing.T) {
	tests := []struct {
		name          string
		payload       string
		wantType      string
		wantSucceeded bool
		wantPreserved string
	}{
		{
			name:          "completed",
			payload:       `{"type":"response.done","response":{"id":"resp_1","status":"completed","metadata":{"exact_integer":9007199254740993}}}`,
			wantType:      "response.completed",
			wantSucceeded: true,
			wantPreserved: `"exact_integer":9007199254740993`,
		},
		{
			name:          "incomplete",
			payload:       `{"type":"response.done","response":{"id":"resp_2","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"metadata":{"trace":"keep-incomplete"}}}`,
			wantType:      "response.incomplete",
			wantSucceeded: true,
			wantPreserved: `"trace":"keep-incomplete"`,
		},
		{
			name:          "failed",
			payload:       `{"type":"response.done","response":{"id":"resp_3","status":"failed","error":{"type":"server_error","code":"upstream_failure","message":"boom"},"metadata":{"trace":"keep-failed"}}}`,
			wantType:      "response.failed",
			wantSucceeded: false,
			wantPreserved: `"code":"upstream_failure"`,
		},
		{
			name:          "error overrides completed status",
			payload:       `{"type":"response.done","response":{"id":"resp_4","status":"completed","error":{"type":"server_error","message":"late failure"},"metadata":{"trace":"keep-error"}}}`,
			wantType:      "response.failed",
			wantSucceeded: false,
			wantPreserved: `"trace":"keep-error"`,
		},
		{
			name:          "unknown status fails closed",
			payload:       `{"type":"response.done","response":{"id":"resp_5","status":"mystery","metadata":{"trace":"keep-unknown"}}}`,
			wantType:      "response.failed",
			wantSucceeded: false,
			wantPreserved: `"trace":"keep-unknown"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			type serverResult struct {
				err       error
				succeeded bool
			}
			serverResultChannel := make(chan serverResult, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
				if err != nil {
					serverResultChannel <- serverResult{err: err}
					return
				}
				defer conn.Close()
				writer := newResponsesWebSocketSSEWriter(context.Background(), conn)
				_, err = writer.Write([]byte("data: " + test.payload + "\n\n"))
				if err == nil {
					err = writer.Finish()
				}
				serverResultChannel <- serverResult{err: err, succeeded: writer.SucceededTurn()}
			}))
			defer server.Close()

			url := "ws" + strings.TrimPrefix(server.URL, "http")
			conn, _, err := websocket.DefaultDialer.Dial(url, nil)
			require.NoError(t, err)
			defer conn.Close()

			_, frame, err := conn.ReadMessage()
			require.NoError(t, err)
			var event struct {
				Type string `json:"type"`
			}
			require.NoError(t, common.Unmarshal(frame, &event))
			assert.Equal(t, test.wantType, event.Type)
			assert.Contains(t, string(frame), test.wantPreserved)
			result := <-serverResultChannel
			require.NoError(t, result.err)
			assert.Equal(t, test.wantSucceeded, result.succeeded)
		})
	}
}

func TestResponsesWebSocketSSEWriterMapsEmptySuccessToBadGateway(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNoContent} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			serverErr := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
				if err != nil {
					serverErr <- err
					return
				}
				defer conn.Close()
				writer := newResponsesWebSocketSSEWriter(context.Background(), conn)
				writer.WriteHeader(status)
				serverErr <- writer.Finish()
			}))
			defer server.Close()

			url := "ws" + strings.TrimPrefix(server.URL, "http")
			conn, _, err := websocket.DefaultDialer.Dial(url, nil)
			require.NoError(t, err)
			defer conn.Close()
			_, createdFrame, err := conn.ReadMessage()
			require.NoError(t, err)
			require.JSONEq(t,
				`{"type":"response.created","response":{"id":"resp_gateway_failed","object":"response","status":"in_progress"}}`,
				string(createdFrame),
			)
			_, frame, err := conn.ReadMessage()
			require.NoError(t, err)
			require.JSONEq(t,
				`{"type":"response.failed","response":{"id":"resp_gateway_failed","object":"response","status":"failed","error":{"message":"upstream returned an empty response stream","type":"server_error","code":502}}}`,
				string(frame),
			)
			require.NoError(t, <-serverErr)
		})
	}
}

func TestResponsesWebSocketSSEWriterPreservesStructuredHTTPError(t *testing.T) {
	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		writer := newResponsesWebSocketSSEWriter(context.Background(), conn)
		writer.WriteHeader(http.StatusConflict)
		_, err = writer.Write([]byte(
			`{"error":{"message":"credential binding changed","type":"new_api_error","code":"channel:invalid_key"}}`,
		))
		if err != nil {
			serverErr <- err
			return
		}
		serverErr <- writer.Finish()
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	_, createdFrame, err := conn.ReadMessage()
	require.NoError(t, err)
	require.JSONEq(t,
		`{"type":"response.created","response":{"id":"resp_gateway_failed","object":"response","status":"in_progress"}}`,
		string(createdFrame),
	)
	_, frame, err := conn.ReadMessage()
	require.NoError(t, err)
	require.JSONEq(t,
		`{"type":"response.failed","response":{"id":"resp_gateway_failed","object":"response","status":"failed","error":{"message":"credential binding changed","type":"new_api_error","code":"channel:invalid_key"}}}`,
		string(frame),
	)
	require.NoError(t, <-serverErr)
}

func TestResponsesWebSocketSSEWriterMissingTerminalReusesObservedResponseID(t *testing.T) {
	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		writer := newResponsesWebSocketSSEWriter(context.Background(), conn)
		_, err = writer.Write([]byte(
			"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_partial\",\"status\":\"in_progress\"}}\n\n" +
				"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n",
		))
		if err != nil {
			serverErr <- err
			return
		}
		serverErr <- writer.Finish()
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	_, firstFrame, err := conn.ReadMessage()
	require.NoError(t, err)
	require.JSONEq(t,
		`{"type":"response.created","response":{"id":"resp_partial","status":"in_progress"}}`,
		string(firstFrame),
	)
	_, secondFrame, err := conn.ReadMessage()
	require.NoError(t, err)
	require.JSONEq(t,
		`{"type":"response.output_text.delta","delta":"hi"}`,
		string(secondFrame),
	)
	_, failureFrame, err := conn.ReadMessage()
	require.NoError(t, err)
	require.JSONEq(t,
		`{"type":"response.failed","response":{"id":"resp_partial","object":"response","status":"failed","error":{"message":"upstream stream ended before completion","type":"server_error","code":502}}}`,
		string(failureFrame),
	)
	require.NoError(t, <-serverErr)
}

func TestResponsesWebSocketReadPumpCancelsOnDisconnect(t *testing.T) {
	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		ctx, cancel := context.WithCancel(r.Context())
		frames := make(chan responsesWebSocketClientFrame, 1)
		go readResponsesWebSocketClientFrames(ctx, cancel, conn, frames)
		select {
		case <-ctx.Done():
			serverErr <- nil
		case <-time.After(2 * time.Second):
			serverErr <- context.DeadlineExceeded
		}
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	require.NoError(t, <-serverErr)
}

// A turn that chains onto a prior response's server-side state must be detected
// as stateful so the credential switch is disabled; a self-contained turn must
// not be, so it stays free to fail over.
func TestResponsesWebSocketFrameReferencesPriorResponse(t *testing.T) {
	cases := []struct {
		name  string
		frame map[string]any
		want  bool
	}{
		{"present", map[string]any{"previous_response_id": "resp_1"}, true},
		{"empty", map[string]any{"previous_response_id": ""}, false},
		{"whitespace", map[string]any{"previous_response_id": "   "}, false},
		{"absent", map[string]any{"model": "gpt-5.6-sol"}, false},
		{"non_string", map[string]any{"previous_response_id": 123}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, responsesWebSocketFrameReferencesPriorResponse(tc.frame))
		})
	}
}

func TestResponsesWebSocketContinuationRequiresLiveConnectionBeforeRelay(t *testing.T) {
	require.Nil(t, responsesWebSocketContinuationError("", nil))

	apiError := responsesWebSocketContinuationError("resp_1", &responsesws.Session{})
	require.NotNil(t, apiError)
	require.Equal(t, http.StatusBadRequest, apiError.StatusCode)
	require.Equal(t, types.ErrorCodeWebSocketConnectionLimitReached, apiError.GetErrorCode())
	require.True(t, types.IsSkipRetryError(apiError))
}

func TestResponsesWebSocketContinuationRequiresIDOwnedByLiveConnectionBeforeRelay(t *testing.T) {
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
			if err := conn.WriteMessage(
				websocket.TextMessage,
				[]byte(`{"type":"response.completed","response":{"id":"resp_owned"}}`),
			); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	session := &responsesws.Session{}
	defer session.Close()
	driver := &ordinaryResponsesSessionTestDriver{upstreamURL: server.URL}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
		ChannelId:         204,
		ChannelType:       constant.ChannelTypeOpenAI,
		ApiKey:            "standard-api-key",
		UpstreamModelName: "gpt-test",
	}}
	response, err := session.DoRequest(
		newResponsesWebSocketControllerTestContext(),
		driver,
		info,
		strings.NewReader(`{"model":"gpt-test","input":"first"}`),
	)
	require.NoError(t, err)
	_, err = io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())

	require.Nil(t, responsesWebSocketContinuationError("resp_owned", session))
	apiError := responsesWebSocketContinuationError("resp_unknown", session)
	require.NotNil(t, apiError)
	assert.Equal(t, types.ErrorCodeWebSocketConnectionLimitReached, apiError.GetErrorCode())
}

func newResponsesWebSocketControllerTestContext() *gin.Context {
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return context
}

func TestResponsesWebSocketTurnPinKeepsStatefulCredentialFixed(t *testing.T) {
	cases := []struct {
		name                    string
		channelID               int
		referencesPriorResponse bool
		existingSpecificChannel string
		wantSpecificChannel     bool
		wantSpecificValue       string
		wantInternalPin         bool
		wantRetryDisabled       bool
		wantContinuation        bool
	}{
		{"self-contained pinned turn", 42, false, "", true, "42", true, true, false},
		{"self-contained turn with explicit pin", 42, false, "99", true, "42", false, true, false},
		{"stateful pinned turn", 42, true, "", true, "42", false, true, true},
		{"stateful first turn", 0, true, "", false, "", false, true, true},
		{"self-contained first turn", 0, false, "", false, "", false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			turn, _ := gin.CreateTestContext(httptest.NewRecorder())
			if tc.existingSpecificChannel != "" {
				common.SetContextKey(turn, constant.ContextKeyTokenSpecificChannelId, tc.existingSpecificChannel)
			}
			applyResponsesWebSocketTurnPin(turn, tc.channelID, tc.referencesPriorResponse)

			specificChannel, exists := common.GetContextKey(turn, constant.ContextKeyTokenSpecificChannelId)
			require.Equal(t, tc.wantSpecificChannel, exists)
			if exists {
				require.Equal(t, tc.wantSpecificValue, specificChannel)
			}
			require.Equal(t, tc.wantInternalPin, turn.GetBool(responsesWebSocketInternalPinKey))
			require.Equal(t, tc.wantRetryDisabled, service.IsSubscriptionOAuthRetryDisabled(turn))
			require.Equal(t, tc.wantContinuation, responsesws.IsContinuationRequired(turn))
		})
	}
}

// An upstream that streams data without an SSE frame boundary must not grow the
// pending buffer without bound: past the cap Write returns an error and stops.
func TestResponsesWebSocketSSEWriterBoundsPendingBuffer(t *testing.T) {
	w := newResponsesWebSocketSSEWriter(context.Background(), nil)
	w.pending = []byte(strings.Repeat("a", maxResponsesWebSocketPendingBytes))
	pendingBefore := len(w.pending)
	_, writeErr := w.Write([]byte("b"))
	require.Error(t, writeErr)
	require.Contains(t, writeErr.Error(), "buffer limit")
	require.Len(t, w.pending, pendingBefore)
}

func TestResponsesWebSocketSSEBoundarySupportsLFAndCRLFWithoutRewriting(t *testing.T) {
	tests := []struct {
		name      string
		payload   string
		wantIndex int
		wantWidth int
	}{
		{name: "lf", payload: "data: one\n\ndata: two", wantIndex: len("data: one"), wantWidth: 2},
		{name: "crlf", payload: "data: one\r\n\r\ndata: two", wantIndex: len("data: one"), wantWidth: 4},
		{name: "earliest", payload: "a\r\n\r\nb\n\n", wantIndex: 1, wantWidth: 4},
		{name: "incomplete", payload: "data: one\r\n", wantIndex: -1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			index, width := responsesWebSocketSSEBoundary([]byte(test.payload))
			assert.Equal(t, test.wantIndex, index)
			assert.Equal(t, test.wantWidth, width)
		})
	}
}

// A self-contained turn that changes model releases the internal channel pin and
// rebinds; a continuation or an unbound session never triggers the rebind.
func TestShouldRebindResponsesWebSocketModel(t *testing.T) {
	tests := []struct {
		name            string
		pinnedModel     string
		requestModel    string
		referencesPrior bool
		want            bool
	}{
		{"self-contained switch", "gpt-a", "gpt-b", false, true},
		{"same model", "gpt-a", "gpt-a", false, false},
		{"case-insensitive same model", "gpt-a", "GPT-A", false, false},
		{"continuation never rebinds", "gpt-a", "gpt-b", true, false},
		{"no prior binding", "", "gpt-b", false, false},
		{"missing request model", "gpt-a", "", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, shouldRebindResponsesWebSocketModel(tc.pinnedModel, tc.requestModel, tc.referencesPrior))
		})
	}
}

// The downstream keepalive pump must ping an idle client so intermediaries see
// traffic between turns, and a failed ping must cancel the connection context so
// a dead client releases the session's resources.
func TestResponsesWebSocketKeepalive(t *testing.T) {
	const testKeepaliveInterval = 20 * time.Millisecond

	t.Run("idle client receives pings", func(t *testing.T) {
		pings := make(chan struct{}, 4)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go runResponsesWebSocketKeepalive(ctx, cancel, conn, testKeepaliveInterval)
			// Keep the server-side reader alive for the duration of the test.
			_, _, _ = conn.ReadMessage()
		}))
		defer server.Close()

		url := "ws" + strings.TrimPrefix(server.URL, "http")
		conn, _, err := websocket.DefaultDialer.Dial(url, nil)
		require.NoError(t, err)
		defer conn.Close()
		conn.SetPingHandler(func(string) error {
			select {
			case pings <- struct{}{}:
			default:
			}
			return nil
		})
		// The client must be reading for its ping handler to run.
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()

		select {
		case <-pings:
		case <-time.After(2 * time.Second):
			t.Fatal("idle client received no keepalive ping")
		}
	})

	t.Run("failed ping cancels the connection", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// Close the raw connection under the pump: the next ping write fails and
			// must cancel the context.
			_ = conn.Close()
			go runResponsesWebSocketKeepalive(ctx, cancel, conn, testKeepaliveInterval)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
				t.Error("failed keepalive ping did not cancel the connection context")
			}
		}))
		defer server.Close()

		url := "ws" + strings.TrimPrefix(server.URL, "http")
		conn, _, err := websocket.DefaultDialer.Dial(url, nil)
		require.NoError(t, err)
		defer conn.Close()
		// Wait for the server handler to finish its assertions.
		_, _, _ = conn.ReadMessage()
	})
}
