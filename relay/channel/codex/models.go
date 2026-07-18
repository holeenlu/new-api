package codex

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/service"
)

const (
	codexModelsClientVersion = "0.144.0-alpha.4"
	codexModelsUserAgent     = "codex_cli_rs/0.144.0-alpha.4 (Linux; amd64) Codex"
	maxCodexModelsBodyBytes  = 1 << 20
)

type UpstreamModel struct {
	Slug             string `json:"slug"`
	ID               string `json:"id"`
	Model            string `json:"model"`
	ContextWindow    int    `json:"context_window"`
	MaxContextWindow int    `json:"max_context_window"`
}

type upstreamModelsResponse struct {
	Models []UpstreamModel `json:"models"`
	Data   []UpstreamModel `json:"data"`
}

func (m UpstreamModel) Name() string {
	for _, name := range []string{m.Slug, m.ID, m.Model} {
		if name = strings.TrimSpace(name); name != "" {
			return name
		}
	}
	return ""
}

func (m UpstreamModel) Metadata() dto.UpstreamModelMetadata {
	contextWindow := m.ContextWindow
	maxContextWindow := m.MaxContextWindow
	if contextWindow <= 0 || contextWindow > dto.MaxUpstreamModelContextWindow {
		return dto.UpstreamModelMetadata{}
	}
	if maxContextWindow == 0 {
		maxContextWindow = contextWindow
	}
	if maxContextWindow < contextWindow || maxContextWindow > dto.MaxUpstreamModelContextWindow {
		return dto.UpstreamModelMetadata{}
	}
	return dto.UpstreamModelMetadata{
		ContextWindow:    contextWindow,
		MaxContextWindow: maxContextWindow,
		Complete:         true,
	}
}

// FetchUpstreamModelCatalog returns the complete model records advertised by
// this ChatGPT account. Duplicate names are collapsed conservatively.
func FetchUpstreamModelCatalog(ctx context.Context, client *http.Client, baseURL string, rawKey string) ([]UpstreamModel, error) {
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
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, service.ApplyChannelErrorPolicy(
			constant.ChannelTypeCodex,
			service.RelayErrorHandler(ctx, resp),
		)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCodexModelsBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxCodexModelsBodyBytes {
		return nil, fmt.Errorf("codex upstream model response exceeds %d bytes", maxCodexModelsBodyBytes)
	}

	var payload upstreamModelsResponse
	if err := common.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("codex upstream model response is invalid: %w", err)
	}
	models := payload.Models
	if len(models) == 0 {
		models = payload.Data
	}
	result := make([]UpstreamModel, 0, len(models))
	indexes := make(map[string]int, len(models))
	for _, item := range models {
		name := item.Name()
		if name == "" {
			continue
		}
		item.Slug = name
		metadata := item.Metadata()
		item.ContextWindow = metadata.ContextWindow
		item.MaxContextWindow = metadata.MaxContextWindow
		if index, ok := indexes[name]; ok {
			existing := result[index].Metadata()
			if !existing.Complete || !metadata.Complete {
				result[index].ContextWindow = 0
				result[index].MaxContextWindow = 0
			} else {
				result[index].ContextWindow = min(existing.ContextWindow, metadata.ContextWindow)
				result[index].MaxContextWindow = min(existing.MaxContextWindow, metadata.MaxContextWindow)
			}
			continue
		}
		indexes[name] = len(result)
		result = append(result, item)
	}
	return result, nil
}

// FetchUpstreamModels preserves the existing names-only API used by model
// synchronization while the richer catalog is cached separately.
func FetchUpstreamModels(ctx context.Context, client *http.Client, baseURL string, rawKey string) ([]string, error) {
	catalog, err := FetchUpstreamModelCatalog(ctx, client, baseURL, rawKey)
	if err != nil {
		return nil, err
	}
	models := make([]string, 0, len(catalog))
	for _, item := range catalog {
		models = append(models, item.Name())
	}
	return models, nil
}
