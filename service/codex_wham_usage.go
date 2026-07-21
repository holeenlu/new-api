package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"
)

const (
	codexWhamUsageEvidenceTTL = 30 * time.Second
	codexWhamUsageFailureTTL  = 5 * time.Second
)

var (
	codexWhamUsageRequests singleflight.Group
	codexWhamUsageCache    = newSubscriptionOAuthUsageEvidenceCache[codexWhamUsageResult]()
)

type CodexWhamUsageWindow struct {
	UsedPercent       *float64 `json:"used_percent"`
	ResetAt           *float64 `json:"reset_at"`
	ResetAfterSeconds *float64 `json:"reset_after_seconds"`
}

type CodexWhamRateLimit struct {
	Allowed         *bool                 `json:"allowed"`
	LimitReached    *bool                 `json:"limit_reached"`
	PrimaryWindow   *CodexWhamUsageWindow `json:"primary_window"`
	SecondaryWindow *CodexWhamUsageWindow `json:"secondary_window"`
}

type CodexWhamUsageSnapshot struct {
	RateLimit  *CodexWhamRateLimit `json:"rate_limit"`
	ObservedAt time.Time           `json:"observed_at"`
}

type codexWhamUsageResult struct {
	statusCode int
	snapshot   *CodexWhamUsageSnapshot
	err        error
}

func FetchCodexWhamUsage(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	accessToken string,
	accountID string,
) (statusCode int, body []byte, err error) {
	return doCodexWhamRequest(ctx, client, http.MethodGet, baseURL, "/backend-api/wham/usage", accessToken, accountID, nil)
}

// FetchCodexWhamUsageSnapshot coalesces simultaneous evidence checks and keeps
// a bounded short-lived result for one credential. Credential cooldown state
// remains owned by subscription_oauth_capacity.go.
func FetchCodexWhamUsageSnapshot(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	accessToken string,
	accountID string,
	fingerprint string,
) (statusCode int, snapshot *CodexWhamUsageSnapshot, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if client == nil {
		return 0, nil, fmt.Errorf("nil http client")
	}
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return 0, nil, fmt.Errorf("Codex OAuth credential fingerprint is empty")
	}
	normalizedBaseURL, err := normalizeCodexWhamBaseURL(baseURL)
	if err != nil {
		return 0, nil, err
	}
	// Relay attempts can allocate distinct clients for the same account. Keep
	// the provider origin in the key, but do not let the client pointer turn one
	// burst of opaque 429s into an equal burst against the Wham endpoint.
	requestKey := fmt.Sprintf("%s:%s", fingerprint, normalizedBaseURL)
	if result, ok := codexWhamUsageCache.get(requestKey); ok {
		return result.statusCode, result.snapshot, result.err
	}
	resultChannel := codexWhamUsageRequests.DoChan(requestKey, func() (any, error) {
		if result, ok := codexWhamUsageCache.get(requestKey); ok {
			return result, nil
		}
		statusCode, body, requestErr := FetchCodexWhamUsage(ctx, client, normalizedBaseURL, accessToken, accountID)
		result := codexWhamUsageResult{statusCode: statusCode, err: requestErr}
		if requestErr == nil && statusCode >= http.StatusOK && statusCode < http.StatusMultipleChoices {
			result.snapshot, result.err = ParseCodexWhamUsageSnapshot(body, time.Now())
		}
		ttl := codexWhamUsageFailureTTL
		if result.err == nil && result.snapshot != nil {
			ttl = codexWhamUsageEvidenceTTL
		}
		if !errors.Is(result.err, context.Canceled) && !errors.Is(result.err, context.DeadlineExceeded) {
			codexWhamUsageCache.put(requestKey, result, ttl)
		}
		return result, nil
	})

	select {
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case call := <-resultChannel:
		if call.Err != nil {
			return 0, nil, call.Err
		}
		result, ok := call.Val.(codexWhamUsageResult)
		if !ok {
			return 0, nil, fmt.Errorf("invalid Codex usage result")
		}
		return result.statusCode, result.snapshot, result.err
	}
}

