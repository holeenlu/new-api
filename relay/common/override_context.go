package common

import (
	"github.com/QuantumNous/new-api/types"
)

func BuildParamOverrideContext(info *RelayInfo) map[string]interface{} {
	if info == nil {
		return nil
	}

	ctx := make(map[string]interface{})
	if info.ChannelMeta != nil && info.ChannelMeta.UpstreamModelName != "" {
		ctx["model"] = info.ChannelMeta.UpstreamModelName
		ctx["upstream_model"] = info.ChannelMeta.UpstreamModelName
	}
	if info.OriginModelName != "" {
		ctx["original_model"] = info.OriginModelName
		if _, exists := ctx["model"]; !exists {
			ctx["model"] = info.OriginModelName
		}
	}

	if info.RequestURLPath != "" {
		requestPath := info.RequestURLPath
		if requestPath != "" {
			ctx["request_path"] = requestPath
		}
	}

	ctx[paramOverrideContextRequestHeaders] = buildRequestHeadersContext(info.RequestHeaders)

	headerOverrideSource := GetEffectiveHeaderOverride(info)
	ctx[paramOverrideContextHeaderOverride] = sanitizeHeaderOverrideMap(headerOverrideSource)

	ctx["retry_index"] = info.RetryIndex
	ctx["is_retry"] = info.RetryIndex > 0
	ctx["retry"] = map[string]interface{}{
		"index":    info.RetryIndex,
		"is_retry": info.RetryIndex > 0,
	}

	if info.LastError != nil {
		code := string(info.LastError.GetErrorCode())
		errorType := string(info.LastError.GetErrorType())
		lastError := map[string]interface{}{
			"status_code": info.LastError.StatusCode,
			"message":     info.LastError.Error(),
			"code":        code,
			"error_code":  code,
			"type":        errorType,
			"error_type":  errorType,
			"skip_retry":  types.IsSkipRetryError(info.LastError),
		}
		ctx["last_error"] = lastError
		ctx["last_error_status_code"] = info.LastError.StatusCode
		ctx["last_error_message"] = info.LastError.Error()
		ctx["last_error_code"] = code
		ctx["last_error_type"] = errorType
	}

	ctx["is_channel_test"] = info.IsChannelTest
	return ctx
}
