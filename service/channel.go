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

func IsSubscriptionOAuthTransientError(channelType int, err *types.NewAPIError) bool {
	if err == nil || (channelType != constant.ChannelTypeClaudeCode && channelType != constant.ChannelTypeCodex) {
		return false
	}
	return err.StatusCode >= 500 && err.StatusCode <= 599
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
	modelMentioned := strings.Contains(lowerMessage, "model")
	modelUnavailable := strings.Contains(lowerMessage, "not supported") ||
		strings.Contains(lowerMessage, "unsupported") ||
		strings.Contains(lowerMessage, "not found") ||
		strings.Contains(lowerMessage, "does not exist") ||
		strings.Contains(lowerMessage, "not available") ||
		strings.Contains(lowerMessage, "no access")
	if modelMentioned && modelUnavailable &&
		(err.StatusCode == http.StatusBadRequest || err.StatusCode == http.StatusForbidden ||
			err.StatusCode == http.StatusNotFound || err.StatusCode == http.StatusUnprocessableEntity) {
		return types.NewErrorWithStatusCode(
			errors.New("selected model is not supported by this OAuth account"),
			types.ErrorCodeModelNotSupported,
			err.StatusCode,
		)
	}
	switch err.StatusCode {
	case http.StatusUnauthorized:
		return types.NewErrorWithStatusCode(
			errors.New("OAuth credential is invalid or expired"),
			types.ErrorCodeOAuthUnauthorized,
			err.StatusCode,
		)
	case http.StatusForbidden:
		return types.NewErrorWithStatusCode(
			errors.New("OAuth account is not permitted to access this resource"),
			types.ErrorCodeOAuthForbidden,
			err.StatusCode,
		)
	default:
		return err
	}
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
