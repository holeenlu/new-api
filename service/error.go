package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/types"
)

func MidjourneyErrorWrapper(code int, desc string) *dto.MidjourneyResponse {
	return &dto.MidjourneyResponse{
		Code:        code,
		Description: desc,
	}
}

func MidjourneyErrorWithStatusCodeWrapper(code int, desc string, statusCode int) *dto.MidjourneyResponseWithStatusCode {
	return &dto.MidjourneyResponseWithStatusCode{
		StatusCode: statusCode,
		Response:   *MidjourneyErrorWrapper(code, desc),
	}
}

//// OpenAIErrorWrapper wraps an error into an OpenAIErrorWithStatusCode
//func OpenAIErrorWrapper(err error, code string, statusCode int) *dto.OpenAIErrorWithStatusCode {
//	text := err.Error()
//	lowerText := strings.ToLower(text)
//	if !strings.HasPrefix(lowerText, "get file base64 from url") && !strings.HasPrefix(lowerText, "mime type is not supported") {
//		if strings.Contains(lowerText, "post") || strings.Contains(lowerText, "dial") || strings.Contains(lowerText, "http") {
//			common.SysLog(fmt.Sprintf("error: %s", text))
//			text = "请求上游地址失败"
//		}
//	}
//	openAIError := dto.OpenAIError{
//		Message: text,
//		Type:    "new_api_error",
//		Code:    code,
//	}
//	return &dto.OpenAIErrorWithStatusCode{
//		Error:      openAIError,
//		StatusCode: statusCode,
//	}
//}
//
//func OpenAIErrorWrapperLocal(err error, code string, statusCode int) *dto.OpenAIErrorWithStatusCode {
//	openaiErr := OpenAIErrorWrapper(err, code, statusCode)
//	openaiErr.LocalError = true
//	return openaiErr
//}

func ClaudeErrorWrapper(err error, code string, statusCode int) *dto.ClaudeErrorWithStatusCode {
	text := err.Error()
	lowerText := strings.ToLower(text)
	if !strings.HasPrefix(lowerText, "get file base64 from url") {
		if strings.Contains(lowerText, "post") || strings.Contains(lowerText, "dial") || strings.Contains(lowerText, "http") {
			common.SysLog(fmt.Sprintf("error: %s", text))
			text = "请求上游地址失败"
		}
	}
	claudeError := types.ClaudeError{
		Message: text,
		Type:    "new_api_error",
	}
	return &dto.ClaudeErrorWithStatusCode{
		Error:      claudeError,
		StatusCode: statusCode,
	}
}

func ClaudeErrorWrapperLocal(err error, code string, statusCode int) *dto.ClaudeErrorWithStatusCode {
	claudeErr := ClaudeErrorWrapper(err, code, statusCode)
	claudeErr.LocalError = true
	return claudeErr
}

const maxUpstreamErrorResponseBytes = 1 << 20

func sanitizeUpstreamErrorMessage(message string) string {
	message = common.RedactSensitiveCredentials(strings.TrimSpace(message))
	message = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, message)
	runes := []rune(message)
	if len(runes) > common.LocalLogContentLimit {
		return fmt.Sprintf("%s... [truncated]", string(runes[:common.LocalLogContentLimit]))
	}
	return message
}

func buildUpstreamErrorSummary(resp *http.Response, responseBody []byte, truncated bool, message string) error {
	parts := []string{fmt.Sprintf("bad response status code %d", resp.StatusCode)}
	if message = sanitizeUpstreamErrorMessage(message); message != "" {
		parts = append(parts, "message: "+message)
	}
	if contentType := strings.TrimSpace(resp.Header.Get("Content-Type")); contentType != "" {
		parts = append(parts, "content_type: "+sanitizeUpstreamErrorMessage(contentType))
	}
	responseBytes := len(responseBody)
	if resp.ContentLength > 0 {
		responseBytes = int(resp.ContentLength)
	}
	if truncated && resp.ContentLength <= 0 {
		parts = append(parts, fmt.Sprintf("response_bytes: >=%d", responseBytes+1))
	} else {
		parts = append(parts, fmt.Sprintf("response_bytes: %d", responseBytes))
	}
	for _, headerName := range []string{common.UpstreamRequestIdKey, "x-request-id", "request-id", "x-amzn-requestid"} {
		if requestID := sanitizeUpstreamErrorMessage(resp.Header.Get(headerName)); requestID != "" {
			parts = append(parts, "upstream_request_id: "+requestID)
			break
		}
	}
	return errors.New(strings.Join(parts, ", "))
}

