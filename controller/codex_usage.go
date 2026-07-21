package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/codex"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

const codexOAuthUsageCorrelationTimeout = 5 * time.Second

func GetCodexChannelUsage(c *gin.Context) {
	fetchCodexChannelWhamData(
		c,
		service.FetchCodexWhamUsage,
		"failed to fetch codex usage",
		"获取用量信息失败，请稍后重试",
	)
}

func GetCodexChannelRateLimitResetCredits(c *gin.Context) {
	fetchCodexChannelWhamData(
		c,
		service.FetchCodexWhamRateLimitResetCredits,
		"failed to fetch codex reset credits",
		"获取重置次数详情失败，请稍后重试",
	)
}

func ResetCodexChannelUsage(c *gin.Context) {
	fetchCodexChannelWhamData(
		c,
		service.ConsumeCodexWhamRateLimitResetCredit,
		"failed to reset codex usage",
		"重置用量失败，请稍后重试",
	)
}

type codexWhamFetchFunc func(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	accessToken string,
	accountID string,
) (statusCode int, body []byte, err error)

func fetchCodexChannelWhamData(
	c *gin.Context,
	fetch codexWhamFetchFunc,
	logPrefix string,
	userMessage string,
) {
	channelId, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		common.ApiError(c, fmt.Errorf("invalid channel id: %w", err))
		return
	}

	ch, err := model.GetChannelById(channelId, true)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if ch == nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "channel not found"})
		return
	}
	if ch.Type != constant.ChannelTypeCodex {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "channel type is not Codex"})
		return
	}
	if ch.ChannelInfo.IsMultiKey {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "multi-key channel is not supported"})
		return
	}

	oauthKey, err := codex.ParseOAuthKey(strings.TrimSpace(ch.Key))
	if err != nil {
		common.SysError("failed to parse oauth key: " + err.Error())
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "解析凭证失败，请检查渠道配置"})
		return
	}
	accessToken := strings.TrimSpace(oauthKey.AccessToken)
	accountID := strings.TrimSpace(oauthKey.AccountID)
	if accessToken == "" {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "codex channel: access_token is required"})
		return
	}
	if accountID == "" {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "codex channel: account_id is required"})
		return
	}
	responseHeaderTimeout := time.Duration(common.SubscriptionOAuthResponseHeaderTimeout) * time.Second
	if responseHeaderTimeout <= 0 {
		responseHeaderTimeout = 30 * time.Second
	}
	client, err := service.GetHttpClientWithResponseHeaderTimeout(ch.GetSetting().Proxy, responseHeaderTimeout)
	if err != nil {
		common.ApiError(c, err)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), responseHeaderTimeout+5*time.Second)
	defer cancel()

	statusCode, body, err := fetch(ctx, client, ch.GetBaseURL(), accessToken, accountID)
	if err != nil {
		common.SysError(logPrefix + ": " + err.Error())
		c.JSON(http.StatusOK, gin.H{"success": false, "message": userMessage})
		return
	}

	if statusCode == http.StatusUnauthorized && strings.TrimSpace(oauthKey.RefreshToken) != "" {
		refreshCtx, refreshCancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
		refreshedKey, refreshedChannel, refreshErr := service.RefreshCodexChannelCredential(
			refreshCtx,
			ch.Id,
			service.CodexCredentialRefreshOptions{
				ResetCaches:         true,
				ExpectedAccessToken: oauthKey.AccessToken,
			},
		)
		refreshCancel()
		if refreshErr != nil {
			common.SysError(logPrefix + " credential refresh failed: " + refreshErr.Error())
		} else {
			oauthKey = refreshedKey
			ch = refreshedChannel
			accountID = strings.TrimSpace(oauthKey.AccountID)
			client, err = service.GetHttpClientWithResponseHeaderTimeout(ch.GetSetting().Proxy, responseHeaderTimeout)
			if err != nil {
				common.ApiError(c, err)
				return
			}
			ctx2, cancel2 := context.WithTimeout(c.Request.Context(), responseHeaderTimeout+5*time.Second)
			statusCode, body, err = fetch(ctx2, client, ch.GetBaseURL(), oauthKey.AccessToken, accountID)
			cancel2()
			if err != nil {
				common.SysError(logPrefix + " after refresh: " + err.Error())
				c.JSON(http.StatusOK, gin.H{"success": false, "message": userMessage})
				return
			}
		}
	}

	var payload any
	if common.Unmarshal(body, &payload) != nil {
		payload = string(body)
	}

	ok := statusCode >= 200 && statusCode < 300
	resp := gin.H{
		"success":         ok,
		"message":         "",
		"upstream_status": statusCode,
		"data":            payload,
	}
	if !ok {
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = fmt.Sprintf("upstream status: %d", statusCode)
		}
		upstreamErr := types.NewOpenAIError(errors.New(message), types.ErrorCodeBadResponseStatusCode, statusCode)
		upstreamErr.UpstreamStatusCode = statusCode
		upstreamErr = service.ApplyChannelErrorPolicy(ch.Type, upstreamErr)
		resp["error_code"] = upstreamErr.GetErrorCode()
		resp["message"] = upstreamErr.Error()
	}
	c.JSON(http.StatusOK, resp)
}

func correlateCodexOAuthUsageLimit(
	c *gin.Context,
	channel *model.Channel,
	apiError *types.NewAPIError,
) *types.NewAPIError {
	if c == nil || channel == nil || apiError == nil ||
		channel.Type != constant.ChannelTypeCodex ||
		apiError.GetUpstreamStatusCode() != http.StatusTooManyRequests {
		return apiError
	}

	oauthKey, err := codex.ParseOAuthKey(
		common.GetContextKeyString(c, constant.ContextKeyChannelKey),
	)
	if err != nil || strings.TrimSpace(oauthKey.AccessToken) == "" || strings.TrimSpace(oauthKey.AccountID) == "" {
		return apiError
	}
	client, err := service.GetHttpClientWithResponseHeaderTimeout(
		channel.GetSetting().Proxy,
		codexOAuthUsageCorrelationTimeout,
	)
	if err != nil {
		logger.LogWarn(c, fmt.Sprintf("Codex OAuth usage correlation skipped: %s", err.Error()))
		return apiError
	}
	keyIndex := common.GetContextKeyInt(c, constant.ContextKeyChannelMultiKeyIndex)
	fingerprint := service.SubscriptionOAuthCredentialFingerprint(
		channel.Type,
		channel.Id,
		keyIndex,
		common.GetContextKeyString(c, constant.ContextKeyChannelKey),
	)
	ctx, cancel := context.WithTimeout(c.Request.Context(), codexOAuthUsageCorrelationTimeout)
	defer cancel()
	statusCode, snapshot, err := service.FetchCodexWhamUsageSnapshot(
		ctx,
		client,
		channel.GetBaseURL(),
		oauthKey.AccessToken,
		oauthKey.AccountID,
		fingerprint,
	)
	if err != nil {
		logger.LogWarn(c, fmt.Sprintf("Codex OAuth usage correlation failed: %s", err.Error()))
		return apiError
	}
	if statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices || snapshot == nil {
		logger.LogWarn(c, fmt.Sprintf("Codex OAuth usage correlation returned status %d", statusCode))
		return apiError
	}
	return service.CorrelateCodexOAuthUsageLimit(apiError, snapshot, time.Now())
}
