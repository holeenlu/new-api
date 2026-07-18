package controller

import (
	"bytes"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/relay/channel/codex"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const maxResponsesWebSocketErrorBytes = 1 << 20

var responsesWebSocketUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(_ *http.Request) bool {
		return true
	},
}

// CodexResponsesWebSocket provides the Responses WebSocket client protocol.
// Each response frame runs through the normal HTTP Responses relay so
// authentication, routing, billing, privacy filtering, and OAuth safety
// policies remain identical to POST /v1/responses. The selected channel is
// pinned for the lifetime of the downstream connection.
func CodexResponsesWebSocket(c *gin.Context) {
	upgradeHeaders := http.Header{}
	if turnState := strings.TrimSpace(c.GetHeader("x-codex-turn-state")); turnState != "" {
		upgradeHeaders.Set("x-codex-turn-state", turnState)
	}
	conn, err := responsesWebSocketUpgrader.Upgrade(c.Writer, c.Request, upgradeHeaders)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(common.MaxRequestBodyBytes())
	if strings.TrimSpace(c.GetHeader("Session-Id")) == "" {
		sessionID := uuid.NewString()
		c.Request.Header.Set("Session-Id", sessionID)
		c.Request.Header.Set("Thread-Id", sessionID)
		c.Request.Header.Set("X-Codex-Window-Id", sessionID+":0")
	}
	session := &codex.ResponsesWebSocketSession{}
	defer session.Close()
	pinnedChannelID := 0
	var previousRequest map[string]any

	engine := gin.New()
	engine.Use(middleware.BodyStorageCleanup())
	engine.Use(middleware.RouteTag("relay"))
	engine.Use(middleware.SystemPerformanceCheck())
	engine.Use(middleware.TokenAuth())
	engine.Use(func(turn *gin.Context) {
		if pinnedChannelID > 0 {
			common.SetContextKey(turn, constant.ContextKeyTokenSpecificChannelId, strconv.Itoa(pinnedChannelID))
			service.DisableSubscriptionOAuthRetry(turn)
		}
		turn.Next()
	})
	engine.Use(middleware.ModelRequestRateLimit())
	engine.Use(middleware.Distribute())
	engine.POST("/v1/responses", func(turn *gin.Context) {
		if common.GetContextKeyInt(turn, constant.ContextKeyChannelType) != constant.ChannelTypeCodex {
			turn.JSON(http.StatusBadRequest, gin.H{"error": types.NewError(errors.New("Responses WebSocket requires a ChatGPT Subscription (Codex) channel"), types.ErrorCodeInvalidRequest).ToOpenAIError()})
			return
		}
		codex.SetResponsesWebSocketSession(turn, session)
		Relay(turn, types.RelayFormatOpenAIResponses)
		if turn.GetBool("relay_affinity_success") {
			session.ConfirmHTTPFallbackSuccess()
		}
		if connectedChannelID := session.ChannelID(); connectedChannelID > 0 {
			pinnedChannelID = connectedChannelID
		}
	})

	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
			continue
		}

		var frame map[string]any
		if err := common.Unmarshal(payload, &frame); err != nil {
			writeResponsesWebSocketError(conn, http.StatusBadRequest, "invalid response.create JSON")
			continue
		}
		frame, err = normalizeResponsesWebSocketFrame(frame, previousRequest)
		if err != nil {
			writeResponsesWebSocketError(conn, http.StatusBadRequest, err.Error())
			continue
		}
		body, err := common.Marshal(frame)
		if err != nil {
			writeResponsesWebSocketError(conn, http.StatusBadRequest, err.Error())
			continue
		}

		request, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, "/v1/responses", bytes.NewReader(body))
		if err != nil {
			writeResponsesWebSocketError(conn, http.StatusInternalServerError, err.Error())
			return
		}
		request.Header = c.Request.Header.Clone()
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "text/event-stream")
		request.RemoteAddr = c.Request.RemoteAddr

		writer := newResponsesWebSocketSSEWriter(conn)
		engine.ServeHTTP(writer, request)
		if err := writer.Finish(); err != nil {
			return
		}
		previousRequest = reusableResponsesWebSocketFields(frame)
	}
}

func reusableResponsesWebSocketFields(frame map[string]any) map[string]any {
	if len(frame) == 0 {
		return nil
	}
	defaults := make(map[string]any, len(frame))
	for key, value := range frame {
		switch key {
		case "input", "previous_response_id", "stream", "generate":
			continue
		}
		defaults[key] = value
	}
	if len(defaults) == 0 {
		return nil
	}
	return defaults
}

