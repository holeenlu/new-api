package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRelayErrorHandlerSurfacesRateLimitHeadersOn429(t *testing.T) {
	header := http.Header{
		"Content-Type":                       []string{"application/json"},
		"Anthropic-Ratelimit-Unified-Status": []string{"rejected"},
		"Anthropic-Ratelimit-Unified-Reset":  []string{fmt.Sprintf("%d", time.Now().Add(3*time.Hour).Unix())},
		"Retry-After":                        []string{"5"},
	}
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit."}}`)),
	}

	err := RelayErrorHandler(context.Background(), response)
	require.NotNil(t, err)
	require.Contains(t, err.Error(), "ratelimit_headers:")
	require.Contains(t, err.Error(), "anthropic-ratelimit-unified-status=rejected")
	require.Contains(t, err.Error(), "retry-after=5")
	// The unified reset (rejected window, hours out) is the authoritative cooldown,
	// taking precedence over the short Retry-After on the same response.
	require.InDelta(t, (3 * time.Hour).Seconds(), err.RetryAfter.Seconds(), 5)
}

func TestRelayErrorHandlerOmitsRateLimitHeadersOnNon429(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"Retry-After":  []string{"5"},
		},
		Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"api_error","message":"boom"}}`)),
	}

	err := RelayErrorHandler(context.Background(), response)
	require.NotNil(t, err)
	require.NotContains(t, err.Error(), "ratelimit_headers:")
}

func TestRelayErrorHandlerPreservesRetryAfter(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"Retry-After":  []string{"12"},
		},
		Body: io.NopCloser(strings.NewReader(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`)),
	}

	err := RelayErrorHandler(context.Background(), response)
	require.NotNil(t, err)
	require.Equal(t, 12*time.Second, err.RetryAfter)
}

func TestSanitizeUpstreamErrorMessagePreventsLogLineInjection(t *testing.T) {
	message := sanitizeUpstreamErrorMessage("first line\r\n[FAKE] second line\tvalue")

	require.Equal(t, "first line  [FAKE] second line value", message)
}

func TestParseRetryAfterCapsOversizedValues(t *testing.T) {
	require.Equal(t, maximumSubscriptionOAuthRetryAfter, ParseRetryAfterHeader("999999999999999999", time.Now()))
}

func TestParseUpstreamRetryDelayUsesStructuredUsageReset(t *testing.T) {
	now := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		body string
		want time.Duration
	}{
		{
			name: "relative reset",
			body: `{"error":{"type":"usage_limit_reached","resets_in_seconds":18000}}`,
			want: 5 * time.Hour,
		},
		{
			name: "absolute reset takes precedence",
			body: fmt.Sprintf(`{"error":{"resets_at":%d,"resets_in_seconds":12}}`, now.Add(6*24*time.Hour).Unix()),
			want: 6 * 24 * time.Hour,
		},
		{
			name: "websocket style wrapper",
			body: `{"type":"error","status":429,"body":{"error":{"type":"usage_limit_reached","reset_after_seconds":7}}}`,
			want: 7 * time.Second,
		},
		{
			name: "stale absolute falls back to relative",
			body: fmt.Sprintf(`{"error":{"resets_at":%d,"resets_in_seconds":45}}`, now.Add(-time.Minute).Unix()),
			want: 45 * time.Second,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			delay := ParseUpstreamRetryDelay(http.Header{"Retry-After": []string{"90"}}, []byte(test.body), now)
			require.Equal(t, test.want, delay)
		})
	}
}

func TestParseUpstreamRetryDelayBoundsMalformedAndOversizedValues(t *testing.T) {
	now := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	require.Equal(t, 2*time.Minute, ParseUpstreamRetryDelay(
		http.Header{"Retry-After": []string{"120"}},
		[]byte(`{"error":{"resets_in_seconds":-1}}`),
		now,
	))
	require.Equal(t, maximumSubscriptionOAuthUsageLimitCooldown, ParseUpstreamRetryDelay(
		http.Header{},
		[]byte(`{"error":{"resets_in_seconds":999999999999999999}}`),
		now,
	))
	require.Zero(t, ParseUpstreamRetryDelay(http.Header{}, []byte(`{"error":{"resets_at":"invalid"}}`), now))
}

