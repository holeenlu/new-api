package controller

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	common.RedisEnabled = false
	os.Exit(m.Run())
}

func setupResponsesCompactBillingDB(t *testing.T) {
	t.Helper()

	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousIsMasterNode := common.IsMasterNode
	previousSQLitePath := common.SQLitePath
	previousMainDatabaseType := common.MainDatabaseType()
	previousLogDatabaseType := common.LogDatabaseType()
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	previousBatchUpdateEnabled := common.BatchUpdateEnabled
	previousLogConsumeEnabled := common.LogConsumeEnabled
	previousErrorLogEnabled := constant.ErrorLogEnabled
	previousCountToken := constant.CountToken
	previousSensitiveEnabled := setting.CheckSensitiveEnabled

	t.Setenv("SQL_DSN", "")
	t.Setenv("LOG_SQL_DSN", "")
	common.IsMasterNode = false
	common.SQLitePath = fmt.Sprintf(
		"file:%s?mode=memory&cache=shared",
		strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()),
	)
	common.MemoryCacheEnabled = false
	common.BatchUpdateEnabled = false
	common.LogConsumeEnabled = true
	constant.ErrorLogEnabled = false
	constant.CountToken = false
	setting.CheckSensitiveEnabled = false

	require.NoError(t, model.InitDB())
	model.LOG_DB = model.DB
	sqlDB, err := model.DB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, model.DB.AutoMigrate(
		&model.User{},
		&model.Token{},
		&model.Channel{},
		&model.Log{},
	))

	t.Cleanup(func() {
		_ = sqlDB.Close()
		model.DB = previousDB
		model.LOG_DB = previousLogDB
		common.IsMasterNode = previousIsMasterNode
		common.SQLitePath = previousSQLitePath
		common.SetDatabaseTypes(previousMainDatabaseType, previousLogDatabaseType)
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
		common.BatchUpdateEnabled = previousBatchUpdateEnabled
		common.LogConsumeEnabled = previousLogConsumeEnabled
		constant.ErrorLogEnabled = previousErrorLogEnabled
		constant.CountToken = previousCountToken
		setting.CheckSensitiveEnabled = previousSensitiveEnabled
	})
}

func configureResponsesCompactPrices(
	t *testing.T,
	sourceModel string,
	mappedModel string,
	group string,
	sourcePrice *float64,
	mappedPrice float64,
	groupRatio float64,
) {
	t.Helper()

	savedModelPrices := ratio_setting.ModelPrice2JSONString()
	savedModelRatios := ratio_setting.ModelRatio2JSONString()
	savedGroupRatios := ratio_setting.GroupRatio2JSONString()
	savedSelfUseMode := operation_setting.SelfUseModeEnabled
	billingConfig, ok := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
	require.True(t, ok)
	savedBillingModes := billing_setting.GetBillingModeCopy()
	t.Cleanup(func() {
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(savedModelPrices))
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(savedModelRatios))
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(savedGroupRatios))
		operation_setting.SelfUseModeEnabled = savedSelfUseMode
		billingConfig.BillingMode = savedBillingModes
	})

	sourceCompactModel := ratio_setting.WithCompactModelSuffix(sourceModel)
	mappedCompactModel := ratio_setting.WithCompactModelSuffix(mappedModel)

	modelPrices := ratio_setting.GetModelPriceCopy()
	delete(modelPrices, sourceModel)
	delete(modelPrices, sourceCompactModel)
	delete(modelPrices, ratio_setting.CompactWildcardModelKey)
	if sourcePrice != nil {
		modelPrices[sourceCompactModel] = *sourcePrice
	}
	modelPrices[mappedCompactModel] = mappedPrice
	modelPricesJSON, err := common.Marshal(modelPrices)
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(string(modelPricesJSON)))

	modelRatios := ratio_setting.GetModelRatioCopy()
	delete(modelRatios, sourceModel)
	delete(modelRatios, sourceCompactModel)
	delete(modelRatios, ratio_setting.CompactWildcardModelKey)
	modelRatios[mappedCompactModel] = 1
	modelRatiosJSON, err := common.Marshal(modelRatios)
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(string(modelRatiosJSON)))

	groupRatios := ratio_setting.GetGroupRatioCopy()
	groupRatios[group] = groupRatio
	groupRatiosJSON, err := common.Marshal(groupRatios)
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(string(groupRatiosJSON)))

	billingModes := billing_setting.GetBillingModeCopy()
	billingModes[sourceCompactModel] = billing_setting.BillingModeRatio
	billingModes[mappedCompactModel] = billing_setting.BillingModeRatio
	billingConfig.BillingMode = billingModes
	operation_setting.SelfUseModeEnabled = false
}

