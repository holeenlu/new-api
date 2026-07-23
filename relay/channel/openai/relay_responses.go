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
	"github.com/QuantumNous/new-api/relay/responsesws"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const maxResponsesStreamPreflightBytes = 64 << 10

// maxResponsesBodyBytes bounds a non-stream Responses body read into memory.
// Larger than the WebSocket per-event bound (16 MiB) because a complete
// response can carry multiple base64 image outputs; a body above this is
// treated as an upstream fault rather than allowed to exhaust gateway memory.
const maxResponsesBodyBytes = 64 << 20

const responsesStreamPreflightTimeout = 30 * time.Second

// readBoundedResponsesBody reads a non-stream upstream body under
// maxResponsesBodyBytes, failing with 502 instead of buffering without bound.
func readBoundedResponsesBody(body io.Reader) ([]byte, *types.NewAPIError) {
	data, err := io.ReadAll(io.LimitReader(body, maxResponsesBodyBytes+1))
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	if len(data) > maxResponsesBodyBytes {
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("upstream response exceeds %d MB", maxResponsesBodyBytes>>20),
			types.ErrorCodeBadResponseBody,
			http.StatusBadGateway,
		)
	}
	return data, nil
}

// responsesBodyMissingResult reports a "success" body with no status, output,
// or usage — not a usable Responses result regardless of id; forwarding it
// would fake success on zero usage. A body carrying usage settles billing and
// is left to the client even when otherwise sparse.
func responsesBodyMissingResult(r *dto.OpenAIResponsesResponse) bool {
	return r == nil || (len(r.Status) == 0 && len(r.Output) == 0 && r.Usage == nil)
}

func OaiResponsesHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)

	// read response body
	var responsesResponse dto.OpenAIResponsesResponse
	responseBody, apiErr := readBoundedResponsesBody(resp.Body)
	if apiErr != nil {
		return nil, apiErr
	}
	err := common.Unmarshal(responseBody, &responsesResponse)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if oaiError := responsesResponse.GetOpenAIError(); oaiError != nil && oaiError.Type != "" {
		return nil, responsesWrappedError(info, resp, responseBody, oaiError)
	}
	if responsesBodyMissingResult(&responsesResponse) {
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("upstream returned a Responses body without status or output"),
			types.ErrorCodeBadResponseBody,
			http.StatusBadGateway,
		)
	}

	if responsesResponse.HasImageGenerationCall() {
		c.Set("image_generation_call", true)
		c.Set("image_generation_call_quality", responsesResponse.GetQuality())
		c.Set("image_generation_call_size", responsesResponse.GetSize())
	}

	// 写入新的 response body
	service.IOCopyBytesGracefully(c, resp, responseBody)

	usage := normalizeResponsesUsage(responsesResponse.Usage)
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
	var terminalResolved atomic.Bool

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
	stopProtocolFailure := func(err error, sr *helper.StreamResult) {
		terminalResolved.Store(true)
		logger.LogError(c, "invalid upstream Responses stream event: "+err.Error())
		info.MarkUpstreamFailureResponse()
		candidate := types.NewErrorWithStatusCode(
			err,
			types.ErrorCodeBadResponseBody,
			http.StatusBadGateway,
		)
		candidate.UpstreamStatusCode = http.StatusBadGateway
		candidate = service.ApplyChannelErrorPolicy(info.ChannelType, candidate)
		if isSubscriptionOAuth && !downstreamCommitted.Load() {
			preflightError = candidate
		} else {
			info.MarkCommittedUpstreamError(candidate)
			if !responsesws.IsSessionResponse(c) {
				emitResponsesStreamProtocolError(c, "upstream returned an invalid Responses stream event")
			}
		}
		sr.Stop(candidate)
	}

	handleStreamData := func(data string, sr *helper.StreamResult) {
		if info.ResponsesStreamEventTransform != nil {
			data = info.ResponsesStreamEventTransform(data)
		}
		if gjson.Get(data, "type").String() == "response.done" {
			normalized, _, err := responsesws.NormalizeResponseDoneEvent([]byte(data))
			if err != nil {
				stopProtocolFailure(fmt.Errorf("normalize response.done event: %w", err), sr)
				return
			}
			data = string(normalized)
		}

		var streamResponse dto.ResponsesStreamResponse
		if err := common.UnmarshalJsonStr(data, &streamResponse); err != nil {
			stopProtocolFailure(fmt.Errorf("decode Responses stream event: %w", err), sr)
			return
		}
		switch streamResponse.Type {
		case "response.completed", "response.incomplete", "response.done",
			"response.failed", "response.error", "error":
			if streamResponse.Response != nil {
				if streamResponse.Response.Usage != nil {
					hasAuthoritativeUsage = true
					*usage = normalizeResponsesUsage(streamResponse.Response.Usage)
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
		cleanTerminal := streamResponse.Type == "response.completed" ||
			streamResponse.Type == "response.incomplete" || streamResponse.Type == "response.done"
		streamError := streamResponse.GetOpenAIError()
		terminalFailure := streamResponse.Type == "response.failed" ||
			streamResponse.Type == "response.error" || streamResponse.Type == "error"
		if cleanTerminal || terminalFailure {
			terminalResolved.Store(true)
		}
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
			candidate.RetryAfter = service.ParseUpstreamRetryDelay(resp.Header, []byte(data), time.Now())
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
			} else {
				// Non-subscription channels treat the downstream as committed from the
				// first event, so the turn cannot be safely retried; log for visibility
				// and forward the failure event to the client below.
				logger.LogError(c, fmt.Sprintf("responses stream terminal failure on non-subscription channel: type=%s message=%s", streamResponse.Type, streamError.Message))
			}
			info.MarkCommittedUpstreamError(candidate)
			relaycommon.MarkResponsesStreamFailureEmitted(c)
			sendResponsesStreamData(c, streamResponse, data)
			sr.Stop(candidate)
			return
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
			case "response.completed", "response.incomplete", "response.done":
				pendingEvents = append(pendingEvents, pendingStreamEvent{response: streamResponse, data: data})
				commitPreflight()
				if !responsesws.IsSessionResponse(c) {
					sr.Done()
				}
				return
			default:
				if strings.TrimSpace(streamResponse.Type) == "" {
					// A typeless event cannot be classified; nothing is committed yet, so
					// fail the preflight and let the relay fail over safely.
					stopProtocolFailure(fmt.Errorf("Responses stream event has no type"), sr)
					return
				}
				if responsesws.IsConnectionScopedEventType(streamResponse.Type) {
					// Connection-scoped extensions (codex.rate_limits arrives FIRST on
					// every Codex turn) are not semantic output: buffer them without
					// committing, or an explicit capacity failure right after them could
					// no longer fail over to a backup credential.
					pendingEvents = append(pendingEvents, pendingStreamEvent{response: streamResponse, data: data})
					pendingBytes += len(data)
					if pendingBytes <= maxResponsesStreamPreflightBytes {
						return
					}
					// Oversized buffer: commit replays every pending event, THIS one
					// included — return so the common send below does not duplicate it.
					commitPreflight()
					return
				}
				// Any other response.* event is real output: commit and stream on.
			}
			commitPreflight()
		}
		sendResponsesStreamData(c, streamResponse, data)
		if cleanTerminal && !responsesws.IsSessionResponse(c) {
			sr.Done()
		}
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
	if !terminalResolved.Load() && info.StreamStatus != nil &&
		info.StreamStatus.EndReason != relaycommon.StreamEndReasonClientGone {
		endErr := info.StreamStatus.EndError
		if endErr == nil {
			endErr = fmt.Errorf("upstream Responses stream ended with reason %q before a terminal event", info.StreamStatus.EndReason)
		} else {
			endErr = fmt.Errorf("upstream Responses stream ended before a terminal event: %w", endErr)
		}
		candidate := types.NewErrorWithStatusCode(
			endErr,
			types.ErrorCodeBadResponseBody,
			http.StatusBadGateway,
		)
		candidate.UpstreamStatusCode = http.StatusBadGateway
		candidate = service.ApplyChannelErrorPolicy(info.ChannelType, candidate)
		if isSubscriptionOAuth && !downstreamCommitted.Load() {
			helper.ClearEventStreamHeaders(c)
			return nil, candidate
		}
		info.MarkCommittedUpstreamError(candidate)
		if !responsesws.IsSessionResponse(c) {
			emitResponsesStreamProtocolError(c, "upstream Responses stream ended before a terminal event")
		}
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

func emitResponsesStreamProtocolError(c *gin.Context, message string) {
	failurePayload, err := common.Marshal(gin.H{
		"type": "error",
		"error": gin.H{
			"type":    "server_error",
			"code":    types.ErrorCodeBadResponseBody,
			"message": message,
		},
	})
	if err != nil {
		return
	}
	if helper.ResponseChunkData(
		c,
		dto.ResponsesStreamResponse{Type: "error"},
		string(failurePayload),
	) == nil {
		relaycommon.MarkResponsesStreamFailureEmitted(c)
	}
}

func normalizeResponsesUsage(upstream *dto.Usage) dto.Usage {
	if upstream == nil {
		return dto.Usage{}
	}

	usage := *upstream
	usage.PromptTokens = upstream.InputTokens
	usage.CompletionTokens = upstream.OutputTokens
	usage.TotalTokens = upstream.TotalTokens
	usage.UsageSource = dto.BillingUsageSourceOAIResponses
	usage.UsageSemantic = dto.BillingUsageSemanticOpenAI
	usage.BillingUsage = nil
	if upstream.InputTokensDetails != nil {
		inputDetails := *upstream.InputTokensDetails
		usage.InputTokensDetails = &inputDetails
		usage.PromptTokensDetails = inputDetails
	}
	if upstream.OutputTokensDetails != nil {
		outputDetails := *upstream.OutputTokensDetails
		usage.OutputTokensDetails = &outputDetails
		usage.CompletionTokenDetails = outputDetails
	}
	usage.BillingUsage = dto.NewOpenAIResponsesBillingUsage(&usage)
	return usage
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