func TestParseUpstreamRetryDelayUsesUnifiedUsageResetHeader(t *testing.T) {
	now := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit."}}`)

	rejected := http.Header{}
	rejected.Set("anthropic-ratelimit-unified-status", "rejected")
	rejected.Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
	rejected.Set("anthropic-ratelimit-unified-reset", fmt.Sprintf("%d", now.Add(3*time.Hour).Unix()))
	require.Equal(t, 3*time.Hour, ParseUpstreamRetryDelay(rejected, body, now))

	perWindow := http.Header{}
	perWindow.Set("anthropic-ratelimit-unified-status", "allowed_warning")
	perWindow.Set("anthropic-ratelimit-unified-7d-status", "exceeded")
	perWindow.Set("anthropic-ratelimit-unified-reset", fmt.Sprintf("%d", now.Add(48*time.Hour).Unix()))
	require.Equal(t, 48*time.Hour, ParseUpstreamRetryDelay(perWindow, body, now))

	// A non-exhausted window must not turn a burst 429 into a multi-hour cooldown,
	// even though the unified reset header is far in the future. Fall back to
	// Retry-After instead.
	allowed := http.Header{}
	allowed.Set("anthropic-ratelimit-unified-status", "allowed")
	allowed.Set("anthropic-ratelimit-unified-5h-status", "allowed")
	allowed.Set("anthropic-ratelimit-unified-reset", fmt.Sprintf("%d", now.Add(4*time.Hour).Unix()))
	allowed.Set("Retry-After", "30")
	require.Equal(t, 30*time.Second, ParseUpstreamRetryDelay(allowed, body, now))
}

func TestSubscriptionOAuthUsageLimitFromResponseHeaders(t *testing.T) {
	now := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)

	rejected := http.Header{}
	rejected.Set("anthropic-ratelimit-unified-status", "rejected")
	rejected.Set("anthropic-ratelimit-unified-reset", fmt.Sprintf("%d", now.Add(4*time.Hour).Unix()))
	apiErr := SubscriptionOAuthUsageLimitFromResponseHeaders(rejected, now)
	require.NotNil(t, apiErr)
	require.Equal(t, types.ErrorCodeUpstreamUsageLimit, apiErr.GetErrorCode())
	require.Equal(t, http.StatusTooManyRequests, apiErr.GetUpstreamStatusCode())
	require.Equal(t, 4*time.Hour, apiErr.RetryAfter)

	// Exhausted per-window status but no usable reset timestamp still classifies as
	// a usage limit (reset zero → downstream one-hour fallback), never a nil miss.
	windowNoReset := http.Header{}
	windowNoReset.Set("anthropic-ratelimit-unified-7d-status", "exceeded")
	apiErr = SubscriptionOAuthUsageLimitFromResponseHeaders(windowNoReset, now)
	require.NotNil(t, apiErr)
	require.Equal(t, types.ErrorCodeUpstreamUsageLimit, apiErr.GetErrorCode())
	require.Zero(t, apiErr.RetryAfter)

	// A healthy response must not be misread as exhausted.
	allowed := http.Header{}
	allowed.Set("anthropic-ratelimit-unified-status", "allowed")
	allowed.Set("anthropic-ratelimit-unified-5h-status", "allowed")
	require.Nil(t, SubscriptionOAuthUsageLimitFromResponseHeaders(allowed, now))
	require.Nil(t, SubscriptionOAuthUsageLimitFromResponseHeaders(nil, now))
}

