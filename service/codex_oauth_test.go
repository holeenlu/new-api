package service

import (
	"context"
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
