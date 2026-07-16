package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

func TestCreateCodexOAuthAuthorizationFlowUsesEnvironmentConfiguration(t *testing.T) {
	t.Setenv("CODEX_OAUTH_CLIENT_ID", "configured-client")
	t.Setenv("CODEX_OAUTH_REDIRECT_URI", "https://admin.example.com/oauth/callback")
	t.Setenv("CODEX_OAUTH_SCOPE", "openid   offline_access custom.scope")

	flow, err := CreateCodexOAuthAuthorizationFlow()
	require.NoError(t, err)
	authorizeURL, err := url.Parse(flow.AuthorizeURL)
	require.NoError(t, err)
	require.Equal(t, "configured-client", authorizeURL.Query().Get("client_id"))
	require.Equal(t, "https://admin.example.com/oauth/callback", authorizeURL.Query().Get("redirect_uri"))
	require.Equal(t, "openid offline_access custom.scope", authorizeURL.Query().Get("scope"))
}

func TestCreateCodexOAuthAuthorizationFlowRejectsInvalidRedirectURI(t *testing.T) {
	t.Setenv("CODEX_OAUTH_REDIRECT_URI", "not-an-absolute-url")

	_, err := CreateCodexOAuthAuthorizationFlow()

	require.ErrorContains(t, err, "CODEX_OAUTH_REDIRECT_URI")
}

func TestRefreshCodexOAuthTokenClassifiesUnauthorizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	_, err := refreshCodexOAuthToken(context.Background(), server.Client(), server.URL, "client", "refresh-secret")

	var upstreamErr *CodexOAuthUpstreamError
	require.ErrorAs(t, err, &upstreamErr)
	require.Equal(t, types.ErrorCodeOAuthUnauthorized, upstreamErr.Code)
	require.NotContains(t, err.Error(), "refresh-secret")
}

func TestRefreshCodexOAuthTokenKeepsExistingRefreshTokenWhenNotRotated(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		require.Equal(t, "refresh-secret", r.Form.Get("refresh_token"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"next-access","expires_in":3600}`))
	}))
	defer server.Close()

	result, err := refreshCodexOAuthToken(context.Background(), server.Client(), server.URL, "client", "refresh-secret")

	require.NoError(t, err)
	require.Equal(t, "next-access", result.AccessToken)
	require.Equal(t, "refresh-secret", result.RefreshToken)
}

func TestCodexOAuthAuthorizationFailureClassification(t *testing.T) {
	t.Run("rejected authorization code", func(t *testing.T) {
		err := newCodexOAuthUpstreamError(http.StatusBadRequest, "code exchange")
		var upstreamErr *CodexOAuthUpstreamError
		require.ErrorAs(t, err, &upstreamErr)
		require.Equal(t, types.ErrorCodeInvalidRequest, upstreamErr.Code)
		require.Contains(t, upstreamErr.Message, "redirect URI")
	})

	t.Run("rejected credential refresh", func(t *testing.T) {
		err := newCodexOAuthUpstreamError(http.StatusBadRequest, "refresh")
		var upstreamErr *CodexOAuthUpstreamError
		require.ErrorAs(t, err, &upstreamErr)
		require.Contains(t, upstreamErr.Message, "reauthorize")
		require.NotContains(t, upstreamErr.Message, "redirect URI")
	})

	t.Run("transport timeout", func(t *testing.T) {
		err := newCodexOAuthTransportError(context.DeadlineExceeded, "code exchange")
		var upstreamErr *CodexOAuthUpstreamError
		require.ErrorAs(t, err, &upstreamErr)
		require.Equal(t, types.ErrorCodeDoRequestFailed, upstreamErr.Code)
		require.Contains(t, upstreamErr.Message, "timed out")
		require.True(t, errors.Is(err, context.DeadlineExceeded))
	})

	t.Run("invalid success response", func(t *testing.T) {
		err := newCodexOAuthInvalidResponseError("code exchange", errors.New("invalid JSON"))
		var upstreamErr *CodexOAuthUpstreamError
		require.ErrorAs(t, err, &upstreamErr)
		require.Equal(t, types.ErrorCodeBadResponseBody, upstreamErr.Code)
		require.NotContains(t, upstreamErr.Message, "invalid JSON")
	})
}
