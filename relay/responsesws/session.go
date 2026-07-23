package responsesws

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
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
	idleTimeout              = 5 * time.Minute
	writeTimeout             = 30 * time.Second
	controlFrameWriteTimeout = time.Second

	// Keep one upstream event bounded before gorilla/websocket allocates it in
	// full. The downstream SSE bridge applies the same limit to an unframed event.
	maxUpstreamMessageBytes = 16 << 20

	// Bound connection-local continuation state. Evicting the oldest id is safe:
	// it can only make a later continuation fail closed locally.
	maxTrackedResponseIDsPerConnection = 4096
	maxTrackedResponseIDBytes          = 4 << 10
	maxTrackedResponseIDTotalBytes     = 1 << 20
)

// FirstEventTimeout bounds how long a freshly established upstream WebSocket may
// stay silent after response.create is written. An upstream that accepts the
// handshake but never speaks the protocol would otherwise stall the client until
// the idle timeout. Because the application request may already be executing, a
// timeout fails without replaying it over HTTP. It is a var so tests can shorten
// it.
var FirstEventTimeout = 30 * time.Second

// ContinuationLivenessFreshness is the maximum age of observed upstream
// activity that lets a continuation write without an extra round trip. Older
// connections are ping-probed before response.create is written. Both vars are
// mutable so protocol tests can shorten the bounds.
var ContinuationLivenessFreshness = 30 * time.Second

var ContinuationProbeTimeout = 3 * time.Second

// MaxConnectionLifetime bounds how long one upstream connection is reused before
// it is proactively recycled. OpenAI caps a Responses WebSocket connection at 60
// minutes. A self-contained turn can reconnect, but a connection-local
// previous_response_id must fail and let the client resend full input context.
// It is a var so tests can shorten it.
var MaxConnectionLifetime = 55 * time.Minute

// ReuseIdleReconnectThreshold recycles an idle upstream connection instead of
// reusing it. A connection can be silently idle-closed by the upstream or an
// intermediary between turns; reconnecting proactively (before the application
// frame is written) is replay-safe, whereas writing into a half-open socket would
// block the reader for the full idleTimeout. Production protocol logs show the
// upstream pings every ~20s and closes after ~2-3 missed pings (~40-60s of
// reader-less idle), so 30s (1.5 ping periods) recycles before the upstream can
// declare the connection dead. It is a var so tests can shorten it.
var ReuseIdleReconnectThreshold = 30 * time.Second

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
	lastUsedAt            time.Time
	channelID             int
	keyIndex              int
	credentialFingerprint string
	model                 string
	responseIDs           map[string]struct{}
	responseIDOrder       []string
	responseIDHead        int
	responseIDBytes       int
	httpFallback          bool
	pendingID             int
	pendingKeyIndex       int
	pendingFingerprint    string
	pendingModel          string

	// Connection-reader state. The permanent per-connection reader owns
	// every ReadMessage; a turn registers turnEvents/turnDone to consume business
	// frames and clears them when its stream ends. connStop and lifetime are owned
	// by the same connection generation.
	turnEvents chan upstreamMessage
	turnDone   chan struct{}
	connStop   chan struct{}
	lifetime   *time.Timer
	generation uint64

	lastActivity  *atomic.Int64
	probeMu       sync.Mutex
	probeNonce    string
	probeAck      chan struct{}
	probeSequence uint64
}

// upstreamMessage is one business frame — or the terminating read error —
// delivered by the connection's sole reader to the active turn's consumer.
type upstreamMessage struct {
	payload []byte
	err     error
	class   upstreamEventClass
}

type upstreamEventClass uint8

const (
	upstreamTurnEvent upstreamEventClass = iota
	upstreamConnectionMetadata
)

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
		streamFinished := false
		if b.done != nil {
			select {
			case <-b.done:
				streamFinished = true
			default:
			}
		}
		if !streamFinished && b.session != nil {
			// An unfinished reader leaves upstream events in flight. Drop its
			// connection before another turn can start a reader.
			b.session.invalidate(b.conn)
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
	if s.connStop != nil {
		close(s.connStop)
		s.connStop = nil
	}
	if s.lifetime != nil {
		s.lifetime.Stop()
		s.lifetime = nil
	}
	s.lastActivity = nil
	s.connectedAt = time.Time{}
	s.resetResponseIDsLocked()
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

// HasLiveConnection reports whether this downstream session still owns the
// upstream WebSocket required to resolve connection-local response ids.
func (s *Session) HasLiveConnection() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hasUsableConnectionLocked()
}

