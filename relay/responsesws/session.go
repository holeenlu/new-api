package responsesws

import (
	"bytes"
	"context"
	"crypto/tls"
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
	idleTimeout  = 5 * time.Minute
	writeTimeout = 30 * time.Second

	// Keep one upstream event bounded before gorilla/websocket allocates it in
	// full. The downstream SSE bridge applies the same limit to an unframed event.
	maxUpstreamMessageBytes = 16 << 20
)

// FirstEventTimeout bounds how long a freshly established upstream WebSocket may
// stay silent after response.create is written. An upstream that accepts the
// handshake but never speaks the protocol would otherwise stall the client until
// the idle timeout. Because the application request may already be executing, a
// timeout fails without replaying it over HTTP. It is a var so tests can shorten
// it.
var FirstEventTimeout = 30 * time.Second

// MaxConnectionLifetime bounds how long one upstream connection is reused before
// it is proactively recycled. OpenAI caps a Responses WebSocket connection at 60
// minutes. A self-contained turn can reconnect, but a connection-local
// previous_response_id must fail and let the client resend full input context.
// It is a var so tests can shorten it.
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
// Capacity leases are per-turn, not per-connection: an idle but still-open
// connection holds no OAuth concurrency slot.
type Session struct {
	mu                    sync.Mutex
	conn                  *websocket.Conn
	connectedAt           time.Time
	channelID             int
	keyIndex              int
	credentialFingerprint string
	model                 string
	httpFallback          bool
	pendingID             int
	pendingKeyIndex       int
	pendingFingerprint    string
	pendingModel          string
}

// CredentialBinding identifies the credential that authenticated the upstream
// connection. A persistent Responses connection is credential-local, so the
// controller must restore this exact binding before building every later turn.
type CredentialBinding struct {
	ChannelID   int
	KeyIndex    int
	Fingerprint string
}

// responseBody ties the HTTP/SSE facade to one upstream WebSocket reader. If a
// relay handler stops consuming before the terminal event, closing the body must
// also close that upstream connection; otherwise a stale reader can race the
// next turn on the same socket.
type responseBody struct {
	io.ReadCloser
	session *Session
	conn    *websocket.Conn
	done    <-chan struct{}
	once    sync.Once
	err     error
}

