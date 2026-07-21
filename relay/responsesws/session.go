package responsesws

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

const (
	sessionKey = "responses_websocket_session"

	// idleTimeout bounds the gap between two upstream events once streaming has
	// started, acting as an idle rather than a total-duration timeout.
	idleTimeout = 5 * time.Minute
)

// FirstEventTimeout bounds how long a freshly established upstream WebSocket may
// stay silent before it is treated as not supporting the Responses WebSocket
// protocol. An upstream that accepts the handshake but never speaks the protocol
// would otherwise stall the client until the idle timeout; instead the session
// falls back to HTTP. It is a var so tests can shorten it.
var FirstEventTimeout = 30 * time.Second

// MaxConnectionLifetime bounds how long one upstream connection is reused before
// it is proactively recycled. OpenAI caps a Responses WebSocket connection at 60
// minutes; recycling a little earlier avoids failing a turn when the upstream
// closes mid-stream. It is a var so tests can shorten it.
var MaxConnectionLifetime = 55 * time.Minute

// ErrUpgradeRequired signals that the upstream declined the WebSocket upgrade and
// the session should serve the turn (and the rest of the connection) over HTTP.
var ErrUpgradeRequired = errors.New("responses websocket upgrade required")

// Driver supplies the upstream-specific operations the shared Responses
// WebSocket session needs: how to reach the upstream, how to reserve per-request
// capacity, and how to fall back to HTTP. Isolating these behind an interface
// lets the session logic (handshake, streaming, probe, fallback) be reused by any
// Responses-capable channel.
//
// A nil *service.SubscriptionOAuthLease means the channel has no per-request
// capacity limit (e.g. a standard API-key channel); the session guards nil leases
// on every path.
type Driver interface {
	// DialUpstream returns the ws:// or wss:// URL and headers for the upstream
	// Responses WebSocket handshake.
	DialUpstream(c *gin.Context, info *relaycommon.RelayInfo) (string, http.Header, error)
	// AcquireCapacity reserves any per-request capacity for the upstream and
	// returns the lease to release later, or (nil, nil) when unlimited.
	AcquireCapacity(c *gin.Context, info *relaycommon.RelayInfo) (*service.SubscriptionOAuthLease, error)
	// DoHTTPFallback performs the equivalent HTTP Responses request, reusing
	// reuseLease when non-nil.
	DoHTTPFallback(c *gin.Context, info *relaycommon.RelayInfo, body []byte, reuseLease *service.SubscriptionOAuthLease) (*http.Response, error)
	// OnUpstreamConnected runs after a successful handshake to apply channel
	// pinning and retry policy for the connection's lifetime.
	OnUpstreamConnected(c *gin.Context, info *relaycommon.RelayInfo)
}

// Session owns one client WebSocket's upstream connection: a single channel,
// model, and credential are pinned for its lifetime, and turns run sequentially.
type Session struct {
	mu            sync.Mutex
	conn          *websocket.Conn
	connectedAt   time.Time
	channelID     int
	model         string
	httpFallback  bool
	lease         *service.SubscriptionOAuthLease
	fallbackLease *service.SubscriptionOAuthLease
	pendingID     int
	pendingModel  string
}

// SetSession stores the session on the request context for the adaptor to pick up.
func SetSession(c *gin.Context, session *Session) {
	if c != nil && session != nil {
		c.Set(sessionKey, session)
	}
}

// SessionFromContext returns the session bound to the request, or nil.
func SessionFromContext(c *gin.Context) *Session {
	if c == nil {
		return nil
	}
	value, exists := c.Get(sessionKey)
	if !exists {
		return nil
	}
	session, _ := value.(*Session)
	return session
}

func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	if s.conn != nil {
		err = s.conn.Close()
		s.conn = nil
	}
	if s.lease != nil {
		s.lease.Release()
		s.lease = nil
	}
	if s.fallbackLease != nil {
		s.fallbackLease.Release()
		s.fallbackLease = nil
	}
	return err
}

