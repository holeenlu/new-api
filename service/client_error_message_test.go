package service

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/types"

	"github.com/stretchr/testify/require"
)

func TestLocalizedRelayErrorMessage(t *testing.T) {
	require.NoError(t, i18n.Init())

	now := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	resetIn := 3*time.Hour + 30*time.Minute
	wantReset := now.Add(resetIn).Format("2006-01-02 15:04")

	tests := []struct {
		name        string
		lang        string
		code        types.ErrorCode
		retryAfter  time.Duration
		wantOK      bool
		wantContain []string
	}{
		{
			name:        "usage limit zh-CN with reset",
			lang:        i18n.LangZhCN,
			code:        types.ErrorCodeUpstreamUsageLimit,
			retryAfter:  resetIn,
			wantOK:      true,
			wantContain: []string{"订阅用量已达上限", wantReset},
		},
		{
			name:        "usage limit zh-TW with reset uses traditional characters",
			lang:        i18n.LangZhTW,
			code:        types.ErrorCodeUpstreamUsageLimit,
			retryAfter:  resetIn,
			wantOK:      true,
			wantContain: []string{"訂閱用量已達上限", wantReset},
		},
		{
			name:        "usage limit en with reset",
			lang:        i18n.LangEn,
			code:        types.ErrorCodeUpstreamUsageLimit,
			retryAfter:  resetIn,
			wantOK:      true,
			wantContain: []string{"subscription usage limit", wantReset},
		},
		{
			name:        "usage limit zh-CN without reset falls back",
			lang:        i18n.LangZhCN,
			code:        types.ErrorCodeUpstreamUsageLimit,
			retryAfter:  0,
			wantOK:      true,
			wantContain: []string{"上游未提供恢复时间", "额度重置后会自动恢复"},
		},
		{
			name:        "rate limited zh-CN with retry seconds",
			lang:        i18n.LangZhCN,
			code:        types.ErrorCodeUpstreamRateLimited,
			retryAfter:  5 * time.Second,
			wantOK:      true,
			wantContain: []string{"上游请求过于频繁", "5 秒"},
		},
		{
			name:        "quota exhausted zh-CN",
			lang:        i18n.LangZhCN,
			code:        types.ErrorCodeUpstreamQuotaExhausted,
			retryAfter:  0,
			wantOK:      true,
			wantContain: []string{"额度已耗尽"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			msg, ok := localizedRelayErrorMessage(test.lang, test.code, test.retryAfter, types.SubscriptionOAuthUsageWindows{}, now)
			require.Equal(t, test.wantOK, ok)
			for _, want := range test.wantContain {
				require.Contains(t, msg, want)
			}
			// A resolved template must never leak the raw i18n key or an
			// unrendered {{.Var}} placeholder to the client.
			require.NotContains(t, msg, "relay_error.")
			require.NotContains(t, msg, "{{")
		})
	}
}

// Unclassified codes must keep their original (verbose) message so no error
// regresses to a generic one.
func TestLocalizedRelayErrorMessageSkipsUnknownCodes(t *testing.T) {
	msg, ok := localizedRelayErrorMessage(i18n.LangZhCN, types.ErrorCodeBadResponseStatusCode, time.Minute, types.SubscriptionOAuthUsageWindows{}, time.Now())
	require.False(t, ok)
	require.Empty(t, msg)
}

func TestLocalizedRelayErrorMessageDistinguishesSubscriptionUsageWindows(t *testing.T) {
	require.NoError(t, i18n.Init())
	now := time.Date(2026, time.July, 24, 1, 0, 0, 0, time.UTC)

	message, ok := localizedRelayErrorMessage(
		i18n.LangZhCN,
		types.ErrorCodeUpstreamUsageLimit,
		7*24*time.Hour,
		types.SubscriptionOAuthUsageWindows{
			FiveHourExhausted:  true,
			FiveHourRetryAfter: 5 * time.Hour,
			SevenDayExhausted:  true,
			SevenDayRetryAfter: 7 * 24 * time.Hour,
		},
		now,
	)
	require.True(t, ok)
	require.Contains(t, message, "5 小时订阅用量窗口已达上限")
	require.Contains(t, message, now.Add(5*time.Hour).Format("2006-01-02 15:04"))
	require.Contains(t, message, "周订阅用量窗口已达上限")
	require.Contains(t, message, now.Add(7*24*time.Hour).Format("2006-01-02 15:04"))
}
