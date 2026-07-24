package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/responsesws"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const maxResponsesWebSocketErrorBytes = 1 << 20

// maxResponsesWebSocketPendingBytes bounds the unframed SSE bytes buffered while
// waiting for an event boundary ("\n\n"). The shared upstream transport permits
// a 16 MiB JSON message, so leave room for its SSE event/data framing instead of
// rejecting an otherwise valid maximum-size event at the downstream bridge.
const maxResponsesWebSocketPendingBytes = (16 << 20) + (64 << 10)

const (
	maxResponsesWebSocketQueuedFrames = 1
	responsesWebSocketWriteTimeout    = 30 * time.Second
	// responsesWebSocketControlWriteTimeout bounds a keepalive control-frame
	// write, mirroring the 1s bound gorilla uses for control frames so a slow
	// client cannot stall the keepalive pump.
	responsesWebSocketControlWriteTimeout = time.Second
	// responsesWebSocketKeepaliveInterval paces server→client pings so
	// intermediaries (reverse proxies, NAT gateways — commonly 60s idle limits)
	// keep the idle downstream connection open between turns, and a dead client
	// is detected instead of holding session resources. Kept below the common
	// 60s intermediary timeout with margin; clients answer pings in the protocol
	// stack automatically, so no client change is needed.
	responsesWebSocketKeepaliveInterval = 25 * time.Second
)

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
	connectionContext, cancelConnection := context.WithCancel(c.Request.Context())
	defer cancelConnection()
	clientFrames := make(chan responsesWebSocketClientFrame, maxResponsesWebSocketQueuedFrames)
	go readResponsesWebSocketClientFrames(connectionContext, cancelConnection, conn, clientFrames)
	go runResponsesWebSocketKeepalive(connectionContext, cancelConnection, conn, responsesWebSocketKeepaliveInterval)
	pinnedChannelID := 0
	// pinnedModel is the client-level model bound to the session's current
	// channel affinity. A self-contained turn that requests a different model
	// releases the internal pin and the session binding BEFORE distribution, so
	// the new model re-enters ordinary channel selection instead of being routed
	// to (and rejected by) the old model's channel.
	pinnedModel := ""
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
		applyResponsesWebSocketTurnPin(turn, pinnedChannelID, turnReferencesPriorResponse)
		turn.Next()
	})
	engine.Use(middleware.ModelRequestRateLimit())
	engine.Use(middleware.Distribute())
	bindingFailure := false
	engine.POST("/v1/responses", func(turn *gin.Context) {
		if apiError := restoreResponsesWebSocketCredentialBinding(turn, session); apiError != nil {
			bindingFailure = true
			status := apiError.StatusCode
			if status < http.StatusBadRequest {
				status = http.StatusInternalServerError
			}
			turn.AbortWithStatusJSON(status, gin.H{"error": apiError.ToOpenAIError()})
			return
		}
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
		var clientFrame responsesWebSocketClientFrame
		select {
		case <-connectionContext.Done():
			return
		case frame, ok := <-clientFrames:
			if !ok || connectionContext.Err() != nil {
				return
			}
			clientFrame = frame
		}

		var frame map[string]any
		if err := common.UnmarshalWithNumber(clientFrame.payload, &frame); err != nil {
			if writeResponsesWebSocketError(conn, http.StatusBadRequest, "invalid response.create JSON", "") != nil {
				return
			}
			continue
		}
		frame, previousRequest, err = beginResponsesWebSocketTurn(frame, previousRequest)
		if err != nil {
			if writeResponsesWebSocketError(conn, http.StatusBadRequest, err.Error(), "") != nil {
				return
			}
			continue
		}
		previousResponseID := strings.TrimSpace(stringValue(frame["previous_response_id"]))
		turnReferencesPriorResponse = previousResponseID != ""
		if apiError := responsesWebSocketContinuationError(previousResponseID, session); apiError != nil {
			if writeResponsesWebSocketErrorWithCode(
				conn,
				apiError.StatusCode,
				apiError.ToOpenAIError().Message,
				apiError.GetErrorCode(),
				"",
			) != nil {
				return
			}
			continue
		}
		requestModel := strings.TrimSpace(stringValue(frame["model"]))
		if shouldRebindResponsesWebSocketModel(pinnedModel, requestModel, turnReferencesPriorResponse) {
			// Self-contained model switch: release the controller-owned pin and the
			// session's connection/channel/response-id binding before distribution.
			// An externally requested specific-channel pin is re-installed by
			// TokenAuth on every turn and is deliberately not touched here.
			pinnedChannelID = 0
			pinnedModel = ""
			session.ResetChannelForRetry()
		}
		body, err := common.Marshal(frame)
		if err != nil {
			if writeResponsesWebSocketError(conn, http.StatusBadRequest, err.Error(), "") != nil {
				return
			}
			continue
		}

		request, err := http.NewRequestWithContext(connectionContext, http.MethodPost, "/v1/responses", bytes.NewReader(body))
		if err != nil {
			_ = writeResponsesWebSocketError(conn, http.StatusInternalServerError, err.Error(), "")
			return
		}
		request.Header = c.Request.Header.Clone()
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "text/event-stream")
		request.RemoteAddr = c.Request.RemoteAddr

		writer := newResponsesWebSocketSSEWriter(request.Context(), conn)
		bindingFailure = false
		engine.ServeHTTP(writer, request)
		if pinnedChannelID != 0 {
			pinnedModel = requestModel
		} else {
			pinnedModel = ""
		}
		if err := writer.Finish(); err != nil {
			return
		}
		if bindingFailure {
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

// runResponsesWebSocketKeepalive is the downstream connection's keepalive pump.
// Between turns the client WebSocket carries no traffic, so an intermediary idle
// timeout can silently kill it; the client's next message is then written into a
// dead socket and hangs until a transport-level timeout. Periodic server→client
// pings keep the connection visibly active (clients answer pongs in the protocol
// stack). WriteControl is documented safe to call concurrently with other
// connection writes, so this needs no coordination with the SSE writer. A failed
// ping means the client is gone: cancel the connection so the reader and any
// in-flight turn are released promptly.
func runResponsesWebSocketKeepalive(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			deadline := time.Now().Add(responsesWebSocketControlWriteTimeout)
			if err := conn.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
				cancel()
				return
			}
		}
	}
}

