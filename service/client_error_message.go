package service

import (
	"time"

	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

// LocalizedRelayErrorMessage returns a concise, user-facing message for a stable
// gateway error code in the request's resolved language (user setting →
// Accept-Language → deployment default), replacing the verbose upstream
// diagnostic summary that stays in the backend logs. It returns ok=false for
// codes without a friendly template so the caller preserves the original
// message. The reset time for a usage-limit is derived from err.RetryAfter.
func LocalizedRelayErrorMessage(c *gin.Context, err *types.NewAPIError) (string, bool) {
	if err == nil {
		return "", false
	}
	lang := i18n.DefaultLang
	if c != nil {
		lang = i18n.GetLangFromContext(c)
	}
	return localizedRelayErrorMessage(lang, err.GetErrorCode(), err.RetryAfter, time.Now())
}

// localizedRelayErrorMessage picks the message key and template data for a code,
// then localizes it through the shared go-i18n bundle so the wording lives in
// the project's locale files (en / zh-CN / zh-TW) like every other backend
// string. Codes without a template return ok=false.
func localizedRelayErrorMessage(
	lang string,
	code types.ErrorCode,
	retryAfter time.Duration,
	now time.Time,
) (string, bool) {
	resetAt := ""
	retrySeconds := 0
	if retryAfter > 0 {
		resetAt = now.Add(retryAfter).Format("2006-01-02 15:04")
		retrySeconds = int((retryAfter + time.Second - 1) / time.Second)
	}

	var (
		key  string
		data map[string]any
	)
	switch code {
	case types.ErrorCodeUpstreamUsageLimit:
		if resetAt != "" {
			key, data = i18n.MsgRelayErrUsageLimit, map[string]any{"ResetAt": resetAt}
		} else {
			key = i18n.MsgRelayErrUsageLimitNoReset
		}
	case types.ErrorCodeUpstreamRateLimited:
		if retrySeconds > 0 {
			key, data = i18n.MsgRelayErrRateLimited, map[string]any{"RetrySeconds": retrySeconds}
		} else {
			key = i18n.MsgRelayErrRateLimitedNoRetry
		}
	case types.ErrorCodeUpstreamQuotaExhausted:
		key = i18n.MsgRelayErrQuotaExhausted
	case types.ErrorCodeUpstreamAccountDisabled:
		key = i18n.MsgRelayErrAccountDisabled
	case types.ErrorCodeOAuthUnauthorized:
		key = i18n.MsgRelayErrOAuthUnauthorized
	case types.ErrorCodeOAuthForbidden:
		key = i18n.MsgRelayErrOAuthForbidden
	case types.ErrorCodeModelAtCapacity:
		key = i18n.MsgRelayErrModelAtCapacity
	case types.ErrorCodeModelNotSupported:
		key = i18n.MsgRelayErrModelNotSupported
	case types.ErrorCodeOAuthChannelConcurrencyLimit:
		if retrySeconds > 0 {
			key, data = i18n.MsgRelayErrConcurrencyLimit, map[string]any{"RetrySeconds": retrySeconds}
		} else {
			key = i18n.MsgRelayErrConcurrencyLimitNoRetry
		}
	default:
		return "", false
	}
	return i18n.Translate(lang, key, data), true
}
