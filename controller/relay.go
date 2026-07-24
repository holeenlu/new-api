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
	"github.com/QuantumNous/new-api/relay/channel/codex"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relay/responsesws"
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
			if newAPIError.RetryAfter > 0 {
				setRelayRetryAfterHeader(c, newAPIError.RetryAfter)
			} else if newAPIError.StatusCode == http.StatusGatewayTimeout &&
				relaycommon.IsSubscriptionOAuthChannel(c.GetInt("channel_type")) {
				c.Header("Retry-After", "2")
			}
			if relaycommon.IsResponsesStreamFailureEmitted(c) {
				return
			}
			// Surface a concise, localized message to the client for stable gateway
			// error codes; the verbose upstream diagnostic stays in the log above.
			clientMessage := newAPIError.Error()
			if localized, ok := service.LocalizedRelayErrorMessage(c, newAPIError); ok {
				clientMessage = localized
			}
			newAPIError.SetMessage(common.MessageWithRequestId(clientMessage, requestId))
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

	if relayInfo.RelayMode != relayconstant.RelayModeResponsesCompact {
		priceData, priceErr := helper.ModelPriceHelper(c, relayInfo, tokens, meta)
		if priceErr != nil {
			newAPIError = types.NewError(priceErr, types.ErrorCodeModelPriceError, types.ErrOptionWithStatusCode(http.StatusBadRequest))
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
	}

	defer func() {
		// A terminal violation settles the existing reservation directly to the fee.
		// Every other failure follows the ordinary refund path.
		if newAPIError != nil {
			newAPIError = service.NormalizeViolationFeeError(newAPIError)
			if relayInfo.RelayMode == relayconstant.RelayModeResponsesCompact {
				// Compact pricing is scoped to each mapped attempt and restored before
				// control returns here. Re-resolve the request's effective group ratio so
				// a provider violation cannot bypass its fixed fee through a zero-value
				// restored PriceData.
				relayInfo.PriceData.GroupRatioInfo = helper.HandleGroupRatio(c, relayInfo)
			}
			if !service.ChargeViolationFeeIfNeeded(c, relayInfo, newAPIError) && relayInfo.Billing != nil {
				relayInfo.Billing.Refund(c)
			}
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

		attemptReservation := trackRetryAttempt(c, retryParam, channel)
		if attemptReservation != service.SubscriptionOAuthAttemptReserved {
			if attemptReservation == service.SubscriptionOAuthAttemptRequestExhausted {
				// The request has reached its global guard. Returning the last real
				// upstream error terminates streaming clients instead of selecting
				// candidates without consuming a new attempt.
				newAPIError = relayInfo.LastError
				if newAPIError == nil {
					newAPIError = types.NewErrorWithStatusCode(
						errors.New("subscription OAuth relay attempt budget exhausted"),
						types.ErrorCodeDoRequestFailed,
						http.StatusServiceUnavailable,
					)
				}
				break
			}
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
			// A terminal Responses failure may arrive after SSE output was committed.
			// ResponsesHelper deliberately completes usage settlement in that case and
			// returns success so the request is neither retried nor refunded. Preserve
			// the upstream failure as a channel-health/error-log outcome here.
			if committedError := relayInfo.CommittedUpstreamError(); committedError != nil {
				recordChannelAttemptError(c, channel, committedError)
			}
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
		// A successful OAuth refresh retries before recording the channel error.
		// Keep the actual upstream rejection as the request's terminal fallback so
		// the global attempt guard can still close the client request instead of
		// leaving a stream open with no error.
		relayInfo.LastError = newAPIError
		if refreshErr, retry := refreshCodexCredentialForRetry(c, relayInfo, retryParam, channel, newAPIError); retry {
			continue
		} else if refreshErr != nil {
			newAPIError = refreshErr
		}
		newAPIError = recordChannelAttemptError(c, channel, newAPIError)
		relayInfo.LastError = newAPIError

		if isSubscriptionOAuth {
			if !shouldContinueSubscriptionOAuthRetry(c, relayInfo, retryParam, newAPIError) {
				if relayInfo.RelayMode == relayconstant.RelayModeResponses && !c.Writer.Written() {
					emitResponsesStreamPreflightFailure(c)
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
		if sample, ok := perfmetrics.SnapshotRelaySample(relayInfo, false, 0); ok {
			gopool.Go(func() {
				perfmetrics.Record(sample)
			})
		}
	}
}

func emitResponsesStreamPreflightFailure(c *gin.Context) bool {
	data, exists := relaycommon.GetResponsesStreamPreflightFailureEvent(c)
	if !exists {
		return false
	}

	eventType := "response.failed"
	var streamResponse dto.ResponsesStreamResponse
	if err := common.UnmarshalJsonStr(data, &streamResponse); err == nil && streamResponse.Type != "" {
		eventType = streamResponse.Type
	}
	helper.SetEventStreamHeaders(c)
	if helper.ResponseChunkData(c, dto.ResponsesStreamResponse{Type: eventType}, data) != nil {
		return false
	}
	relaycommon.MarkResponsesStreamFailureEmitted(c)
	return true
}

func refreshCodexCredentialForRetry(
	c *gin.Context,
	info *relaycommon.RelayInfo,
	retryParam *service.RetryParam,
	channel *model.Channel,
	apiError *types.NewAPIError,
) (*types.NewAPIError, bool) {
	if c == nil || info == nil || retryParam == nil || channel == nil ||
		!service.ShouldRefreshCodexOAuthCredential(channel.Type, apiError) ||
		c.Writer.Written() ||
		(service.IsSubscriptionOAuthRetryDisabled(c) && !hasReplaySafeResponsesWebSocketInternalPin(c)) {
		return apiError, false
	}
	written, _ := info.UpstreamAttemptState()
	if (written && !info.HasUpstreamFailureResponse()) || !retryParam.ClaimSubscriptionOAuthCredentialRefresh() {
		return apiError, false
	}
	oauthKey, err := codex.ParseOAuthKey(common.GetContextKeyString(c, constant.ContextKeyChannelKey))
	if err != nil || strings.TrimSpace(oauthKey.AccessToken) == "" {
		return apiError, false
	}
	_, _, err = service.RefreshCodexChannelCredential(c.Request.Context(), channel.Id, service.CodexCredentialRefreshOptions{
		ResetCaches:         true,
		ExpectedAccessToken: oauthKey.AccessToken,
	})
	if err == nil {
		logger.LogInfo(c, fmt.Sprintf("refreshed Codex OAuth credential after upstream authorization rejection: channel=%d", channel.Id))
		return nil, true
	}
	logger.LogWarn(c, fmt.Sprintf("Codex OAuth credential refresh failed: channel=%d error=%s", channel.Id, err.Error()))
	if !service.IsPermanentCodexOAuthRefreshFailure(err) {
		return apiError, false
	}
	refreshError := types.NewErrorWithStatusCode(
		fmt.Errorf("Codex OAuth refresh credential was rejected; reauthorization is required: %w", err),
		types.ErrorCodeOAuthUnauthorized,
		http.StatusUnauthorized,
	)
	refreshError.UpstreamStatusCode = http.StatusUnauthorized
	return refreshError, false
}

func shouldRetryOrdinaryRelay(c *gin.Context, info *relaycommon.RelayInfo, apiError *types.NewAPIError, retryTimes int) bool {
	if c == nil || info == nil || c.Writer == nil || c.Writer.Written() {
		return false
	}
	internalWebSocketPin := hasReplaySafeResponsesWebSocketInternalPin(c)
	if !shouldRetry(c, apiError, retryTimes, !internalWebSocketPin) {
		return false
	}
	written, responseStarted := info.UpstreamAttemptState()
	if written || responseStarted {
		return false
	}
	if internalWebSocketPin {
		// A self-contained Responses turn is pinned only to reuse its current
		// upstream connection. A rejected WebSocket handshake happens before the
		// response.create frame is written, so release that internal binding before
		// ordinary channel selection fails over. User/admin pins and stateful
		// previous_response_id turns never carry this marker and remain fixed.
		if session := responsesws.SessionFromContext(c); session != nil {
			session.ResetChannelForRetry()
		}
	}
	return true
}

func shouldContinueSubscriptionOAuthRetry(
	c *gin.Context,
	info *relaycommon.RelayInfo,
	retryParam *service.RetryParam,
	apiError *types.NewAPIError,
) bool {
	written, responseStarted := info.UpstreamAttemptState()
	_, specificChannel := c.Get("specific_channel_id")
	internalWebSocketPin := hasReplaySafeResponsesWebSocketInternalPin(c)
	if internalWebSocketPin {
		// This pin is an implementation detail of the reusable upstream
		// WebSocket, not an administrator/user request to force one channel.
		specificChannel = false
	}
	decision, retryAfter := retryParam.DecideSubscriptionOAuthContinuation(service.SubscriptionOAuthRetryObservation{
		ChannelType:             c.GetInt("channel_type"),
		Error:                   apiError,
		UpstreamRequestWritten:  written,
		UpstreamResponseStarted: responseStarted,
		ExplicitFailureResponse: info.HasUpstreamFailureResponse(),
		DownstreamStarted:       c.Writer.Written(),
		RetryDisabled:           service.IsSubscriptionOAuthRetryDisabled(c) && !internalWebSocketPin,
		SpecificChannel:         specificChannel,
		Retryable:               shouldRetry(c, apiError, common.SubscriptionOAuthUpstreamRetryTimes, !internalWebSocketPin),
	})
	if retryAfter > 0 {
		setRelayRetryAfterHeader(c, retryAfter)
	}
	if decision == service.SubscriptionOAuthSwitchCredential {
		if session := responsesws.SessionFromContext(c); session != nil {
			session.ResetChannelForRetry()
		}
	}
	return decision != service.SubscriptionOAuthRetryStop
}

// hasReplaySafeResponsesWebSocketInternalPin distinguishes a controller-owned
// channel-affinity hint from a stateful continuation. Parameter overrides can
// add previous_response_id after the turn middleware installed the internal pin;
// once that happens, the pin must no longer authorize credential failover.
func hasReplaySafeResponsesWebSocketInternalPin(c *gin.Context) bool {
	return c != nil && c.GetBool(responsesWebSocketInternalPinKey) && !responsesws.IsContinuationRequired(c)
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

func setRelayRetryAfterHeader(c *gin.Context, retryAfter time.Duration) {
	if c == nil || retryAfter <= 0 {
		return
	}
	seconds := int((retryAfter + time.Second - 1) / time.Second)
	c.Header("Retry-After", strconv.Itoa(max(seconds, 1)))
}

func trackRetryAttempt(c *gin.Context, retryParam *service.RetryParam, channel *model.Channel) service.SubscriptionOAuthAttemptReservation {
	if retryParam.Boundary == nil {
		retryParam.Boundary = service.NewRetryBoundary(channel, retryParam.EffectiveGroup)
		if retryParam.Boundary != nil {
			retryParam.CandidateFilter = retryParam.Boundary.Allows
		}
	}
	if retryParam.Boundary == nil {
		return service.SubscriptionOAuthAttemptReserved
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
		reservation := retryParam.ReserveSubscriptionOAuthAttempt(channel.Id, keyIndex, fingerprint)
		if reservation != service.SubscriptionOAuthAttemptReserved {
			return reservation
		}
		service.RecordSubscriptionOAuthCredential(c, fingerprint)
	}
	if channel.ChannelInfo.IsMultiKey {
		retryParam.Boundary.MarkAttempt(channel, common.GetContextKeyInt(c, constant.ContextKeyChannelMultiKeyIndex))
		return service.SubscriptionOAuthAttemptReserved
	}
	retryParam.Boundary.MarkAttempt(channel)
	return service.SubscriptionOAuthAttemptReserved
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
			retryParam.RejectSubscriptionOAuthReplayTarget()
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
		if !retryParam.RestartSubscriptionOAuthCapacityCycle() {
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

func shouldRetry(c *gin.Context, openaiErr *types.NewAPIError, retryTimes int, respectSpecificChannel bool) bool {
	if openaiErr == nil {
		return false
	}
	if _, ok := c.Get("specific_channel_id"); ok && respectSpecificChannel {
		return false
	}
	if service.IsSubscriptionOAuthCapacityFailure(c.GetInt("channel_type"), openaiErr) {
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
	credentialQuarantined := service.QuarantineSubscriptionOAuthCredential(channelError, err)
	// 不要使用context获取渠道信息，异步处理时可能会出现渠道信息不一致的情况
	// do not use context to get channel info, there may be inconsistent channel info when processing asynchronously
	if !credentialQuarantined && service.ShouldDisableChannelForType(channelError.ChannelType, err) && channelError.AutoBan {
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

// recordChannelAttemptError applies the channel error policy to a failed relay
// attempt and records the outcome against channel health: a subscription-OAuth
// capacity failover is logged without penalizing the channel, while every other
// failure is charged to the channel via processChannelError. It returns the
// policy-adjusted error so the caller can store it as the last error for retry
// decisions. Sharing it keeps per-attempt error accounting identical across
// every relay entry point (Relay and CodexAlphaSearch).
func recordChannelAttemptError(c *gin.Context, channel *model.Channel, apiError *types.NewAPIError) *types.NewAPIError {
	if channel.Type == constant.ChannelTypeCodex {
		apiError = correlateCodexOAuthUsageLimit(c, channel, apiError)
	}
	apiError = service.ApplyChannelErrorPolicy(channel.Type, apiError)
	if service.IsSubscriptionOAuthCapacityFailure(channel.Type, apiError) {
		logger.LogWarn(c, fmt.Sprintf(
			"subscription OAuth capacity failover: channel=%d credential=%s",
			channel.Id,
			service.SubscriptionOAuthCredentialPreview(c),
		))
	} else {
		processChannelError(c, *types.NewChannelError(
			channel.Id,
			channel.Type,
			channel.Name,
			channel.ChannelInfo.IsMultiKey,
			common.GetContextKeyString(c, constant.ContextKeyChannelKey),
			channel.GetAutoBan(),
		), apiError)
	}
	return apiError
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
		if taskErr == nil {
			return
		}
		if relayInfo.PersistedTaskID > 0 {
			// Marker ownership begins before the upstream write. Never race its
			// atomic refund with BillingSession.Refund.
			if !service.FailTaskSubmission(c, relayInfo.PersistedTaskID, taskErr.Message) {
				common.SysError(fmt.Sprintf("task %d failure refund remains pending", relayInfo.PersistedTaskID))
			}
			return
		}
		if relayInfo.Billing != nil {
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
		if result == nil || result.Task == nil {
			common.SysError("accepted upstream task has no persisted marker")
		} else if settleErr := service.CommitTaskSubmission(c, relayInfo, result.Task, result.Quota); settleErr != nil {
			common.SysError("settle task billing error: " + settleErr.Error())
		}
		if result != nil && result.Task != nil {
			// Consumption/log statistics must reflect the funding amount that is
			// actually refundable from the durable task ledger, not an uncommitted
			// post-submit estimate.
			relayInfo.PriceData.Quota = result.Task.Quota
		}
		service.LogTaskConsumption(c, relayInfo)
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