// HasTransportState reports whether this downstream session is already bound
// to an upstream transport or credential. A dynamically disabled WebSocket
// channel must keep routing such a session through its state machine even after
// the socket was lost, so model affinity and fail-closed continuation rules are
// not bypassed by a direct HTTP call.
func (s *Session) HasTransportState() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn != nil || s.httpFallback || s.channelID != 0 || s.pendingID != 0 || s.model != ""
}

// CanContinue reports whether previousResponseID was observed on the current,
// still-usable upstream WebSocket. Merely having some live connection is not
// sufficient: response ids are owned by the connection generation that emitted
// them and must never be replayed onto a replacement connection.
func (s *Session) CanContinue(previousResponseID string) bool {
	if s == nil || strings.TrimSpace(previousResponseID) == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasUsableConnectionLocked() {
		return false
	}
	_, ok := s.responseIDs[previousResponseID]
	return ok
}

func (s *Session) hasUsableConnectionLocked() bool {
	if s.conn == nil || s.httpFallback {
		return false
	}
	if !s.connectedAt.IsZero() && time.Since(s.connectedAt) >= MaxConnectionLifetime {
		// Admission can reject a continuation before doRequest runs. Reclaim the
		// expired socket and its connection-local ids here as well, otherwise a
		// client that sends only rejected continuations could retain both until the
		// downstream session eventually disconnects.
		s.invalidateLocked(s.conn)
		return false
	}
	return true
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
	s.resetResponseIDsLocked()
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
	return s.doRequest(c, driver, info, requestBody, true)
}

// DoRequestOnCurrentTransport keeps an already-live upstream WebSocket as the
// owner of this downstream session without permitting a new WebSocket dial. It
// is used when channel configuration disables new WebSocket upgrades while a
// session is already active. If the current connection becomes unusable before
// a self-contained turn is written, the session closes it and explicitly
// degrades to HTTP; a connection-local continuation still fails closed.
func (s *Session) DoRequestOnCurrentTransport(c *gin.Context, driver Driver, info *relaycommon.RelayInfo, requestBody io.Reader) (*http.Response, error) {
	return s.doRequest(c, driver, info, requestBody, false)
}

