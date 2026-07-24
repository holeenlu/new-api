package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/types"
)

func MidjourneyErrorWrapper(code int, desc string) *dto.MidjourneyResponse {
	return &dto.MidjourneyResponse{
		Code:        code,
		Description: desc,
	}
}

func MidjourneyErrorWithStatusCodeWrapper(code int, desc string, statusCode int) *dto.MidjourneyResponseWithStatusCode {
	return &dto.MidjourneyResponseWithStatusCode{
		StatusCode: statusCode,
		Response:   *MidjourneyErrorWrapper(code, desc),
	}
}

func ClaudeErrorWrapper(err error, code string, statusCode int) *dto.ClaudeErrorWithStatusCode {
	text := err.Error()
	lowerText := strings.ToLower(text)
	if !strings.HasPrefix(lowerText, "get file base64 from url") {
		if strings.Contains(lowerText, "post") || strings.Contains(lowerText, "dial") || strings.Contains(lowerText, "http") {
			common.SysLog(fmt.Sprintf("error: %s", text))
			// No request context reaches this shared wrapper, so localize the masked
			// transport-failure message to the deployment default language (honoring
			// DEFAULT_LANGUAGE) instead of hardcoding one language.
			text = i18n.Translate(i18n.DefaultLang, i18n.MsgRelayErrUpstreamRequestFailed)
		}
	}
	claudeError := types.ClaudeError{
		Message: text,
		Type:    "new_api_error",
	}
	return &dto.ClaudeErrorWithStatusCode{
		Error:      claudeError,
		StatusCode: statusCode,
	}
}

func ClaudeErrorWrapperLocal(err error, code string, statusCode int) *dto.ClaudeErrorWithStatusCode {
	claudeErr := ClaudeErrorWrapper(err, code, statusCode)
	claudeErr.LocalError = true
	return claudeErr
}

const maxUpstreamErrorResponseBytes = 1 << 20

func sanitizeUpstreamErrorMessage(message string) string {
	message = common.RedactSensitiveCredentials(strings.TrimSpace(message))
	message = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, message)
	runes := []rune(message)
	if len(runes) > common.LocalLogContentLimit {
		return fmt.Sprintf("%s... [truncated]", string(runes[:common.LocalLogContentLimit]))
	}
	return message
}

func buildUpstreamErrorSummary(resp *http.Response, responseBody []byte, truncated bool, message string) error {
	parts := []string{fmt.Sprintf("bad response status code %d", resp.StatusCode)}
	if message = sanitizeUpstreamErrorMessage(message); message != "" {
		parts = append(parts, "message: "+message)
	}
	if contentType := strings.TrimSpace(resp.Header.Get("Content-Type")); contentType != "" {
		parts = append(parts, "content_type: "+sanitizeUpstreamErrorMessage(contentType))
	}
	responseBytes := len(responseBody)
	if resp.ContentLength > 0 {
		responseBytes = int(resp.ContentLength)
	}
	if truncated && resp.ContentLength <= 0 {
		parts = append(parts, fmt.Sprintf("response_bytes: >=%d", responseBytes+1))
	} else {
		parts = append(parts, fmt.Sprintf("response_bytes: %d", responseBytes))
	}
	// On a 429, surface the upstream rate-limit reset metadata so an operator can
	// tell a plan/usage-window exhaustion (unified-status rejected, reset hours
	// out) from a short per-minute throughput limit (unified-status allowed,
	// input-tokens reset seconds out) without reproducing the request. Only
	// present headers are included, so non-Anthropic upstreams add at most
	// retry-after.
	if resp.StatusCode == http.StatusTooManyRequests {
		var rateLimitParts []string
		for _, headerName := range []string{
			"retry-after",
			"anthropic-ratelimit-unified-status",
			"anthropic-ratelimit-unified-reset",
			"anthropic-ratelimit-unified-5h-status",
			"anthropic-ratelimit-unified-5h-reset",
			"anthropic-ratelimit-unified-7d-status",
			"anthropic-ratelimit-unified-7d-reset",
			"anthropic-ratelimit-input-tokens-reset",
			"anthropic-ratelimit-tokens-reset",
		} {
			if value := sanitizeUpstreamErrorMessage(resp.Header.Get(headerName)); value != "" {
				rateLimitParts = append(rateLimitParts, headerName+"="+value)
			}
		}
		if len(rateLimitParts) > 0 {
			parts = append(parts, "ratelimit_headers: "+strings.Join(rateLimitParts, "; "))
		}
	}
	for _, headerName := range []string{common.UpstreamRequestIdKey, "x-request-id", "request-id", "x-amzn-requestid"} {
		if requestID := sanitizeUpstreamErrorMessage(resp.Header.Get(headerName)); requestID != "" {
			parts = append(parts, "upstream_request_id: "+requestID)
			break
		}
	}
	return errors.New(strings.Join(parts, ", "))
}

