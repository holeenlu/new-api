package codex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

const (
	codexResponsesWebSocketSessionKey = "codex_responses_websocket_session"
	codexResponsesWebSocketBeta       = "responses_websockets=2026-02-06"
)

var errCodexResponsesWebSocketUpgradeRequired = errors.New("codex responses websocket upgrade required")

type ResponsesWebSocketSession struct {
	mu            sync.Mutex
	conn          *websocket.Conn
	channelID     int
	model         string
	httpFallback  bool
	lease         *service.SubscriptionOAuthLease
	fallbackLease *service.SubscriptionOAuthLease
	pendingID     int
	pendingModel  string
}

func SetResponsesWebSocketSession(c *gin.Context, session *ResponsesWebSocketSession) {
	if c != nil && session != nil {
		c.Set(codexResponsesWebSocketSessionKey, session)
	}
}

func responsesWebSocketSessionFromContext(c *gin.Context) *ResponsesWebSocketSession {
	if c == nil {
		return nil
	}
	value, exists := c.Get(codexResponsesWebSocketSessionKey)
	if !exists {
		return nil
	}
	session, _ := value.(*ResponsesWebSocketSession)
	return session
}

func (s *ResponsesWebSocketSession) Close() error {
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

func (s *ResponsesWebSocketSession) ChannelID() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.channelID
}

