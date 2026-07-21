package codex

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/responsesws"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

// codexResponsesWebSocketBeta is the OpenAI-Beta value the ChatGPT/Codex
// subscription backend requires for the Responses WebSocket handshake.
const codexResponsesWebSocketBeta = "responses_websockets=2026-02-06"

// The Codex adaptor drives the shared Responses WebSocket session against the
// ChatGPT subscription backend (OAuth credential + private Codex path).
var _ responsesws.Driver = (*Adaptor)(nil)

// DialUpstream implements responsesws.Driver.
func (a *Adaptor) DialUpstream(c *gin.Context, info *relaycommon.RelayInfo) (string, http.Header, error) {
	httpURL, err := a.GetRequestURL(info)
	if err != nil {
		return "", nil, err
	}
	webSocketURL := httpURL
	switch {
	case strings.HasPrefix(webSocketURL, "https://"):
		webSocketURL = "wss://" + strings.TrimPrefix(webSocketURL, "https://")
	case strings.HasPrefix(webSocketURL, "http://"):
		webSocketURL = "ws://" + strings.TrimPrefix(webSocketURL, "http://")
	default:
		return "", nil, fmt.Errorf("codex responses websocket: unsupported URL scheme")
	}

	headers := make(http.Header)
	if err := a.SetupRequestHeader(c, &headers, info); err != nil {
		return "", nil, err
	}
	headers.Set("OpenAI-Beta", codexResponsesWebSocketBeta)
	headers.Del("Accept")
	headers.Del("Content-Type")
	return webSocketURL, headers, nil
}

// AcquireCapacity implements responsesws.Driver.
func (a *Adaptor) AcquireCapacity(c *gin.Context, info *relaycommon.RelayInfo) (*service.SubscriptionOAuthLease, error) {
	return acquireCodexOAuthCapacity(c, info)
}

// DoHTTPFallback implements responsesws.Driver.
func (a *Adaptor) DoHTTPFallback(c *gin.Context, info *relaycommon.RelayInfo, body []byte, reuseLease *service.SubscriptionOAuthLease) (*http.Response, error) {
	if reuseLease != nil {
		return doCodexHTTPResponseRequestWithLease(c, a, info, bytes.NewReader(body), reuseLease)
	}
	return doCodexHTTPResponseRequest(c, a, info, bytes.NewReader(body))
}

// OnUpstreamConnected implements responsesws.Driver. Session pinning is owned
// by responsesws.Session; the relay remains free to fail over before any
// downstream event is committed.
func (a *Adaptor) OnUpstreamConnected(_ *gin.Context, _ *relaycommon.RelayInfo) {}
