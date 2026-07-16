package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/google/uuid"
)

func FetchCodexWhamUsage(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	accessToken string,
	accountID string,
) (statusCode int, body []byte, err error) {
	return doCodexWhamRequest(ctx, client, http.MethodGet, baseURL, "/backend-api/wham/usage", accessToken, accountID, nil)
}

func FetchCodexWhamRateLimitResetCredits(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	accessToken string,
	accountID string,
) (statusCode int, body []byte, err error) {
	return doCodexWhamRequest(ctx, client, http.MethodGet, baseURL, "/backend-api/wham/rate-limit-reset-credits", accessToken, accountID, nil)
}

func ConsumeCodexWhamRateLimitResetCredit(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	accessToken string,
	accountID string,
) (statusCode int, body []byte, err error) {
	requestBody, err := common.Marshal(map[string]string{
		"redeem_request_id": uuid.NewString(),
	})
	if err != nil {
		return 0, nil, err
	}

	return doCodexWhamRequest(
		ctx,
		client,
		http.MethodPost,
		baseURL,
		"/backend-api/wham/rate-limit-reset-credits/consume",
		accessToken,
		accountID,
		bytes.NewReader(requestBody),
	)
}

const maxCodexWhamResponseBytes = 1 << 20

func doCodexWhamRequest(
	ctx context.Context,
	client *http.Client,
	method string,
	baseURL string,
	path string,
	accessToken string,
	accountID string,
	requestBody io.Reader,
) (statusCode int, body []byte, err error) {
	if client == nil {
		return 0, nil, fmt.Errorf("nil http client")
	}
	baseURL, err = normalizeCodexWhamBaseURL(baseURL)
	if err != nil {
		return 0, nil, err
	}
	accessToken = strings.TrimSpace(accessToken)
	accountID = strings.TrimSpace(accountID)
	if accessToken == "" {
		return 0, nil, fmt.Errorf("empty accessToken")
	}
	if accountID == "" {
		return 0, nil, fmt.Errorf("empty accountID")
	}

	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, requestBody)
	if err != nil {
		return 0, nil, err
	}
	setCodexWhamRequestHeaders(req, accessToken, accountID)
	if requestBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	body, err = io.ReadAll(io.LimitReader(resp.Body, maxCodexWhamResponseBytes+1))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if len(body) > maxCodexWhamResponseBytes {
		return resp.StatusCode, nil, fmt.Errorf("codex usage response exceeds %d bytes", maxCodexWhamResponseBytes)
	}
	return resp.StatusCode, body, nil
}

func normalizeCodexWhamBaseURL(baseURL string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	parsed, err := url.ParseRequestURI(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("invalid Codex baseURL")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("Codex baseURL must not contain user info")
	}

	for _, marker := range []string{"/backend-api/codex", "/backend-api/wham", "/backend-api"} {
		index := strings.Index(parsed.Path, marker)
		if index < 0 {
			continue
		}
		markerEnd := index + len(marker)
		if markerEnd != len(parsed.Path) && parsed.Path[markerEnd] != '/' {
			continue
		}
		parsed.Path = parsed.Path[:index]
		break
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func setCodexWhamRequestHeaders(req *http.Request, accessToken string, accountID string) {
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("chatgpt-account-id", accountID)
	req.Header.Set("Accept", "application/json")
	if req.Header.Get("originator") == "" {
		req.Header.Set("originator", "codex_cli_rs")
	}
}
