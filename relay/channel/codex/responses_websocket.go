package codex

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
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

const (
	codexResponsesWebSocketSessionKey = "codex_responses_websocket_session"
	codexResponsesWebSocketBeta       = "responses_websockets=2026-02-06"
	maxResponsesWebSocketRequestBytes = 16 << 20
	maxResponsesWebSocketErrorBytes   = 64 << 10
)

var errCodexResponsesWebSocketUpgradeRequired = errors.New("codex responses websocket upgrade required")

type ResponsesWebSocketSession struct {
	mu           sync.Mutex
	conn         *websocket.Conn
	channelID    int
	model        string
	httpFallback bool
	release      func()
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
	if s.release != nil {
		s.release()
		s.release = nil
	}
	return err
}

func (s *ResponsesWebSocketSession) doRequest(c *gin.Context, adaptor *Adaptor, info *relaycommon.RelayInfo, requestBody io.Reader) (*http.Response, error) {
	if s == nil || c == nil || adaptor == nil || info == nil || requestBody == nil {
		return nil, errors.New("codex responses websocket: invalid request")
	}
	body, err := io.ReadAll(io.LimitReader(requestBody, maxResponsesWebSocketRequestBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxResponsesWebSocketRequestBytes {
		return nil, errors.New("codex responses websocket request is too large")
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
		return doCodexHTTPResponseRequest(c, adaptor, info, bytes.NewReader(httpBody))
	}
	if s.conn == nil {
		if err := s.connect(c, adaptor, info); err != nil {
			if errors.Is(err, errCodexResponsesWebSocketUpgradeRequired) {
				s.channelID = info.ChannelId
				s.model = model
				s.httpFallback = true
				return doCodexHTTPResponseRequest(c, adaptor, info, bytes.NewReader(httpBody))
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

func (s *ResponsesWebSocketSession) invalidateLocked(conn *websocket.Conn) {
	if s.conn != conn {
		return
	}
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
	if s.release != nil {
		s.release()
		s.release = nil
	}
}

func (s *ResponsesWebSocketSession) connect(c *gin.Context, adaptor *Adaptor, info *relaycommon.RelayInfo) error {
	release, acquired := acquireCodexOAuthSlot(info.ChannelId)
	if !acquired {
		return errors.New("codex OAuth channel concurrency limit reached; retry later")
	}
	if err := waitForCodexOAuthTurn(c.Request.Context(), info.ChannelId); err != nil {
		release()
		return err
	}

	httpURL, err := adaptor.GetRequestURL(info)
	if err != nil {
		release()
		return err
	}
	webSocketURL := httpURL
	switch {
	case strings.HasPrefix(webSocketURL, "https://"):
		webSocketURL = "wss://" + strings.TrimPrefix(webSocketURL, "https://")
	case strings.HasPrefix(webSocketURL, "http://"):
		webSocketURL = "ws://" + strings.TrimPrefix(webSocketURL, "http://")
	default:
		release()
		return fmt.Errorf("codex responses websocket: unsupported URL scheme")
	}

	headers := make(http.Header)
	if err := adaptor.SetupRequestHeader(c, &headers, info); err != nil {
		release()
		return err
	}
	headers.Set("OpenAI-Beta", codexResponsesWebSocketBeta)
	headers.Del("Accept")
	headers.Del("Content-Type")

	client, err := service.GetHttpClientWithProxy(info.ChannelSetting.Proxy)
	if err != nil {
		release()
		return err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 30 * time.Second, EnableCompression: true}
	if transport, ok := client.Transport.(*http.Transport); ok && transport != nil {
		dialer.Proxy = transport.Proxy
		dialer.NetDialContext = transport.DialContext
		dialer.TLSClientConfig = transport.TLSClientConfig
	}
	conn, response, err := dialer.DialContext(c.Request.Context(), webSocketURL, headers)
	var responseBody []byte
	if response != nil && response.Body != nil {
		responseBody, _ = io.ReadAll(io.LimitReader(response.Body, maxResponsesWebSocketErrorBytes+1))
		response.Body.Close()
	}
	if err != nil {
		release()
		if response != nil && response.StatusCode == http.StatusUpgradeRequired {
			return errCodexResponsesWebSocketUpgradeRequired
		}
		if response != nil && response.StatusCode > 0 {
			detail := strings.TrimSpace(string(responseBody))
			if len(responseBody) > maxResponsesWebSocketErrorBytes {
				detail += " (truncated)"
			}
			if detail != "" {
				return fmt.Errorf("codex responses websocket dial failed: status=%d: %s", response.StatusCode, detail)
			}
			return fmt.Errorf("codex responses websocket dial failed: status=%d: %w", response.StatusCode, err)
		}
		return fmt.Errorf("codex responses websocket dial failed: %w", err)
	}
	conn.EnableWriteCompression(false)
	s.conn = conn
	s.channelID = info.ChannelId
	s.model = strings.TrimSpace(info.UpstreamModelName)
	s.release = release
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