func (s *Session) ChannelID() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.channelID
}

func (s *Session) ConfirmHTTPFallbackSuccess() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.httpFallback || s.channelID != 0 || s.pendingID == 0 {
		return
	}
	s.channelID = s.pendingID
	s.model = s.pendingModel
	s.pendingID = 0
	s.pendingModel = ""
}

// DoRequest runs one Responses turn over the upstream WebSocket, returning an
// SSE-framed HTTP response the relay handler streams to the client. It falls back
// to HTTP when the upstream does not support WebSocket.
func (s *Session) DoRequest(c *gin.Context, driver Driver, info *relaycommon.RelayInfo, requestBody io.Reader) (*http.Response, error) {
	if s == nil || c == nil || driver == nil || info == nil || requestBody == nil {
		return nil, errors.New("responses websocket: invalid request")
	}
	requestLimit := common.MaxRequestBodyBytes()
	body, err := io.ReadAll(io.LimitReader(requestBody, requestLimit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > requestLimit {
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("responses websocket request exceeds %d MB: %w", requestLimit>>20, common.ErrRequestBodyTooLarge),
			types.ErrorCodeReadRequestBodyFailed,
			http.StatusRequestEntityTooLarge,
			types.ErrOptionWithSkipRetry(),
		)
	}
	httpBody := append([]byte(nil), body...)
	var payload map[string]any
	if err := common.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	payload["type"] = "response.create"
	body, err = common.Marshal(payload)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.channelID != 0 && s.channelID != info.ChannelId {
		return nil, errors.New("responses websocket cannot switch channels within one session")
	}
	model := strings.TrimSpace(info.UpstreamModelName)
	if s.model != "" && !strings.EqualFold(s.model, model) {
		return nil, errors.New("responses websocket cannot switch models within one session")
	}
	if s.httpFallback {
		return s.doHTTPFallbackLocked(c, driver, info, model, httpBody)
	}
	if s.conn != nil && !s.connectedAt.IsZero() && time.Since(s.connectedAt) >= MaxConnectionLifetime {
		// Proactively recycle the connection before the upstream's 60-minute cap
		// so a long-lived session does not fail a turn when the upstream closes
		// mid-stream. The block below re-establishes it.
		logger.LogInfo(c, "responses websocket connection reached its lifetime; reconnecting")
		s.invalidateLocked(s.conn)
	}
	justConnected := false
	if s.conn == nil {
		if err := s.connect(c, driver, info); err != nil {
			if errors.Is(err, ErrUpgradeRequired) {
				s.httpFallback = true
				return s.doHTTPFallbackLocked(c, driver, info, model, httpBody)
			}
			return nil, err
		}
		justConnected = true
	}
	if err := s.conn.WriteMessage(websocket.TextMessage, body); err != nil {
		s.invalidateLocked(s.conn)
		if justConnected {
			// A brand-new connection that cannot accept the first write is not a
			// usable WebSocket transport; degrade to HTTP for this session.
			s.httpFallback = true
			logger.LogWarn(c, "responses websocket write failed on a new connection; falling back to HTTP: "+err.Error())
			return s.doHTTPFallbackLocked(c, driver, info, model, httpBody)
		}
		return nil, err
	}

	conn := s.conn
	var firstPayload []byte
	if justConnected {
		// Probe a fresh connection for a first event. An upstream that accepts the
		// handshake but never speaks the Responses WebSocket protocol would
		// otherwise stall the client until the idle timeout; fall back to HTTP.
		payload, probeErr := readInitialEvent(conn)
		if probeErr != nil {
			s.invalidateLocked(conn)
			s.httpFallback = true
			logger.LogWarn(c, "responses websocket produced no initial event; falling back to HTTP: "+probeErr.Error())
			return s.doHTTPFallbackLocked(c, driver, info, model, httpBody)
		}
		firstPayload = payload
	}

	reader, writer := io.Pipe()
	go s.streamAsSSE(c.Request.Context(), conn, writer, firstPayload)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: reader,
	}, nil
}