func (s *Session) doRequest(c *gin.Context, driver Driver, info *relaycommon.RelayInfo, requestBody io.Reader, allowNewWebSocket bool) (*http.Response, error) {
	if s == nil || c == nil || driver == nil || info == nil || requestBody == nil {
		return nil, errors.New("responses websocket: invalid request")
	}
	c.Set(sessionResponseKey, false)
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
	if err := common.UnmarshalWithNumber(body, &payload); err != nil {
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
	modelValue, ok := payload["model"].(string)
	if !ok || strings.TrimSpace(modelValue) == "" {
		return nil, types.NewErrorWithStatusCode(
			errors.New("responses websocket outbound model is missing or invalid"),
			types.ErrorCodeInvalidRequest,
			http.StatusBadRequest,
			types.ErrOptionWithSkipRetry(),
		)
	}
	model := modelValue

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.channelID != 0 && s.channelID != info.ChannelId {
		return nil, types.NewErrorWithStatusCode(
			errors.New("responses websocket cannot switch channels within one session"),
			types.ErrorCodeInvalidRequest,
			http.StatusConflict,
			types.ErrOptionWithSkipRetry(),
		)
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
	previousResponseID, _ := payload["previous_response_id"].(string)
	continuation := strings.TrimSpace(previousResponseID) != ""
	// Bind affinity to the exact provider payload rather than RelayInfo. Raw
	// passthrough intentionally skips model mapping, and parameter overrides can
	// also change the body after mapping; only the outbound JSON is authoritative.
	// A model change is legal for a self-contained turn — the official protocol
	// scopes a connection to an account, not a model — so drop the old
	// connection-local binding and rebind below. A continuation references state
	// produced under the old model's connection and stays fail-closed. The primary
	// rebind path is the controller (it must clear its channel pin BEFORE
	// distribution); this in-session relaxation covers a model change the
	// controller cannot see, e.g. one injected by a channel parameter override.
	if s.model != "" && s.model != model {
		if continuation {
			return nil, types.NewErrorWithStatusCode(
				errors.New("responses websocket cannot continue a previous response under a different model"),
				types.ErrorCodeInvalidRequest,
				http.StatusConflict,
				types.ErrOptionWithSkipRetry(),
			)
		}
		s.invalidateLocked(s.conn)
		s.model = ""
	}

	if s.conn != nil && !s.connectedAt.IsZero() && time.Since(s.connectedAt) >= MaxConnectionLifetime {
		// Proactively recycle the connection before the upstream's 60-minute cap
		// for a self-contained turn. A previous_response_id is connection-local and
		// cannot be transparently replayed on a replacement connection.
		if allowNewWebSocket {
			logger.LogInfo(c, "responses websocket connection reached its lifetime; reconnecting")
		} else {
			logger.LogInfo(c, "responses websocket connection reached its lifetime; new upgrades are disabled, falling back to HTTP")
		}
		s.invalidateLocked(s.conn)
	}
	if continuation {
		_, ownedByCurrentConnection := s.responseIDs[previousResponseID]
		if s.conn != nil && !s.httpFallback && ownedByCurrentConnection {
			// The exact response id was emitted by this connection generation. When
			// recent upstream activity is stale, prove the same socket is still live
			// before writing the connection-local continuation frame.
			if err := s.probeContinuationLocked(c, s.conn); err != nil {
				logger.LogWarn(c, "responses websocket continuation liveness probe failed: "+err.Error())
				s.invalidateLocked(s.conn)
				return nil, NewContinuationUnavailableError()
			}
		} else {
			// previous_response_id identifies state owned by the live upstream
			// WebSocket. It is unsafe to establish a replacement connection or send the
			// request over HTTP because neither transport can resolve that state, and a
			// replay could execute the continuation against the wrong conversation.
			return nil, NewContinuationUnavailableError()
		}
	}
	if s.httpFallback {
		// Already degraded to HTTP for this session; the fallback reserves its own
		// per-request capacity. Continuations were rejected above because their
		// previous_response_id is connection-local.
		return s.doHTTPFallbackLocked(c, driver, info, model, httpBody, nil)
	}
	if s.conn == nil && !allowNewWebSocket {
		// Channel configuration may be disabled while this downstream session is
		// open. Never turn that into a new upstream WebSocket dial. The prior socket
		// (if any) has already been invalidated above, so an HTTP response id cannot
		// later be mixed with state owned by an old WebSocket.
		s.httpFallback = true
		return s.doHTTPFallbackLocked(c, driver, info, model, httpBody, nil)
	}

	// Reserve capacity for THIS turn only. The lease is released when the turn's
	// response body closes (success) or on the failure paths below, so an idle but
	// still-open upstream connection never pins an OAuth concurrency slot.
	lease, err := driver.AcquireCapacity(c, info)
	if err != nil {
		return nil, err
	}
	// D: recycle a connection idle beyond the threshold rather than reusing a socket
	// the upstream may have silently closed. Only a self-contained turn (no
	// previous_response_id) may recycle here: the reconnect happens before the
	// application frame is written, so it is replay-safe. A continuation depends on
	// state owned by THIS connection and must never move to a replacement socket, so
	// it keeps the connection; if that connection is already dead, H fails it within
	// FirstEventTimeout so the client resends full input context instead.
	if s.conn != nil && !continuation && !s.lastUsedAt.IsZero() && time.Since(s.lastUsedAt) > ReuseIdleReconnectThreshold {
		logger.LogInfo(c, fmt.Sprintf("responses websocket timing: reused connection idle %dms exceeds threshold, reconnecting", time.Since(s.lastUsedAt).Milliseconds()))
		s.invalidateLocked(s.conn)
	}
	justConnected := false
	if s.conn == nil {
		if err := s.connect(c, driver, info, model); err != nil {
			if errors.Is(err, ErrUpgradeRequired) {
				// Reuse this turn's lease for the HTTP fallback rather than reserving a
				// second slot for the same turn.
				s.httpFallback = true
				return s.doHTTPFallbackLocked(c, driver, info, model, httpBody, lease)
			}
			written, responseStarted := info.UpstreamAttemptState()
			if lease != nil {
				lease.FinishFailedAttempt(written || responseStarted)
			}
			return nil, err
		}
		justConnected = true
	}
	// Register this turn as the reader's consumer BEFORE writing the request:
	// the response can start arriving the moment the frame is on the wire, and an
	// unregistered business frame would be treated as an idle-period protocol
	// violation. First-event and idle timeouts are enforced on the consumer side
	// (never as read deadlines).
	turnEvents := make(chan upstreamMessage, 4)
	turnDone := make(chan struct{})
	s.turnEvents = turnEvents
	s.turnDone = turnDone
	clearTurn := func() {
		if s.turnEvents == turnEvents {
			s.turnEvents = nil
			s.turnDone = nil
		}
		close(turnDone)
	}
	// A WebSocket write error is ambiguous: the peer may have received the full
	// application frame even when the local write reports failure. Mark the
	// attempt before writing and never reconnect or fall back after this point.
	info.MarkUpstreamRequestWritten()
	_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := s.conn.WriteMessage(websocket.TextMessage, body); err != nil {
		clearTurn()
		s.invalidateLocked(s.conn)
		if lease != nil {
			lease.FinishFailedAttempt(true)
		}
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("responses websocket write failed after the request may have reached upstream: %w", err),
			types.ErrorCodeDoRequestFailed,
			http.StatusBadGateway,
			types.ErrOptionWithSkipRetry(),
		)
	}

	conn := s.conn

	// TEMP diagnostic: capture write timing for every turn and reuse context for
	// an existing connection.
	diag := &reuseTurnDiag{writeAt: time.Now()}
	if !justConnected {
		diag.reused = true
		diag.connAge = time.Since(s.connectedAt)
		diag.idleGap = time.Since(s.lastUsedAt)
	}
	// lastUsedAt is refreshed when this turn's stream ends (connection returns to
	// idle), so a later turn's idle_gap / recycle threshold measure true idle time — not
	// including this turn's generation time.

	reader, writer := io.Pipe()
	streamDone := make(chan struct{})
	go s.streamAsSSE(c.Request.Context(), conn, writer, turnEvents, turnDone, streamDone, diag)
	streamBody := &responseBody{
		ReadCloser: reader,
		session:    s,
		conn:       conn,
		done:       streamDone,
	}
	var responseBody io.ReadCloser = streamBody
	if lease != nil {
		// Finalize the per-turn lease exactly like the HTTP path: the relay closes
		// this body when the stream ends (releasing the slot) and marks the turn's
		// success/failure (resolving the credential's health / recovery probe).
		service.BindSubscriptionOAuthResponseLease(c, lease)
		responseBody = service.NewSubscriptionOAuthResponseBody(responseBody, lease)
	}
	c.Set(sessionResponseKey, true)
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
	if s.connStop != nil {
		close(s.connStop)
		s.connStop = nil
	}
	if s.lifetime != nil {
		s.lifetime.Stop()
		s.lifetime = nil
	}
	s.lastActivity = nil
	s.connectedAt = time.Time{}
	s.resetResponseIDsLocked()
}

func (s *Session) connect(c *gin.Context, driver Driver, info *relaycommon.RelayInfo, model string) error {
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
	dialStart := time.Now()
	conn, response, err := dialer.DialContext(c.Request.Context(), webSocketURL, headers)
	if err != nil {
		logger.LogError(c, "responses websocket handshake failed: "+err.Error())
		if response != nil && response.StatusCode >= http.StatusBadRequest && response.StatusCode != http.StatusUpgradeRequired {
			// A genuine upstream HTTP status reached us (e.g. 429 or 5xx). Surface it
			// so the relay applies its normal retry/failover and Retry-After handling
			// instead of masking a rate limit or outage behind an HTTP fallback. This
			// response belongs to the WebSocket handshake, not to response.create: no
			// application frame has been written, so the application attempt remains
			// replay-safe and an ordinary API-key channel may fail over. Keep the
			// explicit failure marker for channel health and subscription-OAuth retry
			// classification without marking the application response as started.
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
		// Either the upstream explicitly reported that this upgrade is unsupported
		// (426), returned a non-error HTTP response instead of 101, or the dial produced
		// no usable HTTP response at all — a malformed/HTTP-2 framed reply, a TLS error,
		// or a connection-level failure. In every one of these cases the endpoint does
		// not speak the Responses WebSocket protocol, so serve this turn and the rest
		// of the connection over HTTP rather than returning a misleading 2xx/3xx API
		// error or failing the turn hard.
		return ErrUpgradeRequired
	}
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	conn.EnableWriteCompression(false)
	conn.SetReadLimit(maxUpstreamMessageBytes)
	activity := &atomic.Int64{}
	activity.Store(time.Now().UnixNano())
	// Mirror gorilla's default ping handler while recording activity. The
	// permanent reader keeps this handler active between turns, so upstream
	// keepalive pings are answered without a second periodic ping goroutine.
	conn.SetPingHandler(func(message string) error {
		err := conn.WriteControl(websocket.PongMessage, []byte(message), time.Now().Add(controlFrameWriteTimeout))
		if err == nil {
			activity.Store(time.Now().UnixNano())
			return nil
		}

		logger.LogWarn(context.Background(), "responses websocket protocol: upstream_ping_received pong_error="+err.Error())
		if err == websocket.ErrCloseSent {
			return nil
		}
		if networkErr, ok := err.(net.Error); ok && networkErr.Temporary() {
			return nil
		}
		return err
	})
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
	s.model = model
	s.resetResponseIDsLocked()
	// Activity baseline and on-demand continuation-probe hook. Pong callbacks run
	// inside the permanent reader and deliberately avoid s.mu: doRequest holds that
	// lock while awaiting the probe acknowledgement.
	s.lastActivity = activity
	conn.SetPongHandler(func(message string) error {
		activity.Store(time.Now().UnixNano())
		s.probeMu.Lock()
		if s.probeAck != nil && s.probeNonce == message {
			ack := s.probeAck
			s.probeAck = nil
			s.probeNonce = ""
			close(ack)
		}
		s.probeMu.Unlock()
		return nil
	})
	stop := make(chan struct{})
	s.connStop = stop
	if MaxConnectionLifetime > 0 {
		lifetime := MaxConnectionLifetime
		s.lifetime = time.AfterFunc(lifetime, func() {
			s.mu.Lock()
			if s.conn == conn {
				logger.LogInfo(context.Background(), "responses websocket connection reached its lifetime; closing upstream connection")
				s.invalidateLocked(conn)
			}
			s.mu.Unlock()
		})
	}
	s.generation++
	generation := s.generation
	go s.runConnectionReader(conn, stop, activity, generation)
	driver.OnUpstreamConnected(c, info)
	// TEMP diagnostic: handshake latency. Grep "responses websocket timing".
	logger.LogInfo(c, fmt.Sprintf("responses websocket timing: handshake %dms", time.Since(dialStart).Milliseconds()))
	return nil
}

func (s *Session) probeContinuationLocked(c *gin.Context, conn *websocket.Conn) error {
	if s.lastActivity == nil {
		return errors.New("upstream connection has no activity state")
	}
	lastActivity := s.lastActivity.Load()
	if lastActivity > 0 && ContinuationLivenessFreshness > 0 &&
		time.Since(time.Unix(0, lastActivity)) <= ContinuationLivenessFreshness {
		return nil
	}

	s.probeSequence++
	nonce := fmt.Sprintf("continuation-%d", s.probeSequence)
	ack := make(chan struct{})
	s.probeMu.Lock()
	s.probeNonce = nonce
	s.probeAck = ack
	s.probeMu.Unlock()
	clearProbe := func() {
		s.probeMu.Lock()
		if s.probeAck == ack {
			s.probeAck = nil
			s.probeNonce = ""
		}
		s.probeMu.Unlock()
	}
	if err := conn.WriteControl(websocket.PingMessage, []byte(nonce), time.Now().Add(controlFrameWriteTimeout)); err != nil {
		clearProbe()
		return fmt.Errorf("write liveness ping: %w", err)
	}

	timeout := ContinuationProbeTimeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var requestDone <-chan struct{}
	if c != nil && c.Request != nil {
		requestDone = c.Request.Context().Done()
	}
	select {
	case <-ack:
		return nil
	case <-timer.C:
		clearProbe()
		return fmt.Errorf("liveness pong not received within %s", timeout)
	case <-requestDone:
		clearProbe()
		return c.Request.Context().Err()
	case <-s.connStop:
		clearProbe()
		return errors.New("upstream connection closed during liveness probe")
	}
}

// runConnectionReader is the connection's sole reader: every ReadMessage for
// this connection generation happens here, for the connection's whole lifetime.
// While a turn is active it routes business frames to that turn's consumer;
// while idle it consumes connection metadata and keeps gorilla processing
// upstream ping/pong control frames. It NEVER sets a read deadline: a timeout
// corrupts the connection, and with a shared reader that corruption would
// outlive the turn that caused it. All turn-level timeouts live on the consumer
// side.
func (s *Session) runConnectionReader(conn *websocket.Conn, stop <-chan struct{}, activity *atomic.Int64, generation uint64) {
	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			// A read error is terminal for this connection generation. Forget the
			// socket and its response ids atomically before notifying the consumer: a
			// full turn queue must never leave a dead connection visible as reusable.
			s.mu.Lock()
			if s.conn != conn {
				s.mu.Unlock()
				return
			}
			events, turnDone := s.turnEvents, s.turnDone
			s.invalidateLocked(conn)
			s.mu.Unlock()
			if events != nil {
				select {
				case events <- upstreamMessage{err: err}:
				case <-turnDone:
				}
			}
			return
		}
		activity.Store(time.Now().UnixNano())
		if messageType != websocket.TextMessage {
			continue
		}
		payload = bytes.TrimSpace(payload)
		if len(payload) == 0 {
			continue
		}
		eventType, class, terminal := classifyUpstreamEvent(payload)
		delivered, turnDone := s.deliverToTurn(conn, upstreamMessage{payload: payload, class: class})
		if !delivered {
			s.mu.Lock()
			current := s.conn == conn
			s.mu.Unlock()
			if !current {
				return
			}
			if class == upstreamConnectionMetadata {
				logger.LogInfo(context.Background(), fmt.Sprintf(
					"responses websocket protocol: idle-period connection event type=%q generation=%d discarded",
					eventType, generation,
				))
				continue
			}
			logger.LogWarn(context.Background(), fmt.Sprintf(
				"responses websocket protocol: idle-period turn event type=%q generation=%d; invalidating connection",
				eventType, generation,
			))
			s.invalidate(conn)
			return
		}
		if terminal {
			// Do not prefetch beyond the turn boundary. Wait until the consumer has
			// forwarded the terminal and deregistered this turn; late connection-level
			// metadata is then classified in the idle state instead of leaking into the
			// next turn or being mistaken for a protocol violation.
			select {
			case <-turnDone:
			case <-stop:
				return
			}
		}
	}
}