func RelayErrorHandler(ctx context.Context, resp *http.Response) (newApiErr *types.NewAPIError) {
	// Header-only fallback for the rare body-read-failure path below, capped at the
	// ordinary transient bound. The success path overwrites this with
	// ParseUpstreamRetryDelay, which alone may reach the longer usage-limit bound
	// from structured reset metadata; a bare Retry-After header must not widen the
	// cooldown to days on a read error.
	retryAfter := parseRetryAfterValue(
		resp.Header.Get("Retry-After"),
		time.Now(),
		maximumSubscriptionOAuthRetryAfter,
	)
	var usageWindows types.SubscriptionOAuthUsageWindows
	defer func() {
		if newApiErr != nil {
			newApiErr.RetryAfter = retryAfter
			newApiErr.UpstreamStatusCode = resp.StatusCode
			newApiErr.UsageWindows = usageWindows
		}
	}()
	newApiErr = types.InitOpenAIError(types.ErrorCodeBadResponseStatusCode, resp.StatusCode)

	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamErrorResponseBytes+1))
	CloseResponseBodyGracefully(resp)
	if err != nil {
		newApiErr.Err = fmt.Errorf("bad response status code %d, failed to read upstream error response", resp.StatusCode)
		return
	}
	truncated := len(responseBody) > maxUpstreamErrorResponseBytes
	if truncated {
		responseBody = responseBody[:maxUpstreamErrorResponseBytes]
	}
	now := time.Now()
	retryAfter = ParseUpstreamRetryDelay(resp.Header, responseBody, now)
	usageWindows = subscriptionOAuthUsageWindowsFromHeaders(resp.Header, now)

	var errResponse dto.GeneralErrorResponse
	err = common.Unmarshal(responseBody, &errResponse)
	if err != nil {
		newApiErr.Err = buildUpstreamErrorSummary(resp, responseBody, truncated, "")
		logger.LogError(ctx, newApiErr.Error())
		return
	}

	if common.GetJsonType(errResponse.Error) == "object" {
		// General format error (OpenAI, Anthropic, Gemini, etc.)
		oaiError := errResponse.TryToOpenAIError()
		if oaiError != nil {
			oaiError.Message = sanitizeUpstreamErrorMessage(oaiError.Message)
			newApiErr = types.WithOpenAIError(*oaiError, resp.StatusCode)
			newApiErr.Err = buildUpstreamErrorSummary(resp, responseBody, truncated, oaiError.Message)
			return
		}
	}
	message := sanitizeUpstreamErrorMessage(errResponse.ToMessage())
	newApiErr = types.NewOpenAIError(errors.New(message), types.ErrorCodeBadResponseStatusCode, resp.StatusCode)
	newApiErr.Err = buildUpstreamErrorSummary(resp, responseBody, truncated, message)
	return
}

func ParseRetryAfterHeader(value string, now time.Time) time.Duration {
	return parseRetryAfterValue(value, now, maximumSubscriptionOAuthRetryAfter)
}

// ParseUpstreamRetryDelay preserves long subscription usage-window resets from
// structured upstream errors while keeping malformed values bounded. Ordinary
// burst-rate cooldowns are still capped later by SubscriptionOAuthRetryCooldown.
func ParseUpstreamRetryDelay(headers http.Header, responseBody []byte, now time.Time) time.Duration {
	// Anthropic can report multiple exhausted subscription windows on one
	// inference response. Their latest reset is the routing constraint, so it
	// takes precedence over a generic body reset for a single representative
	// window.
	if delay := parseUnifiedUsageReset(headers, now); delay > 0 {
		return delay
	}
	var payload map[string]any
	if len(responseBody) > 0 && common.UnmarshalWithNumber(responseBody, &payload) == nil {
		if delay := findUpstreamResetDelay(payload, now, true, 0); delay > 0 {
			return delay
		}
		if delay := findUpstreamResetDelay(payload, now, false, 0); delay > 0 {
			return delay
		}
	}
	return parseRetryAfterValue(headers.Get("Retry-After"), now, maximumSubscriptionOAuthUsageLimitCooldown)
}

