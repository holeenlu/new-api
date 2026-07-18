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
	if err != nil && isSubscriptionOAuthChannelType(channelType) &&
		err.GetUpstreamStatusCode() == http.StatusTooManyRequests {
		return false
	}
	if IsSubscriptionOAuthTransientError(channelType, err) {
		return false
	}
	return ShouldDisableChannel(err)
}

func IsSubscriptionOAuthTransientError(channelType int, err *types.NewAPIError) bool {
	if err == nil || !isSubscriptionOAuthChannelType(channelType) {
		return false
	}
	statusCode := err.GetUpstreamStatusCode()
	if statusCode == http.StatusTooManyRequests {
		return common.SubscriptionOAuthRetry429
	}
	return statusCode >= 500 && statusCode <= 599
}

func IsSubscriptionOAuthConcurrencyLimit(channelType int, err *types.NewAPIError) bool {
	return err != nil && isSubscriptionOAuthChannelType(channelType) &&
		err.GetErrorCode() == types.ErrorCodeOAuthChannelConcurrencyLimit
}

func IsSubscriptionOAuthAccountUnavailable(channelType int, err *types.NewAPIError) bool {
	if err == nil || !isSubscriptionOAuthChannelType(channelType) {
		return false
	}
	return err.GetErrorCode() == types.ErrorCodeOAuthUnauthorized ||
		err.GetErrorCode() == types.ErrorCodeOAuthForbidden ||
		err.GetErrorCode() == types.ErrorCodeUpstreamAccountDisabled ||
		err.GetErrorCode() == types.ErrorCodeUpstreamQuotaExhausted
}

// QuarantineSubscriptionOAuthCredential persistently removes every reference
// to a rejected credential from routing. Unlike automatic channel banning,
// this account-safety rule is not optional.
func QuarantineSubscriptionOAuthCredential(channelError types.ChannelError, err *types.NewAPIError) bool {
	if !IsSubscriptionOAuthAccountUnavailable(channelError.ChannelType, err) {
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
	return err != nil && isSubscriptionOAuthChannelType(channelType) &&
		err.GetErrorCode() == types.ErrorCodeModelNotSupported
}

func IsSubscriptionOAuthModelAtCapacity(channelType int, err *types.NewAPIError) bool {
	return err != nil && isSubscriptionOAuthChannelType(channelType) &&
		err.GetErrorCode() == types.ErrorCodeModelAtCapacity
}

// ApplyChannelErrorPolicy classifies subscription account failures and keeps
// only transient transport failures eligible for the request-local retry loop.
func ApplyChannelErrorPolicy(channelType int, err *types.NewAPIError) *types.NewAPIError {
	if err == nil || !isSubscriptionOAuthChannelType(channelType) {
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
	lowerMessage := strings.ToLower(err.Error())
	lowerCode := strings.ToLower(string(err.GetErrorCode()))
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
	if containsSubscriptionOAuthErrorMarker(lowerMessage, lowerCode, subscriptionOAuthAccountDisabledMarkers) {
		return err.Reclassify(err.Err, types.ErrorCodeUpstreamAccountDisabled)
	}
	if containsSubscriptionOAuthErrorMarker(lowerMessage, lowerCode, subscriptionOAuthQuotaExhaustedMarkers) {
		return err.Reclassify(err.Err, types.ErrorCodeUpstreamQuotaExhausted)
	}
	switch statusCode {
	case http.StatusUnauthorized:
		return err.Reclassify(errors.New("OAuth credential is invalid or expired"), types.ErrorCodeOAuthUnauthorized)
	case http.StatusForbidden:
		return err.Reclassify(errors.New("OAuth account is not permitted to access this resource"), types.ErrorCodeOAuthForbidden)
	case http.StatusTooManyRequests:
		return err.Reclassify(err.Err, types.ErrorCodeUpstreamRateLimited)
	default:
		return err
	}
}

func isSubscriptionOAuthChannelType(channelType int) bool {
	return channelType == constant.ChannelTypeCodex || channelType == constant.ChannelTypeClaudeCode
}

var subscriptionOAuthAccountDisabledMarkers = []string{
	"this organization has been disabled",
	"organization disabled",
	"account disabled",
	"account has been disabled",
}

var subscriptionOAuthQuotaExhaustedMarkers = []string{
	"your credit balance is too low",
	"you exceeded your current quota",
	"余额不足",
	"额度已用完",
	"调用次数已达上限",
	"insufficient_quota",
	"credit exhausted",
	"out of quota",
	"no quota available",
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
