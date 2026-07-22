package relay

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	appconstant "github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/relay/channel/codex"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relay/responsesws"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

func ResponsesHelper(c *gin.Context, info *relaycommon.RelayInfo) (newAPIError *types.NewAPIError) {
	info.InitChannelMeta(c)
	if info.RelayMode == relayconstant.RelayModeResponsesCompact {
		switch info.ApiType {
		case appconstant.APITypeOpenAI, appconstant.APITypeCodex:
		default:
			return types.NewErrorWithStatusCode(
				fmt.Errorf("unsupported endpoint %q for api type %d", "/v1/responses/compact", info.ApiType),
				types.ErrorCodeInvalidRequest,
				http.StatusBadRequest,
				types.ErrOptionWithSkipRetry(),
			)
		}
	}

	var responsesReq *dto.OpenAIResponsesRequest
	switch req := info.Request.(type) {
	case *dto.OpenAIResponsesRequest:
		responsesReq = req
	case *dto.OpenAIResponsesCompactionRequest:
		// Only fields documented for POST /v1/responses/compact are forwarded:
		// model, input, instructions, previous_response_id, prompt_cache_key,
		// prompt_cache_options, prompt_cache_retention, service_tier.
		// Undocumented Codex-parity fields (tools, reasoning, text) are parsed
		// for client compatibility but intentionally not sent upstream.
		responsesReq = &dto.OpenAIResponsesRequest{
			Model:                req.Model,
			Input:                req.Input,
			Instructions:         req.Instructions,
			PreviousResponseID:   req.PreviousResponseID,
			ParallelToolCalls:    req.ParallelToolCalls,
			ServiceTier:          req.ServiceTier,
			PromptCacheKey:       req.PromptCacheKey,
			PromptCacheOptions:   req.PromptCacheOptions,
			PromptCacheRetention: req.PromptCacheRetention,
		}
	default:
		return types.NewErrorWithStatusCode(
			fmt.Errorf("invalid request type, expected dto.OpenAIResponsesRequest or dto.OpenAIResponsesCompactionRequest, got %T", info.Request),
			types.ErrorCodeInvalidRequest,
			http.StatusBadRequest,
			types.ErrOptionWithSkipRetry(),
		)
	}

	request, err := common.DeepCopy(responsesReq)
	if err != nil {
		return types.NewError(fmt.Errorf("failed to copy request to GeneralOpenAIRequest: %w", err), types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}

	originModelName := info.OriginModelName
	originPriceData := info.PriceData
	originTieredBillingSnapshot := info.TieredBillingSnapshot
	originBillingRequestInput := info.BillingRequestInput
	if info.RelayMode == relayconstant.RelayModeResponsesCompact {
		defer func() {
			info.OriginModelName = originModelName
			info.PriceData = originPriceData
			info.TieredBillingSnapshot = originTieredBillingSnapshot
			info.BillingRequestInput = originBillingRequestInput
		}()
	}

	err = helper.ModelMappedHelper(c, info, request)
	if err != nil {
		return types.NewError(err, types.ErrorCodeChannelModelMappedError, types.ErrOptionWithSkipRetry())
	}
	adaptor := GetAdaptor(info.ApiType)
	if adaptor == nil {
		return types.NewError(fmt.Errorf("invalid api type: %d", info.ApiType), types.ErrorCodeInvalidApiType, types.ErrOptionWithSkipRetry())
	}
	adaptor.Init(info)
	if session := responsesws.SessionFromContext(c); session != nil &&
		strings.TrimSpace(responsesReq.PreviousResponseID) != "" {
		// Raw body passthrough skips conversion and final-JSON inspection below.
		// Detect a continuation from the already parsed request so it can never
		// escape onto HTTP or a replacement upstream connection after its owning
		// WebSocket has been lost.
		responsesws.MarkContinuationRequired(c)
		if !session.CanContinue(responsesReq.PreviousResponseID) {
			return responsesws.NewContinuationUnavailableError()
		}
	}
	var requestBody io.Reader
	passThroughEnabled := relaycommon.IsRequestPassThroughEnabled(info)
	if info.RelayMode == relayconstant.RelayModeResponsesCompact {
		// Compact always uses the converted request. Besides enforcing the endpoint's
		// documented field whitelist, this ensures suffix removal, model mapping and
		// parameter overrides are visible before pricing is frozen.
		passThroughEnabled = false
	}
	if passThroughEnabled {
		storage, err := common.GetBodyStorage(c)
		if err != nil {
			return types.NewError(err, types.ErrorCodeReadRequestBodyFailed, types.ErrOptionWithSkipRetry())
		}
		body, size, closer, err := relaycommon.NewPrivacyFilteredPassThroughJSONBody(storage, info.ChannelSetting.Proxy)
		if err != nil {
			return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
		}
		if closer != nil {
			defer closer.Close()
		}
		info.UpstreamRequestBodySize = size
		requestBody = body
	} else {
		convertedRequest, err := adaptor.ConvertOpenAIResponsesRequest(c, info, *request)
		if err != nil {
			return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
		}
		relaycommon.AppendRequestConversionFromRequest(info, convertedRequest)
		jsonData, err := common.Marshal(convertedRequest)
		if err != nil {
			return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
		}

		// remove disabled fields for OpenAI Responses API
		jsonData, err = relaycommon.RemoveDisabledFieldsForChannel(jsonData, info)
		if err != nil {
			return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
		}

		// apply param override
		if len(info.ParamOverride) > 0 {
			jsonData, err = relaycommon.ApplyParamOverrideForChannel(jsonData, info)
			if err != nil {
				return newAPIErrorFromParamOverride(err)
			}
		}
		if info.ChannelType == appconstant.ChannelTypeCodex && info.IsStream &&
			codex.IsResponsesLiteRequest(info) {
			jsonData, err = codex.FilterResponsesLitePayload(jsonData)
			if err != nil {
				return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
			}
		}
		preparedBody, err := relaycommon.PrepareOutboundJSON(jsonData, info.ChannelSetting.Proxy)
		if err != nil {
			return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
		}
		finalJSON := preparedBody.Bytes()
		var finalRequest dto.OpenAIResponsesRequest
		if err := common.Unmarshal(finalJSON, &finalRequest); err != nil {
			return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
		}
		if session := responsesws.SessionFromContext(c); session != nil {
			if strings.TrimSpace(finalRequest.PreviousResponseID) != "" {
				responsesws.MarkContinuationRequired(c)
				if !session.CanContinue(finalRequest.PreviousResponseID) {
					return responsesws.NewContinuationUnavailableError()
				}
			}
			if info.RelayMode != relayconstant.RelayModeResponsesCompact &&
				(info.ChannelOtherSettings.ResponsesWebSocketEnabled || session.HasTransportState() || responsesws.IsContinuationRequired(c)) {
				finalModel := strings.TrimSpace(finalRequest.Model)
				if finalModel == "" {
					return types.NewErrorWithStatusCode(
						errors.New("responses websocket upstream model is empty after request overrides"),
						types.ErrorCodeChannelParamOverrideInvalid,
						http.StatusBadRequest,
						types.ErrOptionWithSkipRetry(),
					)
				}
				// Session affinity must follow the actual provider request. A model
				// override is applied after ordinary mapping and may otherwise let the
				// request body switch models while the session compares the old name.
				info.UpstreamModelName = finalModel
			}
		}
		if info.RelayMode == relayconstant.RelayModeResponsesCompact {
			finalModel := strings.TrimSpace(finalRequest.Model)
			if finalModel == "" {
				return types.NewErrorWithStatusCode(
					errors.New("responses compact upstream model is empty after request overrides"),
					types.ErrorCodeChannelParamOverrideInvalid,
					http.StatusBadRequest,
					types.ErrOptionWithSkipRetry(),
				)
			}

			// Freeze compact pricing from the privacy-filtered provider payload. The
			// same PreparedOutboundJSON creates the transport body below, keeping the
			// model, token estimate, request expressions, and sent bytes in sync.
			info.UpstreamModelName = finalModel
			info.OriginModelName = ratio_setting.WithCompactModelSuffix(finalModel)
			info.TieredBillingSnapshot = nil
			info.BillingRequestInput = &billingexpr.RequestInput{Body: finalJSON}
			// Recount from the provider-ready payload. Codex conversion can add a
			// channel system prompt and parameter overrides can replace request fields;
			// reusing the client DTO's estimate would reserve against a body that is not
			// the one sent upstream.
			compactMeta := finalRequest.GetTokenCountMeta()
			if compactMeta == nil {
				compactMeta = &types.TokenCountMeta{}
			}
			contextModelName := common.GetContextKeyString(c, appconstant.ContextKeyOriginalModel)
			common.SetContextKey(c, appconstant.ContextKeyOriginalModel, finalModel)
			finalPromptTokens, countErr := service.EstimateRequestToken(c, compactMeta, info)
			common.SetContextKey(c, appconstant.ContextKeyOriginalModel, contextModelName)
			if countErr != nil {
				return types.NewError(countErr, types.ErrorCodeCountTokenFailed)
			}
			info.SetEstimatePromptTokens(finalPromptTokens)
			compactPriceData, err := helper.ModelPriceHelper(c, info, finalPromptTokens, compactMeta)
			if err != nil {
				return types.NewError(err, types.ErrorCodeModelPriceError, types.ErrOptionWithSkipRetry(), types.ErrOptionWithStatusCode(http.StatusBadRequest))
			}
			if !compactPriceData.FreeModel {
				if info.Billing == nil {
					// Compact skips the client-payload estimate and creates its first
					// session only after the provider-ready JSON is frozen. This is the
					// final pre-write reservation boundary, so the trusted-wallet bypass
					// is not valid even on the first attempt.
					if apiError := service.PreConsumeBillingStrict(c, compactPriceData.QuotaToPreConsume, info); apiError != nil {
						return apiError
					}
				} else if err := info.Billing.Reserve(compactPriceData.QuotaToPreConsume); err != nil {
					var apiError *types.NewAPIError
					if errors.As(err, &apiError) {
						return apiError
					}
					return types.NewErrorWithStatusCode(
						err,
						types.ErrorCodePreConsumeTokenQuotaFailed,
						http.StatusForbidden,
						types.ErrOptionWithSkipRetry(),
						types.ErrOptionWithNoRecordErrorLog(),
					)
				}
			}
		}

		logger.LogDebug(c, "upstream request body omitted from logs (%d bytes)", len(finalJSON))
		body, size, closer, err := preparedBody.NewBody()
		if err != nil {
			return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
		}
		defer closer.Close()
		jsonData = nil
		info.UpstreamRequestBodySize = size
		requestBody = body
	}

	var httpResp *http.Response
	resp, err := adaptor.DoRequest(c, info, requestBody)
	if err != nil {
		return types.NewOpenAIError(err, types.ErrorCodeDoRequestFailed, http.StatusInternalServerError)
	}

	statusCodeMappingStr := c.GetString("status_code_mapping")

	if resp != nil {
		httpResp = resp.(*http.Response)

		if httpResp.StatusCode != http.StatusOK {
			info.MarkUpstreamFailureResponse()
			newAPIError = service.RelayErrorHandler(c.Request.Context(), httpResp)
			// reset status code 重置状态码
			service.ResetStatusCode(newAPIError, statusCodeMappingStr)
			return newAPIError
		}
	}

	usage, newAPIError := adaptor.DoResponse(c, httpResp, info)
	if newAPIError != nil {
		// reset status code 重置状态码
		service.ResetStatusCode(newAPIError, statusCodeMappingStr)
		return newAPIError
	}
	if committedError := info.CommittedUpstreamError(); committedError != nil {
		committedError = service.NormalizeViolationFeeError(committedError)
		info.MarkCommittedUpstreamError(committedError)
		if service.IsViolationFeeCode(committedError.GetErrorCode()) {
			// A committed provider-policy terminal has already been sent to the SSE
			// client, but its configured fee replaces ordinary usage billing. Return it
			// before PostTextConsumeQuota so Relay settles the existing reservation to
			// the violation fee without refunding, retrying, or appending another body.
			return committedError
		}
	}

	usageDto := usage.(*dto.Usage)
	if info.RelayMode == relayconstant.RelayModeResponsesCompact {
		service.PostTextConsumeQuota(c, info, usageDto, nil)
		return nil
	}

	if strings.HasPrefix(info.OriginModelName, "gpt-4o-audio") {
		service.PostAudioConsumeQuota(c, info, usageDto, "")
	} else {
		service.PostTextConsumeQuota(c, info, usageDto, nil)
	}
	return nil
}
