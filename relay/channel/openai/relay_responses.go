package openai

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const maxResponsesStreamPreflightBytes = 64 << 10

const responsesStreamPreflightTimeout = 30 * time.Second

func OaiResponsesHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)

	// read response body
	var responsesResponse dto.OpenAIResponsesResponse
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	err = common.Unmarshal(responseBody, &responsesResponse)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if oaiError := responsesResponse.GetOpenAIError(); oaiError != nil && oaiError.Type != "" {
		return nil, responsesWrappedError(info, resp, responseBody, oaiError)
	}

	if responsesResponse.HasImageGenerationCall() {
		c.Set("image_generation_call", true)
		c.Set("image_generation_call_quality", responsesResponse.GetQuality())
		c.Set("image_generation_call_size", responsesResponse.GetSize())
	}

	// 写入新的 response body
	service.IOCopyBytesGracefully(c, resp, responseBody)

	// compute usage
	usage := dto.Usage{}
	if responsesResponse.Usage != nil {
		usage.PromptTokens = responsesResponse.Usage.InputTokens
		usage.CompletionTokens = responsesResponse.Usage.OutputTokens
		usage.TotalTokens = responsesResponse.Usage.TotalTokens
		if responsesResponse.Usage.InputTokensDetails != nil {
			usage.PromptTokensDetails.CachedTokens = responsesResponse.Usage.InputTokensDetails.CachedTokens
			usage.PromptTokensDetails.CacheWriteTokens = responsesResponse.Usage.InputTokensDetails.CacheWriteTokens
		}
	}
	if info == nil || info.ResponsesUsageInfo == nil || info.ResponsesUsageInfo.BuiltInTools == nil {
		return &usage, nil
	}
	// 解析 Tools 用量
	for _, tool := range responsesResponse.Tools {
		buildToolinfo, ok := info.ResponsesUsageInfo.BuiltInTools[common.Interface2String(tool["type"])]
		if !ok || buildToolinfo == nil {
			logger.LogError(c, fmt.Sprintf("BuiltInTools not found for tool type: %v", tool["type"]))
			continue
		}
		buildToolinfo.CallCount++
	}
	return &usage, nil
}

func OaiResponsesStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		logger.LogError(c, "invalid response or response body")
		return nil, types.NewError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse)
	}

	defer service.CloseResponseBodyGracefully(resp)

	var usage = &dto.Usage{}
	var responseTextBuilder strings.Builder
	type pendingStreamEvent struct {
		response dto.ResponsesStreamResponse
		data     string
	}
	pendingEvents := make([]pendingStreamEvent, 0, 2)
	pendingBytes := 0
	var downstreamCommitted atomic.Bool
	isSubscriptionOAuth := relaycommon.IsSubscriptionOAuthChannel(info.ChannelType)
	if !isSubscriptionOAuth {
		downstreamCommitted.Store(true)
	}
	var preflightError *types.NewAPIError
	hasAuthoritativeUsage := false

	flushPendingEvents := func() {
		for _, event := range pendingEvents {
			sendResponsesStreamData(c, event.response, event.data)
		}
		pendingEvents = pendingEvents[:0]
		pendingBytes = 0
	}
	commitPreflight := func() {
		if downstreamCommitted.Swap(true) {
			return
		}
		helper.CopyCodexSSEHeaders(c, resp)
		flushPendingEvents()
	}

	handleStreamData := func(data string, sr *helper.StreamResult) {
		if info.ResponsesStreamEventTransform != nil {
			data = info.ResponsesStreamEventTransform(data)
		}

		var streamResponse dto.ResponsesStreamResponse
		if err := common.UnmarshalJsonStr(data, &streamResponse); err != nil {
			logger.LogError(c, "failed to unmarshal stream response: "+err.Error())
			sr.Error(err)
			return
		}
		switch streamResponse.Type {
		case "response.completed", "response.incomplete", "response.done":
			if streamResponse.Response != nil {
				if streamResponse.Response.Usage != nil {
					hasAuthoritativeUsage = true
					usage.PromptTokens = streamResponse.Response.Usage.InputTokens
					usage.CompletionTokens = streamResponse.Response.Usage.OutputTokens
					usage.TotalTokens = streamResponse.Response.Usage.TotalTokens
					if streamResponse.Response.Usage.InputTokensDetails != nil {
						usage.PromptTokensDetails.CachedTokens = streamResponse.Response.Usage.InputTokensDetails.CachedTokens
						usage.PromptTokensDetails.CacheWriteTokens = streamResponse.Response.Usage.InputTokensDetails.CacheWriteTokens
					}
				}
				if streamResponse.Response.HasImageGenerationCall() {
					c.Set("image_generation_call", true)
					c.Set("image_generation_call_quality", streamResponse.Response.GetQuality())
					c.Set("image_generation_call_size", streamResponse.Response.GetSize())
				}
			}
		case "response.output_text.delta":
			responseTextBuilder.WriteString(streamResponse.Delta)
		case dto.ResponsesOutputTypeItemDone:
			if streamResponse.Item != nil && streamResponse.Item.Type == dto.BuildInCallWebSearchCall &&
				info != nil && info.ResponsesUsageInfo != nil && info.ResponsesUsageInfo.BuiltInTools != nil {
				if webSearchTool, exists := info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolWebSearchPreview]; exists && webSearchTool != nil {
					webSearchTool.CallCount++
				}
			}
		}
		streamError := streamResponse.GetOpenAIError()
		terminalFailure := streamResponse.Type == "response.failed" ||
			streamResponse.Type == "response.error" || streamResponse.Type == "error"
		if terminalFailure {
			if streamError == nil {
				streamError = &types.OpenAIError{Type: "unknown_error", Message: "upstream Responses stream failed without error details"}
			}
			// Record the upstream failure for every channel so channel health and
			// error logs stay observable even when the downstream is already committed
			// and the turn cannot be safely retried.
			info.MarkUpstreamFailureResponse()
			upstreamStatus := responsesStreamErrorStatus(data, streamError)
			candidate := types.WithOpenAIError(*streamError, upstreamStatus)
			candidate.UpstreamStatusCode = upstreamStatus
			candidate.RetryAfter = service.ParseUpstreamRetryDelay(http.Header{}, []byte(data), time.Now())
			candidate = service.ApplyChannelErrorPolicy(info.ChannelType, candidate)
			if isSubscriptionOAuth {
				if !downstreamCommitted.Load() {
					// Preflight stage: nothing committed downstream yet, so record any
					// terminal failure (response.failed / response.error / error) — not
					// just response.failed — so a client gets a structured terminal after
					// retries, and fail over safely.
					relaycommon.SetResponsesStreamPreflightFailureEvent(c, data)
					preflightError = candidate
					sr.Stop(candidate)
					return
				}
				sr.Error(candidate)
			} else {
				// Non-subscription channels treat the downstream as committed from the
				// first event, so the turn cannot be safely retried; log for visibility
				// and forward the failure event to the client below.
				logger.LogError(c, fmt.Sprintf("responses stream terminal failure on non-subscription channel: type=%s message=%s", streamResponse.Type, streamError.Message))
				sr.Error(candidate)
			}
		}
		if isSubscriptionOAuth && !downstreamCommitted.Load() {
			switch streamResponse.Type {
			case "response.created", "response.in_progress", "response.queued":
				pendingEvents = append(pendingEvents, pendingStreamEvent{response: streamResponse, data: data})
				pendingBytes += len(data)
				if pendingBytes <= maxResponsesStreamPreflightBytes {
					return
				}
				commitPreflight()
				return
			case "response.completed", "response.done":
				pendingEvents = append(pendingEvents, pendingStreamEvent{response: streamResponse, data: data})
				commitPreflight()
				return
			}
			commitPreflight()
		}
		sendResponsesStreamData(c, streamResponse, data)
	}
	if isSubscriptionOAuth {
		helper.StreamScannerHandlerWithPreflight(
			c,
			resp,
			info,
			responsesStreamPreflightTimeout,
			func() bool { return downstreamCommitted.Load() },
			handleStreamData,
		)
	} else {
		helper.StreamScannerHandler(c, resp, info, handleStreamData)
	}
	if preflightError != nil {
		helper.ClearEventStreamHeaders(c)
		return nil, preflightError
	}
	if isSubscriptionOAuth && !downstreamCommitted.Load() && info.StreamStatus != nil &&
		info.StreamStatus.EndReason == relaycommon.StreamEndReasonScannerErr && info.StreamStatus.EndError != nil {
		helper.ClearEventStreamHeaders(c)
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("upstream stream failed before output: %w", info.StreamStatus.EndError),
			types.ErrorCodeDoRequestFailed,
			http.StatusBadGateway,
		)
	}
	if isSubscriptionOAuth && !downstreamCommitted.Load() && info.StreamStatus != nil &&
		info.StreamStatus.EndReason == relaycommon.StreamEndReasonTimeout {
		helper.ClearEventStreamHeaders(c)
		preflightErr := info.StreamStatus.EndError
		if preflightErr == nil {
			preflightErr = fmt.Errorf("upstream stream preflight timed out")
		}
		return nil, types.NewErrorWithStatusCode(
			preflightErr,
			types.ErrorCodeDoRequestFailed,
			http.StatusBadGateway,
		)
	}
	if len(pendingEvents) > 0 {
		commitPreflight()
	}

	if !hasAuthoritativeUsage && usage.CompletionTokens == 0 {
		// 计算输出文本的 token 数量
		tempStr := responseTextBuilder.String()
		if len(tempStr) > 0 {
			// 非正常结束，使用输出文本的 token 数量
			completionTokens := service.CountTextToken(tempStr, info.UpstreamModelName)
			usage.CompletionTokens = completionTokens
		}
	}

	if !hasAuthoritativeUsage && usage.PromptTokens == 0 && usage.CompletionTokens != 0 {
		usage.PromptTokens = info.GetEstimatePromptTokens()
	}

	if !hasAuthoritativeUsage {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}

	return usage, nil
}