func (s *Session) doHTTPFallbackLocked(
	c *gin.Context,
	driver Driver,
	info *relaycommon.RelayInfo,
	model string,
	body []byte,
) (*http.Response, error) {
	var reuseLease *service.SubscriptionOAuthLease
	if s.fallbackLease != nil {
		reuseLease = s.fallbackLease
		s.fallbackLease = nil
	}
	response, err := driver.DoHTTPFallback(c, info, body, reuseLease)
	if err != nil || response == nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		s.pendingID = 0
		s.pendingModel = ""
		return response, err
	}
	s.pendingID = info.ChannelId
	s.pendingModel = model
	return response, nil
}

func (s *Session) invalidateLocked(conn *websocket.Conn) {
	if s.conn != conn {
		return
	}
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
	if s.lease != nil {
		s.lease.Release()
		s.lease = nil
	}
	if s.fallbackLease != nil {
		s.fallbackLease.Release()
		s.fallbackLease = nil
	}
}

func (s *Session) connect(c *gin.Context, driver Driver, info *relaycommon.RelayInfo) error {
	lease, err := driver.AcquireCapacity(c, info)
	if err != nil {
		return err
	}

	webSocketURL, headers, err := driver.DialUpstream(c, info)
	if err != nil {
		if lease != nil {
			lease.Abandon()
		}
		return err
	}

	client, err := service.GetHttpClientWithProxy(info.ChannelSetting.Proxy)
	if err != nil {
		if lease != nil {
			lease.Abandon()
		}
		return err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 30 * time.Second, EnableCompression: true}
	if transport, ok := client.Transport.(*http.Transport); ok && transport != nil {
		dialer.Proxy = transport.Proxy
		dialer.NetDialContext = transport.DialContext
		dialer.TLSClientConfig = transport.TLSClientConfig
	}
	conn, response, err := dialer.DialContext(c.Request.Context(), webSocketURL, headers)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil {
		logger.LogError(c, "responses websocket handshake failed: "+err.Error())
		if response != nil && response.StatusCode > 0 && response.StatusCode != http.StatusUpgradeRequired {
			// A genuine upstream HTTP status reached us (e.g. 429 or 5xx). Surface it
			// so the relay applies its normal retry/failover and Retry-After handling
			// instead of masking a rate limit or outage behind an HTTP fallback.
			if lease != nil {
				lease.Release()
			}
			info.MarkUpstreamResponseStarted()
			info.MarkUpstreamFailureResponse()
			apiError := types.NewErrorWithStatusCode(
				fmt.Errorf("responses websocket handshake failed with status %d", response.StatusCode),
				types.ErrorCodeBadResponseStatusCode,
				response.StatusCode,
			)
			apiError.UpstreamStatusCode = response.StatusCode
			apiError.RetryAfter = service.ParseRetryAfterHeader(response.Header.Get("Retry-After"), time.Now())
			return apiError
		}
		// Either the upstream explicitly asked to upgrade elsewhere (426) or the dial
		// produced no usable HTTP response at all — a malformed/HTTP-2 framed reply,
		// a TLS error, or a connection-level failure. In every one of these cases the
		// endpoint does not speak the Responses WebSocket protocol, so serve this turn
		// and the rest of the connection over HTTP rather than failing the turn hard.
		s.fallbackLease = lease
		return ErrUpgradeRequired
	}
	conn.EnableWriteCompression(false)
	// Only record the upstream request as written after a successful handshake; a
	// dial that never reached upstream must stay retryable so the failover policy
	// is not suppressed.
	info.MarkUpstreamRequestWritten()
	s.conn = conn
	s.connectedAt = time.Now()
	s.channelID = info.ChannelId
	s.model = strings.TrimSpace(info.UpstreamModelName)
	s.lease = lease
	driver.OnUpstreamConnected(c, info)
	return nil
}

