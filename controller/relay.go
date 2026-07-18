package controller

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	perfmetrics "github.com/QuantumNous/new-api/pkg/perf_metrics"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/samber/lo"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func relayHandler(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	var err *types.NewAPIError
	switch info.RelayMode {
	case relayconstant.RelayModeImagesGenerations, relayconstant.RelayModeImagesEdits:
		err = relay.ImageHelper(c, info)
	case relayconstant.RelayModeAudioSpeech:
		fallthrough
	case relayconstant.RelayModeAudioTranslation:
		fallthrough
	case relayconstant.RelayModeAudioTranscription:
		err = relay.AudioHelper(c, info)
	case relayconstant.RelayModeRerank:
		err = relay.RerankHelper(c, info)
	case relayconstant.RelayModeEmbeddings:
		err = relay.EmbeddingHelper(c, info)
	case relayconstant.RelayModeResponses, relayconstant.RelayModeResponsesCompact:
		err = relay.ResponsesHelper(c, info)
	default:
		err = relay.TextHelper(c, info)
	}
	return err
}

func geminiRelayHandler(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	var err *types.NewAPIError
	if strings.Contains(c.Request.URL.Path, "embed") {
		err = relay.GeminiEmbeddingHandler(c, info)
	} else {
		err = relay.GeminiHelper(c, info)
	}
	return err
}

