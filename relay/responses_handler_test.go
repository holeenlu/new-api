package relay

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/responsesws"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type compactBillingRecorder struct {
	info                    *relaycommon.RelayInfo
	preConsumedQuota        int
	reserveTarget           int
	modelAtReserve          string
	priceDataAtReserve      types.PriceData
	tieredSnapshotAtReserve *billingexpr.BillingSnapshot
	requestInputAtReserve   *billingexpr.RequestInput
	upstreamModelAtReserve  string
	reserveErr              error
	settleCalls             int
}

func (b *compactBillingRecorder) Settle(int) error {
	b.settleCalls++
	return nil
}

func (b *compactBillingRecorder) Refund(*gin.Context) {}

func (b *compactBillingRecorder) NeedsRefund() bool { return false }

func (b *compactBillingRecorder) FundingCommitted() bool { return false }

func (b *compactBillingRecorder) GetPreConsumedQuota() int { return b.preConsumedQuota }

func (b *compactBillingRecorder) Reserve(targetQuota int) error {
	b.reserveTarget = targetQuota
	b.modelAtReserve = b.info.OriginModelName
	b.priceDataAtReserve = b.info.PriceData
	b.tieredSnapshotAtReserve = b.info.TieredBillingSnapshot
	b.requestInputAtReserve = b.info.BillingRequestInput
	b.upstreamModelAtReserve = b.info.UpstreamModelName
	return b.reserveErr
}

func configureCompactRatioPrice(t *testing.T, compactModel, group string, price float64) {
	t.Helper()
	savedModelPrices := ratio_setting.ModelPrice2JSONString()
	savedModelRatios := ratio_setting.ModelRatio2JSONString()
	savedGroupRatios := ratio_setting.GroupRatio2JSONString()
	billingConfig, ok := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
	require.True(t, ok)
	savedBillingModes := billing_setting.GetBillingModeCopy()
	t.Cleanup(func() {
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(savedModelPrices))
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(savedModelRatios))
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(savedGroupRatios))
		billingConfig.BillingMode = savedBillingModes
	})

	modelPrices := ratio_setting.GetModelPriceCopy()
	modelPrices[compactModel] = price
	modelPricesJSON, err := common.Marshal(modelPrices)
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(string(modelPricesJSON)))
	modelRatios := ratio_setting.GetModelRatioCopy()
	modelRatios[compactModel] = 1
	modelRatiosJSON, err := common.Marshal(modelRatios)
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(string(modelRatiosJSON)))
	groupRatios := ratio_setting.GetGroupRatioCopy()
	groupRatios[group] = 1
	groupRatiosJSON, err := common.Marshal(groupRatios)
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(string(groupRatiosJSON)))
	billingModes := billing_setting.GetBillingModeCopy()
	billingModes[compactModel] = billing_setting.BillingModeRatio
	billingConfig.BillingMode = billingModes
}

