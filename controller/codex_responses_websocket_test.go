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