// deliverToTurn hands one message to the currently registered turn consumer,
// returning its completion signal when delivered. A false result means no turn
// is active (or this connection generation has already been replaced).
// If the turn ends while the reader is blocked handing over a frame, the
// turnDone close unblocks the send and the delivery re-evaluates against the
// (possibly cleared) registration.
func (s *Session) deliverToTurn(conn *websocket.Conn, msg upstreamMessage) (bool, <-chan struct{}) {
	for {
		s.mu.Lock()
		current := s.conn == conn
		events, done := s.turnEvents, s.turnDone
		s.mu.Unlock()
		if !current {
			return false, nil
		}
		if events == nil {
			return false, nil
		}
		select {
		case events <- msg:
			return true, done
		case <-done:
			// The turn ended mid-delivery; re-check the registration.
			continue
		}
	}
}

func classifyUpstreamEvent(payload []byte) (string, upstreamEventClass, bool) {
	var event struct {
		Type string `json:"type"`
	}
	if err := common.Unmarshal(payload, &event); err != nil {
		return "", upstreamTurnEvent, false
	}
	eventType := strings.TrimSpace(event.Type)
	if eventType == "" {
		return "", upstreamTurnEvent, false
	}
	if eventType == "error" {
		return eventType, upstreamTurnEvent, true
	}
	if !strings.HasPrefix(eventType, "response.") {
		// The Responses protocol may add connection-scoped extension events over
		// time. Treat any valid, typed non-response event as connection metadata
		// instead of maintaining a whitelist that would break on protocol growth.
		return eventType, upstreamConnectionMetadata, false
	}
	switch eventType {
	case "response.completed", "response.incomplete", "response.failed", "response.error", "response.done":
		return eventType, upstreamTurnEvent, true
	default:
		return eventType, upstreamTurnEvent, false
	}
}