func (b *responseBody) Close() error {
	if b == nil {
		return nil
	}
	b.once.Do(func() {
		if b.session != nil {
			if b.done == nil {
				b.session.invalidate(b.conn)
			} else {
				select {
				case <-b.done:
					// A terminal event already ended the reader; keep the connection reusable.
				default:
					b.session.invalidate(b.conn)
				}
			}
		}
		if b.ReadCloser != nil {
			b.err = b.ReadCloser.Close()
		}
	})
	return b.err
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
	// Per-turn leases are released by the turn's response body (or its failure
	// path), so there is no connection-scoped lease to release here.
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

// Binding returns the credential currently pinned to the session.
func (s *Session) Binding() (CredentialBinding, bool) {
	if s == nil {
		return CredentialBinding{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.channelID == 0 || s.credentialFingerprint == "" {
		return CredentialBinding{}, false
	}
	return CredentialBinding{
		ChannelID:   s.channelID,
		KeyIndex:    s.keyIndex,
		Fingerprint: s.credentialFingerprint,
	}, true
}

// ResetChannelForRetry releases the current upstream connection and its
// channel binding before the relay selects another compatible credential. It
// is used only while the current turn is still replay-safe; the relay owns that
// decision because it knows whether downstream output has started.
func (s *Session) ResetChannelForRetry() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.invalidateLocked(s.conn)
	s.channelID = 0
	s.keyIndex = 0
	s.credentialFingerprint = ""
	s.model = ""
	s.httpFallback = false
	s.pendingID = 0
	s.pendingKeyIndex = 0
	s.pendingFingerprint = ""
	s.pendingModel = ""
	s.mu.Unlock()
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
	s.keyIndex = s.pendingKeyIndex
	s.credentialFingerprint = s.pendingFingerprint
	s.model = s.pendingModel
	s.pendingID = 0
	s.pendingKeyIndex = 0
	s.pendingFingerprint = ""
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
	// Streaming is implicit and background execution is unsupported on the
	// Responses WebSocket transport. Keep these fields in httpBody for a possible
	// HTTP fallback, but never send them in an upstream WebSocket event.
	delete(payload, "stream")
	delete(payload, "stream_options")
	delete(payload, "background")
	body, err = common.Marshal(payload)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.channelID != 0 && s.channelID != info.ChannelId {
		return nil, errors.New("responses websocket cannot switch channels within one session")
	}
	credentialFingerprint := service.SubscriptionOAuthCredentialFingerprint(
		info.ChannelType,
		info.ChannelId,
		info.ChannelMultiKeyIndex,
		info.ApiKey,
	)
	if s.credentialFingerprint != "" && s.credentialFingerprint != credentialFingerprint {
		return nil, types.NewErrorWithStatusCode(
			errors.New("responses websocket cannot switch credentials within one session"),
			types.ErrorCodeInvalidRequest,
			http.StatusConflict,
			types.ErrOptionWithSkipRetry(),
		)
	}
	model := strings.TrimSpace(info.UpstreamModelName)
	if s.model != "" && !strings.EqualFold(s.model, model) {
		return nil, errors.New("responses websocket cannot switch models within one session")
	}
	if s.httpFallback {
		// Already degraded to HTTP for this session; the fallback reserves its own
		// per-request capacity.
		return s.doHTTPFallbackLocked(c, driver, info, model, httpBody, nil)
	}

	if s.conn != nil && !s.connectedAt.IsZero() && time.Since(s.connectedAt) >= MaxConnectionLifetime {
		// Proactively recycle the connection before the upstream's 60-minute cap
		// for a self-contained turn. A previous_response_id is connection-local and
		// cannot be transparently replayed on a replacement connection.
		logger.LogInfo(c, "responses websocket connection reached its lifetime; reconnecting")
		s.invalidateLocked(s.conn)
		previousResponseID, _ := payload["previous_response_id"].(string)
		if strings.TrimSpace(previousResponseID) != "" {
			return nil, types.NewErrorWithStatusCode(
				errors.New("responses websocket connection lifetime reached; reconnect and retry with full input context"),
				types.ErrorCodeWebSocketConnectionLimitReached,
				http.StatusBadRequest,
				types.ErrOptionWithSkipRetry(),
			)
		}
	}

	// Reserve capacity for THIS turn only. The lease is released when the turn's
	// response body closes (success) or on the failure paths below, so an idle but
	// still-open upstream connection never pins an OAuth concurrency slot.
	lease, err := driver.AcquireCapacity(c, info)
	if err != nil {
		return nil, err
	}
	justConnected := false
	if s.conn == nil {
		if err := s.connect(c, driver, info); err != nil {
			if errors.Is(err, ErrUpgradeRequired) {
				// Reuse this turn's lease for the HTTP fallback rather than reserving a
				// second slot for the same turn.
				s.httpFallback = true
				return s.doHTTPFallbackLocked(c, driver, info, model, httpBody, lease)
			}
			written, responseStarted := info.UpstreamAttemptState()
			lease.FinishFailedAttempt(written || responseStarted)
			return nil, err
		}
		justConnected = true
	}
	// A WebSocket write error is ambiguous: the peer may have received the full
	// application frame even when the local write reports failure. Mark the
	// attempt before writing and never reconnect or fall back after this point.
	info.MarkUpstreamRequestWritten()
	_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := s.conn.WriteMessage(websocket.TextMessage, body); err != nil {
		s.invalidateLocked(s.conn)
		lease.FinishFailedAttempt(true)
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("responses websocket write failed after the request may have reached upstream: %w", err),
			types.ErrorCodeDoRequestFailed,
			http.StatusBadGateway,
			types.ErrOptionWithSkipRetry(),
		)
	}

	conn := s.conn
	var firstPayload []byte
	if justConnected {
		// Probe a fresh connection for a first event. An upstream that accepts the
		// handshake but never speaks the Responses WebSocket protocol would
		// otherwise stall the client until the idle timeout. Because the request
		// frame is already on the wire, a failed probe fails the turn rather than
		// replaying (see the probe-error branch below). Close the connection if the
		// client disconnects mid-probe so the blocked read (and its capacity lease)
		// is released promptly instead of held for the full FirstEventTimeout.
		probeDone := make(chan struct{})
		if reqCtx := c.Request.Context(); reqCtx != nil {
			go func() {
				select {
				case <-reqCtx.Done():
					_ = conn.Close()
				case <-probeDone:
				}
			}()
		}
		payload, probeErr := readInitialEvent(conn)
		close(probeDone)
		if probeErr != nil {
			s.invalidateLocked(conn)
			// The request frame was written, so this is a real failed attempt against
			// the credential.
			lease.FinishFailedAttempt(true)
			// Otherwise the response.create frame was already written to this
			// connection, so the upstream may have received and begun executing it — a
			// silent probe cannot prove otherwise. Do NOT replay the turn over HTTP,
			// and do not let the relay resend it on another credential: a duplicate
			// would re-execute non-idempotent work (tool calls, code edits) and
			// double-consume the upstream account. Fail this turn instead (SkipRetry).
			// HTTP fallback stays reserved for failures before the application frame is
			// written (for example, an unsupported upgrade).
			logger.LogWarn(c, "responses websocket produced no initial event after the request was sent; failing the turn without replay: "+probeErr.Error())
			return nil, types.NewErrorWithStatusCode(
				fmt.Errorf("responses websocket upstream produced no output within %s after the request was sent: %w", FirstEventTimeout, probeErr),
				types.ErrorCodeDoRequestFailed,
				http.StatusBadGateway,
				types.ErrOptionWithSkipRetry(),
			)
		}
		firstPayload = payload
	}

	reader, writer := io.Pipe()
	streamDone := make(chan struct{})
	go s.streamAsSSE(c.Request.Context(), conn, writer, firstPayload, streamDone)
	var responseBody io.ReadCloser = &responseBody{
		ReadCloser: reader,
		session:    s,
		conn:       conn,
		done:       streamDone,
	}
	if lease != nil {
		// Finalize the per-turn lease exactly like the HTTP path: the relay closes
		// this body when the stream ends (releasing the slot) and marks the turn's
		// success/failure (resolving the credential's health / recovery probe).
		service.BindSubscriptionOAuthResponseLease(c, lease)
		responseBody = service.NewSubscriptionOAuthResponseBody(responseBody, lease)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: responseBody,
	}, nil
}

func (s *Session) doHTTPFallbackLocked(
	c *gin.Context,
	driver Driver,
	info *relaycommon.RelayInfo,
	model string,
	body []byte,
	reuseLease *service.SubscriptionOAuthLease,
) (*http.Response, error) {
	response, err := driver.DoHTTPFallback(c, info, body, reuseLease)
	if err != nil || response == nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		s.pendingID = 0
		s.pendingKeyIndex = 0
		s.pendingFingerprint = ""
		s.pendingModel = ""
		return response, err
	}
	s.pendingID = info.ChannelId
	s.pendingKeyIndex = info.ChannelMultiKeyIndex
	s.pendingFingerprint = service.SubscriptionOAuthCredentialFingerprint(
		info.ChannelType,
		info.ChannelId,
		info.ChannelMultiKeyIndex,
		info.ApiKey,
	)
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
	s.connectedAt = time.Time{}
}

