package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
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

// StatusCodeMappingRisk describes a channel mapping that changes an upstream
// timeout response. A 504/524 may mean that the provider already accepted and
// billed the request, so changing its response semantics must be confirmed
// explicitly by the caller before it is saved.
type StatusCodeMappingRisk struct {
	From int
	To   int
}

// ValidateStatusCodeMapping validates both syntax and the HTTP status range.
// It returns the high-risk 504/524 mappings separately so callers can require
// an explicit confirmation without weakening the normal validation path.
func ValidateStatusCodeMapping(mapping string) ([]StatusCodeMappingRisk, error) {
	mapping = strings.TrimSpace(mapping)
	if mapping == "" || mapping == "{}" {
		return nil, nil
	}

	parsed := make(map[string]any)
	if err := common.Unmarshal([]byte(mapping), &parsed); err != nil {
		return nil, fmt.Errorf("invalid status code mapping JSON: %w", err)
	}
	if parsed == nil {
		return nil, errors.New("status code mapping must be a JSON object")
	}

	risks := make([]StatusCodeMappingRisk, 0)
	for rawFrom, rawTo := range parsed {
		normalizedFrom := strings.TrimSpace(rawFrom)
		if len(normalizedFrom) != 3 ||
			normalizedFrom[0] < '1' || normalizedFrom[0] > '5' ||
			normalizedFrom[1] < '0' || normalizedFrom[1] > '9' ||
			normalizedFrom[2] < '0' || normalizedFrom[2] > '9' {
			return nil, fmt.Errorf("invalid source status code: %s", rawFrom)
		}
		from, _ := strconv.Atoi(normalizedFrom)
		to, ok := parseStatusCodeMappingValue(rawTo)
		if !ok {
			return nil, fmt.Errorf("invalid target status code for %d", from)
		}
		if (from == http.StatusGatewayTimeout || from == 524) && from != to {
			risks = append(risks, StatusCodeMappingRisk{From: from, To: to})
		}
	}

	sort.Slice(risks, func(i, j int) bool {
		if risks[i].From == risks[j].From {
			return risks[i].To < risks[j].To
		}
		return risks[i].From < risks[j].From
	})
	return risks, nil
}

// NewStatusCodeMappingRisks returns only risky mappings newly introduced by
// the current value. Existing risky mappings do not block unrelated edits.
func NewStatusCodeMappingRisks(original, current string) ([]StatusCodeMappingRisk, error) {
	// A legacy original value may be invalid because it predates server-side
	// validation. Ignore that error only for the risk diff; the submitted current
	// value is still validated strictly and must be corrected before it is saved.
	originalRisks, _ := ValidateStatusCodeMapping(original)
	currentRisks, err := ValidateStatusCodeMapping(current)
	if err != nil {
		return nil, err
	}

	known := make(map[StatusCodeMappingRisk]struct{}, len(originalRisks))
	for _, risk := range originalRisks {
		known[risk] = struct{}{}
	}
	newRisks := make([]StatusCodeMappingRisk, 0, len(currentRisks))
	for _, risk := range currentRisks {
		if _, exists := known[risk]; !exists {
			newRisks = append(newRisks, risk)
		}
	}
	return newRisks, nil
}

func parseStatusCodeMappingValue(value any) (int, bool) {
	switch v := value.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return 0, false
		}
		statusCode, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, false
		}
		return statusCode, statusCode >= 100 && statusCode <= 599
	case float64:
		if v != math.Trunc(v) || v < 100 || v > 599 {
			return 0, false
		}
		return int(v), true
	case int:
		return v, v >= 100 && v <= 599
	case json.Number:
		statusCode, err := strconv.Atoi(v.String())
		if err != nil {
			return 0, false
		}
		return statusCode, statusCode >= 100 && statusCode <= 599
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
