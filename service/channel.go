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
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/bytedance/gopkg/util/gopool"
)

func formatNotifyType(channelId int, status int) string {
	return fmt.Sprintf("%s_%d_%d", dto.NotifyTypeChannelUpdate, channelId, status)
}

// disable & notify
func DisableChannel(channelError types.ChannelError, reason string) {
	common.SysLog(fmt.Sprintf("通道「%s」（#%d）发生错误，准备禁用，原因：%s", channelError.ChannelName, channelError.ChannelId, common.LocalLogPreview(reason)))

	// 检查是否启用自动禁用功能
	if !channelError.AutoBan {
		common.SysLog(fmt.Sprintf("通道「%s」（#%d）未启用自动禁用功能，跳过禁用操作", channelError.ChannelName, channelError.ChannelId))
		return
	}

	success := model.UpdateChannelStatus(channelError.ChannelId, channelError.UsingKey, common.ChannelStatusAutoDisabled, reason)
	if success {
		subject := fmt.Sprintf("通道「%s」（#%d）已被禁用", channelError.ChannelName, channelError.ChannelId)
		content := fmt.Sprintf("通道「%s」（#%d）已被禁用，原因：%s", channelError.ChannelName, channelError.ChannelId, reason)
		NotifyRootUser(formatNotifyType(channelError.ChannelId, common.ChannelStatusAutoDisabled), subject, content)
	}
}

func EnableChannel(channelId int, usingKey string, channelName string) {
	success := model.UpdateChannelStatus(channelId, usingKey, common.ChannelStatusEnabled, "")
	if success {
		subject := fmt.Sprintf("通道「%s」（#%d）已被启用", channelName, channelId)
		content := fmt.Sprintf("通道「%s」（#%d）已被启用", channelName, channelId)
		NotifyRootUser(formatNotifyType(channelId, common.ChannelStatusEnabled), subject, content)
	}
}

func ShouldDisableChannel(err *types.NewAPIError) bool {
	if !common.AutomaticDisableChannelEnabled {
		return false
	}
	if err == nil {
		return false
	}
	if types.IsChannelError(err) {
		return true
	}
	if types.IsSkipRetryError(err) {
		return false
	}
	if operation_setting.ShouldDisableByStatusCode(err.StatusCode) {
		return true
	}

	lowerMessage := strings.ToLower(err.Error())
	search, _ := AcSearch(lowerMessage, operation_setting.AutomaticDisableKeywords, true)
	return search
}

// ShouldDisableChannelForType keeps transient subscription transport failures
// from being mistaken for invalid OAuth credentials. A 5xx may be retried or
// routed elsewhere, but it must not change the channel's enabled state.
func ShouldDisableChannelForType(channelType int, err *types.NewAPIError) bool {
	if err != nil && (channelType == constant.ChannelTypeClaudeCode || channelType == constant.ChannelTypeCodex) &&
		err.GetUpstreamStatusCode() == http.StatusTooManyRequests {
		return false
	}
	if IsSubscriptionOAuthTransientError(channelType, err) {
		return false
	}
	return ShouldDisableChannel(err)
}

func IsSubscriptionOAuthTransientError(channelType int, err *types.NewAPIError) bool {
	if err == nil || (channelType != constant.ChannelTypeClaudeCode && channelType != constant.ChannelTypeCodex) {
		return false
	}
	statusCode := err.GetUpstreamStatusCode()
	if statusCode == http.StatusTooManyRequests {
		return common.SubscriptionOAuthRetry429
	}
	return statusCode >= 500 && statusCode <= 599
}

func IsSubscriptionOAuthConcurrencyLimit(channelType int, err *types.NewAPIError) bool {
	return err != nil &&
		(channelType == constant.ChannelTypeClaudeCode || channelType == constant.ChannelTypeCodex) &&
		err.GetErrorCode() == types.ErrorCodeOAuthChannelConcurrencyLimit
}