func TestResponsesCompactFreezesMappedPriceAndRejectsReserveBeforeUpstream(t *testing.T) {
	compactModel := ratio_setting.WithCompactModelSuffix("mapped-model")
	testGroup := "responses-compact-billing-test"
	configureCompactRatioPrice(t, compactModel, testGroup, 0.02)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("model_mapping", `{"original-model":"mapped-model"}`)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "://must-not-reach-upstream")

	originalPriceData := types.PriceData{
		ModelRatio:        2,
		CompletionRatio:   3,
		QuotaToPreConsume: 11,
	}
	originalSnapshot := &billingexpr.BillingSnapshot{
		BillingMode: "tiered_expr",
		ModelName:   "original-model",
		ExprString:  `tier("original", p * 1)`,
	}
	originalRequestInput := &billingexpr.RequestInput{Body: []byte(`{"source":"original"}`)}
	info := &relaycommon.RelayInfo{
		RelayMode:             relayconstant.RelayModeResponsesCompact,
		OriginModelName:       "original-model",
		RequestURLPath:        "/v1/responses/compact",
		PriceData:             originalPriceData,
		TieredBillingSnapshot: originalSnapshot,
		BillingRequestInput:   originalRequestInput,
		Request:               &dto.OpenAIResponsesCompactionRequest{Model: "original-model"},
		UserSetting:           dto.UserSetting{},
		UsingGroup:            testGroup,
		UserGroup:             testGroup,
		TokenGroup:            testGroup,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenAI,
			ApiType:           constant.APITypeOpenAI,
			ChannelBaseUrl:    "://must-not-reach-upstream",
			UpstreamModelName: "original-model",
		},
	}
	info.SetEstimatePromptTokens(10)
	reserveError := types.NewErrorWithStatusCode(
		errors.New("reserve rejected"),
		types.ErrorCodeInsufficientUserQuota,
		http.StatusForbidden,
		types.ErrOptionWithSkipRetry(),
		types.ErrOptionWithNoRecordErrorLog(),
	)
	billing := &compactBillingRecorder{
		info:             info,
		preConsumedQuota: 1,
		reserveErr:       reserveError,
	}
	info.Billing = billing

	apiError := ResponsesHelper(c, info)

	require.Same(t, reserveError, apiError)
	require.Equal(t, types.ErrorCodeInsufficientUserQuota, apiError.GetErrorCode())
	require.Equal(t, http.StatusForbidden, apiError.StatusCode)
	require.Greater(t, billing.reserveTarget, billing.preConsumedQuota)
	require.Equal(t, compactModel, billing.modelAtReserve)
	require.Equal(t, billing.reserveTarget, billing.priceDataAtReserve.QuotaToPreConsume)
	require.Nil(t, billing.tieredSnapshotAtReserve)
	require.NotNil(t, billing.requestInputAtReserve)
	var frozenRequest dto.OpenAIResponsesRequest
	require.NoError(t, common.Unmarshal(billing.requestInputAtReserve.Body, &frozenRequest))
	require.Equal(t, "mapped-model", frozenRequest.Model)
	require.Equal(t, "original-model", info.OriginModelName)
	require.Equal(t, originalPriceData, info.PriceData)
	require.Same(t, originalSnapshot, info.TieredBillingSnapshot)
	require.Same(t, originalRequestInput, info.BillingRequestInput)
	require.NotNil(t, info.Billing)
	require.False(t, info.PriceData.FreeModel)
}

func TestResponsesCommittedViolationReturnsBeforeOrdinaryUsageSettlement(t *testing.T) {
	service.InitHttpClient()
	previousStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousStreamingTimeout })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"type\":\"invalid_request_error\",\"code\":\"content_policy_violation\",\"message\":\"Failed check: SAFETY_CHECK_TYPE\"},\"usage\":{\"input_tokens\":5,\"output_tokens\":0,\"total_tokens\":5}}}\n\n",
		))
	}))
	defer server.Close()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	common.SetContextKey(c, constant.ContextKeyChannelId, 81)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, server.URL)
	common.SetContextKey(c, constant.ContextKeyChannelKey, "test-key")
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-test")

	info := &relaycommon.RelayInfo{
		IsStream:        true,
		DisablePing:     true,
		RelayMode:       relayconstant.RelayModeResponses,
		OriginModelName: "gpt-test",
		RequestURLPath:  "/v1/responses",
		Request: &dto.OpenAIResponsesRequest{
			Model:  "gpt-test",
			Stream: common.GetPointer(true),
		},
	}
	billing := &compactBillingRecorder{info: info}
	info.Billing = billing

	apiError := ResponsesHelper(c, info)

	require.NotNil(t, apiError)
	require.Equal(t, types.ErrorCodeViolationFeeGrokCSAM, apiError.GetErrorCode())
	require.True(t, types.IsSkipRetryError(apiError))
	require.Zero(t, billing.settleCalls)
	require.True(t, relaycommon.IsResponsesStreamFailureEmitted(c))
	require.Contains(t, recorder.Body.String(), "response.failed")
}

