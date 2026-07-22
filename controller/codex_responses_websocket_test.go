package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
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

		writer := newResponsesWebSocketSSEWriter(conn)
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
		writer := newResponsesWebSocketSSEWriter(conn)
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

	_, frame, err := conn.ReadMessage()
	require.NoError(t, err)
	require.JSONEq(t,
		`{"type":"error","error":{"message":"Too Many Requests","type":"rate_limit_error","code":429,"retry_after":30}}`,
		string(frame),
	)
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
				writer := newResponsesWebSocketSSEWriter(conn)
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

// An upstream that streams data without an SSE frame boundary must not grow the
// pending buffer without bound: past the cap Write returns an error and stops.
func TestResponsesWebSocketSSEWriterBoundsPendingBuffer(t *testing.T) {
	w := newResponsesWebSocketSSEWriter(nil)
	chunk := []byte(strings.Repeat("a", 1<<20)) // 1 MiB, no "\n\n"
	var writeErr error
	for i := 0; i < 32 && writeErr == nil; i++ {
		_, writeErr = w.Write(chunk)
	}
	require.Error(t, writeErr)
	require.Contains(t, writeErr.Error(), "buffer limit")
}