func (s *Session) connect(c *gin.Context, driver Driver, info *relaycommon.RelayInfo) error {
	webSocketURL, headers, err := driver.DialUpstream(c, info)
	if err != nil {
		return err
	}

	client, err := service.GetHttpClientWithProxy(info.ChannelSetting.Proxy)
	if err != nil {
		return err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 30 * time.Second, EnableCompression: true}
	if transport, ok := client.Transport.(*http.Transport); ok && transport != nil {
		dialer.Proxy = transport.Proxy
		dialer.NetDialContext = transport.DialContext
		// A WebSocket handshake is an HTTP/1.1 Upgrade, but the shared relay
		// transport enables HTTP/2 (ForceAttemptHTTP2), so once it has served an
		// HTTP/2 request Go mutates its TLSClientConfig to advertise "h2" in
		// NextProtos. Copying that verbatim makes the TLS ALPN negotiate h2 against
		// HTTP/2 edges (e.g. Cloudflare-fronted chatgpt.com); the upgrade request is
		// then answered with HTTP/2 frames the dialer cannot parse ("malformed HTTP
		// response"). Clone the config but pin ALPN to http/1.1 so the handshake
		// succeeds while preserving proxy/TLS-verification settings.
		tlsConfig := transport.TLSClientConfig.Clone()
		if tlsConfig == nil {
			tlsConfig = &tls.Config{}
		}
		tlsConfig.NextProtos = []string{"http/1.1"}
		dialer.TLSClientConfig = tlsConfig
	}
	conn, response, err := dialer.DialContext(c.Request.Context(), webSocketURL, headers)
	if err != nil {
		logger.LogError(c, "responses websocket handshake failed: "+err.Error())
		if response != nil && response.StatusCode > 0 && response.StatusCode != http.StatusUpgradeRequired {
			// A genuine upstream HTTP status reached us (e.g. 429 or 5xx). Surface it
			// so the relay applies its normal retry/failover and Retry-After handling
			// instead of masking a rate limit or outage behind an HTTP fallback. The
			// caller finalizes this turn's capacity lease (responseStarted is now set,
			// so it counts as a real attempt against the credential).
			info.MarkUpstreamResponseStarted()
			info.MarkUpstreamFailureResponse()
			if response.Body != nil {
				return service.RelayErrorHandler(c.Request.Context(), response)
			}
			apiError := types.NewErrorWithStatusCode(
				fmt.Errorf("responses websocket handshake failed with status %d", response.StatusCode),
				types.ErrorCodeBadResponseStatusCode,
				response.StatusCode,
			)
			apiError.UpstreamStatusCode = response.StatusCode
			apiError.RetryAfter = service.ParseRetryAfterHeader(response.Header.Get("Retry-After"), time.Now())
			return apiError
		}
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		// Either the upstream explicitly asked to upgrade elsewhere (426) or the dial
		// produced no usable HTTP response at all — a malformed/HTTP-2 framed reply,
		// a TLS error, or a connection-level failure. In every one of these cases the
		// endpoint does not speak the Responses WebSocket protocol, so serve this turn
		// and the rest of the connection over HTTP rather than failing the turn hard.
		return ErrUpgradeRequired
	}
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	conn.EnableWriteCompression(false)
	conn.SetReadLimit(maxUpstreamMessageBytes)
	s.conn = conn
	s.connectedAt = time.Now()
	s.channelID = info.ChannelId
	s.keyIndex = info.ChannelMultiKeyIndex
	s.credentialFingerprint = service.SubscriptionOAuthCredentialFingerprint(
		info.ChannelType,
		info.ChannelId,
		info.ChannelMultiKeyIndex,
		info.ApiKey,
	)
	s.model = strings.TrimSpace(info.UpstreamModelName)
	driver.OnUpstreamConnected(c, info)
	return nil
}

