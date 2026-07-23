package openai

import (
	"bufio"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/relayconvert"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

func OaiResponsesToChatHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}

	defer service.CloseResponseBodyGracefully(resp)

	var responsesResp dto.OpenAIResponsesResponse
	body, apiErr := readBoundedResponsesBody(c, resp.Body)
	if apiErr != nil {
		return nil, apiErr
	}

	if err := common.Unmarshal(body, &responsesResp); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}

	if oaiError := responsesResp.GetOpenAIError(); oaiError != nil && oaiError.Type != "" {
		// This handler only runs on upstream 200s, so a wrapped error must derive
		// its real status (429 stays retryable), keep Retry-After, and mark the
		// upstream failure — exactly what the native path does.
		return nil, responsesWrappedError(info, resp, body, oaiError)
	}
	if responsesBodyMissingResult(&responsesResp) {
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("upstream returned a Responses body without status or output"),
			types.ErrorCodeBadResponseBody,
			http.StatusBadGateway,
		)
	}

	chatResult, err := relayconvert.ConvertResponse(c, info, types.RelayFormatOpenAI, &responsesResp)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	chatResp, ok := chatResult.Value.(*dto.OpenAITextResponse)
	if !ok {
		return nil, types.NewOpenAIError(fmt.Errorf("expected OpenAI chat response, got %T", chatResult.Value), types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if chatID := helper.GetResponseID(c); chatID != "" {
		chatResp.Id = chatID
	}
	usage := chatResult.Usage

	if usage == nil || usage.TotalTokens == 0 {
		text := service.ExtractOutputTextFromResponses(&responsesResp)
		usage = service.ResponseText2Usage(c, text, info.UpstreamModelName, info.GetEstimatePromptTokens())
		chatResp.Usage = *usage
	}

	responseValue := any(chatResp)
	if info.RelayFormat != types.RelayFormatOpenAI {
		targetResult, err := relayconvert.ConvertResponse(c, info, info.RelayFormat, chatResp)
		if err != nil {
			return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
		}
		responseValue = targetResult.Value
	}
	responseBody, err := common.Marshal(responseValue)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
	}

	service.IOCopyBytesGracefully(c, resp, responseBody)
	return usage, nil
}

func OaiResponsesToChatBufferedStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	defer service.CloseResponseBodyGracefully(resp)

	accumulator := relayconvert.NewResponsesBufferedAccumulator()
	var finalResponse *dto.OpenAIResponsesResponse
	var streamErr *types.NewAPIError

	// This loop reads the upstream directly (no shared scanner), so it needs its
	// own idle guard: any line (comments included, they are legitimate SSE
	// keepalive) refreshes the idle window, and only valid data events refresh
	// the longer valid-data window — an upstream emitting nothing but comments
	// cannot hold the request open forever. Client cancellation propagates via
	// the request context bound in DoApiRequest.
	idleTimeout := time.Duration(constant.StreamingTimeout) * time.Second
	validDataTimeout := 2 * idleTimeout
	idleWatchdog := time.AfterFunc(idleTimeout, func() { _ = resp.Body.Close() })
	defer idleWatchdog.Stop()
	validWatchdog := time.AfterFunc(validDataTimeout, func() { _ = resp.Body.Close() })
	defer validWatchdog.Stop()

	scanner := helper.NewStreamScanner(resp.Body)
	scanner.Split(bufio.ScanLines)
	for scanner.Scan() {
		idleWatchdog.Reset(idleTimeout)
		line := scanner.Text()
		if len(line) < 6 || line[:5] != "data:" {
			continue
		}
		data := line[5:]
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			if data == "[DONE]" {
				break
			}
			continue
		}
		validWatchdog.Reset(validDataTimeout)

		var streamResp dto.ResponsesStreamResponse
		if err := common.UnmarshalJsonStr(data, &streamResp); err != nil {
			logger.LogError(c, "failed to unmarshal buffered responses stream event: "+err.Error())
			streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
			break
		}
		accumulator.ProcessEvent(&streamResp)
		switch streamResp.Type {
		case "response.completed", "response.done", "response.incomplete":
			finalResponse = streamResp.Response
			if streamResp.Type == "response.incomplete" {
				if finalResponse == nil {
					finalResponse = &dto.OpenAIResponsesResponse{}
				}
				if len(finalResponse.Status) == 0 {
					finalResponse.Status = []byte(`"incomplete"`)
				}
			}
		case "response.failed", "response.error", "error":
			// Top-level `error` is a terminal failure too; GetOpenAIError covers
			// both the top-level error field and response.error.
			info.MarkUpstreamFailureResponse()
			if oaiErr := streamResp.GetOpenAIError(); oaiErr != nil && oaiErr.Type != "" {
				streamErr = types.WithOpenAIError(*oaiErr, responsesStreamErrorStatus(data, oaiErr))
			} else {
				streamErr = types.NewOpenAIError(fmt.Errorf("responses stream error: %s", streamResp.Type), types.ErrorCodeBadResponse, http.StatusBadGateway)
			}
			streamErr.RetryAfter = service.ParseUpstreamRetryDelay(resp.Header, []byte(data), time.Now())
		}
		if streamErr != nil || finalResponse != nil {
			break
		}
	}
	if streamErr != nil {
		return nil, streamErr
	}
	if err := scanner.Err(); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	if finalResponse == nil {
		// The upstream ended (EOF / [DONE] / idle guard) without a terminal event.
		// Nothing has been written downstream yet, so fail the request instead of
		// fabricating a "completed" response that would hide the upstream failure
		// and settle billing on truncated output.
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("upstream Responses stream ended before a terminal event"),
			types.ErrorCodeBadResponseBody,
			http.StatusBadGateway,
		)
	}
	accumulator.SupplementResponseOutput(finalResponse)

	chatResult, err := relayconvert.ConvertResponse(c, info, types.RelayFormatOpenAI, finalResponse)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	chatResp, ok := chatResult.Value.(*dto.OpenAITextResponse)
	if !ok {
		return nil, types.NewOpenAIError(fmt.Errorf("expected OpenAI chat response, got %T", chatResult.Value), types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if chatID := helper.GetResponseID(c); chatID != "" {
		chatResp.Id = chatID
	}
	usage := chatResult.Usage
	if usage == nil || usage.TotalTokens == 0 {
		text := service.ExtractOutputTextFromResponses(finalResponse)
		usage = service.ResponseText2Usage(c, text, info.UpstreamModelName, info.GetEstimatePromptTokens())
		chatResp.Usage = *usage
	}

	responseValue := any(chatResp)
	if info.RelayFormat != types.RelayFormatOpenAI {
		targetResult, err := relayconvert.ConvertResponse(c, info, info.RelayFormat, chatResp)
		if err != nil {
			return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
		}
		responseValue = targetResult.Value
	}
	responseBody, err := common.Marshal(responseValue)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
	}

	service.IOCopyBytesGracefully(c, resp, responseBody)
	return usage, nil
}

func OaiResponsesToChatStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}

	defer service.CloseResponseBodyGracefully(resp)

	responseId := helper.GetResponseID(c)
	createAt := time.Now().Unix()
	state, err := relayconvert.NewResponseStreamState(types.RelayFormatOpenAIResponses, info.RelayFormat, relayconvert.ResponseStreamOptions{
		ID:      responseId,
		Model:   info.UpstreamModelName,
		Created: createAt,
	})
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	streamErr := (*types.NewAPIError)(nil)

	if info.RelayFormat == types.RelayFormatClaude && info.ClaudeConvertInfo == nil {
		info.ClaudeConvertInfo = &relaycommon.ClaudeConvertInfo{LastMessagesType: relaycommon.LastMessageTypeNone}
	}

	sendGeminiResponse := func(geminiResponse *dto.GeminiChatResponse) bool {
		if geminiResponse == nil {
			return true
		}
		geminiResponseStr, err := common.Marshal(geminiResponse)
		if err != nil {
			streamErr = types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
			return false
		}
		c.Render(-1, common.CustomEvent{Data: "data: " + string(geminiResponseStr)})
		_ = helper.FlushWriter(c)
		return true
	}

	sendStreamResult := func(result relayconvert.ResponseResult) bool {
		switch value := result.Value.(type) {
		case dto.ChatCompletionsStreamResponse:
			if len(value.Choices) == 0 && value.Usage == nil {
				return true
			}
			if err := helper.ObjectData(c, &value); err != nil {
				streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
				return false
			}
			return true
		case *dto.ChatCompletionsStreamResponse:
			if value == nil || (len(value.Choices) == 0 && value.Usage == nil) {
				return true
			}
			if err := helper.ObjectData(c, value); err != nil {
				streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
				return false
			}
			return true
		case dto.ClaudeResponse:
			if err := helper.ClaudeData(c, value); err != nil {
				streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
				return false
			}
			return true
		case *dto.ClaudeResponse:
			if value == nil {
				return true
			}
			if err := helper.ClaudeData(c, *value); err != nil {
				streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
				return false
			}
			return true
		case dto.GeminiChatResponse:
			return sendGeminiResponse(&value)
		case *dto.GeminiChatResponse:
			return sendGeminiResponse(value)
		default:
			streamErr = types.NewOpenAIError(fmt.Errorf("unsupported converted stream response type %T", result.Value), types.ErrorCodeBadResponse, http.StatusInternalServerError)
			return false
		}
	}

	sawTerminal := false
	hasAuthoritativeUsage := false
	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		if streamErr != nil {
			sr.Stop(streamErr)
			return
		}

		var streamResp dto.ResponsesStreamResponse
		if err := common.UnmarshalJsonStr(data, &streamResp); err != nil {
			logger.LogError(c, "failed to unmarshal responses stream event: "+err.Error())
			sr.Error(err)
			return
		}

		// captureAuthoritativeUsage stores a terminal event's usage as the
		// authoritative settlement record: the total is completed on the upstream
		// copy BEFORE normalization so the snapshot (including the nested
		// BillingUsage) carries one consistent set of numbers, and the flag stops
		// the estimate fallback from overwriting it — an explicit zero stays zero.
		captureAuthoritativeUsage := func(upstream *dto.Usage) {
			upstreamUsage := *upstream
			if upstreamUsage.TotalTokens == 0 {
				upstreamUsage.TotalTokens = upstreamUsage.InputTokens + upstreamUsage.OutputTokens
			}
			authoritativeUsage := normalizeResponsesUsage(&upstreamUsage)
			state.SetUsage(&authoritativeUsage)
			hasAuthoritativeUsage = true
		}

		switch streamResp.Type {
		case "response.completed", "response.done", "response.incomplete":
			sawTerminal = true
		case "response.error", "response.failed", "error":
			// Top-level `error` is terminal too; GetOpenAIError covers both the
			// top-level error field and response.error.
			sawTerminal = true
			info.MarkUpstreamFailureResponse()
			// A failure event never reaches the conversion chain, so capture its
			// authoritative usage here, before stopping.
			if streamResp.Response != nil && streamResp.Response.Usage != nil {
				captureAuthoritativeUsage(streamResp.Response.Usage)
			}
			if oaiErr := streamResp.GetOpenAIError(); oaiErr != nil && oaiErr.Type != "" {
				streamErr = types.WithOpenAIError(*oaiErr, responsesStreamErrorStatus(data, oaiErr))
			} else {
				streamErr = types.NewOpenAIError(fmt.Errorf("responses stream error: %s", streamResp.Type), types.ErrorCodeBadResponse, http.StatusBadGateway)
			}
			// Preserve the upstream's cooldown signal alongside the status code so
			// credential recovery does not degrade to a default window.
			streamErr.RetryAfter = service.ParseUpstreamRetryDelay(resp.Header, []byte(data), time.Now())
			sr.Stop(streamErr)
			return
		}

		results, err := relayconvert.ConvertStreamResponseChunk(c, info, state, &streamResp)
		if err != nil {
			streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
			sr.Stop(streamErr)
			return
		}
		// A clean terminal's authoritative usage is captured AFTER the conversion
		// chain (which may store its own un-completed copy) but BEFORE the
		// downstream writes: a client that disconnects right at the terminal must
		// not turn an explicit zero consumption into a billed estimate.
		switch streamResp.Type {
		case "response.completed", "response.done", "response.incomplete":
			if streamResp.Response != nil && streamResp.Response.Usage != nil {
				captureAuthoritativeUsage(streamResp.Response.Usage)
			}
		}
		for _, result := range results {
			if !sendStreamResult(result) {
				sr.Stop(streamErr)
				return
			}
		}
	})

	usage := state.Usage()
	// Estimate ONLY when no authoritative usage exists. An authoritative record
	// is trusted verbatim: explicit zero consumption must not be replaced by a
	// billed prompt estimate, and a record without total_tokens (only
	// input/output) must not be discarded because its total reads as zero.
	if !hasAuthoritativeUsage && (usage == nil || usage.TotalTokens == 0) {
		usage = service.ResponseText2Usage(c, state.UsageText(), info.UpstreamModelName, info.GetEstimatePromptTokens())
		state.SetUsage(usage)
	}

	// emitConvertedStreamFailure surfaces a committed-stream failure in the
	// terminal shape the TARGET protocol defines: OpenAI clients get the
	// `data: {"error":...}` chunk the official API uses mid-stream, Claude
	// clients get the protocol's `event: error`. Formats without a standard
	// in-stream error shape (e.g. Gemini) end truncated — the missing normal
	// termination is itself the failure signal.
	emitConvertedStreamFailure := func(apiErr *types.NewAPIError) {
		openaiErr := apiErr.ToOpenAIError()
		switch info.RelayFormat {
		case types.RelayFormatOpenAI:
			if payload, marshalErr := common.Marshal(gin.H{"error": openaiErr}); marshalErr == nil {
				c.Render(-1, common.CustomEvent{Data: "data: " + string(payload)})
				_ = helper.FlushWriter(c)
			}
		case types.RelayFormatClaude:
			// Map to the Claude protocol's own error taxonomy (api_error,
			// rate_limit_error, overloaded_error, ...) — a raw gateway type like
			// new_api_error is not a Claude-recognizable error type.
			_ = helper.ClaudeData(c, dto.ClaudeResponse{Type: "error", Error: apiErr.ToClaudeError()})
		}
	}

	if streamErr != nil {
		if !c.Writer.Written() {
			// Nothing reached the client: fail through the normal error path (the
			// written-request guard still governs whether the relay may retry).
			return nil, streamErr
		}
		// Committed stream: appending a plain JSON error to a live SSE stream is
		// not a legal event, so emit the protocol terminal failure instead, record
		// the error for billing/health, and settle usage on received output.
		info.MarkCommittedUpstreamError(streamErr)
		emitConvertedStreamFailure(streamErr)
		return usage, nil
	}

	if !sawTerminal && info.StreamStatus != nil &&
		info.StreamStatus.EndReason != relaycommon.StreamEndReasonClientGone {
		// The upstream stream ended (EOF / timeout / scanner error) without a
		// terminal Responses event. Do not fabricate a normal chat ending
		// (finalize + [DONE]) over a truncated turn.
		endErr := info.StreamStatus.EndError
		if endErr == nil {
			endErr = fmt.Errorf("upstream Responses stream ended with reason %q before a terminal event", info.StreamStatus.EndReason)
		} else {
			endErr = fmt.Errorf("upstream Responses stream ended before a terminal event: %w", endErr)
		}
		candidate := types.NewErrorWithStatusCode(endErr, types.ErrorCodeBadResponseBody, http.StatusBadGateway)
		candidate.UpstreamStatusCode = http.StatusBadGateway
		candidate = service.ApplyChannelErrorPolicy(info.ChannelType, candidate)
		if !c.Writer.Written() {
			// Nothing reached the client yet: fail the request through the normal
			// error path (retry-eligible for ordinary channels).
			return nil, candidate
		}
		// Committed: record the failure, surface the protocol terminal failure,
		// and end without a normal termination so no client mistakes the
		// truncation for success. Usage settles on what was received.
		info.MarkCommittedUpstreamError(candidate)
		emitConvertedStreamFailure(candidate)
		return usage, nil
	}

	if info.RelayFormat == types.RelayFormatClaude && info.ClaudeConvertInfo != nil {
		info.ClaudeConvertInfo.Usage = usage
	}
	finalResults, err := relayconvert.FinalizeStreamResponse(c, info, state)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	for _, result := range finalResults {
		if !sendStreamResult(result) {
			return nil, streamErr
		}
	}
	if info.RelayFormat == types.RelayFormatOpenAI && info.ShouldIncludeUsage && usage != nil {
		if err := helper.ObjectData(c, helper.GenerateFinalUsageResponse(responseId, createAt, info.UpstreamModelName, *usage)); err != nil {
			return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
		}
	}

	if info.RelayFormat == types.RelayFormatOpenAI {
		helper.Done(c)
	}
	return usage, nil
}
