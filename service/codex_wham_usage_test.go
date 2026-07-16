package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexWhamRequestsUseSharedAuthenticationAndPaths(t *testing.T) {
	type requestCapture struct {
		method      string
		path        string
		authorize   string
		accountID   string
		originator  string
		contentType string
		body        []byte
	}

	requests := make(chan requestCapture, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- requestCapture{
			method:      r.Method,
			path:        r.URL.Path,
			authorize:   r.Header.Get("Authorization"),
			accountID:   r.Header.Get("chatgpt-account-id"),
			originator:  r.Header.Get("originator"),
			contentType: r.Header.Get("Content-Type"),
			body:        body,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)

	tests := []struct {
		name        string
		method      string
		path        string
		invoke      func() (int, []byte, error)
		expectsBody bool
	}{
		{
			name:   "usage",
			method: http.MethodGet,
			path:   "/backend-api/wham/usage",
			invoke: func() (int, []byte, error) {
				return FetchCodexWhamUsage(context.Background(), server.Client(), server.URL, "access-token", "account-id")
			},
		},
		{
			name:   "reset credits",
			method: http.MethodGet,
			path:   "/backend-api/wham/rate-limit-reset-credits",
			invoke: func() (int, []byte, error) {
				return FetchCodexWhamRateLimitResetCredits(context.Background(), server.Client(), server.URL, "access-token", "account-id")
			},
		},
		{
			name:        "consume reset credit",
			method:      http.MethodPost,
			path:        "/backend-api/wham/rate-limit-reset-credits/consume",
			expectsBody: true,
			invoke: func() (int, []byte, error) {
				return ConsumeCodexWhamRateLimitResetCredit(context.Background(), server.Client(), server.URL, "access-token", "account-id")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			statusCode, body, err := test.invoke()
			require.NoError(t, err)
			assert.Equal(t, http.StatusOK, statusCode)
			assert.JSONEq(t, `{"ok":true}`, string(body))

			request := <-requests
			assert.Equal(t, test.method, request.method)
			assert.Equal(t, test.path, request.path)
			assert.Equal(t, "Bearer access-token", request.authorize)
			assert.Equal(t, "account-id", request.accountID)
			assert.Equal(t, "codex_cli_rs", request.originator)
			if test.expectsBody {
				assert.Equal(t, "application/json", request.contentType)
				var payload map[string]string
				require.NoError(t, common.Unmarshal(request.body, &payload))
				assert.NotEmpty(t, payload["redeem_request_id"])
			} else {
				assert.Empty(t, request.body)
			}
		})
	}
}

func TestCodexWhamRequestRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", maxCodexWhamResponseBytes+1))
	}))
	t.Cleanup(server.Close)

	statusCode, body, err := FetchCodexWhamUsage(context.Background(), server.Client(), server.URL, "access-token", "account-id")

	require.Error(t, err)
	assert.Equal(t, http.StatusOK, statusCode)
	assert.Nil(t, body)
}

func TestCodexWhamRequestNormalizesConfiguredCodexBaseURL(t *testing.T) {
	paths := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(server.Close)

	tests := []struct {
		name         string
		baseURL      string
		expectedPath string
	}{
		{name: "origin", baseURL: server.URL, expectedPath: "/backend-api/wham/usage"},
		{name: "codex API root", baseURL: server.URL + "/backend-api/codex", expectedPath: "/backend-api/wham/usage"},
		{name: "responses endpoint", baseURL: server.URL + "/backend-api/codex/responses", expectedPath: "/backend-api/wham/usage"},
		{name: "gateway prefix", baseURL: server.URL + "/gateway/backend-api/codex", expectedPath: "/gateway/backend-api/wham/usage"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := FetchCodexWhamUsage(context.Background(), server.Client(), test.baseURL, "access-token", "account-id")
			require.NoError(t, err)
			assert.Equal(t, test.expectedPath, <-paths)
		})
	}
}
