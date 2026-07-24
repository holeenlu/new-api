package service

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/types"

	"github.com/bytedance/gopkg/util/gopool"
)

// ShouldDisableChannelForType keeps transient subscription transport failures
// from being mistaken for invalid OAuth credentials. A 5xx may be retried or
// routed elsewhere, but it must not change the channel's enabled state.
func ShouldDisableChannelForType(channelType int, err *types.NewAPIError) bool {
	if IsSubscriptionOAuthAccountUnavailable(channelType, err) {
		return IsSubscriptionOAuthPersistentAccountFailure(channelType, err)
	}
	if err != nil && constant.IsSubscriptionOAuthChannel(channelType) &&
		err.GetUpstreamStatusCode() == http.StatusTooManyRequests {
		return false
	}
	if IsSubscriptionOAuthTransientError(channelType, err) {
		return false
	}
	return ShouldDisableChannel(err)
}

func IsSubscriptionOAuthTransientError(channelType int, err *types.NewAPIError) bool {
	if err == nil || !constant.IsSubscriptionOAuthChannel(channelType) {
		return false
	}
	statusCode := err.GetUpstreamStatusCode()
	if statusCode == http.StatusTooManyRequests {
		return common.SubscriptionOAuthRetry429
	}
	return statusCode >= 500 && statusCode <= 599
}

func IsSubscriptionOAuthConcurrencyLimit(channelType int, err *types.NewAPIError) bool {
	return err != nil && constant.IsSubscriptionOAuthChannel(channelType) &&
		err.GetErrorCode() == types.ErrorCodeOAuthChannelConcurrencyLimit
}

// IsSubscriptionOAuthCapacityFailure identifies a rejection created by the
// local credential capacity/cooldown state. Its public error code may preserve
// the upstream cause (for example, upstream_rate_limited), so retry routing
// must use the wrapped source rather than treating every local rejection as an
// active-concurrency error.
func IsSubscriptionOAuthCapacityFailure(channelType int, err *types.NewAPIError) bool {
	return err != nil && constant.IsSubscriptionOAuthChannel(channelType) &&
		(IsSubscriptionOAuthCapacityError(err) || IsSubscriptionOAuthConcurrencyLimit(channelType, err))
}

func IsSubscriptionOAuthActiveCapacityFailure(channelType int, err *types.NewAPIError) bool {
	return err != nil && constant.IsSubscriptionOAuthChannel(channelType) &&
		(IsSubscriptionOAuthActiveCapacityError(err) || IsSubscriptionOAuthConcurrencyLimit(channelType, err))
}

func IsSubscriptionOAuthKnownCooldownFailure(channelType int, err *types.NewAPIError) bool {
	return err != nil && constant.IsSubscriptionOAuthChannel(channelType) &&
		IsSubscriptionOAuthCooldownCapacityError(err)
}

func IsSubscriptionOAuthAccountUnavailable(channelType int, err *types.NewAPIError) bool {
	if err == nil || !constant.IsSubscriptionOAuthChannel(channelType) {
		return false
	}
	return err.GetErrorCode() == types.ErrorCodeOAuthUnauthorized ||
		err.GetErrorCode() == types.ErrorCodeOAuthForbidden ||
		err.GetErrorCode() == types.ErrorCodeUpstreamAccountDisabled ||
		err.GetErrorCode() == types.ErrorCodeUpstreamQuotaExhausted
}

// IsSubscriptionOAuthPersistentAccountFailure distinguishes an explicitly
// dead credential/account from a status-only authorization rejection. A bare
// 401/403 still excludes the credential from the current request and opens a
// short circuit, but it must not mutate channel state: proxies and provider
// policy layers can produce those statuses for recoverable request failures.
func IsSubscriptionOAuthPersistentAccountFailure(channelType int, err *types.NewAPIError) bool {
	if !IsSubscriptionOAuthAccountUnavailable(channelType, err) {
		return false
	}
	switch err.GetErrorCode() {
	case types.ErrorCodeUpstreamAccountDisabled, types.ErrorCodeUpstreamQuotaExhausted:
		return true
	case types.ErrorCodeOAuthUnauthorized, types.ErrorCodeOAuthForbidden:
		message, code := subscriptionOAuthErrorMarkerText(err)
		return containsSubscriptionOAuthErrorMarker(message, code, subscriptionOAuthCredentialRejectedMarkers)
	default:
		return false
	}
}