func Relay(c *gin.Context, relayFormat types.RelayFormat) {

	requestId := c.GetString(common.RequestIdKey)
	//group := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
	//originalModel := common.GetContextKeyString(c, constant.ContextKeyOriginalModel)

	var (
		newAPIError *types.NewAPIError
		ws          *websocket.Conn
	)

	if relayFormat == types.RelayFormatOpenAIRealtime {
		var err error
		ws, err = upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			helper.WssError(c, ws, types.NewError(err, types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry()).ToOpenAIError())
			return
		}
		defer ws.Close()
	}

	defer func() {
		if newAPIError != nil {
			logger.LogError(c, fmt.Sprintf(
				"relay error: status_code=%d error_type=%v error_code=%v detail=%s",
				newAPIError.StatusCode,
				newAPIError.GetErrorType(),
				newAPIError.GetErrorCode(),
				newAPIError.Error(),
			))
			if newAPIError.StatusCode == http.StatusGatewayTimeout &&
				relaycommon.IsSubscriptionOAuthChannel(c.GetInt("channel_type")) {
				c.Header("Retry-After", "2")
			}
			if relaycommon.IsResponsesStreamFailureEmitted(c) {
				return
			}
			newAPIError.SetMessage(common.MessageWithRequestId(newAPIError.Error(), requestId))
			switch relayFormat {
			case types.RelayFormatOpenAIRealtime:
				helper.WssError(c, ws, newAPIError.ToOpenAIError())
			case types.RelayFormatClaude:
				c.JSON(newAPIError.StatusCode, gin.H{
					"type":  "error",
					"error": newAPIError.ToClaudeError(),
				})
			default:
				c.JSON(newAPIError.StatusCode, gin.H{
					"error": newAPIError.ToOpenAIError(),
				})
			}
		}
	}()

	request, err := helper.GetAndValidateRequest(c, relayFormat)
	if err != nil {
		// Map "request body too large" to 413 so clients can handle it correctly
		if common.IsRequestBodyTooLargeError(err) || errors.Is(err, common.ErrRequestBodyTooLarge) {
			newAPIError = types.NewErrorWithStatusCode(err, types.ErrorCodeReadRequestBodyFailed, http.StatusRequestEntityTooLarge, types.ErrOptionWithSkipRetry())
		} else {
			newAPIError = types.NewError(err, types.ErrorCodeInvalidRequest)
		}
		return
	}

	relayInfo, err := relaycommon.GenRelayInfo(c, relayFormat, request, ws)
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeGenRelayInfoFailed)
		return
	}

	needSensitiveCheck := setting.ShouldCheckPromptSensitive()
	needCountToken := constant.CountToken
	// Avoid building huge CombineText (strings.Join) when token counting and sensitive check are both disabled.
	var meta *types.TokenCountMeta
	if needSensitiveCheck || needCountToken {
		meta = request.GetTokenCountMeta()
	} else {
		meta = fastTokenCountMetaForPricing(request)
	}

	if needSensitiveCheck && meta != nil {
		contains, words := service.CheckSensitiveText(meta.CombineText)
		if contains {
			logger.LogWarn(c, fmt.Sprintf("user sensitive words detected: %s", strings.Join(words, ", ")))
			newAPIError = types.NewError(err, types.ErrorCodeSensitiveWordsDetected)
			return
		}
	}

	tokens, err := service.EstimateRequestToken(c, meta, relayInfo)
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeCountTokenFailed)
		return
	}

	relayInfo.SetEstimatePromptTokens(tokens)

	priceData, err := helper.ModelPriceHelper(c, relayInfo, tokens, meta)
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeModelPriceError, types.ErrOptionWithStatusCode(http.StatusBadRequest))
		return
	}

	// common.SetContextKey(c, constant.ContextKeyTokenCountMeta, meta)

	if priceData.FreeModel {
		logger.LogInfo(c, fmt.Sprintf("模型 %s 免费，跳过预扣费", relayInfo.OriginModelName))
	} else {
		newAPIError = service.PreConsumeBilling(c, priceData.QuotaToPreConsume, relayInfo)
		if newAPIError != nil {
			return
		}
	}

	defer func() {
		// Only return quota if downstream failed and quota was actually pre-consumed
		if newAPIError != nil {
			newAPIError = service.NormalizeViolationFeeError(newAPIError)
			if relayInfo.Billing != nil {
				relayInfo.Billing.Refund(c)
			}
			service.ChargeViolationFeeIfNeeded(c, relayInfo, newAPIError)
		}
	}()

	retryParam := service.NewRetryParam(c, relayInfo.TokenGroup, relayInfo.OriginModelName, c.Request.URL.Path)
	relayInfo.RetryIndex = 0
	relayInfo.LastError = nil

	retryTimes := common.RetryTimes
	isSubscriptionOAuth := relaycommon.IsSubscriptionOAuthChannel(
		common.GetContextKeyInt(c, constant.ContextKeyChannelType),
	)
	if isSubscriptionOAuth {
		retryTimes = common.SubscriptionOAuthUpstreamRetryTimes
	}
	for {
		if !isSubscriptionOAuth && retryParam.GetRetry() > retryTimes {
			break
		}
		channel, channelErr := getChannel(c, relayInfo, retryParam)
		if channelErr != nil {
			logger.LogError(c, channelErr.Error())
			if relayInfo.LastError != nil && channelErr.StatusCode != 499 &&
				(isSubscriptionOAuth || retryParam.GetRetry() > 0) {
				newAPIError = relayInfo.LastError
			} else {
				newAPIError = channelErr
			}
			break
		}

		if !trackRetryAttempt(c, retryParam, channel) {
			continue
		}
		retryParam.RecordAttempt()
		if isSubscriptionOAuth {
			relayInfo.RetryIndex = retryParam.AttemptIndex()
		} else {
			relayInfo.RetryIndex = retryParam.GetRetry()
		}
		addUsedChannel(c, channel.Id)
		attempt := len(c.GetStringSlice("use_channel"))
		service.ApplyRelayDataPolicyHeaders(c, channel, attempt)
		clearStaleRetryAfter(c)
		relaycommon.ClearResponsesStreamPreflightFailureEvent(c)
		relayInfo.ResetUpstreamAttemptState()
		bodyStorage, bodyErr := common.GetBodyStorage(c)
		if bodyErr != nil {
			// Ensure consistent 413 for oversized bodies even when error occurs later (e.g., retry path)
			if common.IsRequestBodyTooLargeError(bodyErr) || errors.Is(bodyErr, common.ErrRequestBodyTooLarge) {
				newAPIError = types.NewErrorWithStatusCode(bodyErr, types.ErrorCodeReadRequestBodyFailed, http.StatusRequestEntityTooLarge, types.ErrOptionWithSkipRetry())
			} else {
				newAPIError = types.NewErrorWithStatusCode(bodyErr, types.ErrorCodeReadRequestBodyFailed, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
			}
			break
		}
		c.Request.Body = io.NopCloser(bodyStorage)

		switch relayFormat {
		case types.RelayFormatOpenAIRealtime:
			newAPIError = relay.WssHelper(c, relayInfo)
		case types.RelayFormatClaude:
			newAPIError = relay.ClaudeHelper(c, relayInfo)
		case types.RelayFormatGemini:
			newAPIError = geminiRelayHandler(c, relayInfo)
		default:
			newAPIError = relayHandler(c, relayInfo)
		}
		retryParam.CaptureSubscriptionOAuthAttemptMetadata(c)

		if newAPIError == nil {
			affinitySuccess := true
			if relayInfo.StreamStatus != nil &&
				(!relayInfo.StreamStatus.IsNormalEnd() || relayInfo.StreamStatus.HasErrors()) {
				affinitySuccess = false
			}
			if isSubscriptionOAuth && affinitySuccess {
				retryParam.MarkSubscriptionOAuthSuccess()
			} else if isSubscriptionOAuth {
				retryParam.MarkSubscriptionOAuthFailure()
			}
			c.Set("relay_affinity_success", affinitySuccess)
			relayInfo.LastError = nil
			return
		}
		retryParam.MarkSubscriptionOAuthFailure()

		newAPIError = service.NormalizeViolationFeeError(newAPIError)
		newAPIError = service.ApplyChannelErrorPolicy(channel.Type, newAPIError)
		relayInfo.LastError = newAPIError

		if service.IsSubscriptionOAuthConcurrencyLimit(channel.Type, newAPIError) {
			logger.LogWarn(c, fmt.Sprintf(
				"subscription OAuth capacity failover: channel=%d credential=%s",
				channel.Id,
				service.SubscriptionOAuthCredentialPreview(c),
			))
		} else {
			processChannelError(c, *types.NewChannelError(channel.Id, channel.Type, channel.Name, channel.ChannelInfo.IsMultiKey, common.GetContextKeyString(c, constant.ContextKeyChannelKey), channel.GetAutoBan()), newAPIError)
		}

		if isSubscriptionOAuth {
			if !shouldContinueSubscriptionOAuthRetry(c, relayInfo, retryParam, newAPIError) {
				if relayInfo.RelayMode == relayconstant.RelayModeResponses && !c.Writer.Written() {
					if data, exists := relaycommon.GetResponsesStreamPreflightFailureEvent(c); exists {
						helper.SetEventStreamHeaders(c)
						if helper.ResponseChunkData(c, dto.ResponsesStreamResponse{Type: "response.failed"}, data) == nil {
							relaycommon.MarkResponsesStreamFailureEmitted(c)
						}
					}
				}
				break
			}
			continue
		}
		if !shouldRetryOrdinaryRelay(c, relayInfo, newAPIError, retryTimes-retryParam.GetRetry()) {
			break
		}
		retryParam.IncreaseRetry()
	}

	useChannel := c.GetStringSlice("use_channel")
	if len(useChannel) > 1 {
		retryLogStr := fmt.Sprintf("重试：%s", strings.Trim(strings.Join(strings.Fields(fmt.Sprint(useChannel)), "->"), "[]"))
		logger.LogInfo(c, retryLogStr)
	}
	if newAPIError != nil {
		gopool.Go(func() {
			perfmetrics.RecordRelaySample(relayInfo, false, 0)
		})
	}
}

