package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/types"
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

func TestFetchCodexWhamUsageSnapshotCachesEvidenceAndFailures(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		hasUsage   bool
	}{
		{
			name:       "fresh usage evidence",
			statusCode: http.StatusOK,
			body:       `{"rate_limit":{"allowed":true}}`,
			hasUsage:   true,
		},
		{
			name:       "failed lookup",
			statusCode: http.StatusTooManyRequests,
			body:       `{"error":"rate limited"}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				w.WriteHeader(test.statusCode)
				_, _ = io.WriteString(w, test.body)
			}))
			t.Cleanup(server.Close)
			previousCache := codexWhamUsageCache
			codexWhamUsageCache = newSubscriptionOAuthUsageEvidenceCache[codexWhamUsageResult]()
			t.Cleanup(func() { codexWhamUsageCache = previousCache })

			for range 2 {
				status, snapshot, err := FetchCodexWhamUsageSnapshot(
					context.Background(), server.Client(), server.URL,
					"access-token", "account-id", "codex-cache-"+test.name,
				)
				require.NoError(t, err)
				assert.Equal(t, test.statusCode, status)
				assert.Equal(t, test.hasUsage, snapshot != nil)
			}
			assert.Equal(t, 1, requests)
		})
	}
}

func TestFetchCodexWhamUsageSnapshotRefreshesExpiredEvidence(t *testing.T) {
	now := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = io.WriteString(w, `{"rate_limit":{"allowed":true}}`)
	}))
	t.Cleanup(server.Close)
	previousCache := codexWhamUsageCache
	codexWhamUsageCache = newSubscriptionOAuthUsageEvidenceCache[codexWhamUsageResult]()
	codexWhamUsageCache.now = func() time.Time { return now }
	t.Cleanup(func() { codexWhamUsageCache = previousCache })

	_, _, err := FetchCodexWhamUsageSnapshot(
		context.Background(), server.Client(), server.URL,
		"access-token", "account-id", "codex-expiry-cache",
	)
	require.NoError(t, err)
	now = now.Add(codexWhamUsageEvidenceTTL + time.Second)
	_, _, err = FetchCodexWhamUsageSnapshot(
		context.Background(), server.Client(), server.URL,
		"access-token", "account-id", "codex-expiry-cache",
	)
	require.NoError(t, err)
	assert.Equal(t, 2, requests)
}

func TestCorrelateCodexOAuthUsageLimitRequiresFreshExhaustionEvidence(t *testing.T) {
	now := time.Date(2026, time.July, 21, 10, 0, 0, 0, time.UTC)
	resetAt := float64(now.Add(5 * time.Hour).Unix())
	used := 100.0
	allowed := false
	limitReached := true
	snapshot := &CodexWhamUsageSnapshot{
		ObservedAt: now,
		RateLimit: &CodexWhamRateLimit{
			Allowed:      &allowed,
			LimitReached: &limitReached,
			PrimaryWindow: &CodexWhamUsageWindow{
				UsedPercent: &used,
				ResetAt:     &resetAt,
			},
		},
	}
	newRateLimitError := func() *types.NewAPIError {
		err := types.NewOpenAIError(
			errors.New("exceeded retry limit, last status: 429 Too Many Requests"),
			types.ErrorCodeBadResponseStatusCode,
			http.StatusTooManyRequests,
		)
		err.UpstreamStatusCode = http.StatusTooManyRequests
		return err
	}

	correlated := CorrelateCodexOAuthUsageLimit(newRateLimitError(), snapshot, now.Add(time.Second))
	assert.Equal(t, types.ErrorCodeUpstreamUsageLimit, correlated.GetErrorCode())
	assert.Equal(t, 5*time.Hour-time.Second, correlated.RetryAfter)

	available := 99.9
	snapshot.RateLimit.Allowed = nil
	snapshot.RateLimit.LimitReached = nil
	snapshot.RateLimit.PrimaryWindow.UsedPercent = &available
	transient := CorrelateCodexOAuthUsageLimit(newRateLimitError(), snapshot, now.Add(time.Second))
	assert.Equal(t, types.ErrorCodeBadResponseStatusCode, transient.GetErrorCode())

	snapshot.RateLimit.PrimaryWindow.UsedPercent = &used
	stale := CorrelateCodexOAuthUsageLimit(newRateLimitError(), snapshot, now.Add(codexWhamUsageEvidenceTTL+time.Second))
	assert.Equal(t, types.ErrorCodeBadResponseStatusCode, stale.GetErrorCode())
}

func TestParseCodexWhamUsageSnapshotAcceptsWindowResetDelay(t *testing.T) {
	now := time.Date(2026, time.July, 21, 10, 0, 0, 0, time.UTC)
	snapshot, err := ParseCodexWhamUsageSnapshot([]byte(`{
		"rate_limit": {
			"allowed": false,
			"limit_reached": true,
			"primary_window": {"used_percent": 100, "reset_after_seconds": 3600}
		}
	}`), now)

	require.NoError(t, err)
	exhausted, retryAfter := snapshot.exhaustedRetryAfter(now)
	assert.True(t, exhausted)
	assert.Equal(t, time.Hour, retryAfter)
}

func TestCodexWhamExplicitLimitFlagDoesNotRequireWindowDetails(t *testing.T) {
	now := time.Date(2026, time.July, 21, 10, 0, 0, 0, time.UTC)
	limitReached := true
	snapshot := &CodexWhamUsageSnapshot{
		ObservedAt: now,
		RateLimit:  &CodexWhamRateLimit{LimitReached: &limitReached},
	}

	exhausted, retryAfter := snapshot.exhaustedRetryAfter(now)

	assert.True(t, exhausted)
	assert.Zero(t, retryAfter)
}

func TestCorrelateCodexOAuthUsageLimitClearsBurstRetryAfterWhenNoReset(t *testing.T) {
	now := time.Date(2026, time.July, 21, 10, 0, 0, 0, time.UTC)
	limitReached := true
	// Window is exhausted but the upstream supplied no reset time.
	snapshot := &CodexWhamUsageSnapshot{
		ObservedAt: now,
		RateLimit:  &CodexWhamRateLimit{LimitReached: &limitReached},
	}
	// The ambiguous 429 carried a short burst Retry-After (e.g. 30s from the
	// header). A snapshot-confirmed usage limit must drop it so the downstream
	// cooldown applies its 1h fallback rather than re-probing in 30s.
	err := types.NewOpenAIError(
		errors.New("429 Too Many Requests"),
		types.ErrorCodeBadResponseStatusCode,
		http.StatusTooManyRequests,
	)
	err.UpstreamStatusCode = http.StatusTooManyRequests
	err.RetryAfter = 30 * time.Second

	correlated := CorrelateCodexOAuthUsageLimit(err, snapshot, now.Add(time.Second))
	require.Equal(t, types.ErrorCodeUpstreamUsageLimit, correlated.GetErrorCode())
	assert.Zero(t, correlated.RetryAfter)
}