// ShouldRefreshCodexOAuthCredential identifies authentication failures that
// can be repaired by rotating the access token. An expired access token is not
// durable evidence that the account or refresh credential is invalid.
func ShouldRefreshCodexOAuthCredential(channelType int, err *types.NewAPIError) bool {
	if channelType != constant.ChannelTypeCodex || err == nil {
		return false
	}
	if err.GetUpstreamStatusCode() == http.StatusUnauthorized {
		return true
	}
	message, code := subscriptionOAuthUpstreamMarkerText(err)
	return containsSubscriptionOAuthErrorMarker(message, code, subscriptionOAuthAccessTokenExpiredMarkers)
}

// IsPermanentCodexOAuthRefreshFailure is intentionally limited to evidence
// produced by the token endpoint. Transport failures, 429s and 5xx responses
// leave the channel enabled so another request can refresh it later.
func IsPermanentCodexOAuthRefreshFailure(err error) bool {
	if err == nil {
		return false
	}
	var upstreamErr *CodexOAuthUpstreamError
	if errors.As(err, &upstreamErr) {
		switch upstreamErr.StatusCode {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusUnprocessableEntity:
			return true
		default:
			return false
		}
	}
	message := strings.ToLower(err.Error())
	return containsSubscriptionOAuthErrorMarker(message, "", subscriptionOAuthRefreshCredentialRejectedMarkers)
}

// IsSubscriptionOAuthUsageLimit reports whether a 429 is a plan/usage-limit
// exhaustion (such as a five-hour or weekly cap) rather than a short burst rate
// limit.
// These reset in hours-to-days, so routing must cool the credential down far
// longer than a transient 429 instead of re-probing the exhausted account every
// few minutes.
func IsSubscriptionOAuthUsageLimit(channelType int, err *types.NewAPIError) bool {
	if err == nil || !constant.IsSubscriptionOAuthChannel(channelType) {
		return false
	}
	if err.GetErrorCode() == types.ErrorCodeUpstreamUsageLimit {
		return true
	}
	if err.GetUpstreamStatusCode() != http.StatusTooManyRequests {
		return false
	}
	if err.UsageWindows.IsExhausted() {
		return true
	}
	if subscriptionOAuthUsageWindowReset(err) {
		return true
	}
	lowerMessage, lowerCode := subscriptionOAuthErrorMarkerText(err)
	return containsSubscriptionOAuthErrorMarker(lowerMessage, lowerCode, subscriptionOAuthUsageLimitMarkers)
}

// subscriptionOAuthUsageWindowReset recognizes a plan/usage-window exhaustion
// from the reset window's magnitude, independent of message wording. A 429 whose
// reset exceeds the transient burst cap resets in hours-to-days, not seconds, so
// it must be cooled for its whole window rather than re-probed after the
// 15-minute transient cap. The reset is populated onto err.RetryAfter by
// RelayErrorHandler from a structured resets_at/resets_in_seconds body field,
// Anthropic's anthropic-ratelimit-unified-reset header, or the Retry-After
// header, so this catches an exhausted subscription window even when the message
// carries no usage-limit wording.
func subscriptionOAuthUsageWindowReset(err *types.NewAPIError) bool {
	return err != nil &&
		err.GetUpstreamStatusCode() == http.StatusTooManyRequests &&
		err.RetryAfter > maximumSubscriptionOAuthRetryAfter
}