func shouldRetryOrdinaryRelay(c *gin.Context, info *relaycommon.RelayInfo, apiError *types.NewAPIError, retryTimes int) bool {
	if !shouldRetry(c, apiError, retryTimes) || c == nil || c.Writer == nil || c.Writer.Written() {
		return false
	}
	written, responseStarted := info.UpstreamAttemptState()
	return !written && !responseStarted
}

func shouldContinueSubscriptionOAuthRetry(
	c *gin.Context,
	info *relaycommon.RelayInfo,
	retryParam *service.RetryParam,
	apiError *types.NewAPIError,
) bool {
	written, responseStarted := info.UpstreamAttemptState()
	_, specificChannel := c.Get("specific_channel_id")
	decision, retryAfter := retryParam.DecideSubscriptionOAuthContinuation(service.SubscriptionOAuthRetryObservation{
		ChannelType:             c.GetInt("channel_type"),
		Error:                   apiError,
		UpstreamRequestWritten:  written,
		UpstreamResponseStarted: responseStarted,
		ExplicitFailureResponse: info.HasUpstreamFailureResponse(),
		DownstreamStarted:       c.Writer.Written(),
		RetryDisabled:           service.IsSubscriptionOAuthRetryDisabled(c),
		SpecificChannel:         specificChannel,
		Retryable:               shouldRetry(c, apiError, common.SubscriptionOAuthUpstreamRetryTimes),
	})
	if retryAfter > 0 {
		seconds := int((retryAfter + time.Second - 1) / time.Second)
		c.Header("Retry-After", strconv.Itoa(max(seconds, 1)))
	}
	return decision != service.SubscriptionOAuthRetryStop
}