func TestRelayErrorHandlerPreservesIndividualAnthropicUsageWindows(t *testing.T) {
	now := time.Now()
	fiveHourReset := now.Add(5 * time.Hour).Truncate(time.Second)
	weeklyReset := now.Add(6 * 24 * time.Hour).Truncate(time.Second)
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header: http.Header{
			"Content-Type":                          []string{"application/json"},
			"Anthropic-Ratelimit-Unified-5h-Status": []string{"rejected"},
			"Anthropic-Ratelimit-Unified-5h-Reset":  []string{fmt.Sprintf("%d", fiveHourReset.Unix())},
			"Anthropic-Ratelimit-Unified-7d-Status": []string{"exceeded"},
			"Anthropic-Ratelimit-Unified-7d-Reset":  []string{fmt.Sprintf("%d", weeklyReset.Unix())},
		},
		Body: io.NopCloser(strings.NewReader(`{"error":{"message":"usage limit reached","type":"usage_limit_reached"}}`)),
	}

	err := RelayErrorHandler(context.Background(), response)
	require.True(t, err.UsageWindows.FiveHourExhausted)
	require.True(t, err.UsageWindows.SevenDayExhausted)
	require.InDelta(t, (5 * time.Hour).Seconds(), err.UsageWindows.FiveHourRetryAfter.Seconds(), 2)
	require.InDelta(t, (6 * 24 * time.Hour).Seconds(), err.UsageWindows.SevenDayRetryAfter.Seconds(), 2)
	require.InDelta(t, (6 * 24 * time.Hour).Seconds(), err.RetryAfter.Seconds(), 2)
}

func TestAnthropicUsageWindowHeaderWithoutResetDrivesCooldownAndFailover(t *testing.T) {
	require.NoError(t, i18n.Init())
	tests := []struct {
		name              string
		headers           http.Header
		wantFiveHour      bool
		wantSevenDay      bool
		wantUnified       bool
		wantMessageDetail string
	}{
		{
			name: "five hour window",
			headers: http.Header{
				"Anthropic-Ratelimit-Unified-5h-Status": []string{"rejected"},
			},
			wantFiveHour:      true,
			wantMessageDetail: "5 小时订阅用量窗口已达上限",
		},
		{
			name: "weekly window",
			headers: http.Header{
				"Anthropic-Ratelimit-Unified-7d-Status": []string{"rejected"},
			},
			wantSevenDay:      true,
			wantMessageDetail: "周订阅用量窗口已达上限",
		},
		{
			name: "top level rejection without window detail",
			headers: http.Header{
				"Anthropic-Ratelimit-Unified-Status": []string{"rejected"},
			},
			wantUnified:       true,
			wantMessageDetail: "上游未提供恢复时间",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.headers.Set("Content-Type", "application/json")
			response := &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     test.headers,
				Body: io.NopCloser(strings.NewReader(
					`{"error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit."}}`,
				)),
			}

			relayError := RelayErrorHandler(context.Background(), response)
			require.True(t, relayError.UsageWindows.IsExhausted())
			require.Equal(t, test.wantFiveHour, relayError.UsageWindows.FiveHourExhausted)
			require.Equal(t, test.wantSevenDay, relayError.UsageWindows.SevenDayExhausted)
			require.Equal(t, test.wantUnified, relayError.UsageWindows.UnifiedExhausted)
			require.Zero(t, relayError.RetryAfter)

			classified := ApplyChannelErrorPolicy(constant.ChannelTypeClaudeCode, relayError)
			require.Equal(t, types.ErrorCodeUpstreamUsageLimit, classified.GetErrorCode())
			message, ok := localizedRelayErrorMessage(
				i18n.LangZhCN,
				classified.GetErrorCode(),
				classified.RetryAfter,
				classified.UsageWindows,
				time.Now(),
			)
			require.True(t, ok)
			require.Contains(t, message, test.wantMessageDetail)

			fingerprint := "anthropic-header-no-reset-" + test.name
			state := replaceSubscriptionOAuthStateForTest(t, fingerprint)
			retryParam := &RetryParam{}
			require.Equal(t, SubscriptionOAuthAttemptReserved, retryParam.ReserveSubscriptionOAuthAttempt(1, 0, fingerprint))
			decision, cooldown := retryParam.DecideSubscriptionOAuthContinuation(SubscriptionOAuthRetryObservation{
				ChannelType: constant.ChannelTypeClaudeCode,
				Error:       classified,
				Retryable:   true,
			})
			require.Equal(t, SubscriptionOAuthSwitchCredential, decision)
			require.Equal(t, subscriptionOAuthUsageLimitCooldown, cooldown)
			state.mu.Lock()
			require.Equal(t, subscriptionOAuthCooldownUsageLimit, state.cooldownReason)
			require.Equal(t, subscriptionOAuthUsageLimitCooldown, state.cooldownDuration)
			state.mu.Unlock()
		})
	}
}

