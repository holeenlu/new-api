package controller

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestChannelTestAPIErrorPreservesTypedUpstreamError(t *testing.T) {
	upstreamErr := types.NewErrorWithStatusCode(
		errors.New("credential concurrency limit reached"),
		types.ErrorCodeOAuthChannelConcurrencyLimit,
		http.StatusServiceUnavailable,
	)
	upstreamErr.UpstreamStatusCode = http.StatusTooManyRequests

	actual := channelTestAPIError(upstreamErr, types.ErrorCodeDoRequestFailed, http.StatusInternalServerError)

	require.Same(t, upstreamErr, actual)
	require.Equal(t, http.StatusServiceUnavailable, actual.StatusCode)
	require.Equal(t, http.StatusTooManyRequests, actual.GetUpstreamStatusCode())
	require.Equal(t, types.ErrorCodeOAuthChannelConcurrencyLimit, actual.GetErrorCode())
}

func TestSettleTestQuotaUsesTieredBilling(t *testing.T) {
	info := &relaycommon.RelayInfo{
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode:   "tiered_expr",
			ExprString:    `param("stream") == true ? tier("stream", p * 3) : tier("base", p * 2)`,
			ExprHash:      billingexpr.ExprHashString(`param("stream") == true ? tier("stream", p * 3) : tier("base", p * 2)`),
			GroupRatio:    1,
			EstimatedTier: "stream",
			QuotaPerUnit:  common.QuotaPerUnit,
			ExprVersion:   1,
		},
		BillingRequestInput: &billingexpr.RequestInput{
			Body: []byte(`{"stream":true}`),
		},
	}

	quota, result := settleTestQuota(info, types.PriceData{
		ModelRatio:      1,
		CompletionRatio: 2,
	}, &dto.Usage{
		PromptTokens: 1000,
	})

	require.Equal(t, 1500, quota)
	require.NotNil(t, result)
	require.Equal(t, "stream", result.MatchedTier)
}

func TestBuildTestLogOtherInjectsTieredInfo(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())

	info := &relaycommon.RelayInfo{
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode: "tiered_expr",
			ExprString:  `tier("base", p * 2)`,
		},
		ChannelMeta: &relaycommon.ChannelMeta{},
	}
	priceData := types.PriceData{
		GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1},
	}
	usage := &dto.Usage{
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: 12,
		},
	}

	other := buildTestLogOther(ctx, info, priceData, usage, &billingexpr.TieredResult{
		MatchedTier: "base",
	})

	require.Equal(t, "tiered_expr", other["billing_mode"])
	require.Equal(t, "base", other["matched_tier"])
	require.NotEmpty(t, other["expr_b64"])
}

func TestResolveChannelTestUserIDUsesRequestUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set("id", 2)

	userID, err := resolveChannelTestUserID(ctx)

	require.NoError(t, err)
	require.Equal(t, 2, userID)
}

func TestSelectChannelsForAutomaticTestPassiveRecoveryOnlyUsesAutoDisabled(t *testing.T) {
	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled},
		{Id: 2, Status: common.ChannelStatusAutoDisabled},
		{Id: 3, Status: common.ChannelStatusManuallyDisabled},
	}

	selected := selectChannelsForAutomaticTest(channels, operation_setting.ChannelTestModePassiveRecovery)

	require.Len(t, selected, 1)
	require.Equal(t, 2, selected[0].Id)
}

func TestSelectChannelsForAutomaticTestScheduledIncludesSubscriptionChannels(t *testing.T) {
	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled},
		{Id: 2, Status: common.ChannelStatusAutoDisabled},
		{Id: 3, Status: common.ChannelStatusManuallyDisabled},
		{Id: 4, Type: constant.ChannelTypeClaudeCode, Status: common.ChannelStatusEnabled},
		{Id: 5, Type: constant.ChannelTypeCodex, Status: common.ChannelStatusEnabled},
	}

	selected := selectChannelsForAutomaticTest(channels, operation_setting.ChannelTestModeScheduledAll)

	require.Len(t, selected, 4)
	require.Equal(t, []int{1, 2, 4, 5}, []int{
		selected[0].Id,
		selected[1].Id,
		selected[2].Id,
		selected[3].Id,
	})
}

func TestBuildTestRequestLimitsClaudeCodeProbeOutput(t *testing.T) {
	request := buildTestRequest("claude-sonnet-5", "", &model.Channel{Type: constant.ChannelTypeClaudeCode}, false)
	general, ok := request.(*dto.GeneralOpenAIRequest)
	require.True(t, ok)
	require.NotNil(t, general.MaxTokens)
	require.Equal(t, uint(1), *general.MaxTokens)
}

func TestValidateSubscriptionOAuthChannelRejectsUnsafeRequestMutation(t *testing.T) {
	passThroughSetting := `{"pass_through_body_enabled":true}`
	paramOverride := `{"operations":[{"path":"system","mode":"set","value":"replacement"}]}`
	claudeHeaderOverride := `{"Anthropic-Beta":"replacement"}`
	codexHeaderOverride := `{"Originator":"replacement"}`
	tests := []struct {
		name           string
		channelType    int
		key            string
		headerOverride string
	}{
		{name: "claude code", channelType: constant.ChannelTypeClaudeCode, key: "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-token", headerOverride: claudeHeaderOverride},
		{name: "codex", channelType: constant.ChannelTypeCodex, key: `{"access_token":"token","account_id":"account"}`, headerOverride: codexHeaderOverride},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Error(t, validateChannel(&model.Channel{
				Type:    test.channelType,
				Key:     test.key,
				Setting: &passThroughSetting,
			}, true))
			require.Error(t, validateChannel(&model.Channel{
				Type:          test.channelType,
				Key:           test.key,
				ParamOverride: &paramOverride,
			}, true))
			require.Error(t, validateChannel(&model.Channel{
				Type:           test.channelType,
				Key:            test.key,
				HeaderOverride: &test.headerOverride,
			}, true))
			require.NoError(t, validateChannel(&model.Channel{
				Type: test.channelType,
				Key:  test.key,
			}, true))
		})
	}
}

func TestTestAllChannelsRejectsExistingActiveTask(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.SystemTask{}, &model.SystemTaskLock{}))

	existing, err := model.CreateSystemTask(model.SystemTaskTypeChannelTest, nil, nil)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/test", nil)

	TestAllChannels(ctx)

	require.Equal(t, http.StatusConflict, recorder.Code)
	require.Contains(t, recorder.Body.String(), existing.TaskID)
	require.Contains(t, recorder.Body.String(), "已有通道测试任务正在运行或等待中")
}