var upgrader = websocket.Upgrader{
	Subprotocols: []string{"realtime"}, // WS 握手支持的协议，如果有使用 Sec-WebSocket-Protocol，则必须在此声明对应的 Protocol TODO add other protocol
	CheckOrigin: func(r *http.Request) bool {
		return true // 允许跨域
	},
}

func addUsedChannel(c *gin.Context, channelId int) {
	useChannel := c.GetStringSlice("use_channel")
	useChannel = append(useChannel, fmt.Sprintf("%d", channelId))
	c.Set("use_channel", useChannel)
}

func clearStaleRetryAfter(c *gin.Context) {
	if c == nil || c.Writer == nil {
		return
	}
	c.Writer.Header().Del("Retry-After")
}

func trackRetryAttempt(c *gin.Context, retryParam *service.RetryParam, channel *model.Channel) bool {
	if retryParam.Boundary == nil {
		retryParam.Boundary = service.NewRetryBoundary(channel, retryParam.EffectiveGroup)
		if retryParam.Boundary != nil {
			retryParam.CandidateFilter = retryParam.Boundary.Allows
		}
	}
	if retryParam.Boundary == nil {
		return true
	}
	if relaycommon.IsSubscriptionOAuthChannel(channel.Type) {
		service.ClearSubscriptionOAuthAttemptMetadata(c)
		keyIndex := common.GetContextKeyInt(c, constant.ContextKeyChannelMultiKeyIndex)
		fingerprint := service.SubscriptionOAuthCredentialFingerprint(
			channel.Type,
			channel.Id,
			keyIndex,
			common.GetContextKeyString(c, constant.ContextKeyChannelKey),
		)
		if !retryParam.SetSubscriptionOAuthAttempt(channel.Id, keyIndex, fingerprint) {
			return false
		}
		service.RecordSubscriptionOAuthCredential(c, fingerprint)
	}
	if channel.ChannelInfo.IsMultiKey {
		retryParam.Boundary.MarkAttempt(channel, common.GetContextKeyInt(c, constant.ContextKeyChannelMultiKeyIndex))
		return true
	}
	retryParam.Boundary.MarkAttempt(channel)
	return true
}

func fastTokenCountMetaForPricing(request dto.Request) *types.TokenCountMeta {
	if request == nil {
		return &types.TokenCountMeta{}
	}
	meta := &types.TokenCountMeta{
		TokenType: types.TokenTypeTokenizer,
	}
	switch r := request.(type) {
	case *dto.GeneralOpenAIRequest:
		maxCompletionTokens := lo.FromPtrOr(r.MaxCompletionTokens, uint(0))
		maxTokens := lo.FromPtrOr(r.MaxTokens, uint(0))
		if maxCompletionTokens > maxTokens {
			meta.MaxTokens = int(maxCompletionTokens)
		} else {
			meta.MaxTokens = int(maxTokens)
		}
	case *dto.OpenAIResponsesRequest:
		meta.MaxTokens = int(lo.FromPtrOr(r.MaxOutputTokens, uint(0)))
	case *dto.ClaudeRequest:
		meta.MaxTokens = int(lo.FromPtr(r.MaxTokens))
	case *dto.ImageRequest:
		// Pricing for image requests depends on ImagePriceRatio; safe to compute even when CountToken is disabled.
		return r.GetTokenCountMeta()
	default:
		// Best-effort: leave CombineText empty to avoid large allocations.
	}
	return meta
}

