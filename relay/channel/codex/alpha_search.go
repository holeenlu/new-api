package codex

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"

	rootcommon "github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

const (
	maxAlphaSearchRequestBytes  = 16 << 20
	maxAlphaSearchResponseBytes = 32 << 20
)

func SanitizeAlphaSearchBody(body []byte) ([]byte, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, errors.New("request body is required")
	}
	if len(body) > maxAlphaSearchRequestBytes {
		return nil, errors.New("search request body is too large")
	}
	var payload map[string]any
	if err := rootcommon.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	delete(payload, "prompt_cache_key")
	delete(payload, "prompt_cache_retention")
	return rootcommon.Marshal(payload)
}

func DoAlphaSearch(c *gin.Context, info *relaycommon.RelayInfo, body []byte) (*http.Response, error) {
	if c == nil || info == nil {
		return nil, errors.New("codex alpha search: invalid relay context")
	}
	body, _, err := relaycommon.FilterUpstreamLocationData(body, info.ChannelSetting.Proxy != "")
	if err != nil {
		return nil, fmt.Errorf("filter Codex alpha search location data: %w", err)
	}
	lease, err := acquireCodexOAuthCapacity(c, info)
	if err != nil {
		return nil, err
	}
	service.BindSubscriptionOAuthResponseLease(c, lease)

	oauthKey, err := ParseOAuthKey(strings.TrimSpace(info.ApiKey))
	if err != nil {
		lease.Abandon()
		return nil, err
	}
	if strings.TrimSpace(oauthKey.AccessToken) == "" || strings.TrimSpace(oauthKey.AccountID) == "" {
		lease.Abandon()
		return nil, errors.New("codex alpha search: OAuth access_token and account_id are required")
	}

	requestURL := relaycommon.GetFullRequestURL(
		info.ChannelBaseUrl,
		"/backend-api/codex/alpha/search",
		info.ChannelType,
	)
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, requestURL, bytes.NewReader(body))
	if err != nil {
		lease.Abandon()
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(oauthKey.AccessToken))
	req.Header.Set("ChatGPT-Account-ID", strings.TrimSpace(oauthKey.AccountID))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Originator", CodexOAuthOriginator)
	req.Header.Set("User-Agent", CodexOAuthUserAgent)
	for _, name := range []string{
		"Version",
		"Session_id",
		"X-Session-ID",
		"X-Client-Request-Id",
	} {
		if value := strings.TrimSpace(c.GetHeader(name)); value != "" {
			req.Header.Set(name, value)
		}
	}

	timeout := time.Duration(rootcommon.SubscriptionOAuthResponseHeaderTimeout) * time.Second
	client, err := service.GetHttpClientWithResponseHeaderTimeout(info.ChannelSetting.Proxy, timeout)
	if err != nil {
		lease.Abandon()
		return nil, err
	}
	trace := &httptrace.ClientTrace{
		WroteHeaders:         info.MarkUpstreamRequestWritten,
		GotFirstResponseByte: info.MarkUpstreamResponseStarted,
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := client.Do(req)
	if err != nil {
		written, _ := info.UpstreamAttemptState()
		if written {
			lease.Release()
		} else {
			lease.Abandon()
		}
		return nil, err
	}
	if resp == nil || resp.Body == nil {
		written, _ := info.UpstreamAttemptState()
		if written {
			lease.Release()
		} else {
			lease.Abandon()
		}
		return nil, errors.New("codex alpha search: upstream returned an empty response")
	}
	resp.Body = &codexOAuthResponseBody{ReadCloser: resp.Body, lease: lease}
	return resp, nil
}

func ReadAlphaSearchResponse(resp *http.Response) ([]byte, error) {
	if resp == nil || resp.Body == nil {
		return nil, errors.New("codex alpha search: upstream response is empty")
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxAlphaSearchResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read Codex alpha search response: %w", err)
	}
	if len(payload) > maxAlphaSearchResponseBytes {
		return nil, errors.New("codex alpha search: upstream response is too large")
	}
	return payload, nil
}