// invalidate closes and forgets the connection. It acquires the session lock;
// callers already holding it must use invalidateLocked. The turn's capacity lease
// is released separately by its response body, not here.
func (s *Session) invalidate(conn *websocket.Conn) {
	s.mu.Lock()
	s.invalidateLocked(conn)
	s.mu.Unlock()
}

// emitEventAndMaybeStop normalizes one upstream text payload and writes it to the
// SSE stream, returning stop=true when the reader should exit. It invalidates the
// connection and closes the writer with the error on a malformed payload, a
// terminal failure event, or a downstream write failure; a successful
// response.completed or response.incomplete stops while keeping the connection
// for reuse.
func (s *Session) emitEventAndMaybeStop(conn *websocket.Conn, writer *io.PipeWriter, payload []byte) (string, bool, error) {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return "", false, nil
	}
	var event struct {
		Type     string `json:"type"`
		Response struct {
			ID string `json:"id"`
		} `json:"response"`
	}
	if err := common.Unmarshal(payload, &event); err != nil {
		s.invalidate(conn)
		_ = writer.CloseWithError(err)
		return "", true, err
	}
	if event.Type == "response.done" {
		normalized, normalizedType, err := NormalizeResponseDoneEvent(payload)
		if err != nil {
			s.invalidate(conn)
			_ = writer.CloseWithError(err)
			return event.Type, true, err
		}
		payload = normalized
		event.Type = normalizedType
		if err := common.Unmarshal(payload, &event); err != nil {
			s.invalidate(conn)
			_ = writer.CloseWithError(err)
			return event.Type, true, err
		}
	}
	if event.Response.ID != "" {
		if err := s.rememberResponseID(conn, event.Response.ID); err != nil {
			_ = writer.CloseWithError(err)
			return event.Type, true, err
		}
	}
	if _, err := fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", event.Type, payload); err != nil {
		// The downstream reader stopped consuming mid-stream. Invalidate the
		// connection because in-flight events remain unread and reusing it would
		// bleed stale events into the next response.
		s.invalidate(conn)
		return event.Type, true, err
	}
	switch event.Type {
	case "response.completed", "response.incomplete":
		// Clean terminal states: the turn ended and the persistent upstream keeps
		// the connection open for the next response.create, so stop reading but keep
		// it for reuse. response.incomplete (e.g. max output tokens or a content
		// filter) is as terminal as response.completed; omitting it here would leave
		// the read loop blocked on the idle upstream until idleTimeout, hanging the
		// turn and stalling the next one.
		return event.Type, true, nil
	case "response.failed", "response.error", "error":
		// Terminal failure: the connection may carry residual upstream state, so drop
		// it rather than risk bleeding events into a reused turn.
		s.invalidate(conn)
		return event.Type, true, nil
	}
	return event.Type, false, nil
}