func (s *ResponsesWebSocketSession) ConfirmHTTPFallbackSuccess() {
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

func (s *ResponsesWebSocketSession) doRequest(c *gin.Context, adaptor *Adaptor, info *relaycommon.RelayInfo, requestBody io.Reader) (*http.Response, error) {
	if s == nil || c == nil || adaptor == nil || info == nil || requestBody == nil {
		return nil, errors.New("codex responses websocket: invalid request")
	}
	requestLimit := common.MaxRequestBodyBytes()
	body, err := io.ReadAll(io.LimitReader(requestBody, requestLimit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > requestLimit {
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("codex responses websocket request exceeds %d MB: %w", requestLimit>>20, common.ErrRequestBodyTooLarge),
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
		return nil, errors.New("codex responses websocket cannot switch OAuth channels within one session")
	}
	model := strings.TrimSpace(info.UpstreamModelName)
	if s.model != "" && !strings.EqualFold(s.model, model) {
		return nil, errors.New("codex responses websocket cannot switch models within one session")
	}
	if s.httpFallback {
		return s.doHTTPFallbackLocked(c, adaptor, info, model, httpBody)
	}
	if s.conn == nil {
		if err := s.connect(c, adaptor, info); err != nil {
			if errors.Is(err, errCodexResponsesWebSocketUpgradeRequired) {
				s.httpFallback = true
				return s.doHTTPFallbackLocked(c, adaptor, info, model, httpBody)
			}
			return nil, err
		}
	}
	if err := s.conn.WriteMessage(websocket.TextMessage, body); err != nil {
		s.invalidateLocked(s.conn)
		return nil, err
	}

	reader, writer := io.Pipe()
	conn := s.conn
	go s.streamAsSSE(c.Request.Context(), conn, writer)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: reader,
	}, nil
}

func (s *ResponsesWebSocketSession) doHTTPFallbackLocked(
	c *gin.Context,
	adaptor *Adaptor,
	info *relaycommon.RelayInfo,
	model string,
	body []byte,
) (*http.Response, error) {
	var response *http.Response
	var err error
	if s.fallbackLease != nil {
		lease := s.fallbackLease
		s.fallbackLease = nil
		response, err = doCodexHTTPResponseRequestWithLease(c, adaptor, info, bytes.NewReader(body), lease)
	} else {
		response, err = doCodexHTTPResponseRequest(c, adaptor, info, bytes.NewReader(body))
	}
	if err != nil || response == nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		s.pendingID = 0
		s.pendingModel = ""
		return response, err
	}
	s.pendingID = info.ChannelId
	s.pendingModel = model
	return response, nil
}

func (s *ResponsesWebSocketSession) invalidateLocked(conn *websocket.Conn) {
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

func (s *ResponsesWebSocketSession) connect(c *gin.Context, adaptor *Adaptor, info *relaycommon.RelayInfo) error {
	lease, err := acquireCodexOAuthCapacity(c, info)
	if err != nil {
		return err
	}

	httpURL, err := adaptor.GetRequestURL(info)
	if err != nil {
		lease.Abandon()
		return err
	}
	webSocketURL := httpURL
	switch {
	case strings.HasPrefix(webSocketURL, "https://"):
		webSocketURL = "wss://" + strings.TrimPrefix(webSocketURL, "https://")
	case strings.HasPrefix(webSocketURL, "http://"):
		webSocketURL = "ws://" + strings.TrimPrefix(webSocketURL, "http://")
	default:
		lease.Abandon()
		return fmt.Errorf("codex responses websocket: unsupported URL scheme")
	}

	headers := make(http.Header)
	if err := adaptor.SetupRequestHeader(c, &headers, info); err != nil {
		lease.Abandon()
		return err
	}
	headers.Set("OpenAI-Beta", codexResponsesWebSocketBeta)
	headers.Del("Accept")
	headers.Del("Content-Type")

	client, err := service.GetHttpClientWithProxy(info.ChannelSetting.Proxy)
	if err != nil {
		lease.Abandon()
		return err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 30 * time.Second, EnableCompression: true}
	if transport, ok := client.Transport.(*http.Transport); ok && transport != nil {
		dialer.Proxy = transport.Proxy
		dialer.NetDialContext = transport.DialContext
		dialer.TLSClientConfig = transport.TLSClientConfig
	}
	info.MarkUpstreamRequestWritten()
	conn, response, err := dialer.DialContext(c.Request.Context(), webSocketURL, headers)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil {
		logger.LogError(c, "codex responses websocket handshake failed: "+err.Error())
		if response != nil && response.StatusCode == http.StatusUpgradeRequired {
			s.fallbackLease = lease
			return errCodexResponsesWebSocketUpgradeRequired
		}
		lease.Release()
		if response != nil && response.StatusCode > 0 {
			info.MarkUpstreamResponseStarted()
			info.MarkUpstreamFailureResponse()
			apiError := types.NewErrorWithStatusCode(
				fmt.Errorf("codex responses websocket handshake failed with status %d", response.StatusCode),
				types.ErrorCodeBadResponseStatusCode,
				response.StatusCode,
			)
			apiError.UpstreamStatusCode = response.StatusCode
			apiError.RetryAfter = service.ParseRetryAfterHeader(response.Header.Get("Retry-After"), time.Now())
			return apiError
		}
		return types.NewErrorWithStatusCode(
			errors.New("codex responses websocket connection failed"),
			types.ErrorCodeDoRequestFailed,
			http.StatusBadGateway,
		)
	}
	conn.EnableWriteCompression(false)
	s.conn = conn
	s.channelID = info.ChannelId
	s.model = strings.TrimSpace(info.UpstreamModelName)
	s.lease = lease
	common.SetContextKey(c, constant.ContextKeyTokenSpecificChannelId, strconv.Itoa(info.ChannelId))
	service.DisableSubscriptionOAuthRetry(c)
	return nil
}

func (s *ResponsesWebSocketSession) streamAsSSE(ctx context.Context, conn *websocket.Conn, writer *io.PipeWriter) {
	defer writer.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
	defer conn.SetReadDeadline(time.Time{})
	for {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				_ = writer.CloseWithError(err)
				return
			}
		}
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			s.mu.Lock()
			s.invalidateLocked(conn)
			s.mu.Unlock()
			_ = writer.CloseWithError(err)
			return
		}
		if messageType != websocket.TextMessage {
			continue
		}
		payload = bytes.TrimSpace(payload)
		if len(payload) == 0 {
			continue
		}
		var event struct {
			Type string `json:"type"`
		}
		if err := common.Unmarshal(payload, &event); err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		if event.Type == "response.done" {
			var normalized map[string]any
			if err := common.Unmarshal(payload, &normalized); err != nil {
				_ = writer.CloseWithError(err)
				return
			}
			normalized["type"] = "response.completed"
			payload, err = common.Marshal(normalized)
			if err != nil {
				_ = writer.CloseWithError(err)
				return
			}
			event.Type = "response.completed"
		}
		if _, err := fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", event.Type, payload); err != nil {
			return
		}
		if event.Type == "response.completed" || event.Type == "error" {
			if event.Type == "error" {
				s.mu.Lock()
				s.invalidateLocked(conn)
				s.mu.Unlock()
			}
			return
		}
	}
}