// invalidate closes and forgets the connection while releasing its lease. It
// acquires the session lock; callers already holding it must use invalidateLocked.
func (s *Session) invalidate(conn *websocket.Conn) {
	s.mu.Lock()
	s.invalidateLocked(conn)
	s.mu.Unlock()
}

// readInitialEvent reads the first non-empty text event from a freshly
// established upstream connection within FirstEventTimeout. A timeout or read
// error signals that the upstream did not deliver a Responses event, so the
// caller can fall back to the HTTP transport.
func readInitialEvent(conn *websocket.Conn) ([]byte, error) {
	_ = conn.SetReadDeadline(time.Now().Add(FirstEventTimeout))
	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			return nil, err
		}
		if messageType != websocket.TextMessage {
			continue
		}
		if len(bytes.TrimSpace(payload)) == 0 {
			continue
		}
		return payload, nil
	}
}

// emitEventAndMaybeStop normalizes one upstream text payload and writes it to the
// SSE stream, returning stop=true when the reader should exit. It invalidates the
// connection and closes the writer with the error on a malformed payload, a
// terminal failure event, or a downstream write failure; a successful
// response.completed stops while keeping the connection for reuse.
func (s *Session) emitEventAndMaybeStop(conn *websocket.Conn, writer *io.PipeWriter, payload []byte) bool {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return false
	}
	var event struct {
		Type string `json:"type"`
	}
	if err := common.Unmarshal(payload, &event); err != nil {
		s.invalidate(conn)
		_ = writer.CloseWithError(err)
		return true
	}
	if event.Type == "response.done" {
		var normalized map[string]any
		if err := common.Unmarshal(payload, &normalized); err != nil {
			s.invalidate(conn)
			_ = writer.CloseWithError(err)
			return true
		}
		normalized["type"] = "response.completed"
		marshaled, err := common.Marshal(normalized)
		if err != nil {
			s.invalidate(conn)
			_ = writer.CloseWithError(err)
			return true
		}
		payload = marshaled
		event.Type = "response.completed"
	}
	if _, err := fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", event.Type, payload); err != nil {
		// The downstream reader stopped consuming mid-stream. Invalidate the
		// connection because in-flight events remain unread and reusing it would
		// bleed stale events into the next response.
		s.invalidate(conn)
		return true
	}
	if event.Type == "response.completed" || event.Type == "response.failed" || event.Type == "response.error" || event.Type == "error" {
		if event.Type != "response.completed" {
			s.invalidate(conn)
		}
		return true
	}
	return false
}

// streamAsSSE relays upstream WebSocket events to the SSE pipe. firstPayload, if
// non-nil, is a text event already read during the fresh-connection probe and is
// emitted before the read loop continues.
func (s *Session) streamAsSSE(ctx context.Context, conn *websocket.Conn, writer *io.PipeWriter, firstPayload []byte) {
	defer writer.Close()

	// Unblock a stalled ReadMessage when the request is cancelled by closing the
	// connection from a watcher goroutine; otherwise the reader (and its capacity
	// lease) would stay blocked until the idle deadline fires, and a follow-up
	// request could start a second concurrent reader on the same conn.
	done := make(chan struct{})
	defer close(done)
	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				s.invalidate(conn)
			case <-done:
			}
		}()
	}

	if firstPayload != nil {
		if s.emitEventAndMaybeStop(conn, writer, firstPayload) {
			return
		}
	}

	for {
		// Refresh the deadline per message so it acts as an idle timeout rather than
		// a fixed cap on total stream duration.
		_ = conn.SetReadDeadline(time.Now().Add(idleTimeout))
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			s.invalidate(conn)
			_ = writer.CloseWithError(err)
			return
		}
		if messageType != websocket.TextMessage {
			continue
		}
		if s.emitEventAndMaybeStop(conn, writer, payload) {
			return
		}
	}
}