// QuarantineSubscriptionOAuthCredential persistently removes every reference
// to a rejected credential from routing. Unlike automatic channel banning,
// this account-safety rule is not optional.
func QuarantineSubscriptionOAuthCredential(channelError types.ChannelError, err *types.NewAPIError) bool {
	if !IsSubscriptionOAuthPersistentAccountFailure(channelError.ChannelType, err) {
		return false
	}
	reason := fmt.Sprintf("%s: %s", err.GetErrorCode(), common.LocalLogPreview(err.Error()))
	fingerprint := SubscriptionOAuthCredentialFingerprint(channelError.ChannelType, channelError.ChannelId, 0, channelError.UsingKey)
	// Open the credential circuit before the database transaction so concurrent
	// requests cannot select the rejected account while its references are being
	// quarantined and channel caches are refreshed.
	CooldownSubscriptionOAuthCredential(fingerprint, 0, subscriptionOAuthCredentialCooldown)
	affected, quarantineErr := model.QuarantineSubscriptionOAuthCredential(channelError.ChannelType, fingerprint, reason)
	if quarantineErr != nil {
		common.SysError(fmt.Sprintf("failed to quarantine subscription OAuth credential: channel_id=%d error=%s", channelError.ChannelId, quarantineErr.Error()))
		return false
	}
	if len(affected) == 0 {
		return false
	}
	fingerprintPreview := fingerprint
	if len(fingerprintPreview) > 12 {
		fingerprintPreview = fingerprintPreview[:12]
	}
	references := make([]string, 0, min(len(affected), 20))
	for index, reference := range affected {
		if index == 20 {
			references = append(references, fmt.Sprintf("另有 %d 个引用", len(affected)-index))
			break
		}
		location := fmt.Sprintf("通道「%s」（#%d）", reference.ChannelName, reference.ChannelID)
		if reference.MultiKey {
			location += fmt.Sprintf(" Key #%d", reference.KeyIndex+1)
		}
		references = append(references, location)
	}
	subject := fmt.Sprintf("OAuth 凭证已隔离：%s", fingerprintPreview)
	content := fmt.Sprintf("同一 OAuth 凭证的 %d 个渠道引用已从用户路由中隔离，请管理员检查并重新授权后再手动启用。引用：%s；触发通道：「%s」（#%d）；错误代码：%s；原因：%s",
		len(affected), strings.Join(references, "、"), channelError.ChannelName, channelError.ChannelId, err.GetErrorCode(), common.LocalLogPreview(err.Error()))
	gopool.Go(func() {
		NotifyAdminUsers(fmt.Sprintf("%s_credential_%s", dto.NotifyTypeChannelUpdate, fingerprintPreview), subject, content)
	})
	return true
}

func IsSubscriptionOAuthModelUnavailable(channelType int, err *types.NewAPIError) bool {
	return err != nil && constant.IsSubscriptionOAuthChannel(channelType) &&
		err.GetErrorCode() == types.ErrorCodeModelNotSupported
}

func IsSubscriptionOAuthModelAtCapacity(channelType int, err *types.NewAPIError) bool {
	return err != nil && constant.IsSubscriptionOAuthChannel(channelType) &&
		err.GetErrorCode() == types.ErrorCodeModelAtCapacity
}

// ApplyChannelErrorPolicy classifies subscription account failures and keeps
// only transient transport failures eligible for the request-local retry loop.
func ApplyChannelErrorPolicy(channelType int, err *types.NewAPIError) *types.NewAPIError {
	if err == nil || !constant.IsSubscriptionOAuthChannel(channelType) {
		return err
	}
	err = classifySubscriptionOAuthError(err)
	if IsSubscriptionOAuthTransientError(channelType, err) {
		return err
	}
	types.ErrOptionWithSkipRetry()(err)
	return err
}

