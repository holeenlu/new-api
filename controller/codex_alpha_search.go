package controller

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/codex"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

func CodexAlphaSearch(c *gin.Context) {
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	body, err := storage.Bytes()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	body, err = codex.SanitizeAlphaSearchBody(body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": types.NewError(err, types.ErrorCodeInvalidRequest).ToOpenAIError()})
		return
	}

	var routing struct {
		Model string `json:"model"`
		Query string `json:"query"`
	}
	if err := common.Unmarshal(body, &routing); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": types.NewError(err, types.ErrorCodeInvalidRequest).ToOpenAIError()})
		return
	}
	if strings.TrimSpace(routing.Model) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": types.NewError(errors.New("model is required"), types.ErrorCodeInvalidRequest).ToOpenAIError()})
		return
	}
	if setting.ShouldCheckPromptSensitive() {
		if contains, words := service.CheckSensitiveText(routing.Query); contains {
			logger.LogWarn(c, fmt.Sprintf("request rejected by sensitive word policy (%d matches)", len(words)))
			c.JSON(http.StatusBadRequest, gin.H{"error": types.NewError(errors.New("sensitive words detected"), types.ErrorCodeSensitiveWordsDetected, types.ErrOptionWithSkipRetry()).ToOpenAIError()})
			return
		}
	}

	originModel := strings.TrimSpace(routing.Model)
	var basePayload map[string]any
	if err := common.Unmarshal(body, &basePayload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": types.NewError(err, types.ErrorCodeInvalidRequest).ToOpenAIError()})
		return
	}
	initialInfo := relaycommon.GenRelayInfoResponses(c, &dto.OpenAIResponsesRequest{Model: originModel})
	initialInfo.InitChannelMeta(c)
	initialInfo.Action = "alpha_search"
	priceData, err := helper.ModelPriceHelperPerCall(c, initialInfo)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": types.NewError(err, types.ErrorCodeModelPriceError, types.ErrOptionWithSkipRetry()).ToOpenAIError()})
		return
	}
	initialInfo.PriceData = priceData
	if !priceData.FreeModel {
		if billingError := service.PreConsumeBilling(c, priceData.Quota, initialInfo); billingError != nil {
			c.JSON(billingError.StatusCode, gin.H{"error": billingError.ToOpenAIError()})
			return
		}
	}
	billingSettled := false
	defer func() {
		if !billingSettled && initialInfo.Billing != nil {
			initialInfo.Billing.Refund(c)
		}
	}()
	retryParam := service.NewRetryParam(c, initialInfo.TokenGroup, originModel, c.Request.URL.Path)
	var lastError *types.NewAPIError
	firstAttempt := true

	for {
		var channel *model.Channel
		if firstAttempt {
			firstAttempt = false
			channel, err = model.CacheGetChannel(initialInfo.ChannelId)
			if err != nil {
				lastError = types.NewError(err, types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
				break
			}
		} else {
			selectionInfo := relaycommon.GenRelayInfoResponses(c, &dto.OpenAIResponsesRequest{Model: originModel})
			selectionInfo.InitChannelMeta(c)
			var channelError *types.NewAPIError
			channel, channelError = getChannel(c, selectionInfo, retryParam)
			if channelError != nil {
				if lastError == nil || channelError.StatusCode == 499 {
					lastError = channelError
				}
				break
			}
		}

		if !trackRetryAttempt(c, retryParam, channel) {
			continue
		}
		retryParam.RecordAttempt()
		addUsedChannel(c, channel.Id)
		service.ApplyRelayDataPolicyHeaders(c, channel, len(c.GetStringSlice("use_channel")))
		clearStaleRetryAfter(c)

		request := &dto.OpenAIResponsesRequest{Model: originModel}
		initialInfo.InitChannelMeta(c)
		initialInfo.RetryIndex = retryParam.AttemptIndex()
		info := initialInfo
		if info.ChannelType != constant.ChannelTypeCodex {
			lastError = types.NewErrorWithStatusCode(
				errors.New("Codex alpha search requires a ChatGPT Subscription (Codex) channel"),
				types.ErrorCodeInvalidRequest,
				http.StatusBadRequest,
				types.ErrOptionWithSkipRetry(),
			)
			break
		}
		if err := helper.ModelMappedHelper(c, info, request); err != nil {
			lastError = types.NewErrorWithStatusCode(err, types.ErrorCodeChannelModelMappedError, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
			break
		}

		upstreamPayload := make(map[string]any, len(basePayload))
		for key, value := range basePayload {
			upstreamPayload[key] = value
		}
		upstreamPayload["model"] = request.Model
		attemptBody, marshalErr := common.Marshal(upstreamPayload)
		if marshalErr != nil {
			lastError = types.NewErrorWithStatusCode(marshalErr, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError, types.ErrOptionWithSkipRetry())
			break
		}

		resp, requestErr := codex.DoAlphaSearch(c, info, attemptBody)
		retryParam.CaptureSubscriptionOAuthAttemptMetadata(c)
		if requestErr == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			payload, readErr := codex.ReadAlphaSearchResponse(resp)
			_ = resp.Body.Close()
			if readErr == nil {
				retryParam.MarkSubscriptionOAuthSuccess()
				if settleErr := service.SettleBilling(c, info, priceData.Quota); settleErr != nil {
					lastError = types.NewErrorWithStatusCode(settleErr, types.ErrorCodeUpdateDataError, http.StatusInternalServerError, types.ErrOptionWithSkipRetry())
					break
				}
				billingSettled = true
				service.LogAlphaSearchConsumption(c, info, priceData.Quota)
				for _, name := range []string{"Retry-After", "X-Request-ID", "OpenAI-Request-ID"} {
					if value := resp.Header.Get(name); value != "" {
						c.Header(name, value)
					}
				}
				contentType := resp.Header.Get("Content-Type")
				if contentType == "" {
					contentType = "application/json"
				}
				c.Data(resp.StatusCode, contentType, payload)
				return
			}
			requestErr = types.NewErrorWithStatusCode(
				readErr,
				types.ErrorCodeReadResponseBodyFailed,
				http.StatusBadGateway,
			)
		}
		if requestErr == nil {
			info.MarkUpstreamFailureResponse()
			requestErr = service.RelayErrorHandler(c.Request.Context(), resp)
		}
		retryParam.MarkSubscriptionOAuthFailure()

		var apiError *types.NewAPIError
		if c.Request.Context().Err() != nil {
			apiError = types.NewErrorWithStatusCode(
				c.Request.Context().Err(),
				types.ErrorCodeDoRequestFailed,
				499,
				types.ErrOptionWithSkipRetry(),
			)
		} else if !errors.As(requestErr, &apiError) {
			apiError = types.NewErrorWithStatusCode(requestErr, types.ErrorCodeDoRequestFailed, http.StatusBadGateway)
		}
		apiError = recordChannelAttemptError(c, channel, apiError)
		lastError = apiError
		if !shouldContinueSubscriptionOAuthRetry(c, info, retryParam, apiError) {
			break
		}
	}

	if lastError == nil {
		lastError = types.NewErrorWithStatusCode(errors.New("no Codex alpha search channel is available"), types.ErrorCodeGetChannelFailed, http.StatusServiceUnavailable, types.ErrOptionWithSkipRetry())
	}
	status := lastError.StatusCode
	if status <= 0 {
		status = http.StatusBadGateway
	}
	c.JSON(status, gin.H{"error": lastError.ToOpenAIError()})
}