// responsesWebSocketContinuationError enforces the controller-owned admission
// boundary before authentication, channel distribution, parameter overrides or
// billing can run for a connection-local continuation.
func responsesWebSocketContinuationError(previousResponseID string, session *responsesws.Session) *types.NewAPIError {
	if strings.TrimSpace(previousResponseID) == "" {
		return nil
	}
	if session != nil && session.CanContinue(previousResponseID) {
		return nil
	}
	return responsesws.NewContinuationUnavailableError()
}

// shouldRebindResponsesWebSocketModel reports whether a turn is a self-contained
// model switch: the session is bound to one model, the new turn requests a
// different one, and it carries no previous_response_id. The official protocol
// scopes a connection to an account rather than a model, so such a turn rebinds
// (new channel selection, new upstream connection) instead of failing with 409.
// A continuation never rebinds: its previous_response_id is owned by the old
// model's connection and stays fail-closed.
func shouldRebindResponsesWebSocketModel(pinnedModel, requestModel string, referencesPriorResponse bool) bool {
	return pinnedModel != "" && requestModel != "" &&
		!strings.EqualFold(requestModel, pinnedModel) && !referencesPriorResponse
}

func applyResponsesWebSocketTurnPin(turn *gin.Context, pinnedChannelID int, referencesPriorResponse bool) {
	if turn == nil {
		return
	}
	if referencesPriorResponse {
		responsesws.MarkContinuationRequired(turn)
	}
	if pinnedChannelID > 0 {
		_, hadExternalSpecificChannel := common.GetContextKey(turn, constant.ContextKeyTokenSpecificChannelId)
		common.SetContextKey(turn, constant.ContextKeyTokenSpecificChannelId, strconv.Itoa(pinnedChannelID))
		if !referencesPriorResponse && !hadExternalSpecificChannel {
			// A self-contained turn may move to another credential after a provably
			// replay-safe failure. Mark only a pin introduced here as internal so Relay
			// may override it. A specific-channel pin already installed by TokenAuth is
			// an explicit caller constraint: keep its retry policy even though the value
			// is normalized to the credential already bound to this session. Stateful
			// continuations likewise keep retry-disabled specific-channel semantics.
			turn.Set(responsesWebSocketInternalPinKey, true)
		}
		service.DisableSubscriptionOAuthRetry(turn)
		return
	}
	if referencesPriorResponse {
		// This turn depends on server-side state owned by the selected account.
		// Disable credential switching even before the session has established its
		// first reusable upstream connection.
		service.DisableSubscriptionOAuthRetry(turn)
	}
}