func (s *Session) rememberResponseID(conn *websocket.Conn, responseID string) error {
	if responseID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != conn || s.httpFallback {
		return nil
	}
	if len(responseID) > maxTrackedResponseIDBytes {
		s.invalidateLocked(conn)
		return fmt.Errorf(
			"responses websocket response id exceeds the %d-byte limit",
			maxTrackedResponseIDBytes,
		)
	}
	if s.responseIDs == nil {
		s.responseIDs = make(map[string]struct{})
	}
	if _, exists := s.responseIDs[responseID]; exists {
		return nil
	}
	evictedID := ""
	evictedBytes := 0
	if len(s.responseIDOrder) >= maxTrackedResponseIDsPerConnection {
		evictedID = s.responseIDOrder[s.responseIDHead]
		evictedBytes = len(evictedID)
	}
	if s.responseIDBytes-evictedBytes > maxTrackedResponseIDTotalBytes-len(responseID) {
		s.invalidateLocked(conn)
		return fmt.Errorf(
			"responses websocket response ids exceed the %d-byte cumulative limit",
			maxTrackedResponseIDTotalBytes,
		)
	}
	if len(s.responseIDOrder) < maxTrackedResponseIDsPerConnection {
		s.responseIDOrder = append(s.responseIDOrder, responseID)
	} else {
		delete(s.responseIDs, evictedID)
		s.responseIDOrder[s.responseIDHead] = responseID
		s.responseIDHead = (s.responseIDHead + 1) % maxTrackedResponseIDsPerConnection
	}
	s.responseIDs[responseID] = struct{}{}
	s.responseIDBytes += len(responseID) - evictedBytes
	return nil
}

