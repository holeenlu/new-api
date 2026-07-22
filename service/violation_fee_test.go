package service

import (
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// committedViolationFeeSettler 模拟"资金来源已提交、令牌调整失败"的会话:
// FundingCommitted 为 true,收费已发生,消费账目必须保留。
type committedViolationFeeSettler struct {
	actualQuota int
}

func (s *committedViolationFeeSettler) Settle(actualQuota int) error {
	s.actualQuota = actualQuota
	return errors.New("token ledger adjustment failed after funding committed")
}

func (s *committedViolationFeeSettler) Refund(*gin.Context) {}

func (s *committedViolationFeeSettler) NeedsRefund() bool { return false }

func (s *committedViolationFeeSettler) FundingCommitted() bool { return true }

func (s *committedViolationFeeSettler) GetPreConsumedQuota() int { return 0 }

func (s *committedViolationFeeSettler) Reserve(int) error { return nil }

// uncommittedViolationFeeSettler 模拟"资金来源从未提交"的失败会话(例如信任
// 旁路用户的钱包直写失败):NeedsRefund 同样为 false,但 FundingCommitted 为
// false——收费未发生,不得记账。
type uncommittedViolationFeeSettler struct {
	actualQuota int
}

func (s *uncommittedViolationFeeSettler) Settle(actualQuota int) error {
	s.actualQuota = actualQuota
	return errors.New("wallet settle failed before funding committed")
}

func (s *uncommittedViolationFeeSettler) Refund(*gin.Context) {}

func (s *uncommittedViolationFeeSettler) NeedsRefund() bool { return false }

func (s *uncommittedViolationFeeSettler) FundingCommitted() bool { return false }

func (s *uncommittedViolationFeeSettler) GetPreConsumedQuota() int { return 0 }

func (s *uncommittedViolationFeeSettler) Reserve(int) error { return nil }

func configureViolationFee(t *testing.T, amount float64) {
	t.Helper()
	settings := model_setting.GetGrokSettings()
	saved := *settings
	settings.ViolationDeductionEnabled = true
	settings.ViolationDeductionAmount = amount
	t.Cleanup(func() { *settings = saved })
}

func violationFeeError() *types.NewAPIError {
	return types.NewErrorWithStatusCode(
		errors.New(CSAMViolationMarker),
		types.ErrorCodeViolationFeeGrokCSAM,
		http.StatusBadRequest,
	)
}

func TestCalcViolationFeeQuotaUsesSaturatingQuotaConversion(t *testing.T) {
	tests := []struct {
		name      string
		amount    float64
		group     float64
		wantQuota int
		wantClamp common.QuotaClampKind
	}{
		{name: "ordinary fee", amount: 0.001, group: 2, wantQuota: 1000},
		{name: "overflow", amount: math.MaxFloat64, group: 2, wantQuota: common.MaxQuota, wantClamp: common.QuotaClampOverflow},
		{name: "nan", amount: math.NaN(), group: 1, wantQuota: 0, wantClamp: common.QuotaClampNaN},
		{name: "non-positive amount", amount: -1, group: 1, wantQuota: 0},
		{name: "non-positive group", amount: 1, group: 0, wantQuota: 0},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			quota, clamp := calcViolationFeeQuota(test.amount, test.group)

			assert.Equal(t, test.wantQuota, quota)
			if test.wantClamp == "" {
				assert.Nil(t, clamp)
				return
			}
			require.NotNil(t, clamp)
			assert.Equal(t, test.wantClamp, clamp.Kind)
		})
	}
}