func ParseCodexWhamUsageSnapshot(body []byte, observedAt time.Time) (*CodexWhamUsageSnapshot, error) {
	var snapshot CodexWhamUsageSnapshot
	if err := common.Unmarshal(body, &snapshot); err != nil {
		return nil, fmt.Errorf("parse Codex usage response: %w", err)
	}
	if snapshot.RateLimit == nil {
		return nil, fmt.Errorf("Codex usage response contains no rate limit")
	}
	for name, window := range map[string]*CodexWhamUsageWindow{
		"primary_window":   snapshot.RateLimit.PrimaryWindow,
		"secondary_window": snapshot.RateLimit.SecondaryWindow,
	} {
		if window == nil {
			continue
		}
		for field, value := range map[string]*float64{
			"used_percent":        window.UsedPercent,
			"reset_at":            window.ResetAt,
			"reset_after_seconds": window.ResetAfterSeconds,
		} {
			if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0) {
				return nil, fmt.Errorf("Codex usage %s.%s is invalid", name, field)
			}
		}
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	snapshot.ObservedAt = observedAt.UTC()
	return &snapshot, nil
}

// CorrelateCodexOAuthUsageLimit upgrades an ambiguous Codex 429 only when a
// fresh Wham snapshot for the same credential proves the subscription window
// is exhausted. The original 429 remains transient when evidence is missing.
func CorrelateCodexOAuthUsageLimit(
	err *types.NewAPIError,
	snapshot *CodexWhamUsageSnapshot,
	now time.Time,
) *types.NewAPIError {
	if err == nil || snapshot == nil || err.GetUpstreamStatusCode() != http.StatusTooManyRequests {
		return err
	}
	if now.IsZero() {
		now = time.Now()
	}
	if snapshot.ObservedAt.IsZero() || now.Sub(snapshot.ObservedAt) > codexWhamUsageEvidenceTTL ||
		snapshot.ObservedAt.After(now.Add(5*time.Second)) {
		return err
	}
	exhausted, retryAfter := snapshot.exhaustedRetryAfter(now)
	if !exhausted {
		return err
	}
	err = err.Reclassify(err.Err, types.ErrorCodeUpstreamUsageLimit)
	// Overwrite unconditionally: Reclassify preserves the ambiguous 429's original
	// Retry-After, but a snapshot-confirmed usage limit must not keep that short
	// burst value. A zero here (exhausted with no future reset) must flow through
	// so the downstream cooldown applies its 1h fallback instead of re-probing the
	// exhausted account within seconds.
	err.RetryAfter = retryAfter
	return err
}

func (snapshot *CodexWhamUsageSnapshot) exhaustedRetryAfter(now time.Time) (bool, time.Duration) {
	if snapshot == nil || snapshot.RateLimit == nil {
		return false, 0
	}
	if now.IsZero() {
		now = time.Now()
	}
	flagged := snapshot.RateLimit.LimitReached != nil && *snapshot.RateLimit.LimitReached
	if snapshot.RateLimit.Allowed != nil && !*snapshot.RateLimit.Allowed {
		flagged = true
	}
	windows := []*CodexWhamUsageWindow{
		snapshot.RateLimit.PrimaryWindow,
		snapshot.RateLimit.SecondaryWindow,
	}
	exhausted := flagged
	var resetAt time.Time
	for _, window := range windows {
		if window == nil {
			continue
		}
		windowExhausted := window.UsedPercent != nil && *window.UsedPercent >= 100
		if !windowExhausted && !flagged {
			continue
		}
		exhausted = exhausted || windowExhausted
		candidate := codexWhamWindowResetAt(window, snapshot.ObservedAt)
		if candidate.After(now) && candidate.After(resetAt) {
			resetAt = candidate
		}
	}
	if !exhausted {
		return false, 0
	}
	if resetAt.IsZero() {
		return true, 0
	}
	return true, resetAt.Sub(now)
}