// invalidate closes and forgets the connection. It acquires the session lock;
// callers already holding it must use invalidateLocked. The turn's capacity lease
// is released separately by its response body, not here.
func (s *Session) invalidate(conn *websocket.Conn) {
	s.mu.Lock()
	s.invalidateLocked(conn)
	s.mu.Unlock()
}

// readInitialEvent reads the first non-empty text event from a freshly
// established upstream connection within FirstEventTimeout. At this point the
// request frame has already been written, so a timeout or read error must fail
// without replaying the request on another transport.
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
// response.completed or response.incomplete stops while keeping the connection
// for reuse.
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
	switch event.Type {
	case "response.completed", "response.incomplete":
		// Clean terminal states: the turn ended and the persistent upstream keeps
		// the connection open for the next response.create, so stop reading but keep
		// it for reuse. response.incomplete (e.g. max output tokens or a content
		// filter) is as terminal as response.completed; omitting it here would leave
		// the read loop blocked on the idle upstream until idleTimeout, hanging the
		// turn and stalling the next one.
		return true
	case "response.failed", "response.error", "error":
		// Terminal failure: the connection may carry residual upstream state, so drop
		// it rather than risk bleeding events into a reused turn.
		s.invalidate(conn)
		return true
	}
	return false
}

// streamAsSSE relays upstream WebSocket events to the SSE pipe. firstPayload, if
// non-nil, is a text event already read during the fresh-connection probe and is
// emitted before the read loop continues.
func (s *Session) streamAsSSE(ctx context.Context, conn *websocket.Conn, writer *io.PipeWriter, firstPayload []byte, done chan struct{}) {
	defer writer.Close()
	defer close(done)

	// Unblock a stalled ReadMessage when the request is cancelled by closing the
	// connection from a watcher goroutine; otherwise the reader (and its capacity
	// lease) would stay blocked until the idle deadline fires, and a follow-up
	// request could start a second concurrent reader on the same conn.
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
