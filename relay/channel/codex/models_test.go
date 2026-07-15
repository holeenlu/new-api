package codex

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

func TestFetchUpstreamModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/backend-api/codex/models", r.URL.Path)
		require.Equal(t, "Bearer access-token", r.Header.Get("Authorization"))
		require.Equal(t, "account-id", r.Header.Get("Chatgpt-Account-Id"))
		require.Equal(t, "codex_cli_rs", r.Header.Get("Originator"))
		require.Equal(t, codexModelsClientVersion, r.URL.Query().Get("client_version"))
		require.Equal(t, codexModelsUserAgent, r.Header.Get("User-Agent"))
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.6-sol"},{"id":"gpt-5.5"},{"slug":"gpt-5.6-sol"}]}`))
	}))
	defer server.Close()

	models, err := FetchUpstreamModels(context.Background(), server.Client(), server.URL, `{"access_token":"access-token","account_id":"account-id"}`)

	require.NoError(t, err)
	require.Equal(t, []string{"gpt-5.6-sol", "gpt-5.5"}, models)
}

func TestFetchUpstreamModelsClassifiesForbiddenResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"access-token-secret"}`))
	}))
	defer server.Close()

	_, err := FetchUpstreamModels(context.Background(), server.Client(), server.URL, `{"access_token":"access-token-secret","account_id":"account-id"}`)

	var apiErr *types.NewAPIError
	require.True(t, errors.As(err, &apiErr))
	require.Equal(t, types.ErrorCodeOAuthForbidden, apiErr.GetErrorCode())
	require.NotContains(t, err.Error(), "access-token-secret")
}
