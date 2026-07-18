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
		return nil, types.WithOpenAIError(*oaiError, resp.StatusCode)
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

		// 检查当前数据是否包含 completed 状态和 usage 信息
		var streamResponse dto.ResponsesStreamResponse
		if err := common.UnmarshalJsonStr(data, &streamResponse); err != nil {
			logger.LogError(c, "failed to unmarshal stream response: "+err.Error())
			sr.Error(err)
			return
		}
		switch streamResponse.Type {
		case "response.completed":
			if streamResponse.Response != nil {
				if streamResponse.Response.Usage != nil {
					if streamResponse.Response.Usage.InputTokens != 0 {
						usage.PromptTokens = streamResponse.Response.Usage.InputTokens
					}
					if streamResponse.Response.Usage.OutputTokens != 0 {
						usage.CompletionTokens = streamResponse.Response.Usage.OutputTokens
					}
					if streamResponse.Response.Usage.TotalTokens != 0 {
						usage.TotalTokens = streamResponse.Response.Usage.TotalTokens
					}
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
		if isSubscriptionOAuth {
			streamError := streamResponse.GetOpenAIError()
			if streamError != nil {
				candidate := types.WithOpenAIError(*streamError, http.StatusTooManyRequests)
				candidate.UpstreamStatusCode = http.StatusTooManyRequests
				candidate = service.ApplyChannelErrorPolicy(info.ChannelType, candidate)
				if service.IsSubscriptionOAuthModelAtCapacity(info.ChannelType, candidate) {
					info.MarkUpstreamFailureResponse()
					if !downstreamCommitted.Load() {
						if streamResponse.Type == "response.failed" {
							relaycommon.SetResponsesStreamPreflightFailureEvent(c, data)
						}
						preflightError = candidate
						sr.Stop(candidate)
						return
					}
					sr.Error(candidate)
				}
			}
			if !downstreamCommitted.Load() {
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

	if usage.CompletionTokens == 0 {
		// 计算输出文本的 token 数量
		tempStr := responseTextBuilder.String()
		if len(tempStr) > 0 {
			// 非正常结束，使用输出文本的 token 数量
			completionTokens := service.CountTextToken(tempStr, info.UpstreamModelName)
			usage.CompletionTokens = completionTokens
		}
	}

	if usage.PromptTokens == 0 && usage.CompletionTokens != 0 {
		usage.PromptTokens = info.GetEstimatePromptTokens()
	}

	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens

	return usage, nil
}
