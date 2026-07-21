package openai

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/responsesws"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

// The OpenAI-compatible adaptor drives the shared Responses WebSocket session
// against a standard `<base>/responses` endpoint using Bearer (or Azure api-key)
// auth and no per-request capacity lease.
var _ responsesws.Driver = (*Adaptor)(nil)

// DialUpstream implements responsesws.Driver.
func (a *Adaptor) DialUpstream(c *gin.Context, info *relaycommon.RelayInfo) (string, http.Header, error) {
	httpURL, err := a.GetRequestURL(info)
	if err != nil {
		return "", nil, err
	}
	wsURL := httpURL
	switch {
	case strings.HasPrefix(wsURL, "https://"):
		wsURL = "wss://" + strings.TrimPrefix(wsURL, "https://")
	case strings.HasPrefix(wsURL, "http://"):
		wsURL = "ws://" + strings.TrimPrefix(wsURL, "http://")
	default:
		return "", nil, fmt.Errorf("openai responses websocket: unsupported URL scheme")
	}
	headers := make(http.Header)
	if err := a.SetupRequestHeader(c, &headers, info); err != nil {
		return "", nil, err
	}
	headers.Del("Accept")
	headers.Del("Content-Type")
	return wsURL, headers, nil
}

// AcquireCapacity implements responsesws.Driver. Standard API-key channels have
// no per-request capacity lease, so it returns a nil lease.
func (a *Adaptor) AcquireCapacity(_ *gin.Context, _ *relaycommon.RelayInfo) (*service.SubscriptionOAuthLease, error) {
	return nil, nil
}

// DoHTTPFallback implements responsesws.Driver by issuing the ordinary HTTP
// Responses request. There is no lease to reuse for standard channels.
func (a *Adaptor) DoHTTPFallback(c *gin.Context, info *relaycommon.RelayInfo, body []byte, _ *service.SubscriptionOAuthLease) (*http.Response, error) {
	return channel.DoApiRequest(a, c, info, bytes.NewReader(body))
}

// OnUpstreamConnected implements responsesws.Driver by pinning the channel for
// the connection's lifetime. Standard channels have no subscription-OAuth retry
// policy to disable.
func (a *Adaptor) OnUpstreamConnected(c *gin.Context, info *relaycommon.RelayInfo) {
	common.SetContextKey(c, constant.ContextKeyTokenSpecificChannelId, strconv.Itoa(info.ChannelId))
}