func TestRelayErrorHandlerPreservesUsageHeadersWhenBodyReadFails(t *testing.T) {
	now := time.Now()
	resetAt := now.Add(3 * time.Hour).Truncate(time.Second)
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header: http.Header{
			"Anthropic-Ratelimit-Unified-Status": []string{"rejected"},
			"Anthropic-Ratelimit-Unified-Reset":  []string{fmt.Sprintf("%d", resetAt.Unix())},
		},
		Body: io.NopCloser(iotest.ErrReader(fmt.Errorf("connection reset while reading error body"))),
	}

	relayError := RelayErrorHandler(context.Background(), response)
	require.Error(t, relayError)
	require.True(t, relayError.UsageWindows.UnifiedExhausted)
	require.InDelta(t, (3 * time.Hour).Seconds(), relayError.RetryAfter.Seconds(), 2)

	classified := ApplyChannelErrorPolicy(constant.ChannelTypeClaudeCode, relayError)
	require.Equal(t, types.ErrorCodeUpstreamUsageLimit, classified.GetErrorCode())
	require.InDelta(
		t,
		(3 * time.Hour).Seconds(),
		SubscriptionOAuthCredentialCooldownForError(constant.ChannelTypeClaudeCode, classified).Seconds(),
		2,
	)
}

func TestRelayErrorHandlerPreservesUsageResetFromBody(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"error":{"message":"You've reached your usage limit","type":"usage_limit_reached","resets_in_seconds":21600}}`,
		)),
	}

	err := RelayErrorHandler(context.Background(), response)

	require.NotNil(t, err)
	require.Equal(t, 6*time.Hour, err.RetryAfter)
}

func TestResetStatusCode(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name             string
		statusCode       int
		statusCodeConfig string
		expectedCode     int
	}{
		{
			name:             "map string value",
			statusCode:       429,
			statusCodeConfig: `{"429":"503"}`,
			expectedCode:     503,
		},
		{
			name:             "map int value",
			statusCode:       429,
			statusCodeConfig: `{"429":503}`,
			expectedCode:     503,
		},
		{
			name:             "skip invalid string value",
			statusCode:       429,
			statusCodeConfig: `{"429":"bad-code"}`,
			expectedCode:     429,
		},
		{
			name:             "skip status code 200",
			statusCode:       200,
			statusCodeConfig: `{"200":503}`,
			expectedCode:     200,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			newAPIError := &types.NewAPIError{
				StatusCode: tc.statusCode,
			}
			ResetStatusCode(newAPIError, tc.statusCodeConfig)
			require.Equal(t, tc.expectedCode, newAPIError.StatusCode)
		})
	}
}

func TestValidateStatusCodeMapping(t *testing.T) {
	risks, err := ValidateStatusCodeMapping(`{"504":500,"524":"429","500":503}`)
	require.NoError(t, err)
	require.Equal(t, []StatusCodeMappingRisk{
		{From: 504, To: 500},
		{From: 524, To: 429},
	}, risks)
}

func TestValidateStatusCodeMappingRejectsInvalidCodes(t *testing.T) {
	for _, mapping := range []string{
		`{"99":500}`,
		`{"0504":500}`,
		`{"+504":500}`,
		`{"500":600}`,
		`{"500":"not-a-status"}`,
		`{" 504 ":500}`,
		`[]`,
	} {
		_, err := ValidateStatusCodeMapping(mapping)
		require.Error(t, err, mapping)
	}
}

func TestNewStatusCodeMappingRisksOnlyReturnsNewMappings(t *testing.T) {
	risks, err := NewStatusCodeMappingRisks(
		`{"504":500}`,
		`{"504":500,"524":429}`,
	)
	require.NoError(t, err)
	require.Equal(t, []StatusCodeMappingRisk{{From: 524, To: 429}}, risks)
}

func TestRelayErrorHandlerOmitsInvalidJSONBodyFromErrorAndLog(t *testing.T) {
	withDebugEnabled(t, false)

	body := strings.Repeat("b", common.LocalLogContentLimit+256)
	var logBuffer bytes.Buffer

	common.LogWriterMu.Lock()
	oldWriter := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = &logBuffer
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = oldWriter
		common.LogWriterMu.Unlock()
	})

	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"text/plain"}, "X-Request-Id": []string{"req-upstream"}},
	}

	newAPIError := RelayErrorHandler(context.Background(), resp)

	require.NotNil(t, newAPIError)
	require.Contains(t, newAPIError.Error(), "bad response status code 500")
	require.Contains(t, newAPIError.Error(), "content_type: text/plain")
	require.Contains(t, newAPIError.Error(), fmt.Sprintf("response_bytes: %d", len(body)))
	require.Contains(t, newAPIError.Error(), "upstream_request_id: req-upstream")
	require.NotContains(t, newAPIError.Error(), body)
	require.NotContains(t, logBuffer.String(), body)
}

func TestRelayErrorHandlerKeepsStructuredErrorMessage(t *testing.T) {
	message := strings.Repeat("c", common.LocalLogContentLimit+256)
	body := `{"message":"` + message + `"}`
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp)

	require.NotNil(t, newAPIError)
	require.Contains(t, newAPIError.Error(), "bad response status code 500, message: ")
	require.Contains(t, newAPIError.Error(), "[truncated]")
	require.NotContains(t, newAPIError.Error(), body)
}

func TestRelayErrorHandlerKeepsOpenAIErrorMessage(t *testing.T) {
	message := strings.Repeat("d", common.LocalLogContentLimit+256)
	body := `{"error":{"message":"` + message + `","type":"server_error","code":"server_error"}}`
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp)

	require.NotNil(t, newAPIError)
	require.Contains(t, newAPIError.Error(), "bad response status code 500, message: ")
	require.Contains(t, newAPIError.Error(), "[truncated]")
	require.NotContains(t, newAPIError.Error(), body)
	require.Equal(t, types.ErrorCode("server_error"), newAPIError.GetErrorCode())
}

func TestRelayErrorHandlerOmitsInvalidJSONBodyInDebugLog(t *testing.T) {
	withDebugEnabled(t, true)

	body := strings.Repeat("e", common.LocalLogContentLimit+256)
	var logBuffer bytes.Buffer

	common.LogWriterMu.Lock()
	oldWriter := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = &logBuffer
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = oldWriter
		common.LogWriterMu.Unlock()
	})

	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp)

	require.NotNil(t, newAPIError)
	require.NotContains(t, logBuffer.String(), body)
	require.Contains(t, logBuffer.String(), "response_bytes")
}

func withDebugEnabled(t *testing.T, enabled bool) {
	t.Helper()

	oldDebug := common.DebugEnabled
	common.DebugEnabled = enabled
	t.Cleanup(func() {
		common.DebugEnabled = oldDebug
	})
}