func seedResponsesCompactBillingRequest(
	t *testing.T,
	upstreamURL string,
	sourceModel string,
	mappedModel string,
	group string,
	initialQuota int,
) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()

	user := &model.User{
		Id:       1,
		Username: "compact-billing-user",
		Status:   common.UserStatusEnabled,
		Group:    group,
		Quota:    initialQuota,
	}
	token := &model.Token{
		Id:          1,
		UserId:      user.Id,
		Key:         "compact-billing-token",
		Name:        "compact-billing-token",
		Status:      common.TokenStatusEnabled,
		RemainQuota: initialQuota,
		Group:       group,
	}
	mappingBytes, err := common.Marshal(map[string]string{sourceModel: mappedModel})
	require.NoError(t, err)
	channel := &model.Channel{
		Id:           1,
		Type:         constant.ChannelTypeOpenAI,
		Key:          "sk-compact-billing",
		Status:       common.ChannelStatusEnabled,
		Name:         "compact-billing-channel",
		BaseURL:      common.GetPointer(upstreamURL),
		Group:        group,
		ModelMapping: common.GetPointer(string(mappingBytes)),
		AutoBan:      common.GetPointer(0),
	}
	require.NoError(t, model.DB.Create(user).Error)
	require.NoError(t, model.DB.Create(token).Error)
	require.NoError(t, model.DB.Create(channel).Error)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/v1/responses/compact",
		strings.NewReader(fmt.Sprintf(`{"model":%q,"input":"hello"}`, sourceModel)),
	)
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, constant.ContextKeyUserId, user.Id)
	common.SetContextKey(c, constant.ContextKeyUserGroup, group)
	common.SetContextKey(c, constant.ContextKeyUsingGroup, group)
	common.SetContextKey(c, constant.ContextKeyUserQuota, initialQuota)
	common.SetContextKey(c, constant.ContextKeyTokenId, token.Id)
	common.SetContextKey(c, constant.ContextKeyTokenKey, token.Key)
	common.SetContextKey(c, constant.ContextKeyTokenGroup, group)
	common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{BillingPreference: "wallet_only"})
	c.Set("token_name", token.Name)
	require.Nil(t, middleware.SetupContextForSelectedChannel(
		c,
		channel,
		ratio_setting.WithCompactModelSuffix(sourceModel),
	))

	return c, recorder
}

func TestResponsesCompactPricesMappedModelBeforePreConsume(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupResponsesCompactBillingDB(t)
	service.InitHttpClient()

	const (
		sourceModel  = "compact-billing-source"
		mappedModel  = "compact-billing-mapped"
		group        = "compact-billing-group"
		initialQuota = 1000
		mappedPrice  = 0.0001
		mappedQuota  = 50
	)

	tests := []struct {
		name        string
		sourcePrice *float64
	}{
		{
			name: "source model is unpriced",
		},
		{
			name:        "source price exceeds available quota",
			sourcePrice: common.GetPointer(0.01),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configureResponsesCompactPrices(t, sourceModel, mappedModel, group, test.sourcePrice, mappedPrice, 1)

			type capturedRequest struct {
				path string
				body []byte
			}
			captured := make(chan capturedRequest, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				captured <- capturedRequest{path: r.URL.Path, body: body}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"cmp_1","object":"response.compaction","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
			}))
			t.Cleanup(server.Close)

			c, recorder := seedResponsesCompactBillingRequest(
				t,
				server.URL,
				sourceModel,
				mappedModel,
				group,
				initialQuota,
			)

			Relay(c, types.RelayFormatOpenAIResponsesCompaction)

			assert.Equal(t, http.StatusOK, recorder.Code)
			var outbound capturedRequest
			select {
			case outbound = <-captured:
			case <-time.After(2 * time.Second):
				t.Fatal("compact request did not reach the mapped upstream")
			}
			assert.Equal(t, "/v1/responses/compact", outbound.path)
			var request dto.OpenAIResponsesRequest
			require.NoError(t, common.Unmarshal(outbound.body, &request))
			assert.Equal(t, mappedModel, request.Model)

			var user model.User
			require.NoError(t, model.DB.First(&user, 1).Error)
			assert.Equal(t, initialQuota-mappedQuota, user.Quota)
			assert.Equal(t, mappedQuota, user.UsedQuota)
			var token model.Token
			require.NoError(t, model.DB.First(&token, 1).Error)
			assert.Equal(t, initialQuota-mappedQuota, token.RemainQuota)
			assert.Equal(t, mappedQuota, token.UsedQuota)

			require.NoError(t, model.DB.Exec("DELETE FROM logs").Error)
			require.NoError(t, model.DB.Exec("DELETE FROM channels").Error)
			require.NoError(t, model.DB.Exec("DELETE FROM tokens").Error)
			require.NoError(t, model.DB.Exec("DELETE FROM users").Error)
		})
	}
}

