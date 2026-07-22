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
	"github.com/QuantumNous/new-api/relay/responsesws"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const maxResponsesWebSocketErrorBytes = 1 << 20

// maxResponsesWebSocketPendingBytes bounds the unframed SSE bytes buffered while
// waiting for an event boundary ("\n\n"). A well-behaved upstream (events framed
// by the relay) never approaches this; the cap stops a misbehaving one from
// growing the buffer without limit.
const maxResponsesWebSocketPendingBytes = 16 << 20

const responsesWebSocketInternalPinKey = "responses_websocket_internal_pin"

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
	session := &responsesws.Session{}
	defer session.Close()
	pinnedChannelID := 0
	// turnReferencesPriorResponse marks a turn that chains onto a prior response's
	// server-side state (previous_response_id). Such a turn must stay on the account
	// that produced that response; switching credentials would make the reference
	// unresolvable.
	turnReferencesPriorResponse := false
	var previousRequest map[string]any

	engine := gin.New()
	// This chain mirrors the /v1 relay group in router/relay-router.go
	// (RouteTag → SystemPerformanceCheck → TokenAuth → ModelRequestRateLimit →
	// Distribute) so a WebSocket response turn is authenticated, routed, and
	// rate-limited identically to POST /v1/responses. BodyStorageCleanup is added
	// explicitly because this is a fresh sub-engine rather than the global router.
	// Keep it in sync when the relay middleware chain changes.
	engine.Use(middleware.BodyStorageCleanup())
	engine.Use(middleware.RouteTag("relay"))
	engine.Use(middleware.SystemPerformanceCheck())
	engine.Use(middleware.TokenAuth())
	engine.Use(func(turn *gin.Context) {
		if pinnedChannelID > 0 {
			common.SetContextKey(turn, constant.ContextKeyTokenSpecificChannelId, strconv.Itoa(pinnedChannelID))
			turn.Set(responsesWebSocketInternalPinKey, true)
			service.DisableSubscriptionOAuthRetry(turn)
		} else if turnReferencesPriorResponse {
			// Not yet pinned, but this turn depends on a prior response's server-side
			// state owned by the currently selected account. Disable the credential
			// switch so a failure is surfaced (the client can resubmit with full
			// context) rather than retried on an account that cannot resolve the
			// reference.
			service.DisableSubscriptionOAuthRetry(turn)
		}
		turn.Next()
	})
	engine.Use(middleware.ModelRequestRateLimit())
	engine.Use(middleware.Distribute())
	engine.POST("/v1/responses", func(turn *gin.Context) {
		// Bind the session for every turn; each adaptor decides whether to use the
		// upstream WebSocket (Codex always, other channels when
		// ResponsesWebSocketEnabled is set) or serve the turn over HTTP.
		responsesws.SetSession(turn, session)
		Relay(turn, types.RelayFormatOpenAIResponses)
		if turn.GetBool("relay_affinity_success") {
			session.ConfirmHTTPFallbackSuccess()
		}
		pinnedChannelID = session.ChannelID()
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
			writeResponsesWebSocketError(conn, http.StatusBadRequest, "invalid response.create JSON", "")
			continue
		}
		frame, err = normalizeResponsesWebSocketFrame(frame, previousRequest)
		if err != nil {
			writeResponsesWebSocketError(conn, http.StatusBadRequest, err.Error(), "")
			continue
		}
		turnReferencesPriorResponse = responsesWebSocketFrameReferencesPriorResponse(frame)
		body, err := common.Marshal(frame)
		if err != nil {
			writeResponsesWebSocketError(conn, http.StatusBadRequest, err.Error(), "")
			continue
		}

		request, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, "/v1/responses", bytes.NewReader(body))
		if err != nil {
			writeResponsesWebSocketError(conn, http.StatusInternalServerError, err.Error(), "")
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
		if writer.SucceededTurn() {
			// Only a turn that reached a clean terminal (response.completed /
			// response.incomplete) seeds the defaults a later response.append
			// inherits. A failed response.create must not make a following append
			// look valid or carry its parameters forward.
			previousRequest = reusableResponsesWebSocketFields(frame)
		}
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

// responsesWebSocketFrameReferencesPriorResponse reports whether the turn chains
// onto a prior response's server-side state via a non-empty previous_response_id.
// That state is owned by the account that produced it, so such a turn must not
// switch credentials.
func responsesWebSocketFrameReferencesPriorResponse(frame map[string]any) bool {
	return strings.TrimSpace(stringValue(frame["previous_response_id"])) != ""
}

type responsesWebSocketSSEWriter struct {
	mu               sync.Mutex
	conn             *websocket.Conn
	header           http.Header
	status           int
	pending          []byte
	raw              []byte
	sent             int
	sawTerminal      bool
	sawCleanTerminal bool
	err              error
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
	if len(w.pending) > maxResponsesWebSocketPendingBytes {
		// The upstream is streaming an event with no SSE boundary; refuse to buffer
		// it without bound so a misbehaving upstream cannot exhaust memory.
		w.err = errors.New("responses websocket upstream event exceeded the buffer limit without a frame boundary")
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
		if !w.sawTerminal {
			// The relay streamed partial output then ended without a terminal event
			// (e.g. the upstream dropped mid-stream). Emit a synthetic error frame so
			// a strict Responses-WebSocket client does not hang awaiting
			// response.completed.
			return writeResponsesWebSocketError(w.conn, http.StatusBadGateway, "upstream stream ended before completion", "")
		}
		return nil
	}
	if w.status == 0 {
		w.status = http.StatusInternalServerError
	}
	message := strings.TrimSpace(string(w.raw))
	if message == "" {
		message = http.StatusText(w.status)
	}
	// Carry the retry timing the relay resolved (Retry-After was written to this
	// writer's header map) so the client learns when the credential recovers.
	return writeResponsesWebSocketError(w.conn, w.status, message, w.header.Get("Retry-After"))
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
	if responsesWebSocketPayloadIsTerminal(payload) {
		w.sawTerminal = true
	}
	if responsesWebSocketPayloadIsCleanTerminal(payload) {
		w.sawCleanTerminal = true
	}
	w.err = w.conn.WriteMessage(websocket.TextMessage, []byte(payload))
	if w.err == nil {
		w.sent++
	}
}

// SucceededTurn reports whether the turn reached a clean terminal event
// (response.completed / response.incomplete) rather than a failure or a stream
// that ended before completion. Callers use it to decide whether the turn may
// seed state for a following response.append.
func (w *responsesWebSocketSSEWriter) SucceededTurn() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sawCleanTerminal
}