func normalizeResponsesWebSocketFrame(frame map[string]any, previous map[string]any) (map[string]any, error) {
	if frame == nil {
		return nil, errors.New("websocket request must be a JSON object")
	}
	requestType, _ := frame["type"].(string)
	requestType = strings.TrimSpace(requestType)
	if requestType != "response.create" && requestType != "response.append" {
		return nil, errors.New("unsupported websocket request type: " + requestType)
	}
	if requestType == "response.append" && previous == nil {
		return nil, errors.New("response.append received before response.create")
	}

	delete(frame, "type")
	if previous != nil {
		for key, value := range previous {
			switch key {
			case "input", "previous_response_id", "stream", "generate":
				continue
			}
			if _, exists := frame[key]; !exists {
				frame[key] = value
			}
		}
	}
	if strings.TrimSpace(stringValue(frame["model"])) == "" {
		return nil, errors.New("model is required in the first response.create request")
	}
	if input, exists := frame["input"]; exists {
		if requestType == "response.append" {
			if _, ok := input.([]any); !ok {
				return nil, errors.New("response.append requires array field: input")
			}
		}
	} else if requestType == "response.append" {
		return nil, errors.New("response.append requires array field: input")
	} else {
		frame["input"] = []any{}
	}
	frame["stream"] = true
	return frame, nil
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

type responsesWebSocketSSEWriter struct {
	mu      sync.Mutex
	conn    *websocket.Conn
	header  http.Header
	status  int
	pending []byte
	raw     []byte
	sent    int
	err     error
}

func newResponsesWebSocketSSEWriter(conn *websocket.Conn) *responsesWebSocketSSEWriter {
	return &responsesWebSocketSSEWriter{conn: conn, header: make(http.Header)}
}

func (w *responsesWebSocketSSEWriter) Header() http.Header {
	return w.header
}

func (w *responsesWebSocketSSEWriter) WriteHeader(statusCode int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = statusCode
	}
}

func (w *responsesWebSocketSSEWriter) Write(payload []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if len(w.raw) < maxResponsesWebSocketErrorBytes {
		remaining := maxResponsesWebSocketErrorBytes - len(w.raw)
		w.raw = append(w.raw, payload[:min(len(payload), remaining)]...)
	}
	w.pending = append(w.pending, payload...)
	w.drainSSEFrames(false)
	if w.err != nil {
		return 0, w.err
	}
	return len(payload), nil
}

func (w *responsesWebSocketSSEWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.drainSSEFrames(false)
}

func (w *responsesWebSocketSSEWriter) Finish() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.drainSSEFrames(true)
	if w.err != nil {
		return w.err
	}
	if w.sent > 0 {
		return nil
	}
	if w.status == 0 {
		w.status = http.StatusInternalServerError
	}
	message := strings.TrimSpace(string(w.raw))
	if message == "" {
		message = http.StatusText(w.status)
	}
	return writeResponsesWebSocketError(w.conn, w.status, message)
}

func (w *responsesWebSocketSSEWriter) drainSSEFrames(flushRemainder bool) {
	for {
		w.pending = bytes.ReplaceAll(w.pending, []byte("\r\n"), []byte("\n"))
		index := bytes.Index(w.pending, []byte("\n\n"))
		if index < 0 {
			if flushRemainder && len(bytes.TrimSpace(w.pending)) > 0 {
				w.writeSSEBlock(w.pending)
				w.pending = nil
			}
			return
		}
		block := append([]byte(nil), w.pending[:index]...)
		w.pending = w.pending[index+2:]
		w.writeSSEBlock(block)
		if w.err != nil {
			return
		}
	}
}

func (w *responsesWebSocketSSEWriter) writeSSEBlock(block []byte) {
	var dataLines []string
	for _, line := range strings.Split(string(block), "\n") {
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if len(dataLines) == 0 {
		return
	}
	payload := strings.Join(dataLines, "\n")
	if payload == "" || payload == "[DONE]" {
		return
	}
	w.err = w.conn.WriteMessage(websocket.TextMessage, []byte(payload))
	if w.err == nil {
		w.sent++
	}
}

func writeResponsesWebSocketError(conn *websocket.Conn, status int, message string) error {
	if conn == nil {
		return errors.New("responses websocket connection is nil")
	}
	payload, err := common.Marshal(gin.H{
		"type": "error",
		"error": gin.H{
			"message": message,
			"type":    "invalid_request_error",
			"code":    status,
		},
	})
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, payload)
}