func TestResponsesCompactParamOverrideFreezesFinalModelForRequestAndReserve(t *testing.T) {
	compactModel := ratio_setting.WithCompactModelSuffix("override-model")
	testGroup := "responses-compact-override-billing-test"
	configureCompactRatioPrice(t, compactModel, testGroup, 0.02)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("model_mapping", `{"original-model":"mapped-model"}`)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "://must-not-reach-upstream")
	common.SetContextKey(c, constant.ContextKeyChannelParamOverride, map[string]any{
		"model": "override-model",
	})

	info := &relaycommon.RelayInfo{
		RelayMode:       relayconstant.RelayModeResponsesCompact,
		OriginModelName: "original-model",
		RequestURLPath:  "/v1/responses/compact",
		Request:         &dto.OpenAIResponsesCompactionRequest{Model: "original-model"},
		UsingGroup:      testGroup,
		UserGroup:       testGroup,
		TokenGroup:      testGroup,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenAI,
			ApiType:           constant.APITypeOpenAI,
			ChannelBaseUrl:    "://must-not-reach-upstream",
			UpstreamModelName: "original-model",
			ParamOverride: map[string]any{
				"model": "override-model",
			},
		},
	}
	info.SetEstimatePromptTokens(10)
	reserveError := types.NewErrorWithStatusCode(
		errors.New("reserve rejected"),
		types.ErrorCodeInsufficientUserQuota,
		http.StatusForbidden,
		types.ErrOptionWithSkipRetry(),
	)
	billing := &compactBillingRecorder{info: info, reserveErr: reserveError}
	info.Billing = billing

	apiError := ResponsesHelper(c, info)

	require.Same(t, reserveError, apiError)
	require.Equal(t, compactModel, billing.modelAtReserve)
	require.Equal(t, "override-model", billing.upstreamModelAtReserve)
	require.Equal(t, 0.02, billing.priceDataAtReserve.ModelPrice)
	require.Equal(t, billing.reserveTarget, billing.priceDataAtReserve.QuotaToPreConsume)
	require.NotNil(t, billing.requestInputAtReserve)
	var frozenRequest dto.OpenAIResponsesRequest
	require.NoError(t, common.Unmarshal(billing.requestInputAtReserve.Body, &frozenRequest))
	require.Equal(t, "override-model", frozenRequest.Model)
	require.Equal(t, "original-model", info.OriginModelName)
}

func TestResponsesCompactTieredReserveRecountsFinalOutboundPrompt(t *testing.T) {
	compactModel := ratio_setting.WithCompactModelSuffix("override-model")
	testGroup := "responses-compact-final-prompt-test"
	configureCompactRatioPrice(t, compactModel, testGroup, 0.02)

	billingConfig, ok := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
	require.True(t, ok)
	savedExpressions := billing_setting.GetBillingExprCopy()
	t.Cleanup(func() { billingConfig.BillingExpr = savedExpressions })
	billingModes := billing_setting.GetBillingModeCopy()
	billingModes[compactModel] = billing_setting.BillingModeTieredExpr
	billingConfig.BillingMode = billingModes
	billingExpressions := billing_setting.GetBillingExprCopy()
	billingExpressions[compactModel] = `tier("base", p * 1 + c * 1)`
	billingConfig.BillingExpr = billingExpressions
	savedCountToken := constant.CountToken
	constant.CountToken = true
	t.Cleanup(func() { constant.CountToken = savedCountToken })

	finalInstructions := "channel override adds a substantially longer provider prompt for compact billing"
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "original-model")
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "://must-not-reach-upstream")
	common.SetContextKey(c, constant.ContextKeyChannelParamOverride, map[string]any{
		"model":        "override-model",
		"instructions": finalInstructions,
	})

	info := &relaycommon.RelayInfo{
		RelayMode:       relayconstant.RelayModeResponsesCompact,
		OriginModelName: "original-model",
		RequestURLPath:  "/v1/responses/compact",
		Request: &dto.OpenAIResponsesCompactionRequest{
			Model:        "original-model",
			Input:        []byte(`"hello"`),
			Instructions: []byte(`"short"`),
		},
		UsingGroup: testGroup,
		UserGroup:  testGroup,
		TokenGroup: testGroup,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenAI,
			ApiType:           constant.APITypeOpenAI,
			ChannelBaseUrl:    "://must-not-reach-upstream",
			UpstreamModelName: "original-model",
			ParamOverride: map[string]any{
				"model":        "override-model",
				"instructions": finalInstructions,
			},
		},
	}
	info.SetEstimatePromptTokens(1)
	reserveError := types.NewErrorWithStatusCode(
		errors.New("stop after final reserve"),
		types.ErrorCodeInsufficientUserQuota,
		http.StatusForbidden,
		types.ErrOptionWithSkipRetry(),
	)
	billing := &compactBillingRecorder{info: info, reserveErr: reserveError}
	info.Billing = billing

	apiError := ResponsesHelper(c, info)

	require.Same(t, reserveError, apiError)
	require.NotNil(t, billing.tieredSnapshotAtReserve)
	require.NotNil(t, billing.requestInputAtReserve)
	var finalRequest dto.OpenAIResponsesRequest
	require.NoError(t, common.Unmarshal(billing.requestInputAtReserve.Body, &finalRequest))
	finalMeta := finalRequest.GetTokenCountMeta()
	expectedPromptTokens := service.CountTextToken(finalMeta.CombineText, finalRequest.Model)
	require.Greater(t, expectedPromptTokens, 1)
	require.Equal(t, expectedPromptTokens, billing.tieredSnapshotAtReserve.EstimatedPromptTokens)
	require.Equal(t, expectedPromptTokens, info.GetEstimatePromptTokens())
	require.Equal(t, "original-model", common.GetContextKeyString(c, constant.ContextKeyOriginalModel))
}