type responsesWebSocketClientFrame struct {
	payload []byte
}

// readResponsesWebSocketClientFrames is the sole downstream reader. Keeping it
// alive while a turn is running lets a disconnect cancel the turn's upstream
// request immediately. A small queue permits normal pipelining without allowing
// an unbounded number of response.create bodies to accumulate in memory.
func readResponsesWebSocketClientFrames(
	ctx context.Context,
	cancel context.CancelFunc,
	conn *websocket.Conn,
	frames chan<- responsesWebSocketClientFrame,
) {
	defer close(frames)
	defer cancel()
	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
			continue
		}
		select {
		case frames <- responsesWebSocketClientFrame{payload: payload}:
		case <-ctx.Done():
			return
		default:
			// The protocol processes turns serially. More than the bounded queue is a
			// protocol abuse condition; cancel the active turn instead of stopping the
			// read pump and losing disconnect detection.
			return
		}
	}
}

// restoreResponsesWebSocketCredentialBinding replaces the key chosen by
// Distribute with the exact credential that authenticated the reusable upstream
// connection. Pinning only the channel is insufficient for multi-key channels.
func restoreResponsesWebSocketCredentialBinding(turn *gin.Context, session *responsesws.Session) *types.NewAPIError {
	binding, ok := session.Binding()
	if !ok {
		return nil
	}
	channel, err := model.CacheGetChannel(binding.ChannelID)
	if err != nil {
		return types.NewErrorWithStatusCode(
			fmt.Errorf("get responses websocket channel %d: %w", binding.ChannelID, err),
			types.ErrorCodeGetChannelFailed,
			http.StatusServiceUnavailable,
			types.ErrOptionWithSkipRetry(),
		)
	}
	matched, apiError := middleware.SetupContextForSelectedChannelAtCredential(
		turn,
		channel,
		turn.GetString("original_model"),
		binding.KeyIndex,
		binding.Fingerprint,
	)
	if apiError != nil {
		return apiError
	}
	if matched {
		return nil
	}
	return types.NewErrorWithStatusCode(
		errors.New("responses websocket credential binding changed; reconnect to establish a new session"),
		types.ErrorCodeChannelInvalidKey,
		http.StatusConflict,
		types.ErrOptionWithSkipRetry(),
	)
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

// beginResponsesWebSocketTurn applies the defaults state transition that occurs
// before a turn is sent upstream. An explicit response.create starts a new
// sequence, so the last successful create/append can no longer authorize or
// supply defaults to a later response.append unless this new turn itself reaches
// a clean terminal and the caller stores new defaults.
func beginResponsesWebSocketTurn(frame, previous map[string]any) (map[string]any, map[string]any, error) {
	requestType, _ := frame["type"].(string)
	if strings.TrimSpace(requestType) == "response.create" {
		previous = nil
	}
	normalized, err := normalizeResponsesWebSocketFrame(frame, previous)
	return normalized, previous, err
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
	ctx              context.Context
	conn             *websocket.Conn
	header           http.Header
	status           int
	pending          []byte
	raw              []byte
	sent             int
	responseID       string
	sawTerminal      bool
	sawCleanTerminal bool
	err              error
}

func newResponsesWebSocketSSEWriter(ctx context.Context, conn *websocket.Conn) *responsesWebSocketSSEWriter {
	if ctx == nil {
		ctx = context.Background()
	}
	return &responsesWebSocketSSEWriter{ctx: ctx, conn: conn, header: make(http.Header)}
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
	if len(payload) > maxResponsesWebSocketPendingBytes-len(w.pending) {
		// Check before append so an oversized, boundary-free upstream event cannot
		// force the allocation that this limit is intended to prevent.
		w.err = errors.New("responses websocket upstream event exceeded the buffer limit without a frame boundary")
		return 0, w.err
	}
	w.pending = append(w.pending, payload...)
	w.drainSSEFrames(false)
	if w.err != nil {
		return 0, w.err
	}
	return len(payload), nil
}

// SetWriteDeadline lets http.ResponseController propagate the relay's streaming
// write deadline through Gin to the underlying WebSocket connection.
func (w *responsesWebSocketSSEWriter) SetWriteDeadline(deadline time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conn == nil {
		return errors.New("responses websocket connection is nil")
	}
	return w.conn.SetWriteDeadline(deadline)
}

func (w *responsesWebSocketSSEWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.drainSSEFrames(false)
}

func (w *responsesWebSocketSSEWriter) Finish() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ctx != nil && w.ctx.Err() != nil {
		// The downstream connection is already gone. A locally successful socket
		// write here would not prove that the client received the synthetic terminal,
		// and logging it as sent obscures the cancellation that actually ended the
		// turn.
		return w.ctx.Err()
	}
	w.drainSSEFrames(true)
	if w.err != nil {
		return w.err
	}
	if w.sent > 0 {
		if !w.sawTerminal {
			// The relay streamed partial output then ended without a terminal event
			// (e.g. the upstream dropped mid-stream). Emit a synthetic response.failed
			// terminal so a strict Responses-WebSocket client resolves the in-flight
			// turn instead of hanging on "thinking".
			writeErr := writeResponsesWebSocketFailureTerminal(w.conn, http.StatusBadGateway, gin.H{
				"message": "upstream stream ended before completion",
			}, "", w.responseID, false)
			w.logErrorFrameResult("missing_terminal", http.StatusBadGateway, http.StatusBadGateway, writeErr)
			return writeErr
		}
		return nil
	}
	if w.status == 0 {
		w.status = http.StatusInternalServerError
	}
	message := strings.TrimSpace(string(w.raw))
	if w.status >= http.StatusOK && w.status < http.StatusMultipleChoices {
		// A successful HTTP status without even one Responses event is not a
		// successful WebSocket turn. Surface it as a gateway failure rather than an
		// impossible code=200/204 error frame.
		w.status = http.StatusBadGateway
		if message == "" {
			message = "upstream returned an empty response stream"
		}
	}
	if message == "" {
		message = http.StatusText(w.status)
	}
	if upstreamError, ok := responsesWebSocketHTTPError(w.raw); ok {
		writeErr := writeResponsesWebSocketFailureTerminal(w.conn, w.status, upstreamError, w.header.Get("Retry-After"), w.responseID, true)
		w.logErrorFrameResult("http_error", w.status, upstreamError["code"], writeErr)
		return writeErr
	}
	// Carry the retry timing the relay resolved (Retry-After was written to this
	// writer's header map) so the client learns when the credential recovers.
	writeErr := writeResponsesWebSocketFailureTerminal(w.conn, w.status, gin.H{
		"message": message,
	}, w.header.Get("Retry-After"), w.responseID, true)
	w.logErrorFrameResult("http_error", w.status, w.status, writeErr)
	return writeErr
}