func getChannel(c *gin.Context, info *relaycommon.RelayInfo, retryParam *service.RetryParam) (*model.Channel, *types.NewAPIError) {
	if info.ChannelMeta == nil {
		channel, err := model.CacheGetChannel(c.GetInt("channel_id"))
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
		}
		return channel, nil
	}
	var channel *model.Channel
	var selectGroup string
	var err error
	for {
		if target := retryParam.SubscriptionOAuthAttemptTarget(); target != nil {
			channel, err = model.CacheGetChannel(target.ChannelID)
			if err == nil && retryParam.Boundary != nil && retryParam.Boundary.Allows(channel) {
				matched, setupErr := middleware.SetupContextForSelectedChannelAtCredential(
					c,
					channel,
					info.OriginModelName,
					target.KeyIndex,
					target.Fingerprint,
				)
				if setupErr == nil && matched {
					return channel, nil
				}
			}
			retryParam.HandleSubscriptionOAuthCredentialUnavailable()
			continue
		}

		channel, selectGroup, err = service.CacheGetRandomSatisfiedChannel(retryParam)
		if err != nil || channel != nil || retryParam.Boundary == nil || !retryParam.Boundary.HasCapacityExclusions() {
			break
		}
		delay, canRestart := retryParam.NextCapacityCycleBackoff()
		if !canRestart {
			break
		}
		timer := time.NewTimer(delay)
		select {
		case <-c.Request.Context().Done():
			timer.Stop()
			return nil, types.NewErrorWithStatusCode(
				c.Request.Context().Err(),
				types.ErrorCodeDoRequestFailed,
				499,
				types.ErrOptionWithSkipRetry(),
			)
		case <-timer.C:
		}
		if !retryParam.Boundary.RestartCapacityCycle() || !retryParam.StartCapacityReplay() {
			break
		}
	}

	info.PriceData.GroupRatioInfo = helper.HandleGroupRatio(c, info)

	if err != nil {
		return nil, types.NewError(fmt.Errorf("获取分组 %s 下模型 %s 的可用渠道失败（retry）: %s", selectGroup, info.OriginModelName, err.Error()), types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
	}
	if channel == nil {
		return nil, types.NewError(fmt.Errorf("分组 %s 下模型 %s 的可用渠道不存在（retry）", selectGroup, info.OriginModelName), types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
	}

	var excludedMultiKeyIndexes map[int]struct{}
	if retryParam.Boundary != nil {
		excludedMultiKeyIndexes = retryParam.Boundary.ExcludedKeyIndexes(channel)
	}
	newAPIError := middleware.SetupContextForSelectedChannel(c, channel, info.OriginModelName, excludedMultiKeyIndexes)
	if newAPIError != nil {
		return nil, newAPIError
	}
	return channel, nil
}

func shouldRetry(c *gin.Context, openaiErr *types.NewAPIError, retryTimes int) bool {
	if openaiErr == nil {
		return false
	}
	if _, ok := c.Get("specific_channel_id"); ok {
		return false
	}
	if service.IsSubscriptionOAuthConcurrencyLimit(c.GetInt("channel_type"), openaiErr) {
		return true
	}
	if !relaycommon.IsSubscriptionOAuthChannel(c.GetInt("channel_type")) &&
		service.ShouldSkipRetryAfterChannelAffinityFailure(c) {
		return false
	}
	if types.IsChannelError(openaiErr) {
		return true
	}
	if types.IsSkipRetryError(openaiErr) {
		return false
	}
	if operation_setting.IsAlwaysSkipRetryCode(openaiErr.GetErrorCode()) {
		return false
	}
	code := openaiErr.StatusCode
	if code >= 200 && code < 300 {
		return false
	}
	upstreamCode := openaiErr.GetUpstreamStatusCode()
	if operation_setting.IsAlwaysSkipRetryStatusCode(code) {
		return false
	}
	// A channel mapping is an explicit high-risk override. Respect its mapped
	// retry policy, but never let a raw 504/524 bypass the global guard.
	if upstreamCode != 0 && upstreamCode != code &&
		!operation_setting.ShouldRetryByStatusCode(code) {
		return false
	}
	if service.IsSubscriptionOAuthTransientError(c.GetInt("channel_type"), openaiErr) {
		return true
	}
	if retryTimes <= 0 {
		return false
	}
	if code < 100 || code > 599 {
		return false
	}
	return operation_setting.ShouldRetryByStatusCode(code)
}

func processChannelError(c *gin.Context, channelError types.ChannelError, err *types.NewAPIError) {
	errorSummary := fmt.Sprintf(
		"status_code=%d, error_type=%v, error_code=%v",
		err.StatusCode,
		err.GetErrorType(),
		err.GetErrorCode(),
	)
	// Keep the compact summary for automatic channel policy decisions, but retain
	// the actual upstream/network error in runtime and persisted error logs.
	errorDetail := err.ErrorWithStatusCode()
	logger.LogError(c, fmt.Sprintf("channel error (channel #%d): %s; detail: %s", channelError.ChannelId, errorSummary, errorDetail))
	if service.IsSubscriptionOAuthAccountUnavailable(channelError.ChannelType, err) {
		service.QuarantineSubscriptionOAuthCredential(channelError, err)
	}
	// 不要使用context获取渠道信息，异步处理时可能会出现渠道信息不一致的情况
	// do not use context to get channel info, there may be inconsistent channel info when processing asynchronously
	if service.ShouldDisableChannelForType(channelError.ChannelType, err) && channelError.AutoBan {
		gopool.Go(func() {
			service.DisableChannel(channelError, errorSummary)
		})
	}

	if constant.ErrorLogEnabled && types.IsRecordErrorLog(err) {
		// 保存错误日志到mysql中
		userId := c.GetInt("id")
		tokenName := c.GetString("token_name")
		modelName := c.GetString("original_model")
		tokenId := c.GetInt("token_id")
		userGroup := c.GetString("group")
		channelId := c.GetInt("channel_id")
		other := make(map[string]interface{})
		if c.Request != nil && c.Request.URL != nil {
			other["request_path"] = c.Request.URL.Path
		}
		other["error_type"] = err.GetErrorType()
		other["error_code"] = err.GetErrorCode()
		other["status_code"] = err.StatusCode
		other["channel_id"] = channelId
		other["channel_name"] = c.GetString("channel_name")
		other["channel_type"] = c.GetInt("channel_type")
		adminInfo := make(map[string]interface{})
		adminInfo["use_channel"] = c.GetStringSlice("use_channel")
		if credentialPath := service.SubscriptionOAuthCredentialPath(c); len(credentialPath) > 0 {
			adminInfo["subscription_oauth_credential_path"] = credentialPath
			adminInfo["subscription_oauth_credential_fp"] = service.SubscriptionOAuthCredentialPreview(c)
			adminInfo["subscription_oauth_effective_group"] = service.SubscriptionOAuthEffectiveGroup(c)
		}
		isMultiKey := common.GetContextKeyBool(c, constant.ContextKeyChannelIsMultiKey)
		if isMultiKey {
			adminInfo["is_multi_key"] = true
			adminInfo["multi_key_index"] = common.GetContextKeyInt(c, constant.ContextKeyChannelMultiKeyIndex)
		}
		service.AppendChannelAffinityAdminInfo(c, adminInfo)
		other["admin_info"] = adminInfo
		startTime := common.GetContextKeyTime(c, constant.ContextKeyRequestStartTime)
		if startTime.IsZero() {
			startTime = time.Now()
		}
		useTimeSeconds := int(time.Since(startTime).Seconds())
		model.RecordErrorLog(
			c, userId, channelId, modelName, tokenName, errorDetail, tokenId,
			useTimeSeconds, common.GetContextKeyBool(c, constant.ContextKeyIsStream), userGroup, other,
		)
	}

}

func RelayMidjourney(c *gin.Context) {
	relayInfo, err := relaycommon.GenRelayInfo(c, types.RelayFormatMjProxy, nil, nil)

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"description": fmt.Sprintf("failed to generate relay info: %s", err.Error()),
			"type":        "upstream_error",
			"code":        4,
		})
		return
	}

	var mjErr *dto.MidjourneyResponse
	switch relayInfo.RelayMode {
	case relayconstant.RelayModeMidjourneyNotify:
		mjErr = relay.RelayMidjourneyNotify(c)
	case relayconstant.RelayModeMidjourneyTaskFetch, relayconstant.RelayModeMidjourneyTaskFetchByCondition:
		mjErr = relay.RelayMidjourneyTask(c, relayInfo.RelayMode)
	case relayconstant.RelayModeMidjourneyTaskImageSeed:
		mjErr = relay.RelayMidjourneyTaskImageSeed(c)
	case relayconstant.RelayModeSwapFace:
		mjErr = relay.RelaySwapFace(c, relayInfo)
	default:
		mjErr = relay.RelayMidjourneySubmit(c, relayInfo)
	}
	//err = relayMidjourneySubmit(c, relayMode)
	log.Println(mjErr)
	if mjErr != nil {
		statusCode := http.StatusBadRequest
		if mjErr.Code == 30 {
			mjErr.Result = "当前分组负载已饱和，请稍后再试，或升级账户以提升服务质量。"
			statusCode = http.StatusTooManyRequests
		}
		c.JSON(statusCode, gin.H{
			"description": fmt.Sprintf("%s %s", mjErr.Description, mjErr.Result),
			"type":        "upstream_error",
			"code":        mjErr.Code,
		})
		channelId := c.GetInt("channel_id")
		logger.LogError(c, fmt.Sprintf("relay error (channel #%d, status code %d): %s", channelId, statusCode, fmt.Sprintf("%s %s", mjErr.Description, mjErr.Result)))
	}
}