func TestResponsesCompactParamOverrideContinuationRequiresLiveSession(t *testing.T) {
	service.InitHttpClient()

	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, server.URL)
	common.SetContextKey(c, constant.ContextKeyChannelParamOverride, map[string]any{
		"previous_response_id": "resp_previous",
	})
	responsesws.SetSession(c, &responsesws.Session{})

	info := &relaycommon.RelayInfo{
		RelayMode:       relayconstant.RelayModeResponsesCompact,
		OriginModelName: "compact-model",
		RequestURLPath:  "/v1/responses/compact",
		Request: &dto.OpenAIResponsesCompactionRequest{
			Model: "compact-model",
			Input: []byte(`"hello"`),
		},
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenAI,
			ApiType:           constant.APITypeOpenAI,
			ChannelBaseUrl:    server.URL,
			UpstreamModelName: "compact-model",
		},
	}

	apiError := ResponsesHelper(c, info)

	require.NotNil(t, apiError)
	require.Equal(t, types.ErrorCodeWebSocketConnectionLimitReached, apiError.GetErrorCode())
	require.Equal(t, http.StatusBadRequest, apiError.StatusCode)
	require.True(t, types.IsSkipRetryError(apiError))
	require.True(t, responsesws.IsContinuationRequired(c))
	require.Equal(t, int32(0), upstreamCalls.Load())
	require.False(t, c.Writer.Written())
	require.Empty(t, recorder.Body.String())
}

func TestResponsesRawPassThroughContinuationRequiresLiveSession(t *testing.T) {
	service.InitHttpClient()

	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	requestBody := `{"model":"responses-model","previous_response_id":"resp_previous","input":"continue"}`
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(requestBody))
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, server.URL)
	common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{PassThroughBodyEnabled: true})
	responsesws.SetSession(c, &responsesws.Session{})

	info := &relaycommon.RelayInfo{
		RelayMode:       relayconstant.RelayModeResponses,
		OriginModelName: "responses-model",
		RequestURLPath:  "/v1/responses",
		Request: &dto.OpenAIResponsesRequest{
			Model:              "responses-model",
			PreviousResponseID: "resp_previous",
			Input:              []byte(`"continue"`),
		},
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenAI,
			ApiType:           constant.APITypeOpenAI,
			ChannelBaseUrl:    server.URL,
			UpstreamModelName: "responses-model",
			ChannelSetting: dto.ChannelSettings{
				PassThroughBodyEnabled: true,
			},
		},
	}

	apiError := ResponsesHelper(c, info)

	require.NotNil(t, apiError)
	assert.Equal(t, types.ErrorCodeWebSocketConnectionLimitReached, apiError.GetErrorCode())
	assert.Equal(t, http.StatusBadRequest, apiError.StatusCode)
	assert.True(t, types.IsSkipRetryError(apiError))
	assert.True(t, responsesws.IsContinuationRequired(c))
	assert.Zero(t, upstreamCalls.Load())
	assert.False(t, c.Writer.Written())
	assert.Empty(t, recorder.Body.String())
}