func (w *responsesWebSocketSSEWriter) logErrorFrameResult(source string, status int, code any, writeErr error) {
	if writeErr != nil {
		logger.LogWarn(w.ctx, fmt.Sprintf("responses websocket protocol: downstream_error_frame_write_failed source=%s status=%d code=%v err=%s", source, status, code, writeErr.Error()))
		return
	}
	logger.LogInfo(w.ctx, fmt.Sprintf("responses websocket protocol: downstream_error_frame_sent source=%s status=%d code=%v", source, status, code))
}

func (w *responsesWebSocketSSEWriter) drainSSEFrames(flushRemainder bool) {
	for {
		index, boundaryBytes := responsesWebSocketSSEBoundary(w.pending)
		if index < 0 {
			if flushRemainder && len(bytes.TrimSpace(w.pending)) > 0 {
				w.writeSSEBlock(w.pending)
				w.pending = nil
			}
			return
		}
		block := append([]byte(nil), w.pending[:index]...)
		w.pending = w.pending[index+boundaryBytes:]
		w.writeSSEBlock(block)
		if w.err != nil {
			return
		}
	}
}

// responsesWebSocketSSEBoundary finds the earliest complete LF or CRLF event
// boundary without rewriting the entire pending buffer on every streamed chunk.
func responsesWebSocketSSEBoundary(payload []byte) (int, int) {
	lfIndex := bytes.Index(payload, []byte("\n\n"))
	crlfIndex := bytes.Index(payload, []byte("\r\n\r\n"))
	if crlfIndex >= 0 && (lfIndex < 0 || crlfIndex < lfIndex) {
		return crlfIndex, 4
	}
	if lfIndex >= 0 {
		return lfIndex, 2
	}
	return -1, 0
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
	eventType := responsesWebSocketPayloadType(payload)
	if eventType == "response.done" {
		normalizedPayload, normalizedType, err := responsesws.NormalizeResponseDoneEvent([]byte(payload))
		if err != nil {
			w.err = err
			return
		}
		payload = string(normalizedPayload)
		eventType = normalizedType
	}
	if responsesWebSocketEventIsTerminal(eventType) {
		w.sawTerminal = true
	}
	if responsesWebSocketEventIsCleanTerminal(eventType) {
		w.sawCleanTerminal = true
	}
	if responseID := responsesWebSocketPayloadResponseID(payload); responseID != "" {
		w.responseID = responseID
	}
	w.err = writeResponsesWebSocketMessage(w.conn, []byte(payload))
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

// responsesWebSocketPayloadType parses only the top-level event type. Looking
// for quoted substrings is unsafe because a normal delta can contain text such
// as `"response.completed"` without ending the stream.
func responsesWebSocketPayloadType(payload string) string {
	var event struct {
		Type string `json:"type"`
	}
	if err := common.Unmarshal([]byte(payload), &event); err != nil {
		return ""
	}
	return event.Type
}

func responsesWebSocketPayloadResponseID(payload string) string {
	var event struct {
		Response struct {
			ID string `json:"id"`
		} `json:"response"`
	}
	if err := common.Unmarshal([]byte(payload), &event); err != nil {
		return ""
	}
	return strings.TrimSpace(event.Response.ID)
}

// responsesWebSocketEventIsTerminal reports whether a forwarded event is a
// stream-ending Responses event.
func responsesWebSocketEventIsTerminal(eventType string) bool {
	switch eventType {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.error", "error":
		return true
	default:
		return false
	}
}

// responsesWebSocketPayloadIsTerminal is retained as the payload-level helper
// used by focused tests and callers in this package.
func responsesWebSocketPayloadIsTerminal(payload string) bool {
	return responsesWebSocketEventIsTerminal(responsesWebSocketPayloadType(payload))
}

// responsesWebSocketPayloadIsCleanTerminal reports whether a forwarded payload is
// a non-error stream-ending event. It is the success subset of
// responsesWebSocketPayloadIsTerminal and drives response.append inheritance.
func responsesWebSocketEventIsCleanTerminal(eventType string) bool {
	return eventType == "response.completed" || eventType == "response.incomplete"
}

func responsesWebSocketPayloadIsCleanTerminal(payload string) bool {
	eventType := responsesWebSocketPayloadType(payload)
	if eventType == "response.done" {
		_, eventType, _ = responsesws.NormalizeResponseDoneEvent([]byte(payload))
	}
	return responsesWebSocketEventIsCleanTerminal(eventType)
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

func responsesWebSocketHTTPError(raw []byte) (gin.H, bool) {
	var envelope struct {
		Error types.OpenAIError `json:"error"`
	}
	if len(bytes.TrimSpace(raw)) == 0 || common.Unmarshal(raw, &envelope) != nil || envelope.Error.Message == "" {
		return nil, false
	}
	errorObject := gin.H{
		"message": envelope.Error.Message,
	}
	if envelope.Error.Type != "" {
		errorObject["type"] = envelope.Error.Type
	}
	if envelope.Error.Code != nil && fmt.Sprint(envelope.Error.Code) != "" {
		errorObject["code"] = envelope.Error.Code
	}
	if envelope.Error.Param != "" {
		errorObject["param"] = envelope.Error.Param
	}
	if len(envelope.Error.Metadata) > 0 {
		errorObject["metadata"] = envelope.Error.Metadata
	}
	return errorObject, true
}

func writeResponsesWebSocketMessage(conn *websocket.Conn, payload []byte) error {
	if conn == nil {
		return errors.New("responses websocket connection is nil")
	}
	if err := conn.SetWriteDeadline(time.Now().Add(responsesWebSocketWriteTimeout)); err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, payload)
}

func writeResponsesWebSocketErrorObject(conn *websocket.Conn, status int, errorObject gin.H, retryAfter string) error {
	if conn == nil {
		return errors.New("responses websocket connection is nil")
	}
	payload, err := common.Marshal(gin.H{
		"type":  "error",
		"error": normalizeResponsesWebSocketErrorObject(status, errorObject, retryAfter),
	})
	if err != nil {
		return err
	}
	return writeResponsesWebSocketMessage(conn, payload)
}

// normalizeResponsesWebSocketErrorObject fills the defaulted error fields shared
// by every downstream error shape: error type derived from the HTTP status, the
// status as fallback code, and the relay-resolved Retry-After as numeric
// retry_after.
func normalizeResponsesWebSocketErrorObject(status int, errorObject gin.H, retryAfter string) gin.H {
	if errorObject == nil {
		errorObject = gin.H{}
	}
	// A type assertion (not fmt.Sprint) so an absent key — Sprint(nil) is "<nil>",
	// not "" — still receives the status-derived default.
	if typeValue, ok := errorObject["type"].(string); !ok || strings.TrimSpace(typeValue) == "" {
		errorObject["type"] = responsesWebSocketErrorType(status)
	}
	if errorObject["code"] == nil || strings.TrimSpace(fmt.Sprint(errorObject["code"])) == "" {
		errorObject["code"] = status
	}
	if seconds, convErr := strconv.Atoi(strings.TrimSpace(retryAfter)); convErr == nil && seconds > 0 {
		errorObject["retry_after"] = seconds
	}
	return errorObject
}

// writeResponsesWebSocketFailureTerminal emits a gateway-owned terminal chain for
// a turn that entered relay handling but could not reach a normal upstream
// terminal. Before any upstream event arrived, synthesize response.created first
// because Codex clients associate the terminal with a response lifecycle rather
// than a bare error frame. When the stream already exposed a response id, reuse
// it so the synthetic failure matches the in-flight response the client knows.
func writeResponsesWebSocketFailureTerminal(conn *websocket.Conn, status int, errorObject gin.H, retryAfter string, responseID string, includeCreated bool) error {
	if conn == nil {
		return errors.New("responses websocket connection is nil")
	}
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		responseID = "resp_gateway_failed"
	}
	if includeCreated {
		createdPayload, err := common.Marshal(gin.H{
			"type": "response.created",
			"response": gin.H{
				"id":     responseID,
				"object": "response",
				"status": "in_progress",
			},
		})
		if err != nil {
			return err
		}
		if err := writeResponsesWebSocketMessage(conn, createdPayload); err != nil {
			return err
		}
	}
	payload, err := common.Marshal(gin.H{
		"type": "response.failed",
		"response": gin.H{
			"id":     responseID,
			"object": "response",
			"status": "failed",
			"error":  normalizeResponsesWebSocketErrorObject(status, errorObject, retryAfter),
		},
	})
	if err != nil {
		return err
	}
	return writeResponsesWebSocketMessage(conn, payload)
}

// writeResponsesWebSocketError sends a structured error frame. status drives the
// error type; retryAfter, when a positive delta-seconds value, is preserved as a
// numeric retry_after so a strict client can wait rather than hammer.
func writeResponsesWebSocketError(conn *websocket.Conn, status int, message string, retryAfter string) error {
	return writeResponsesWebSocketErrorWithCode(conn, status, message, status, retryAfter)
}

// writeResponsesWebSocketErrorWithCode keeps the protocol error code separate
// from Retry-After. Stateful continuation failures use a stable application code
// even though their HTTP-compatible status remains 400.
func writeResponsesWebSocketErrorWithCode(conn *websocket.Conn, status int, message string, code any, retryAfter string) error {
	errorObject := gin.H{
		"message": message,
		"type":    responsesWebSocketErrorType(status),
		"code":    code,
	}
	return writeResponsesWebSocketErrorObject(conn, status, errorObject, retryAfter)
}