// parseUnifiedUsageReset reads the reset time of an exhausted Anthropic
// subscription usage window from the anthropic-ratelimit-unified-* response
// headers, which carry Unix-second reset timestamps that the JSON error body
// does not. It only reports a reset when the unified status marks the account as
// actually exhausted, so an acceleration/burst 429 that still advertises a
// far-future window reset is not mistaken for a usage-window cooldown. The
// top-level anthropic-ratelimit-unified-reset is only a representative reset.
// Per-window reset headers preserve whether the five-hour, seven-day, or both
// windows are exhausted for client messaging and a later local rejection.
func parseUnifiedUsageReset(headers http.Header, now time.Time) time.Duration {
	if delay := subscriptionOAuthUsageWindowsFromHeaders(headers, now).RetryDelay(); delay > 0 {
		return delay
	}
	if !unifiedRateLimitExhausted(headers) {
		return 0
	}
	return parseUpstreamResetValue(headers.Get("anthropic-ratelimit-unified-reset"), now, true)
}

func unifiedRateLimitExhausted(headers http.Header) bool {
	if strings.EqualFold(strings.TrimSpace(headers.Get("anthropic-ratelimit-unified-status")), "rejected") {
		return true
	}
	for _, window := range []string{"anthropic-ratelimit-unified-5h-status", "anthropic-ratelimit-unified-7d-status"} {
		if unifiedRateLimitWindowExhausted(headers.Get(window)) {
			return true
		}
	}
	return false
}

func subscriptionOAuthUsageWindowsFromHeaders(headers http.Header, now time.Time) types.SubscriptionOAuthUsageWindows {
	if headers == nil {
		return types.SubscriptionOAuthUsageWindows{}
	}
	windows := types.SubscriptionOAuthUsageWindows{
		FiveHourExhausted: unifiedRateLimitWindowExhausted(headers.Get("anthropic-ratelimit-unified-5h-status")),
		SevenDayExhausted: unifiedRateLimitWindowExhausted(headers.Get("anthropic-ratelimit-unified-7d-status")),
	}
	if windows.FiveHourExhausted {
		windows.FiveHourRetryAfter = parseUpstreamResetValue(headers.Get("anthropic-ratelimit-unified-5h-reset"), now, true)
	}
	if windows.SevenDayExhausted {
		windows.SevenDayRetryAfter = parseUpstreamResetValue(headers.Get("anthropic-ratelimit-unified-7d-reset"), now, true)
	}
	representativeReset := parseUpstreamResetValue(headers.Get("anthropic-ratelimit-unified-reset"), now, true)
	if windows.FiveHourExhausted && !windows.SevenDayExhausted && windows.FiveHourRetryAfter == 0 {
		windows.FiveHourRetryAfter = representativeReset
	}
	if windows.SevenDayExhausted && !windows.FiveHourExhausted && windows.SevenDayRetryAfter == 0 {
		windows.SevenDayRetryAfter = representativeReset
	}
	return windows
}

func unifiedRateLimitWindowExhausted(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "exceeded", "rate_limited", "rejected":
		return true
	default:
		return false
	}
}

// SubscriptionOAuthUsageLimitFromResponseHeaders classifies a usage-window
// exhaustion from a response's Anthropic rate-limit headers. A subscription
// account can accept the connection and return HTTP 200 with an empty SSE
// stream while its anthropic-ratelimit-unified-* headers already mark the
// window rejected; detecting it from the headers lets the relay fail over and
// return a usage-limit response immediately instead of idling until the
// streaming timeout. Returns nil when the headers do not prove exhaustion. The
// resulting error's code carries usage_limit semantics, so ApplyChannelErrorPolicy
// keeps it classified as upstream_usage_limit even when no reset timestamp is
// present (the cooldown then uses the one-hour fallback).
func SubscriptionOAuthUsageLimitFromResponseHeaders(headers http.Header, now time.Time) *types.NewAPIError {
	if headers == nil || !unifiedRateLimitExhausted(headers) {
		return nil
	}
	apiErr := types.NewErrorWithStatusCode(
		errors.New("subscription OAuth usage window is exhausted"),
		types.ErrorCodeUpstreamUsageLimit,
		http.StatusTooManyRequests,
	)
	apiErr.UpstreamStatusCode = http.StatusTooManyRequests
	apiErr.UsageWindows = subscriptionOAuthUsageWindowsFromHeaders(headers, now)
	apiErr.RetryAfter = apiErr.UsageWindows.RetryDelay()
	if apiErr.RetryAfter == 0 {
		apiErr.RetryAfter = parseUnifiedUsageReset(headers, now)
	}
	return apiErr
}