func classifySubscriptionOAuthError(err *types.NewAPIError) *types.NewAPIError {
	if err == nil {
		return nil
	}
	lowerMessage, lowerCode := subscriptionOAuthErrorMarkerText(err)
	upstreamMessage, upstreamCode := subscriptionOAuthUpstreamMarkerText(err)
	if containsSubscriptionOAuthErrorMarker(lowerMessage, lowerCode, subscriptionOAuthModelCapacityMarkers) {
		return err.Reclassify(err.Err, types.ErrorCodeModelAtCapacity)
	}
	modelMentioned := strings.Contains(lowerMessage, "model") || strings.Contains(lowerCode, "model")
	modelUnavailable := strings.Contains(lowerMessage, "not supported") ||
		strings.Contains(lowerMessage, "unsupported") ||
		strings.Contains(lowerMessage, "not found") ||
		strings.Contains(lowerMessage, "does not exist") ||
		strings.Contains(lowerMessage, "not available") ||
		strings.Contains(lowerMessage, "no access") ||
		strings.Contains(lowerCode, "not_supported") ||
		strings.Contains(lowerCode, "unsupported") ||
		strings.Contains(lowerCode, "not_found") ||
		strings.Contains(lowerCode, "no_access")
	statusCode := err.GetUpstreamStatusCode()
	if modelMentioned && modelUnavailable &&
		(statusCode == http.StatusBadRequest || statusCode == http.StatusForbidden ||
			statusCode == http.StatusNotFound || statusCode == http.StatusUnprocessableEntity) {
		return err.Reclassify(errors.New("selected model is not supported by this OAuth account"), types.ErrorCodeModelNotSupported)
	}
	if containsSubscriptionOAuthErrorMarker(upstreamMessage, upstreamCode, subscriptionOAuthUnauthorizedMarkers) {
		return err.Reclassify(errors.New("OAuth authorization rejected by upstream"), types.ErrorCodeOAuthUnauthorized)
	}
	if containsSubscriptionOAuthErrorMarker(upstreamMessage, upstreamCode, subscriptionOAuthForbiddenMarkers) {
		return err.Reclassify(errors.New("OAuth access rejected by upstream"), types.ErrorCodeOAuthForbidden)
	}
	if containsSubscriptionOAuthErrorMarker(lowerMessage, lowerCode, subscriptionOAuthAccountDisabledMarkers) {
		return err.Reclassify(err.Err, types.ErrorCodeUpstreamAccountDisabled)
	}
	if err.GetUpstreamStatusCode() == http.StatusTooManyRequests &&
		(err.UsageWindows.IsExhausted() || subscriptionOAuthUsageWindowReset(err) ||
			containsSubscriptionOAuthErrorMarker(lowerMessage, lowerCode, subscriptionOAuthUsageLimitMarkers)) {
		return err.Reclassify(err.Err, types.ErrorCodeUpstreamUsageLimit)
	}
	if containsSubscriptionOAuthErrorMarker(lowerMessage, lowerCode, subscriptionOAuthQuotaExhaustedMarkers) {
		return err.Reclassify(err.Err, types.ErrorCodeUpstreamQuotaExhausted)
	}
	switch statusCode {
	case http.StatusUnauthorized:
		return err.Reclassify(errors.New("OAuth authorization rejected by upstream"), types.ErrorCodeOAuthUnauthorized)
	case http.StatusForbidden:
		return err.Reclassify(errors.New("OAuth access rejected by upstream"), types.ErrorCodeOAuthForbidden)
	case http.StatusTooManyRequests:
		return err.Reclassify(err.Err, types.ErrorCodeUpstreamRateLimited)
	default:
		return err
	}
}

func subscriptionOAuthErrorMarkerText(err *types.NewAPIError) (string, string) {
	message, code := subscriptionOAuthUpstreamMarkerText(err)
	code = strings.ToLower(string(err.GetErrorCode())) + " " + code
	return message, code
}