func codexWhamWindowResetAt(window *CodexWhamUsageWindow, observedAt time.Time) time.Time {
	if window == nil {
		return time.Time{}
	}
	if window.ResetAt != nil && *window.ResetAt > 0 && *window.ResetAt <= float64(math.MaxInt64) {
		return time.Unix(int64(*window.ResetAt), 0)
	}
	if window.ResetAfterSeconds != nil && *window.ResetAfterSeconds > 0 {
		return observedAt.Add(time.Duration(*window.ResetAfterSeconds * float64(time.Second)))
	}
	return time.Time{}
}

func FetchCodexWhamRateLimitResetCredits(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	accessToken string,
	accountID string,
) (statusCode int, body []byte, err error) {
	return doCodexWhamRequest(ctx, client, http.MethodGet, baseURL, "/backend-api/wham/rate-limit-reset-credits", accessToken, accountID, nil)
}

func ConsumeCodexWhamRateLimitResetCredit(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	accessToken string,
	accountID string,
) (statusCode int, body []byte, err error) {
	requestBody, err := common.Marshal(map[string]string{
		"redeem_request_id": uuid.NewString(),
	})
	if err != nil {
		return 0, nil, err
	}

	return doCodexWhamRequest(
		ctx,
		client,
		http.MethodPost,
		baseURL,
		"/backend-api/wham/rate-limit-reset-credits/consume",
		accessToken,
		accountID,
		bytes.NewReader(requestBody),
	)
}

const maxCodexWhamResponseBytes = 1 << 20

func doCodexWhamRequest(
	ctx context.Context,
	client *http.Client,
	method string,
	baseURL string,
	path string,
	accessToken string,
	accountID string,
	requestBody io.Reader,
) (statusCode int, body []byte, err error) {
	if client == nil {
		return 0, nil, fmt.Errorf("nil http client")
	}
	baseURL, err = normalizeCodexWhamBaseURL(baseURL)
	if err != nil {
		return 0, nil, err
	}
	accessToken = strings.TrimSpace(accessToken)
	accountID = strings.TrimSpace(accountID)
	if accessToken == "" {
		return 0, nil, fmt.Errorf("empty accessToken")
	}
	if accountID == "" {
		return 0, nil, fmt.Errorf("empty accountID")
	}

	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, requestBody)
	if err != nil {
		return 0, nil, err
	}
	setCodexWhamRequestHeaders(req, accessToken, accountID)
	if requestBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	body, err = io.ReadAll(io.LimitReader(resp.Body, maxCodexWhamResponseBytes+1))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if len(body) > maxCodexWhamResponseBytes {
		return resp.StatusCode, nil, fmt.Errorf("codex usage response exceeds %d bytes", maxCodexWhamResponseBytes)
	}
	return resp.StatusCode, body, nil
}

func normalizeCodexWhamBaseURL(baseURL string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	parsed, err := url.ParseRequestURI(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("invalid Codex baseURL")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("Codex baseURL must not contain user info")
	}

	for _, marker := range []string{"/backend-api/codex", "/backend-api/wham", "/backend-api"} {
		index := strings.Index(parsed.Path, marker)
		if index < 0 {
			continue
		}
		markerEnd := index + len(marker)
		if markerEnd != len(parsed.Path) && parsed.Path[markerEnd] != '/' {
			continue
		}
		parsed.Path = parsed.Path[:index]
		break
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func setCodexWhamRequestHeaders(req *http.Request, accessToken string, accountID string) {
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("chatgpt-account-id", accountID)
	req.Header.Set("Accept", "application/json")
	if req.Header.Get("originator") == "" {
		req.Header.Set("originator", "codex_cli_rs")
	}
}