func TestResponsesCompactMappedModelDisablesRawBodyPassthrough(t *testing.T) {
	service.InitHttpClient()

	compactModel := ratio_setting.WithCompactModelSuffix("mapped-model")
	testGroup := "responses-compact-passthrough-test"
	configureCompactRatioPrice(t, compactModel, testGroup, 0)

	type capturedRequest struct {
		body []byte
		err  error
	}
	upstreamRequest := make(chan capturedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		upstreamRequest <- capturedRequest{body: body, err: err}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"type":"server_error","message":"stop after capture"}}`))
	}))
	defer server.Close()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/v1/responses/compact",
		strings.NewReader(`{"model":"original-model","input":"hello"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("model_mapping", `{"original-model":"mapped-model"}`)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, server.URL)
	common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{
		PassThroughBodyEnabled: true,
	})
	info := &relaycommon.RelayInfo{
		RelayMode:       relayconstant.RelayModeResponsesCompact,
		OriginModelName: "original-model",
		RequestURLPath:  "/v1/responses/compact",
		Request: &dto.OpenAIResponsesCompactionRequest{
			Model: "original-model",
			Input: []byte(`"hello"`),
		},
		UsingGroup: testGroup,
		UserGroup:  testGroup,
		TokenGroup: testGroup,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenAI,
			ApiType:           constant.APITypeOpenAI,
			ChannelBaseUrl:    server.URL,
			UpstreamModelName: "original-model",
			ChannelSetting: dto.ChannelSettings{
				PassThroughBodyEnabled: true,
			},
		},
	}
	info.SetEstimatePromptTokens(1)
	info.Billing = &compactBillingRecorder{info: info}

	apiError := ResponsesHelper(c, info)

	require.NotNil(t, apiError)
	var captured capturedRequest
	select {
	case captured = <-upstreamRequest:
	case <-time.After(2 * time.Second):
		t.Fatal("mapped compact request did not reach upstream")
	}
	require.NoError(t, captured.err)
	var request dto.OpenAIResponsesRequest
	require.NoError(t, common.Unmarshal(captured.body, &request))
	require.Equal(t, "mapped-model", request.Model)
	require.NotContains(t, string(captured.body), `"model":"original-model"`)
	require.Equal(t, "original-model", info.OriginModelName)
}

func TestResponsesCompactSuffixWithoutMappingDisablesRawBodyPassthrough(t *testing.T) {
	service.InitHttpClient()

	baseModel := "suffix-model"
	compactModel := ratio_setting.WithCompactModelSuffix(baseModel)
	testGroup := "responses-compact-suffix-passthrough-test"
	configureCompactRatioPrice(t, compactModel, testGroup, 0)

	type capturedRequest struct {
		body []byte
		err  error
	}
	upstreamRequest := make(chan capturedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		upstreamRequest <- capturedRequest{body: body, err: err}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"type":"server_error","message":"stop after capture"}}`))
	}))
	defer server.Close()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/v1/responses/compact",
		strings.NewReader(`{"model":"`+compactModel+`","input":"hello","text":{"format":{"type":"json_object"}}}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, server.URL)
	common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{
		PassThroughBodyEnabled: true,
	})
	info := &relaycommon.RelayInfo{
		RelayMode:       relayconstant.RelayModeResponsesCompact,
		OriginModelName: compactModel,
		RequestURLPath:  "/v1/responses/compact",
		Request: &dto.OpenAIResponsesCompactionRequest{
			Model: compactModel,
			Input: []byte(`"hello"`),
			Text:  []byte(`{"format":{"type":"json_object"}}`),
		},
		UsingGroup: testGroup,
		UserGroup:  testGroup,
		TokenGroup: testGroup,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenAI,
			ApiType:           constant.APITypeOpenAI,
			ChannelBaseUrl:    server.URL,
			UpstreamModelName: compactModel,
			ChannelSetting: dto.ChannelSettings{
				PassThroughBodyEnabled: true,
			},
		},
	}
	info.Billing = &compactBillingRecorder{info: info}

	apiError := ResponsesHelper(c, info)

	require.NotNil(t, apiError)
	var captured capturedRequest
	select {
	case captured = <-upstreamRequest:
	case <-time.After(2 * time.Second):
		t.Fatal("suffix compact request did not reach upstream")
	}
	require.NoError(t, captured.err)
	var request dto.OpenAIResponsesRequest
	require.NoError(t, common.Unmarshal(captured.body, &request))
	require.Equal(t, baseModel, request.Model)
	require.NotContains(t, string(captured.body), compactModel)
	require.NotContains(t, string(captured.body), `"text"`)
	require.Equal(t, compactModel, info.OriginModelName)
}