// subscriptionOAuthUpstreamMarkerText deliberately excludes the gateway's
// current classification code. ApplyChannelErrorPolicy may run at both an
// adaptor boundary and the shared relay boundary; treating a previously
// assigned oauth_unauthorized code as fresh upstream evidence would make a
// status-only 401 become persistently quarantined on the second pass.
func subscriptionOAuthUpstreamMarkerText(err *types.NewAPIError) (string, string) {
	message := strings.ToLower(err.Error())
	code := ""
	switch relayError := err.RelayError.(type) {
	case types.OpenAIError:
		message += " " + strings.ToLower(relayError.Message)
		code += " " + strings.ToLower(relayError.Type) + " " + strings.ToLower(fmt.Sprint(relayError.Code))
	case *types.OpenAIError:
		if relayError != nil {
			message += " " + strings.ToLower(relayError.Message)
			code += " " + strings.ToLower(relayError.Type) + " " + strings.ToLower(fmt.Sprint(relayError.Code))
		}
	case types.ClaudeError:
		message += " " + strings.ToLower(relayError.Message)
		code += " " + strings.ToLower(relayError.Type) + " " + strings.ToLower(fmt.Sprint(relayError.Code))
	case *types.ClaudeError:
		if relayError != nil {
			message += " " + strings.ToLower(relayError.Message)
			code += " " + strings.ToLower(relayError.Type) + " " + strings.ToLower(fmt.Sprint(relayError.Code))
		}
	}
	return message, code
}

var subscriptionOAuthUnauthorizedMarkers = []string{
	"oauth_unauthorized",
}

var subscriptionOAuthForbiddenMarkers = []string{
	"oauth_forbidden",
}

// These markers are intentionally narrow. HTTP status alone is not durable
// evidence that an OAuth credential should be disabled in the database.
var subscriptionOAuthCredentialRejectedMarkers = []string{
	"invalid_grant",
	"refresh token was revoked",
	"refresh token has been revoked",
	"refresh credential was revoked",
	"refresh credential has been revoked",
	"oauth credential refresh was rejected; reauthorize the account",
	"reauthorization is required",
}

var subscriptionOAuthAccessTokenExpiredMarkers = []string{
	"access token expired",
	"access token has expired",
	"oauth access token expired",
	"token_expired",
}

var subscriptionOAuthRefreshCredentialRejectedMarkers = []string{
	"invalid_grant",
	"refresh token was revoked",
	"refresh token has been revoked",
	"refresh credential was revoked",
	"refresh credential has been revoked",
	"reauthorize the account",
	"reauthorization is required",
}

var subscriptionOAuthAccountDisabledMarkers = []string{
	"this organization has been disabled",
	"organization has been disabled",
	"organization_disabled",
	"this account has been disabled",
	"account_disabled",
}

var subscriptionOAuthQuotaExhaustedMarkers = []string{
	"your credit balance is too low",
	"余额不足",
	"insufficient_quota",
	"credit exhausted",
	"billing quota exhausted",
}

// subscriptionOAuthUsageLimitMarkers target plan/usage-limit exhaustion (a
// multi-hour, weekly, or monthly cap that auto-resets), distinct from a short
// burst rate limit. They are matched only on a 429 by
// IsSubscriptionOAuthUsageLimit, so the generic "rate limit" phrasing is
// intentionally excluded to avoid over-cooling a transient burst.
var subscriptionOAuthUsageLimitMarkers = []string{
	"usage limit",
	"usage_limit",
	"reached your usage limit",
	"weekly limit",
	"monthly limit",
	"plan limit",
	"你已达到使用上限",
	"使用上限",
}

var subscriptionOAuthModelCapacityMarkers = []string{
	"selected model is at capacity",
	"model is at capacity",
	"model_at_capacity",
	"model capacity",
	"server_is_overloaded",
	"server_overloaded",
	"service_unavailable_error",
	"servers are currently overloaded",
	"server is currently overloaded",
}

func containsSubscriptionOAuthErrorMarker(message, code string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(message, marker) || strings.Contains(code, marker) {
			return true
		}
	}
	return false
}