func responsesWrappedError(info *relaycommon.RelayInfo, resp *http.Response, responseBody []byte, upstreamError *types.OpenAIError) *types.NewAPIError {
	status := resp.StatusCode
	if status >= 200 && status < 300 {
		status = responsesStreamErrorStatus(string(responseBody), upstreamError)
	}

	apiError := types.WithOpenAIError(*upstreamError, status)
	apiError.UpstreamStatusCode = status
	apiError.RetryAfter = service.ParseUpstreamRetryDelay(resp.Header, responseBody, time.Now())
	if info != nil {
		info.MarkUpstreamFailureResponse()
	}
	return apiError
}

func responsesStreamErrorStatus(data string, streamError *types.OpenAIError) int {
	for _, path := range []string{
		"error.status_code",
		"response.error.status_code",
		"error.status",
		"response.error.status",
		"status_code",
	} {
		status := int(gjson.Get(data, path).Int())
		if status >= 400 && status <= 599 {
			return status
		}
	}
	if streamError == nil {
		return http.StatusBadGateway
	}
	marker := strings.ToLower(streamError.Type + " " + fmt.Sprint(streamError.Code) + " " + streamError.Message)
	switch {
	case strings.Contains(marker, "authentication"), strings.Contains(marker, "unauthorized"):
		return http.StatusUnauthorized
	case strings.Contains(marker, "permission"), strings.Contains(marker, "forbidden"):
		return http.StatusForbidden
	case strings.Contains(marker, "request_too_large"):
		return http.StatusRequestEntityTooLarge
	case strings.Contains(marker, "invalid_request"):
		return http.StatusBadRequest
	case strings.Contains(marker, "not_found"):
		return http.StatusNotFound
	case strings.Contains(marker, "rate_limit"), strings.Contains(marker, "usage_limit"),
		strings.Contains(marker, "model_at_capacity"), strings.Contains(marker, "insufficient_quota"):
		return http.StatusTooManyRequests
	case strings.Contains(marker, "service_unavailable"), strings.Contains(marker, "server_is_overloaded"),
		strings.Contains(marker, "server_overloaded"), strings.Contains(marker, "overloaded"):
		return http.StatusServiceUnavailable
	case strings.Contains(marker, "server_error"), strings.Contains(marker, "internal_error"):
		return http.StatusInternalServerError
	default:
		return http.StatusBadGateway
	}
}