func findUpstreamResetDelay(value any, now time.Time, absolute bool, depth int) time.Duration {
	if depth > 4 {
		return 0
	}
	object, ok := value.(map[string]any)
	if !ok {
		return 0
	}
	keys := []string{"resets_in_seconds", "reset_after_seconds", "retry_after"}
	if absolute {
		keys = []string{"resets_at"}
	}
	for _, key := range keys {
		if delay := parseUpstreamResetValue(object[key], now, absolute); delay > 0 {
			return delay
		}
	}
	for _, wrapper := range []string{"error", "body", "response", "detail"} {
		if delay := findUpstreamResetDelay(object[wrapper], now, absolute, depth+1); delay > 0 {
			return delay
		}
	}
	return 0
}

func parseUpstreamResetValue(value any, now time.Time, absolute bool) time.Duration {
	var raw string
	switch typed := value.(type) {
	case json.Number:
		raw = typed.String()
	case string:
		raw = strings.TrimSpace(typed)
	case float64:
		raw = strconv.FormatFloat(typed, 'g', -1, 64)
	default:
		return 0
	}
	if raw == "" {
		return 0
	}
	if absolute {
		if when, err := time.Parse(time.RFC3339, raw); err == nil {
			return boundedUpstreamResetDuration(when.Sub(now))
		}
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return 0
	}
	if absolute {
		if seconds > float64(math.MaxInt64) {
			return 0
		}
		seconds = float64(time.Unix(int64(seconds), 0).Sub(now)) / float64(time.Second)
	}
	if seconds <= 0 {
		return 0
	}
	maximumSeconds := float64(maximumSubscriptionOAuthUsageLimitCooldown / time.Second)
	if seconds >= maximumSeconds {
		return maximumSubscriptionOAuthUsageLimitCooldown
	}
	return time.Duration(seconds * float64(time.Second))
}

func boundedUpstreamResetDuration(duration time.Duration) time.Duration {
	if duration <= 0 {
		return 0
	}
	if duration > maximumSubscriptionOAuthUsageLimitCooldown {
		return maximumSubscriptionOAuthUsageLimitCooldown
	}
	return duration
}

func parseRetryAfterValue(value string, now time.Time, maximum time.Duration) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds >= int64(maximum/time.Second) {
			return maximum
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	duration := when.Sub(now)
	if duration > maximum {
		return maximum
	}
	return duration
}

func ResetStatusCode(newApiErr *types.NewAPIError, statusCodeMappingStr string) {
	if newApiErr == nil {
		return
	}
	if statusCodeMappingStr == "" || statusCodeMappingStr == "{}" {
		return
	}
	statusCodeMapping := make(map[string]any)
	err := common.Unmarshal([]byte(statusCodeMappingStr), &statusCodeMapping)
	if err != nil {
		return
	}
	if newApiErr.StatusCode == http.StatusOK {
		return
	}
	codeStr := strconv.Itoa(newApiErr.StatusCode)
	if value, ok := statusCodeMapping[codeStr]; ok {
		intCode, ok := parseStatusCodeMappingValue(value)
		if !ok {
			return
		}
		newApiErr.StatusCode = intCode
	}
}

// StatusCodeMappingRisk describes a channel mapping that changes an upstream
// timeout response. A 504/524 may mean that the provider already accepted and
// billed the request, so changing its response semantics must be confirmed
// explicitly by the caller before it is saved.
type StatusCodeMappingRisk struct {
	From int
	To   int
}