func (s *Session) resetResponseIDsLocked() {
	s.responseIDs = nil
	s.responseIDOrder = nil
	s.responseIDHead = 0
	s.responseIDBytes = 0
}

// streamAsSSE relays events from the permanent connection reader to the SSE
// pipe. reuseTurnDiag carries temporary first-event timing diagnostics.
type reuseTurnDiag struct {
	writeAt time.Time
	reused  bool
	connAge time.Duration
	idleGap time.Duration
}

func (s *Session) streamAsSSE(ctx context.Context, conn *websocket.Conn, writer *io.PipeWriter, events chan upstreamMessage, turnDone chan struct{}, done chan struct{}, diag *reuseTurnDiag) {
	defer writer.Close()
	defer close(done)
	defer func() {
		// Deregister this turn and return the connection to the idle pool. Clearing
		// the registration BEFORE closing turnDone lets a reader blocked mid-delivery
		// re-evaluate against the cleared state. Runs first (LIFO) so a follow-up
		// turn — which only starts after the pipe reader hits EOF from writer.Close —
		// observes the refreshed idle baseline.
		s.mu.Lock()
		if s.turnEvents == events {
			s.turnEvents = nil
			s.turnDone = nil
		}
		s.lastUsedAt = time.Now()
		s.mu.Unlock()
		close(turnDone)
	}()

	// Unblock a stalled turn when the request is cancelled: closing the connection
	// makes the permanent reader fail and deliver the error here, releasing the
	// capacity lease promptly.
	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				s.invalidate(conn)
			case <-done:
			}
		}()
	}

	// TEMP diagnostic: per-stream protocol summary (metadata only, no payloads).
	// first/last event type + whether a terminal was received distinguishes "clean
	// end" from "upstream died mid-turn". Grep "responses websocket protocol".
	firstEventType := ""
	lastEventType := ""
	var streamEndErr error
	defer func() {
		terminal := false
		switch lastEventType {
		case "response.completed", "response.incomplete", "response.failed", "response.error", "error":
			terminal = true
		}
		endErr := "none"
		if streamEndErr != nil {
			endErr = streamEndErr.Error()
		}
		logger.LogInfo(ctx, fmt.Sprintf(
			"responses websocket protocol: stream summary first_event=%q last_event=%q terminal_received=%t end_error=%q",
			firstEventType, lastEventType, terminal, endErr,
		))
	}()
	noteEvent := func(eventType string) {
		if eventType == "" {
			return
		}
		if firstEventType == "" {
			firstEventType = eventType
		}
		lastEventType = eventType
	}

	waitingForTurnEvent := true
	firstTurnEventDeadline := time.Now().Add(FirstEventTimeout)
	for {
		// Consumer-side timeouts (the permanent reader never sets read deadlines):
		// the first turn-scoped event is bounded by one absolute FirstEventTimeout.
		// Connection metadata may be forwarded but cannot extend that deadline.
		timeout := idleTimeout
		if waitingForTurnEvent {
			timeout = time.Until(firstTurnEventDeadline)
			if timeout <= 0 {
				timeout = time.Nanosecond
			}
		}
		timer := time.NewTimer(timeout)
		var msg upstreamMessage
		var timedOut bool
		select {
		case msg = <-events:
		case <-timer.C:
			timedOut = true
		}
		timer.Stop()
		if timedOut {
			if diag != nil && diag.reused {
				logger.LogWarn(ctx, fmt.Sprintf("responses websocket timing: reused=true conn_age=%dms idle_gap=%dms write_to_first_event FAILED after %dms: timeout",
					diag.connAge.Milliseconds(), diag.idleGap.Milliseconds(), time.Since(diag.writeAt).Milliseconds()))
			}
			bound := idleTimeout
			if waitingForTurnEvent {
				bound = FirstEventTimeout
			}
			err := fmt.Errorf("responses websocket upstream produced no event within %s", bound)
			streamEndErr = err
			s.invalidate(conn)
			_ = writer.CloseWithError(err)
			return
		}
		if msg.err != nil {
			if diag != nil && diag.reused && waitingForTurnEvent {
				logger.LogWarn(ctx, fmt.Sprintf("responses websocket timing: reused=true conn_age=%dms idle_gap=%dms write_to_first_event FAILED after %dms: %s",
					diag.connAge.Milliseconds(), diag.idleGap.Milliseconds(), time.Since(diag.writeAt).Milliseconds(), msg.err.Error()))
			}
			streamEndErr = msg.err
			s.invalidate(conn)
			_ = writer.CloseWithError(msg.err)
			return
		}
		if waitingForTurnEvent && msg.class == upstreamTurnEvent {
			waitingForTurnEvent = false
			if diag != nil {
				if diag.reused {
					logger.LogInfo(ctx, fmt.Sprintf("responses websocket timing: reused=true conn_age=%dms idle_gap=%dms write_to_first_event=%dms",
						diag.connAge.Milliseconds(), diag.idleGap.Milliseconds(), time.Since(diag.writeAt).Milliseconds()))
				} else {
					logger.LogInfo(ctx, fmt.Sprintf("responses websocket timing: first event %dms", time.Since(diag.writeAt).Milliseconds()))
				}
			}
		}
		eventType, stop, eventErr := s.emitEventAndMaybeStop(conn, writer, msg.payload)
		noteEvent(eventType)
		if eventErr != nil {
			streamEndErr = eventErr
		}
		if stop {
			return
		}
	}
}
