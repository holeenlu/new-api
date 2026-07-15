package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/types"
)

const (
	codexModelsClientVersion = "0.144.0-alpha.4"
	codexModelsUserAgent     = "codex_cli_rs/0.144.0-alpha.4 (Linux; amd64) Codex"
)

type upstreamModel struct {
	Slug  string `json:"slug"`
	ID    string `json:"id"`
	Model string `json:"model"`
}

type upstreamModelsResponse struct {
	Models []upstreamModel `json:"models"`
	Data   []upstreamModel `json:"data"`
}

// FetchUpstreamModels returns the models available to this ChatGPT account.
func FetchUpstreamModels(ctx context.Context, client *http.Client, baseURL string, rawKey string) ([]string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	oauthKey, err := ParseOAuthKey(strings.TrimSpace(rawKey))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(oauthKey.AccessToken) == "" || strings.TrimSpace(oauthKey.AccountID) == "" {
		return nil, fmt.Errorf("codex channel: access_token and account_id are required")
	}

	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "https://chatgpt.com"
	}
	if !strings.HasSuffix(baseURL, "/backend-api/codex") {
		if strings.HasSuffix(baseURL, "/backend-api") {
			baseURL += "/codex"
		} else {
			baseURL += "/backend-api/codex"
		}
	}
	modelsURL, err := url.Parse(baseURL + "/models")
	if err != nil {
		return nil, err
	}
	query := modelsURL.Query()
	query.Set("client_version", codexModelsClientVersion)
	modelsURL.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(oauthKey.AccessToken))
	req.Header.Set("chatgpt-account-id", strings.TrimSpace(oauthKey.AccountID))
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("originator", "codex_cli_rs")
	req.Header.Set("User-Agent", codexModelsUserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		detail := strings.TrimSpace(string(body))
		if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
			detail = "upstream rejected the model discovery request"
		}
		if len(detail) > 512 {
			detail = detail[:512]
		}
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return nil, types.NewErrorWithStatusCode(errors.New("OAuth credential is invalid or expired"), types.ErrorCodeOAuthUnauthorized, resp.StatusCode)
		case http.StatusForbidden:
			return nil, types.NewErrorWithStatusCode(errors.New("OAuth account is not permitted to discover models"), types.ErrorCodeOAuthForbidden, resp.StatusCode)
		default:
			return nil, types.NewErrorWithStatusCode(
				fmt.Errorf("codex upstream model request failed: status=%d: %s", resp.StatusCode, common.RedactSensitiveCredentials(detail)),
				types.ErrorCodeBadResponseStatusCode,
				resp.StatusCode,
			)
		}
	}

	var payload upstreamModelsResponse
	if err := common.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("codex upstream model response is invalid: %w", err)
	}
	models := payload.Models
	if len(models) == 0 {
		models = payload.Data
	}
	result := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		name := strings.TrimSpace(model.Slug)
		if name == "" {
			name = strings.TrimSpace(model.ID)
		}
		if name == "" {
			name = strings.TrimSpace(model.Model)
		}
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	return result, nil
}