func RelayNotImplemented(c *gin.Context) {
	err := types.OpenAIError{
		Message: "API not implemented",
		Type:    "new_api_error",
		Param:   "",
		Code:    "api_not_implemented",
	}
	c.JSON(http.StatusNotImplemented, gin.H{
		"error": err,
	})
}

func RelayNotFound(c *gin.Context) {
	err := types.OpenAIError{
		Message: fmt.Sprintf("Invalid URL (%s %s)", c.Request.Method, c.Request.URL.Path),
		Type:    "invalid_request_error",
		Param:   "",
		Code:    "",
	}
	c.JSON(http.StatusNotFound, gin.H{
		"error": err,
	})
}

func RelayTaskFetch(c *gin.Context) {
	relayInfo, err := relaycommon.GenRelayInfo(c, types.RelayFormatTask, nil, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, &dto.TaskError{
			Code:       "gen_relay_info_failed",
			Message:    err.Error(),
			StatusCode: http.StatusInternalServerError,
		})
		return
	}
	if taskErr := relay.RelayTaskFetch(c, relayInfo.RelayMode); taskErr != nil {
		respondTaskError(c, taskErr)
	}
}

func RelayTask(c *gin.Context) {
	relayInfo, err := relaycommon.GenRelayInfo(c, types.RelayFormatTask, nil, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, &dto.TaskError{
			Code:       "gen_relay_info_failed",
			Message:    err.Error(),
			StatusCode: http.StatusInternalServerError,
		})
		return
	}

	if taskErr := relay.ResolveOriginTask(c, relayInfo); taskErr != nil {
		respondTaskError(c, taskErr)
		return
	}

	var result *relay.TaskSubmitResult
	var taskErr *dto.TaskError
	defer func() {
		if taskErr != nil && relayInfo.Billing != nil {
			relayInfo.Billing.Refund(c)
		}
	}()

	retryParam := service.NewRetryParam(c, relayInfo.TokenGroup, relayInfo.OriginModelName, c.Request.URL.Path)

	for ; retryParam.GetRetry() <= common.RetryTimes; retryParam.IncreaseRetry() {
		var channel *model.Channel

		if lockedCh, ok := relayInfo.LockedChannel.(*model.Channel); ok && lockedCh != nil {
			channel = lockedCh
			if retryParam.GetRetry() > 0 {
				var excludedMultiKeyIndexes map[int]struct{}
				if retryParam.Boundary != nil {
					excludedMultiKeyIndexes = retryParam.Boundary.UsedMultiKeyIndexes(channel.Id)
				}
				if setupErr := middleware.SetupContextForSelectedChannel(c, channel, relayInfo.OriginModelName, excludedMultiKeyIndexes); setupErr != nil {
					taskErr = service.TaskErrorWrapperLocal(setupErr.Err, "setup_locked_channel_failed", http.StatusInternalServerError)
					break
				}
			}
		} else {
			var channelErr *types.NewAPIError
			channel, channelErr = getChannel(c, relayInfo, retryParam)
			if channelErr != nil {
				logger.LogError(c, channelErr.Error())
				taskErr = service.TaskErrorWrapperLocal(channelErr.Err, "get_channel_failed", http.StatusInternalServerError)
				break
			}
		}

		addUsedChannel(c, channel.Id)
		_ = trackRetryAttempt(c, retryParam, channel)
		bodyStorage, bodyErr := common.GetBodyStorage(c)
		if bodyErr != nil {
			if common.IsRequestBodyTooLargeError(bodyErr) || errors.Is(bodyErr, common.ErrRequestBodyTooLarge) {
				taskErr = service.TaskErrorWrapperLocal(bodyErr, "read_request_body_failed", http.StatusRequestEntityTooLarge)
			} else {
				taskErr = service.TaskErrorWrapperLocal(bodyErr, "read_request_body_failed", http.StatusBadRequest)
			}
			break
		}
		c.Request.Body = io.NopCloser(bodyStorage)

		result, taskErr = relay.RelayTaskSubmit(c, relayInfo)
		if taskErr == nil {
			break
		}

		if !taskErr.LocalError {
			processChannelError(c,
				*types.NewChannelError(channel.Id, channel.Type, channel.Name, channel.ChannelInfo.IsMultiKey,
					common.GetContextKeyString(c, constant.ContextKeyChannelKey), channel.GetAutoBan()),
				types.NewOpenAIError(taskErr.Error, types.ErrorCodeBadResponseStatusCode, taskErr.StatusCode))
		}

		if !shouldRetryTaskRelay(c, channel.Id, taskErr, common.RetryTimes-retryParam.GetRetry()) {
			break
		}
	}

	useChannel := c.GetStringSlice("use_channel")
	if len(useChannel) > 1 {
		retryLogStr := fmt.Sprintf("重试：%s", strings.Trim(strings.Join(strings.Fields(fmt.Sprint(useChannel)), "->"), "[]"))
		logger.LogInfo(c, retryLogStr)
	}

	// ── 成功：结算 + 日志 + 插入任务 ──
	if taskErr == nil {
		if settleErr := service.SettleBilling(c, relayInfo, result.Quota); settleErr != nil {
			common.SysError("settle task billing error: " + settleErr.Error())
		}
		service.LogTaskConsumption(c, relayInfo)

		task := model.InitTask(result.Platform, relayInfo)
		task.PrivateData.UpstreamTaskID = result.UpstreamTaskID
		task.PrivateData.BillingSource = relayInfo.BillingSource
		task.PrivateData.SubscriptionId = relayInfo.SubscriptionId
		task.PrivateData.TokenId = relayInfo.TokenId
		task.PrivateData.NodeName = common.NodeName
		task.PrivateData.BillingContext = &model.TaskBillingContext{
			ModelPrice:      relayInfo.PriceData.ModelPrice,
			GroupRatio:      relayInfo.PriceData.GroupRatioInfo.GroupRatio,
			ModelRatio:      relayInfo.PriceData.ModelRatio,
			OtherRatios:     relayInfo.PriceData.OtherRatios(),
			OriginModelName: relayInfo.OriginModelName,
			PerCallBilling:  common.StringsContains(constant.TaskPricePatches, relayInfo.OriginModelName) || relayInfo.PriceData.UsePrice,
		}
		task.Quota = result.Quota
		task.Data = result.TaskData
		task.Action = relayInfo.Action
		if insertErr := task.Insert(); insertErr != nil {
			common.SysError("insert task error: " + insertErr.Error())
		}
	}

	if taskErr != nil {
		respondTaskError(c, taskErr)
	}
}

// respondTaskError 统一输出 Task 错误响应（含 429 限流提示改写）
func respondTaskError(c *gin.Context, taskErr *dto.TaskError) {
	if taskErr.StatusCode == http.StatusTooManyRequests {
		taskErr.Message = "当前分组上游负载已饱和，请稍后再试"
	}
	c.JSON(taskErr.StatusCode, taskErr)
}

func shouldRetryTaskRelay(c *gin.Context, channelId int, taskErr *dto.TaskError, retryTimes int) bool {
	if taskErr == nil {
		return false
	}
	if service.ShouldSkipRetryAfterChannelAffinityFailure(c) {
		return false
	}
	if retryTimes <= 0 {
		return false
	}
	if _, ok := c.Get("specific_channel_id"); ok {
		return false
	}
	if taskErr.LocalError {
		return false
	}
	return operation_setting.ShouldRetryByStatusCode(taskErr.StatusCode)
}