// responsesWebSocketPayloadIsTerminal reports whether a forwarded event payload
// is a stream-ending Responses event. It matches the quoted event-type value, so
// a real terminal frame is never missed (its type is always present verbatim);
// an unrelated payload that merely mentions one of these strings only risks a
// false positive, which just suppresses the synthetic terminal — never a spurious
// error after a clean stream.
func responsesWebSocketPayloadIsTerminal(payload string) bool {
	return strings.Contains(payload, `"response.completed"`) ||
		strings.Contains(payload, `"response.failed"`) ||
		strings.Contains(payload, `"response.incomplete"`) ||
		strings.Contains(payload, `"response.error"`) ||
		strings.Contains(payload, `"type":"error"`)
}

// responsesWebSocketPayloadIsCleanTerminal reports whether a forwarded payload is
// a non-error stream-ending event. It is the success subset of
// responsesWebSocketPayloadIsTerminal and drives response.append inheritance.
func responsesWebSocketPayloadIsCleanTerminal(payload string) bool {
	return strings.Contains(payload, `"response.completed"`) ||
		strings.Contains(payload, `"response.incomplete"`)
}

// responsesWebSocketErrorType maps an HTTP status to a Responses error type so a
// client can distinguish retry-worthy conditions (rate limit, server error) from
// permanent ones (auth, invalid request) instead of seeing every failure as an
// invalid_request_error.
func responsesWebSocketErrorType(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status >= 500:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}

// writeResponsesWebSocketError sends a structured error frame. status drives the
// error type; retryAfter, when a positive delta-seconds value, is preserved as a
// numeric retry_after so a strict client can wait rather than hammer.
func writeResponsesWebSocketError(conn *websocket.Conn, status int, message string, retryAfter string) error {
	if conn == nil {
		return errors.New("responses websocket connection is nil")
	}
	errorObject := gin.H{
		"message": message,
		"type":    responsesWebSocketErrorType(status),
		"code":    status,
	}
	if seconds, convErr := strconv.Atoi(strings.TrimSpace(retryAfter)); convErr == nil && seconds > 0 {
		errorObject["retry_after"] = seconds
	}
	payload, err := common.Marshal(gin.H{
		"type":  "error",
		"error": errorObject,
	})
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, payload)
}