func TestViolationFeeCreatesSubscriptionSessionForOtherwiseFreeRequest(t *testing.T) {
	truncate(t)
	configureViolationFee(t, 0.001)

	const (
		userID       = 701
		channelID    = 703
		planID       = 704
		subscription = 705
		initialQuota = 5000
		feeQuota     = 500
	)
	seedUser(t, userID, initialQuota)
	seedChannel(t, channelID)
	require.NoError(t, model.DB.Create(&model.SubscriptionPlan{
		Id:               planID,
		Title:            "Violation fee test plan",
		DurationUnit:     "month",
		DurationValue:    1,
		Enabled:          true,
		TotalAmount:      initialQuota,
		QuotaResetPeriod: "never",
	}).Error)
	require.NoError(t, model.DB.Create(&model.UserSubscription{
		Id:          subscription,
		UserId:      userID,
		PlanId:      planID,
		AmountTotal: initialQuota,
		Status:      "active",
		StartTime:   time.Now().Add(-time.Hour).Unix(),
		EndTime:     time.Now().Add(time.Hour).Unix(),
	}).Error)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	info := &relaycommon.RelayInfo{
		RequestId:       "violation-subscription-request",
		UserId:          userID,
		IsPlayground:    true,
		OriginModelName: "free-model",
		UsingGroup:      "default",
		UserSetting: dto.UserSetting{
			BillingPreference: "subscription_only",
		},
		PriceData: types.PriceData{
			FreeModel: true,
			GroupRatioInfo: types.GroupRatioInfo{
				GroupRatio: 1,
			},
		},
		StartTime: time.Now(),
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId: channelID,
		},
	}

	charged := ChargeViolationFeeIfNeeded(c, info, violationFeeError())

	require.True(t, charged)
	require.NotNil(t, info.Billing)
	assert.Equal(t, BillingSourceSubscription, info.BillingSource)
	var user model.User
	require.NoError(t, model.DB.First(&user, userID).Error)
	assert.Equal(t, initialQuota, user.Quota, "subscription-only fee must not debit wallet quota")
	assert.Equal(t, feeQuota, user.UsedQuota)
	var sub model.UserSubscription
	require.NoError(t, model.DB.First(&sub, subscription).Error)
	assert.Equal(t, int64(feeQuota), sub.AmountUsed)
}

func TestViolationFeeRecordsFundingCommittedSettlementError(t *testing.T) {
	truncate(t)
	configureViolationFee(t, 0.001)

	const (
		userID    = 711
		tokenID   = 712
		channelID = 713
		feeQuota  = 500
	)
	seedUser(t, userID, 5000)
	seedToken(t, tokenID, userID, "violation-committed-token", 5000)
	seedChannel(t, channelID)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	settler := &committedViolationFeeSettler{}
	info := &relaycommon.RelayInfo{
		Billing:         settler,
		UserId:          userID,
		TokenId:         tokenID,
		OriginModelName: "paid-model",
		UsingGroup:      "default",
		PriceData: types.PriceData{GroupRatioInfo: types.GroupRatioInfo{
			GroupRatio: 1,
		}},
		StartTime: time.Now(),
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId: channelID,
		},
	}

	charged := ChargeViolationFeeIfNeeded(c, info, violationFeeError())

	require.True(t, charged)
	assert.Equal(t, feeQuota, settler.actualQuota)
	var log model.Log
	require.NoError(t, model.DB.Where("content = ?", "Violation fee charged").First(&log).Error)
	assert.Equal(t, feeQuota, log.Quota)
	var user model.User
	require.NoError(t, model.DB.First(&user, userID).Error)
	assert.Equal(t, feeQuota, user.UsedQuota)
	var channel model.Channel
	require.NoError(t, model.DB.First(&channel, channelID).Error)
	assert.Equal(t, int64(feeQuota), channel.UsedQuota)
}

// 资金来源从未提交(信任旁路会话 Settle 失败)时,违规费收费未发生:
// 不得写消费日志、不得累计用户/渠道用量,并返回 false 让调用方走退款路径。
func TestViolationFeeSkipsLedgerWhenFundingNotCommitted(t *testing.T) {
	truncate(t)
	configureViolationFee(t, 0.001)

	const (
		userID    = 721
		tokenID   = 722
		channelID = 723
		feeQuota  = 500
	)
	seedUser(t, userID, 5000)
	seedToken(t, tokenID, userID, "violation-uncommitted-token", 5000)
	seedChannel(t, channelID)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	settler := &uncommittedViolationFeeSettler{}
	info := &relaycommon.RelayInfo{
		Billing:         settler,
		UserId:          userID,
		TokenId:         tokenID,
		OriginModelName: "paid-model",
		UsingGroup:      "default",
		PriceData: types.PriceData{GroupRatioInfo: types.GroupRatioInfo{
			GroupRatio: 1,
		}},
		StartTime: time.Now(),
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId: channelID,
		},
	}

	charged := ChargeViolationFeeIfNeeded(c, info, violationFeeError())

	require.False(t, charged)
	assert.Equal(t, feeQuota, settler.actualQuota)
	var count int64
	require.NoError(t, model.DB.Model(&model.Log{}).Where("content = ?", "Violation fee charged").Count(&count).Error)
	assert.Zero(t, count)
	var user model.User
	require.NoError(t, model.DB.First(&user, userID).Error)
	assert.Zero(t, user.UsedQuota)
	var channel model.Channel
	require.NoError(t, model.DB.First(&channel, channelID).Error)
	assert.Zero(t, channel.UsedQuota)
}