// ValidateStatusCodeMapping validates both syntax and the HTTP status range.
// It returns the high-risk 504/524 mappings separately so callers can require
// an explicit confirmation without weakening the normal validation path.
func ValidateStatusCodeMapping(mapping string) ([]StatusCodeMappingRisk, error) {
	mapping = strings.TrimSpace(mapping)
	if mapping == "" || mapping == "{}" {
		return nil, nil
	}

	parsed := make(map[string]any)
	if err := common.Unmarshal([]byte(mapping), &parsed); err != nil {
		return nil, fmt.Errorf("invalid status code mapping JSON: %w", err)
	}
	if parsed == nil {
		return nil, errors.New("status code mapping must be a JSON object")
	}

	risks := make([]StatusCodeMappingRisk, 0)
	for rawFrom, rawTo := range parsed {
		normalizedFrom := strings.TrimSpace(rawFrom)
		if normalizedFrom != rawFrom {
			return nil, fmt.Errorf("source status code must not contain surrounding whitespace: %q", rawFrom)
		}
		if len(normalizedFrom) != 3 ||
			normalizedFrom[0] < '1' || normalizedFrom[0] > '5' ||
			normalizedFrom[1] < '0' || normalizedFrom[1] > '9' ||
			normalizedFrom[2] < '0' || normalizedFrom[2] > '9' {
			return nil, fmt.Errorf("invalid source status code: %s", rawFrom)
		}
		from, _ := strconv.Atoi(normalizedFrom)
		to, ok := parseStatusCodeMappingValue(rawTo)
		if !ok {
			return nil, fmt.Errorf("invalid target status code for %d", from)
		}
		if (from == http.StatusGatewayTimeout || from == 524) && from != to {
			risks = append(risks, StatusCodeMappingRisk{From: from, To: to})
		}
	}

	sort.Slice(risks, func(i, j int) bool {
		if risks[i].From == risks[j].From {
			return risks[i].To < risks[j].To
		}
		return risks[i].From < risks[j].From
	})
	return risks, nil
}

// NewStatusCodeMappingRisks returns only risky mappings newly introduced by
// the current value. Existing risky mappings do not block unrelated edits.
func NewStatusCodeMappingRisks(original, current string) ([]StatusCodeMappingRisk, error) {
	// A legacy original value may be invalid because it predates server-side
	// validation. Ignore that error only for the risk diff; the submitted current
	// value is still validated strictly and must be corrected before it is saved.
	originalRisks, _ := ValidateStatusCodeMapping(original)
	currentRisks, err := ValidateStatusCodeMapping(current)
	if err != nil {
		return nil, err
	}

	known := make(map[StatusCodeMappingRisk]struct{}, len(originalRisks))
	for _, risk := range originalRisks {
		known[risk] = struct{}{}
	}
	newRisks := make([]StatusCodeMappingRisk, 0, len(currentRisks))
	for _, risk := range currentRisks {
		if _, exists := known[risk]; !exists {
			newRisks = append(newRisks, risk)
		}
	}
	return newRisks, nil
}

func parseStatusCodeMappingValue(value any) (int, bool) {
	switch v := value.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return 0, false
		}
		statusCode, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, false
		}
		return statusCode, statusCode >= 100 && statusCode <= 599
	case float64:
		if v != math.Trunc(v) || v < 100 || v > 599 {
			return 0, false
		}
		return int(v), true
	case int:
		return v, v >= 100 && v <= 599
	case json.Number:
		statusCode, err := strconv.Atoi(v.String())
		if err != nil {
			return 0, false
		}
		return statusCode, statusCode >= 100 && statusCode <= 599
	default:
		return 0, false
	}
}

func TaskErrorWrapperLocal(err error, code string, statusCode int) *dto.TaskError {
	openaiErr := TaskErrorWrapper(err, code, statusCode)
	openaiErr.LocalError = true
	return openaiErr
}

func TaskErrorWrapper(err error, code string, statusCode int) *dto.TaskError {
	text := err.Error()
	lowerText := strings.ToLower(text)
	if strings.Contains(lowerText, "post") || strings.Contains(lowerText, "dial") || strings.Contains(lowerText, "http") {
		common.SysLog(fmt.Sprintf("error: %s", text))
		//text = "请求上游地址失败"
		text = common.MaskSensitiveInfo(text)
	}
	//避免暴露内部错误
	taskError := &dto.TaskError{
		Code:       code,
		Message:    text,
		StatusCode: statusCode,
		Error:      err,
	}

	return taskError
}

// TaskErrorFromAPIError 将 PreConsumeBilling 返回的 NewAPIError 转换为 TaskError。
func TaskErrorFromAPIError(apiErr *types.NewAPIError) *dto.TaskError {
	if apiErr == nil {
		return nil
	}
	return &dto.TaskError{
		Code:       string(apiErr.GetErrorCode()),
		Message:    apiErr.Err.Error(),
		StatusCode: apiErr.StatusCode,
		Error:      apiErr.Err,
	}
}