func IsSubscriptionOAuthAccountUnavailable(channelType int, err *types.NewAPIError) bool {
	if err == nil || (channelType != constant.ChannelTypeClaudeCode && channelType != constant.ChannelTypeCodex) {
		return false
	}
	return err.GetErrorCode() == types.ErrorCodeOAuthUnauthorized ||
		err.GetErrorCode() == types.ErrorCodeOAuthForbidden ||
		err.GetErrorCode() == types.ErrorCodeUpstreamAccountDisabled ||
		err.GetErrorCode() == types.ErrorCodeUpstreamQuotaExhausted
}

// QuarantineSubscriptionOAuthCredential persistently removes a rejected
// credential from routing. Unlike automatic channel banning, this safety rule
// is not optional: authorization failures and exhausted provider accounts must
// not be offered to another user in the same retry tag.
func QuarantineSubscriptionOAuthCredential(channelError types.ChannelError, err *types.NewAPIError) bool {
	if !IsSubscriptionOAuthAccountUnavailable(channelError.ChannelType, err) {
		return false
	}
	reason := fmt.Sprintf("%s: %s", err.GetErrorCode(), common.LocalLogPreview(err.Error()))
	if !model.UpdateChannelStatus(channelError.ChannelId, channelError.UsingKey, common.ChannelStatusManuallyDisabled, reason) {
		return false
	}
	subject := fmt.Sprintf("OAuth 凭证已隔离：通道「%s」（#%d）", channelError.ChannelName, channelError.ChannelId)
	content := fmt.Sprintf("通道「%s」（#%d）的 OAuth 凭证已从用户路由中隔离，请管理员检查并重新授权后再手动启用。错误代码：%s；原因：%s",
		channelError.ChannelName, channelError.ChannelId, err.GetErrorCode(), common.LocalLogPreview(err.Error()))
	gopool.Go(func() {
		NotifyAdminUsers(formatNotifyType(channelError.ChannelId, common.ChannelStatusManuallyDisabled), subject, content)
	})
	return true
}

func IsSubscriptionOAuthModelUnavailable(channelType int, err *types.NewAPIError) bool {
	return err != nil &&
		(channelType == constant.ChannelTypeClaudeCode || channelType == constant.ChannelTypeCodex) &&
		err.GetErrorCode() == types.ErrorCodeModelNotSupported
}

// ApplyChannelErrorPolicy prevents subscription-backed channels from
// amplifying client errors and rate-limit responses. Transient upstream 5xx
// failures remain retryable so routing can fail over to another account.
func ApplyChannelErrorPolicy(channelType int, err *types.NewAPIError) *types.NewAPIError {
	if err == nil || (channelType != constant.ChannelTypeClaudeCode && channelType != constant.ChannelTypeCodex) {
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
		return err.Reclassify(
			errors.New("selected model is not supported by this OAuth account"),
			types.ErrorCodeModelNotSupported,
		)
	}
	if containsSubscriptionOAuthErrorMarker(lowerMessage, lowerCode, subscriptionOAuthAccountDisabledMarkers) {
		return err.Reclassify(err.Err, types.ErrorCodeUpstreamAccountDisabled)
	}
	if containsSubscriptionOAuthErrorMarker(lowerMessage, lowerCode, subscriptionOAuthQuotaExhaustedMarkers) {
		return err.Reclassify(err.Err, types.ErrorCodeUpstreamQuotaExhausted)
	}
	switch statusCode {
	case http.StatusUnauthorized:
		return err.Reclassify(
			errors.New("OAuth credential is invalid or expired"),
			types.ErrorCodeOAuthUnauthorized,
		)
	case http.StatusForbidden:
		return err.Reclassify(
			errors.New("OAuth account is not permitted to access this resource"),
			types.ErrorCodeOAuthForbidden,
		)
	case http.StatusTooManyRequests:
		return err.Reclassify(err.Err, types.ErrorCodeUpstreamRateLimited)
	default:
		return err
	}
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

func containsSubscriptionOAuthErrorMarker(message, code string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(message, marker) || strings.Contains(code, marker) {
			return true
		}
	}
	return false
}

func ShouldEnableChannel(newAPIError *types.NewAPIError, status int) bool {
	if !common.AutomaticEnableChannelEnabled {
		return false
	}
	if newAPIError != nil {
		return false
	}
	if status != common.ChannelStatusAutoDisabled {
		return false
	}
	return true
}
