package codex

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/dto"
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

func TestFetchUpstreamModelCatalogPreservesConservativeContextMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[
			{"slug":"gpt-5.6-sol","context_window":1000000,"max_context_window":1000000},
			{"id":"gpt-5.6-sol","context_window":800000,"max_context_window":900000},
			{"slug":"gpt-invalid","context_window":1000000,"max_context_window":400000}
		]}`))
	}))
	defer server.Close()

	catalog, err := FetchUpstreamModelCatalog(
		context.Background(),
		server.Client(),
		server.URL,
		`{"access_token":"access-token","account_id":"account-id"}`,
	)

	require.NoError(t, err)
	require.Len(t, catalog, 2)
	require.Equal(t, "gpt-5.6-sol", catalog[0].Name())
	require.Equal(t, 800000, catalog[0].ContextWindow)
	require.Equal(t, 900000, catalog[0].MaxContextWindow)
	require.True(t, catalog[0].Metadata().Complete)
	require.Equal(t, "gpt-invalid", catalog[1].Name())
	require.False(t, catalog[1].Metadata().Complete)
}

func TestFetchUpstreamModelCatalogFailsClosedForIncompleteDuplicate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[
			{"slug":"gpt-5.6-sol","context_window":1000000,"max_context_window":1000000},
			{"id":"gpt-5.6-sol"}
		]}`))
	}))
	defer server.Close()

	catalog, err := FetchUpstreamModelCatalog(
		context.Background(),
		server.Client(),
		server.URL,
		`{"access_token":"access-token","account_id":"account-id"}`,
	)

	require.NoError(t, err)
	require.Len(t, catalog, 1)
	require.False(t, catalog[0].Metadata().Complete)
}

func TestUpstreamModelMetadataValidation(t *testing.T) {
	tests := []struct {
		name     string
		model    UpstreamModel
		complete bool
		maximum  int
	}{
		{name: "missing context", model: UpstreamModel{MaxContextWindow: 1_000_000}},
		{name: "inverted windows", model: UpstreamModel{ContextWindow: 1_000_000, MaxContextWindow: 500_000}},
		{name: "oversized context", model: UpstreamModel{ContextWindow: dto.MaxUpstreamModelContextWindow + 1}},
		{name: "missing maximum uses active window", model: UpstreamModel{ContextWindow: 400_000}, complete: true, maximum: 400_000},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metadata := test.model.Metadata()
			require.Equal(t, test.complete, metadata.Complete)
			require.Equal(t, test.maximum, metadata.MaxContextWindow)
		})
	}
}

func TestFetchUpstreamModelsClassifiesForbiddenResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "9")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"access-token-secret"}`))
	}))
	defer server.Close()

	_, err := FetchUpstreamModels(context.Background(), server.Client(), server.URL, `{"access_token":"access-token-secret","account_id":"account-id"}`)

	var apiErr *types.NewAPIError
	require.True(t, errors.As(err, &apiErr))
	require.Equal(t, types.ErrorCodeOAuthForbidden, apiErr.GetErrorCode())
	require.Equal(t, http.StatusForbidden, apiErr.GetUpstreamStatusCode())
	require.Equal(t, 9*time.Second, apiErr.RetryAfter)
	require.NotContains(t, err.Error(), "access-token-secret")
}

func TestFetchUpstreamModelsRejectsOversizedSuccessBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxCodexModelsBodyBytes+1)))
	}))
	defer server.Close()

	_, err := FetchUpstreamModels(
		context.Background(),
		server.Client(),
		server.URL,
		`{"access_token":"access-token","account_id":"account-id"}`,
	)

	require.ErrorContains(t, err, "response exceeds")
}