func RelayErrorHandler(ctx context.Context, resp *http.Response) (newApiErr *types.NewAPIError) {
	retryAfter := ParseRetryAfterHeader(resp.Header.Get("Retry-After"), time.Now())
	defer func() {
		if newApiErr != nil {
			newApiErr.RetryAfter = retryAfter
			newApiErr.UpstreamStatusCode = resp.StatusCode
		}
	}()
	newApiErr = types.InitOpenAIError(types.ErrorCodeBadResponseStatusCode, resp.StatusCode)

	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamErrorResponseBytes+1))
	CloseResponseBodyGracefully(resp)
	if err != nil {
		newApiErr.Err = fmt.Errorf("bad response status code %d, failed to read upstream error response", resp.StatusCode)
		return
	}
	truncated := len(responseBody) > maxUpstreamErrorResponseBytes
	if truncated {
		responseBody = responseBody[:maxUpstreamErrorResponseBytes]
	}

	var errResponse dto.GeneralErrorResponse
	err = common.Unmarshal(responseBody, &errResponse)
	if err != nil {
		newApiErr.Err = buildUpstreamErrorSummary(resp, responseBody, truncated, "")
		logger.LogError(ctx, newApiErr.Error())
		return
	}

	if common.GetJsonType(errResponse.Error) == "object" {
		// General format error (OpenAI, Anthropic, Gemini, etc.)
		oaiError := errResponse.TryToOpenAIError()
		if oaiError != nil {
			oaiError.Message = sanitizeUpstreamErrorMessage(oaiError.Message)
			newApiErr = types.WithOpenAIError(*oaiError, resp.StatusCode)
			newApiErr.Err = buildUpstreamErrorSummary(resp, responseBody, truncated, oaiError.Message)
			return
		}
	}
	message := sanitizeUpstreamErrorMessage(errResponse.ToMessage())
	newApiErr = types.NewOpenAIError(errors.New(message), types.ErrorCodeBadResponseStatusCode, resp.StatusCode)
	newApiErr.Err = buildUpstreamErrorSummary(resp, responseBody, truncated, message)
	return
}

func ParseRetryAfterHeader(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds >= int64(maximumSubscriptionOAuthRetryAfter/time.Second) {
			return maximumSubscriptionOAuthRetryAfter
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	duration := when.Sub(now)
	if duration > maximumSubscriptionOAuthRetryAfter {
		return maximumSubscriptionOAuthRetryAfter
	}
	return duration
}

func ResetStatusCode(newApiErr *types.NewAPIError, statusCodeMappingStr string) {
	if newApiErr == nil {
		return
	}
	if statusCodeMappingStr == "" || statusCodeMappingStr == "{}" {
		return
	}
	statusCodeMapping := make(map[string]any)
	err := common.Unmarshal([]byte(statusCodeMappingStr), &statusCodeMapping)
	if err != nil {
		return
	}
	if newApiErr.StatusCode == http.StatusOK {
		return
	}
	codeStr := strconv.Itoa(newApiErr.StatusCode)
	if value, ok := statusCodeMapping[codeStr]; ok {
		intCode, ok := parseStatusCodeMappingValue(value)
		if !ok {
			return
		}
		newApiErr.StatusCode = intCode
	}
}

func parseStatusCodeMappingValue(value any) (int, bool) {
	switch v := value.(type) {
	case string:
		if v == "" {
			return 0, false
		}
		statusCode, err := strconv.Atoi(v)
		if err != nil {
			return 0, false
		}
		return statusCode, true
	case float64:
		if v != math.Trunc(v) {
			return 0, false
		}
		return int(v), true
	case int:
		return v, true
	case json.Number:
		statusCode, err := strconv.Atoi(v.String())
		if err != nil {
			return 0, false
		}
		return statusCode, true
	default:
		return 0, false
	}
}

func TaskErrorWrapperLocal(err error, code string, statusCode int) *dto.TaskError {
	openaiErr := TaskErrorWrapper(err, code, statusCode)
	openaiErr.LocalError = true
	return openaiErr
}

func TaskErrorWrapper(err error, code string, statusCode int) *dto.TaskError {
	text := err.Error()
	lowerText := strings.ToLower(text)
	if strings.Contains(lowerText, "post") || strings.Contains(lowerText, "dial") || strings.Contains(lowerText, "http") {
		common.SysLog(fmt.Sprintf("error: %s", text))
		//text = "请求上游地址失败"
		text = common.MaskSensitiveInfo(text)
	}
	//避免暴露内部错误
	taskError := &dto.TaskError{
		Code:       code,
		Message:    text,
		StatusCode: statusCode,
		Error:      err,
	}

	return taskError
}

// TaskErrorFromAPIError 将 PreConsumeBilling 返回的 NewAPIError 转换为 TaskError。
func TaskErrorFromAPIError(apiErr *types.NewAPIError) *dto.TaskError {
	if apiErr == nil {
		return nil
	}
	return &dto.TaskError{
		Code:       string(apiErr.GetErrorCode()),
		Message:    apiErr.Err.Error(),
		StatusCode: apiErr.StatusCode,
		Error:      apiErr.Err,
	}
}