func TestResponsesCompactFirstAttemptReservesTrustedWalletBeforeUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupResponsesCompactBillingDB(t)
	service.InitHttpClient()

	const (
		sourceModel = "compact-trusted-source"
		mappedModel = "compact-trusted-mapped"
		group       = "compact-trusted-group"
		mappedPrice = 0.0001
		mappedQuota = 50
	)
	initialQuota := common.GetTrustQuota() + 1000
	configureResponsesCompactPrices(t, sourceModel, mappedModel, group, nil, mappedPrice, 1)

	requestArrived := make(chan struct{}, 1)
	releaseResponse := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestArrived <- struct{}{}
		<-releaseResponse
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmp_trusted","object":"response.compaction","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(server.Close)

	c, recorder := seedResponsesCompactBillingRequest(
		t,
		server.URL,
		sourceModel,
		mappedModel,
		group,
		initialQuota,
	)

	var releaseOnce sync.Once
	releaseUpstream := func() {
		releaseOnce.Do(func() { close(releaseResponse) })
	}
	done := make(chan struct{})
	go func() {
		Relay(c, types.RelayFormatOpenAIResponsesCompaction)
		close(done)
	}()
	t.Cleanup(func() {
		releaseUpstream()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	})

	select {
	case <-requestArrived:
	case <-time.After(2 * time.Second):
		t.Fatal("compact request did not reach upstream")
	}
	var reservedUser model.User
	require.NoError(t, model.DB.First(&reservedUser, 1).Error)
	assert.Equal(t, initialQuota-mappedQuota, reservedUser.Quota)
	var reservedToken model.Token
	require.NoError(t, model.DB.First(&reservedToken, 1).Error)
	assert.Equal(t, initialQuota-mappedQuota, reservedToken.RemainQuota)
	assert.Equal(t, mappedQuota, reservedToken.UsedQuota)

	releaseUpstream()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("compact relay did not finish")
	}
	assert.Equal(t, http.StatusOK, recorder.Code)
}

func TestResponsesCompactViolationSettlesMappedReserveToEffectiveGroupFee(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupResponsesCompactBillingDB(t)
	service.InitHttpClient()

	const (
		sourceModel  = "compact-violation-source"
		mappedModel  = "compact-violation-mapped"
		group        = "compact-violation-group"
		initialQuota = 5000
		mappedPrice  = 0.0002
		groupRatio   = 2.0
		reserveQuota = 200
		feeQuota     = 1000
	)
	configureResponsesCompactPrices(t, sourceModel, mappedModel, group, nil, mappedPrice, groupRatio)

	savedGrokSettings := model_setting.GetGrokSettings()
	registeredGrokSettings := config.GlobalConfig.Get("grok")
	require.NotNil(t, registeredGrokSettings)
	require.NoError(t, config.UpdateConfigFromMap(registeredGrokSettings, map[string]string{
		"violation_deduction_enabled": "true",
		"violation_deduction_amount":  "0.001",
	}))
	t.Cleanup(func() {
		require.NoError(t, config.UpdateConfigFromMap(registeredGrokSettings, map[string]string{
			"violation_deduction_enabled": strconv.FormatBool(savedGrokSettings.ViolationDeductionEnabled),
			"violation_deduction_amount":  strconv.FormatFloat(savedGrokSettings.ViolationDeductionAmount, 'g', -1, 64),
		}))
	})

	var upstreamCalls atomic.Int32
	requestArrived := make(chan struct{}, 1)
	releaseResponse := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		requestArrived <- struct{}{}
		<-releaseResponse
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"content_policy_violation","message":"Failed check: SAFETY_CHECK_TYPE"}}`))
	}))
	t.Cleanup(server.Close)

	c, recorder := seedResponsesCompactBillingRequest(
		t,
		server.URL,
		sourceModel,
		mappedModel,
		group,
		initialQuota,
	)

	var releaseOnce sync.Once
	releaseUpstream := func() {
		releaseOnce.Do(func() { close(releaseResponse) })
	}
	done := make(chan struct{})
	go func() {
		Relay(c, types.RelayFormatOpenAIResponsesCompaction)
		close(done)
	}()
	t.Cleanup(func() {
		releaseUpstream()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	})

	select {
	case <-requestArrived:
	case <-time.After(2 * time.Second):
		t.Fatal("mapped compact request did not reach the violating upstream")
	}
	var reservedUser model.User
	require.NoError(t, model.DB.First(&reservedUser, 1).Error)
	assert.Equal(t, initialQuota-reserveQuota, reservedUser.Quota)
	var reservedToken model.Token
	require.NoError(t, model.DB.First(&reservedToken, 1).Error)
	assert.Equal(t, initialQuota-reserveQuota, reservedToken.RemainQuota)
	assert.Equal(t, reserveQuota, reservedToken.UsedQuota)

	releaseUpstream()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("compact violation relay did not finish")
	}

	assert.Equal(t, http.StatusBadRequest, recorder.Code)
	assert.Equal(t, int32(1), upstreamCalls.Load())
	assert.Contains(t, recorder.Body.String(), string(types.ErrorCodeViolationFeeGrokCSAM))
	var user model.User
	require.NoError(t, model.DB.First(&user, 1).Error)
	assert.Equal(t, initialQuota-feeQuota, user.Quota)
	assert.Equal(t, feeQuota, user.UsedQuota)
	var token model.Token
	require.NoError(t, model.DB.First(&token, 1).Error)
	assert.Equal(t, initialQuota-feeQuota, token.RemainQuota)
	assert.Equal(t, feeQuota, token.UsedQuota)

	var channel model.Channel
	require.NoError(t, model.DB.First(&channel, 1).Error)
	assert.Equal(t, int64(feeQuota), channel.UsedQuota)

	var violationLog model.Log
	require.NoError(t, model.DB.Where(
		"type = ? AND content = ?",
		model.LogTypeConsume,
		"Violation fee charged",
	).First(&violationLog).Error)
	assert.Equal(t, feeQuota, violationLog.Quota)
	assert.Equal(t, group, violationLog.Group)
	var other map[string]any
	require.NoError(t, common.UnmarshalJsonStr(violationLog.Other, &other))
	assert.Equal(t, true, other["violation_fee"])
	assert.Equal(t, groupRatio, other["group_ratio"])
}

func TestResponsesCommittedFailureSettlesUsageAndRecordsChannelErrorWithoutRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupResponsesCompactBillingDB(t)
	service.InitHttpClient()
	constant.ErrorLogEnabled = true
	previousStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousStreamingTimeout })

	const (
		modelName    = "responses-committed-failure-model"
		group        = "responses-committed-failure-group"
		initialQuota = 5000
		actualQuota  = 50
	)
	savedModelPrices := ratio_setting.ModelPrice2JSONString()
	savedModelRatios := ratio_setting.ModelRatio2JSONString()
	savedGroupRatios := ratio_setting.GroupRatio2JSONString()
	savedSelfUseMode := operation_setting.SelfUseModeEnabled
	billingConfig, ok := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
	require.True(t, ok)
	savedBillingModes := billing_setting.GetBillingModeCopy()
	t.Cleanup(func() {
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(savedModelPrices))
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(savedModelRatios))
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(savedGroupRatios))
		operation_setting.SelfUseModeEnabled = savedSelfUseMode
		billingConfig.BillingMode = savedBillingModes
	})

	modelPrices := ratio_setting.GetModelPriceCopy()
	modelPrices[modelName] = 0.0001
	modelPricesJSON, err := common.Marshal(modelPrices)
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(string(modelPricesJSON)))
	modelRatios := ratio_setting.GetModelRatioCopy()
	modelRatios[modelName] = 1
	modelRatiosJSON, err := common.Marshal(modelRatios)
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(string(modelRatiosJSON)))
	groupRatios := ratio_setting.GetGroupRatioCopy()
	groupRatios[group] = 1
	groupRatiosJSON, err := common.Marshal(groupRatios)
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(string(groupRatiosJSON)))
	billingModes := billing_setting.GetBillingModeCopy()
	billingModes[modelName] = billing_setting.BillingModeRatio
	billingConfig.BillingMode = billingModes
	operation_setting.SelfUseModeEnabled = false

	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"type":"response.failed","response":{"status":"failed","error":{"type":"server_error","code":"upstream_failure","message":"committed upstream failure"},"usage":{"input_tokens":5,"output_tokens":0,"total_tokens":5}}}`,
			`data: {"type":"response.output_text.delta","delta":"must-not-be-forwarded"}`,
		}, "\n") + "\n"))
	}))
	defer server.Close()

	user := &model.User{
		Id:       1,
		Username: "responses-committed-user",
		Status:   common.UserStatusEnabled,
		Group:    group,
		Quota:    initialQuota,
	}
	token := &model.Token{
		Id:          1,
		UserId:      user.Id,
		Key:         "responses-committed-token",
		Name:        "responses-committed-token",
		Status:      common.TokenStatusEnabled,
		RemainQuota: initialQuota,
		Group:       group,
	}
	channel := &model.Channel{
		Id:      1,
		Type:    constant.ChannelTypeOpenAI,
		Key:     "sk-responses-committed",
		Status:  common.ChannelStatusEnabled,
		Name:    "responses-committed-channel",
		BaseURL: common.GetPointer(server.URL),
		Group:   group,
		AutoBan: common.GetPointer(0),
	}
	require.NoError(t, model.DB.Create(user).Error)
	require.NoError(t, model.DB.Create(token).Error)
	require.NoError(t, model.DB.Create(channel).Error)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(fmt.Sprintf(`{"model":%q,"input":"hello","stream":true}`, modelName)),
	)
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, constant.ContextKeyUserId, user.Id)
	common.SetContextKey(c, constant.ContextKeyUserGroup, group)
	common.SetContextKey(c, constant.ContextKeyUsingGroup, group)
	common.SetContextKey(c, constant.ContextKeyUserQuota, initialQuota)
	common.SetContextKey(c, constant.ContextKeyTokenId, token.Id)
	common.SetContextKey(c, constant.ContextKeyTokenKey, token.Key)
	common.SetContextKey(c, constant.ContextKeyTokenGroup, group)
	common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{BillingPreference: "wallet_only"})
	c.Set("token_name", token.Name)
	require.Nil(t, middleware.SetupContextForSelectedChannel(c, channel, modelName))

	Relay(c, types.RelayFormatOpenAIResponses)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, int32(1), upstreamCalls.Load())
	assert.Contains(t, recorder.Body.String(), "response.failed")
	assert.NotContains(t, recorder.Body.String(), "must-not-be-forwarded")

	var updatedUser model.User
	require.NoError(t, model.DB.First(&updatedUser, user.Id).Error)
	assert.Equal(t, initialQuota-actualQuota, updatedUser.Quota)
	assert.Equal(t, actualQuota, updatedUser.UsedQuota)
	var updatedToken model.Token
	require.NoError(t, model.DB.First(&updatedToken, token.Id).Error)
	assert.Equal(t, initialQuota-actualQuota, updatedToken.RemainQuota)
	assert.Equal(t, actualQuota, updatedToken.UsedQuota)

	var consumeLogs []model.Log
	require.NoError(t, model.DB.Where("type = ?", model.LogTypeConsume).Find(&consumeLogs).Error)
	require.Len(t, consumeLogs, 1)
	assert.Equal(t, actualQuota, consumeLogs[0].Quota)
	var errorLogs []model.Log
	require.NoError(t, model.DB.Where("type = ?", model.LogTypeError).Find(&errorLogs).Error)
	require.Len(t, errorLogs, 1)
	assert.Contains(t, errorLogs[0].Content, "committed upstream failure")
}
