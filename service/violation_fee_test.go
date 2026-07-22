package service

import (
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// committedViolationFeeSettler 模拟原预留高于违规费、资金端已成功退还差额，
// 但令牌端退还失败的会话。FundingCommitted 为 true，最终费用已提交，
// 消费账目必须保留。
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

func (s *committedViolationFeeSettler) GetPreConsumedQuota() int { return 600 }

func (s *committedViolationFeeSettler) Reserve(int) error { return nil }

func (s *committedViolationFeeSettler) ReserveStrict(int) error { return nil }

// uncommittedViolationFeeSettler 模拟原预留高于违规费、资金端退还差额失败的
// 会话。FundingCommitted 为 false，整笔原预留仍应走退款，不得记录违规费。
type uncommittedViolationFeeSettler struct {
	actualQuota int
}

func (s *uncommittedViolationFeeSettler) Settle(actualQuota int) error {
	s.actualQuota = actualQuota
	return errors.New("wallet settle failed before funding committed")
}

func (s *uncommittedViolationFeeSettler) Refund(*gin.Context) {}

func (s *uncommittedViolationFeeSettler) NeedsRefund() bool { return true }

func (s *uncommittedViolationFeeSettler) FundingCommitted() bool { return false }

func (s *uncommittedViolationFeeSettler) GetPreConsumedQuota() int { return 600 }

func (s *uncommittedViolationFeeSettler) Reserve(int) error { return nil }

func (s *uncommittedViolationFeeSettler) ReserveStrict(int) error { return nil }

type rejectingViolationFeeSettler struct {
	reserveTarget int
	settleCalls   int
}

func (s *rejectingViolationFeeSettler) Settle(int) error {
	s.settleCalls++
	return nil
}

func (*rejectingViolationFeeSettler) Refund(*gin.Context) {}

func (*rejectingViolationFeeSettler) NeedsRefund() bool { return true }

func (*rejectingViolationFeeSettler) FundingCommitted() bool { return false }

func (*rejectingViolationFeeSettler) GetPreConsumedQuota() int { return 100 }

func (*rejectingViolationFeeSettler) Reserve(int) error { return nil }

func (s *rejectingViolationFeeSettler) ReserveStrict(target int) error {
	s.reserveTarget = target
	return errors.New("complete fee reservation rejected")
}

func configureViolationFee(t *testing.T, amount float64) {
	t.Helper()
	saved := model_setting.GetGrokSettings()
	registered := config.GlobalConfig.Get("grok")
	require.NotNil(t, registered)
	require.NoError(t, config.UpdateConfigFromMap(registered, map[string]string{
		"violation_deduction_enabled": "true",
		"violation_deduction_amount":  strconv.FormatFloat(amount, 'g', -1, 64),
	}))
	t.Cleanup(func() {
		require.NoError(t, config.UpdateConfigFromMap(registered, map[string]string{
			"violation_deduction_enabled": strconv.FormatBool(saved.ViolationDeductionEnabled),
			"violation_deduction_amount":  strconv.FormatFloat(saved.ViolationDeductionAmount, 'g', -1, 64),
		}))
	})
}

func violationFeeError() *types.NewAPIError {
	return types.NewErrorWithStatusCode(
		errors.New(CSAMViolationMarker),
		types.ErrorCodeViolationFeeGrokCSAM,
		http.StatusBadRequest,
	)
}

func newViolationFeeTestContext() *gin.Context {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	return ctx
}

func newWalletViolationRelayInfo(userID, channelID int) *relaycommon.RelayInfo {
	return &relaycommon.RelayInfo{
		RequestId:       "violation-wallet-request",
		UserId:          userID,
		IsPlayground:    true,
		OriginModelName: "paid-model",
		UsingGroup:      "default",
		UserSetting: dto.UserSetting{
			BillingPreference: "wallet_only",
		},
		PriceData: types.PriceData{GroupRatioInfo: types.GroupRatioInfo{
			GroupRatio: 1,
		}},
		StartTime: time.Now(),
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId: channelID,
		},
	}
}

func TestHasCSAMViolationMarkerChecksOpenAIMessageForUsageGuidelineMarker(t *testing.T) {
	apiErr := types.WithOpenAIError(types.OpenAIError{
		Message: ContentViolatesUsageMarker,
		Code:    "content_policy_violation",
	}, http.StatusBadRequest)
	// Preserve an upstream OpenAI message while simulating a wrapper with no
	// independent Go error text. Marker detection must inspect both views.
	apiErr.Err = errors.New("")

	assert.True(t, HasCSAMViolationMarker(apiErr))
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

func TestViolationFeeRequiresCompleteWalletReservation(t *testing.T) {
	tests := []struct {
		name             string
		initialQuota     int
		preConsumedQuota int
		wantCharged      bool
		wantWalletQuota  int
		wantUsedQuota    int
		wantPreConsumed  int
		wantRefund       bool
	}{
		{
			name:             "remaining balance covers fee delta",
			initialQuota:     1000,
			preConsumedQuota: 100,
			wantCharged:      true,
			wantWalletQuota:  500,
			wantUsedQuota:    500,
			wantPreConsumed:  500,
		},
		{
			name:             "remaining balance cannot cover fee delta",
			initialQuota:     400,
			preConsumedQuota: 100,
			wantCharged:      false,
			wantWalletQuota:  300,
			wantPreConsumed:  100,
			wantRefund:       true,
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			truncate(t)
			configureViolationFee(t, 0.001)
			userID := 730 + index*10
			channelID := userID + 1
			seedUser(t, userID, test.initialQuota)
			seedChannel(t, channelID)
			ctx := newViolationFeeTestContext()
			info := newWalletViolationRelayInfo(userID, channelID)
			require.Nil(t, PreConsumeBilling(ctx, test.preConsumedQuota, info))

			charged := ChargeViolationFeeIfNeeded(ctx, info, violationFeeError())

			assert.Equal(t, test.wantCharged, charged)
			require.NotNil(t, info.Billing)
			assert.Equal(t, test.wantPreConsumed, info.Billing.GetPreConsumedQuota())
			assert.Equal(t, test.wantRefund, info.Billing.NeedsRefund())
			var user model.User
			require.NoError(t, model.DB.First(&user, userID).Error)
			assert.Equal(t, test.wantWalletQuota, user.Quota)
			assert.Equal(t, test.wantUsedQuota, user.UsedQuota)
			var logCount int64
			require.NoError(t, model.DB.Model(&model.Log{}).
				Where("content = ?", "Violation fee charged").Count(&logCount).Error)
			if test.wantCharged {
				assert.EqualValues(t, 1, logCount)
			} else {
				assert.Zero(t, logCount)
			}
		})
	}
}

func TestViolationFeeSettlesExistingReservationDownToExactFee(t *testing.T) {
	configureViolationFee(t, 0.001)

	tests := []struct {
		name           string
		userID         int
		tokenID        int
		channelID      int
		subscriptionID int
		preConsume     func(t *testing.T, info *relaycommon.RelayInfo)
		assertFunding  func(t *testing.T)
	}{
		{
			name:      "wallet",
			userID:    820,
			tokenID:   821,
			channelID: 822,
			preConsume: func(t *testing.T, info *relaycommon.RelayInfo) {
				t.Helper()
				require.Nil(t, PreConsumeBilling(newViolationFeeTestContext(), 800, info))
			},
			assertFunding: func(t *testing.T) {
				t.Helper()
				var user model.User
				require.NoError(t, model.DB.First(&user, 820).Error)
				assert.Equal(t, 2500, user.Quota)
				assert.Equal(t, 500, user.UsedQuota)
			},
		},
		{
			name:           "subscription",
			userID:         830,
			tokenID:        831,
			channelID:      832,
			subscriptionID: 834,
			preConsume: func(t *testing.T, info *relaycommon.RelayInfo) {
				t.Helper()
				info.UserSetting.BillingPreference = "subscription_only"
				require.NoError(t, model.DB.Create(&model.SubscriptionPlan{
					Id:               833,
					Title:            "Exact violation settlement plan",
					DurationUnit:     "month",
					DurationValue:    1,
					Enabled:          true,
					TotalAmount:      5000,
					QuotaResetPeriod: "never",
				}).Error)
				require.NoError(t, model.DB.Create(&model.UserSubscription{
					Id:          834,
					UserId:      830,
					PlanId:      833,
					AmountTotal: 5000,
					Status:      "active",
					StartTime:   time.Now().Add(-time.Hour).Unix(),
					EndTime:     time.Now().Add(time.Hour).Unix(),
				}).Error)
				require.Nil(t, PreConsumeBilling(newViolationFeeTestContext(), 800, info))
			},
			assertFunding: func(t *testing.T) {
				t.Helper()
				var subscription model.UserSubscription
				require.NoError(t, model.DB.First(&subscription, 834).Error)
				assert.Equal(t, int64(500), subscription.AmountUsed)
				var user model.User
				require.NoError(t, model.DB.First(&user, 830).Error)
				assert.Equal(t, 3000, user.Quota)
				assert.Equal(t, 500, user.UsedQuota)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			truncate(t)
			seedUser(t, test.userID, 3000)
			tokenKey := "exact-violation-token-" + test.name
			seedToken(t, test.tokenID, test.userID, tokenKey, 2000)
			seedChannel(t, test.channelID)
			info := newWalletViolationRelayInfo(test.userID, test.channelID)
			info.RequestId = "exact-violation-" + test.name
			info.IsPlayground = false
			info.TokenId = test.tokenID
			info.TokenKey = tokenKey
			test.preConsume(t, info)
			require.Equal(t, 800, info.Billing.GetPreConsumedQuota())

			charged := ChargeViolationFeeIfNeeded(newViolationFeeTestContext(), info, violationFeeError())

			require.True(t, charged)
			assert.False(t, info.Billing.NeedsRefund())
			test.assertFunding(t)
			var token model.Token
			require.NoError(t, model.DB.First(&token, test.tokenID).Error)
			assert.Equal(t, 1500, token.RemainQuota)
			assert.Equal(t, 500, token.UsedQuota)
		})
	}
}

func TestViolationFeeFundingRejectionRestoresStrictTokenDelta(t *testing.T) {
	truncate(t)
	configureViolationFee(t, 0.001)

	const (
		userID    = 840
		tokenID   = 841
		channelID = 842
	)
	seedUser(t, userID, 400)
	seedToken(t, tokenID, userID, "strict-delta-rollback-token", 1000)
	seedChannel(t, channelID)
	info := newWalletViolationRelayInfo(userID, channelID)
	info.IsPlayground = false
	info.TokenId = tokenID
	info.TokenKey = "strict-delta-rollback-token"
	require.Nil(t, PreConsumeBilling(newViolationFeeTestContext(), 100, info))

	charged := ChargeViolationFeeIfNeeded(newViolationFeeTestContext(), info, violationFeeError())

	require.False(t, charged)
	assert.True(t, info.Billing.NeedsRefund())
	var user model.User
	require.NoError(t, model.DB.First(&user, userID).Error)
	assert.Equal(t, 300, user.Quota)
	assert.Zero(t, user.UsedQuota)
	var token model.Token
	require.NoError(t, model.DB.First(&token, tokenID).Error)
	assert.Equal(t, 900, token.RemainQuota)
	assert.Equal(t, 100, token.UsedQuota)
}

func TestViolationFeeStrictlyReservesTrustedWalletSession(t *testing.T) {
	truncate(t)
	configureViolationFee(t, 0.001)

	const (
		userID    = 751
		channelID = 752
		feeQuota  = 500
	)
	initialQuota := common.GetTrustQuota() + 1000
	seedUser(t, userID, initialQuota)
	seedChannel(t, channelID)
	ctx := newViolationFeeTestContext()
	info := newWalletViolationRelayInfo(userID, channelID)
	require.Nil(t, PreConsumeBilling(ctx, 100, info))
	require.NotNil(t, info.Billing)
	assert.Zero(t, info.Billing.GetPreConsumedQuota(), "trusted request should initially bypass reservation")

	charged := ChargeViolationFeeIfNeeded(ctx, info, violationFeeError())

	require.True(t, charged)
	assert.Equal(t, feeQuota, info.Billing.GetPreConsumedQuota())
	var user model.User
	require.NoError(t, model.DB.First(&user, userID).Error)
	assert.Equal(t, initialQuota-feeQuota, user.Quota)
	assert.Equal(t, feeQuota, user.UsedQuota)
}

func TestViolationFeeTrustedWalletUsesAuthoritativeReservationWithBatchUpdates(t *testing.T) {
	truncate(t)
	configureViolationFee(t, 0.001)

	previousBatchUpdateEnabled := common.BatchUpdateEnabled
	common.BatchUpdateEnabled = true
	t.Cleanup(func() { common.BatchUpdateEnabled = previousBatchUpdateEnabled })

	const (
		userID    = 761
		channelID = 762
	)
	initialQuota := common.GetTrustQuota() + 1000
	seedUser(t, userID, initialQuota)
	seedChannel(t, channelID)
	ctx := newViolationFeeTestContext()
	info := newWalletViolationRelayInfo(userID, channelID)
	require.Nil(t, PreConsumeBilling(ctx, 100, info))
	assert.Zero(t, info.Billing.GetPreConsumedQuota())

	charged := ChargeViolationFeeIfNeeded(ctx, info, violationFeeError())

	require.True(t, charged)
	assert.False(t, info.Billing.NeedsRefund())
	var user model.User
	require.NoError(t, model.DB.First(&user, userID).Error)
	assert.Equal(t, initialQuota-500, user.Quota)
	assert.Zero(t, user.UsedQuota, "usage statistics remain queued while batch updates are enabled")
	var logCount int64
	require.NoError(t, model.DB.Model(&model.Log{}).
		Where("content = ?", "Violation fee charged").Count(&logCount).Error)
	assert.Equal(t, int64(1), logCount)
}

func TestViolationFeeFreeWalletUsesAuthoritativeReservationWithBatchUpdates(t *testing.T) {
	truncate(t)
	configureViolationFee(t, 0.001)

	previousBatchUpdateEnabled := common.BatchUpdateEnabled
	common.BatchUpdateEnabled = true
	t.Cleanup(func() { common.BatchUpdateEnabled = previousBatchUpdateEnabled })

	const (
		userID    = 765
		channelID = 766
	)
	seedUser(t, userID, 5000)
	seedChannel(t, channelID)
	ctx := newViolationFeeTestContext()
	info := newWalletViolationRelayInfo(userID, channelID)
	info.Billing = nil

	charged := ChargeViolationFeeIfNeeded(ctx, info, violationFeeError())

	require.True(t, charged)
	require.NotNil(t, info.Billing)
	var user model.User
	require.NoError(t, model.DB.First(&user, userID).Error)
	assert.Equal(t, 4500, user.Quota)
	assert.Zero(t, user.UsedQuota, "usage statistics remain queued while batch updates are enabled")
	var logCount int64
	require.NoError(t, model.DB.Model(&model.Log{}).
		Where("content = ?", "Violation fee charged").Count(&logCount).Error)
	assert.Equal(t, int64(1), logCount)
}

func TestViolationFeeFreeRequestRejectsInsufficientFunding(t *testing.T) {
	t.Run("wallet", func(t *testing.T) {
		truncate(t)
		configureViolationFee(t, 0.001)

		const (
			userID    = 763
			channelID = 764
		)
		seedUser(t, userID, 400)
		seedChannel(t, channelID)
		ctx := newViolationFeeTestContext()
		info := newWalletViolationRelayInfo(userID, channelID)

		charged := ChargeViolationFeeIfNeeded(ctx, info, violationFeeError())

		require.False(t, charged)
		assert.Nil(t, info.Billing)
		var user model.User
		require.NoError(t, model.DB.First(&user, userID).Error)
		assert.Equal(t, 400, user.Quota)
		assert.Zero(t, user.UsedQuota)
	})

	t.Run("subscription", func(t *testing.T) {
		truncate(t)
		configureViolationFee(t, 0.001)

		const (
			userID         = 775
			channelID      = 776
			planID         = 777
			subscriptionID = 778
		)
		seedUser(t, userID, 5000)
		seedChannel(t, channelID)
		require.NoError(t, model.DB.Create(&model.SubscriptionPlan{
			Id:               planID,
			Title:            "Insufficient free violation fee plan",
			DurationUnit:     "month",
			DurationValue:    1,
			Enabled:          true,
			TotalAmount:      200,
			QuotaResetPeriod: "never",
		}).Error)
		require.NoError(t, model.DB.Create(&model.UserSubscription{
			Id:          subscriptionID,
			UserId:      userID,
			PlanId:      planID,
			AmountTotal: 200,
			Status:      "active",
			StartTime:   time.Now().Add(-time.Hour).Unix(),
			EndTime:     time.Now().Add(time.Hour).Unix(),
		}).Error)
		ctx := newViolationFeeTestContext()
		info := newWalletViolationRelayInfo(userID, channelID)
		info.UserSetting.BillingPreference = "subscription_only"

		charged := ChargeViolationFeeIfNeeded(ctx, info, violationFeeError())

		require.False(t, charged)
		assert.Nil(t, info.Billing)
		var subscription model.UserSubscription
		require.NoError(t, model.DB.First(&subscription, subscriptionID).Error)
		assert.Zero(t, subscription.AmountUsed)
		var user model.User
		require.NoError(t, model.DB.First(&user, userID).Error)
		assert.Equal(t, 5000, user.Quota)
		assert.Zero(t, user.UsedQuota)
	})
}

func TestStrictViolationPreConsumeAllowsSubscriptionWithBatchUpdates(t *testing.T) {
	truncate(t)

	previousBatchUpdateEnabled := common.BatchUpdateEnabled
	common.BatchUpdateEnabled = true
	t.Cleanup(func() { common.BatchUpdateEnabled = previousBatchUpdateEnabled })

	const (
		userID         = 795
		channelID      = 796
		planID         = 797
		subscriptionID = 798
		feeQuota       = 500
	)
	seedUser(t, userID, 5000)
	seedChannel(t, channelID)
	require.NoError(t, model.DB.Create(&model.SubscriptionPlan{
		Id:               planID,
		Title:            "Batch-safe violation fee plan",
		DurationUnit:     "month",
		DurationValue:    1,
		Enabled:          true,
		TotalAmount:      1000,
		QuotaResetPeriod: "never",
	}).Error)
	require.NoError(t, model.DB.Create(&model.UserSubscription{
		Id:          subscriptionID,
		UserId:      userID,
		PlanId:      planID,
		AmountTotal: 1000,
		Status:      "active",
		StartTime:   time.Now().Add(-time.Hour).Unix(),
		EndTime:     time.Now().Add(time.Hour).Unix(),
	}).Error)
	ctx := newViolationFeeTestContext()
	info := newWalletViolationRelayInfo(userID, channelID)
	info.RequestId = "batch-subscription-strict-preconsume"
	info.UserSetting.BillingPreference = "subscription_only"

	apiErr := PreConsumeBillingStrict(ctx, feeQuota, info)

	require.Nil(t, apiErr)
	require.NotNil(t, info.Billing)
	assert.Equal(t, BillingSourceSubscription, info.BillingSource)
	assert.Equal(t, feeQuota, info.Billing.GetPreConsumedQuota())
	var subscription model.UserSubscription
	require.NoError(t, model.DB.First(&subscription, subscriptionID).Error)
	assert.Equal(t, int64(feeQuota), subscription.AmountUsed)
	require.NoError(t, info.Billing.Settle(feeQuota))
}

func TestStrictViolationPreConsumeReservesLimitedTokenOutsideBatchLedger(t *testing.T) {
	truncate(t)

	previousBatchUpdateEnabled := common.BatchUpdateEnabled
	common.BatchUpdateEnabled = true
	t.Cleanup(func() { common.BatchUpdateEnabled = previousBatchUpdateEnabled })

	const (
		userID         = 805
		tokenID        = 806
		channelID      = 807
		planID         = 808
		subscriptionID = 809
		feeQuota       = 500
	)
	seedUser(t, userID, 5000)
	seedToken(t, tokenID, userID, "batch-limited-token", 600)
	seedChannel(t, channelID)
	require.NoError(t, model.DB.Create(&model.SubscriptionPlan{
		Id:               planID,
		Title:            "Batch token strict reservation plan",
		DurationUnit:     "month",
		DurationValue:    1,
		Enabled:          true,
		TotalAmount:      1000,
		QuotaResetPeriod: "never",
	}).Error)
	require.NoError(t, model.DB.Create(&model.UserSubscription{
		Id:          subscriptionID,
		UserId:      userID,
		PlanId:      planID,
		AmountTotal: 1000,
		Status:      "active",
		StartTime:   time.Now().Add(-time.Hour).Unix(),
		EndTime:     time.Now().Add(time.Hour).Unix(),
	}).Error)
	ctx := newViolationFeeTestContext()
	info := newWalletViolationRelayInfo(userID, channelID)
	info.RequestId = "batch-limited-token-strict-preconsume"
	info.UserSetting.BillingPreference = "subscription_only"
	info.IsPlayground = false
	info.TokenId = tokenID
	info.TokenKey = "batch-limited-token"

	apiErr := PreConsumeBillingStrict(ctx, feeQuota, info)

	require.Nil(t, apiErr)
	require.NotNil(t, info.Billing)
	assert.Equal(t, BillingSourceSubscription, info.BillingSource)
	assert.Equal(t, feeQuota, info.Billing.GetPreConsumedQuota())
	var subscription model.UserSubscription
	require.NoError(t, model.DB.First(&subscription, subscriptionID).Error)
	assert.Equal(t, int64(feeQuota), subscription.AmountUsed)
	var token model.Token
	require.NoError(t, model.DB.First(&token, tokenID).Error)
	assert.Equal(t, 100, token.RemainQuota)
	assert.Equal(t, feeQuota, token.UsedQuota)
	require.NoError(t, info.Billing.Settle(feeQuota))
}

func TestViolationFeeStrictPreConsumeFallsBackFromSubscriptionToWallet(t *testing.T) {
	truncate(t)
	configureViolationFee(t, 0.001)

	const (
		userID         = 767
		channelID      = 768
		planID         = 769
		subscriptionID = 770
		feeQuota       = 500
	)
	seedUser(t, userID, 5000)
	seedChannel(t, channelID)
	require.NoError(t, model.DB.Create(&model.SubscriptionPlan{
		Id:                  planID,
		Title:               "Wallet fallback violation fee plan",
		DurationUnit:        "month",
		DurationValue:       1,
		Enabled:             true,
		TotalAmount:         100,
		QuotaResetPeriod:    "never",
		AllowWalletOverflow: common.GetPointer(true),
	}).Error)
	require.NoError(t, model.DB.Create(&model.UserSubscription{
		Id:                  subscriptionID,
		UserId:              userID,
		PlanId:              planID,
		AmountTotal:         100,
		Status:              "active",
		StartTime:           time.Now().Add(-time.Hour).Unix(),
		EndTime:             time.Now().Add(time.Hour).Unix(),
		AllowWalletOverflow: true,
	}).Error)
	ctx := newViolationFeeTestContext()
	info := newWalletViolationRelayInfo(userID, channelID)
	info.UserSetting.BillingPreference = "subscription_first"
	info.Billing = nil

	charged := ChargeViolationFeeIfNeeded(ctx, info, violationFeeError())

	require.True(t, charged)
	require.NotNil(t, info.Billing)
	assert.Equal(t, BillingSourceWallet, info.BillingSource)
	var user model.User
	require.NoError(t, model.DB.First(&user, userID).Error)
	assert.Equal(t, 5000-feeQuota, user.Quota)
	assert.Equal(t, feeQuota, user.UsedQuota)
	var subscription model.UserSubscription
	require.NoError(t, model.DB.First(&subscription, subscriptionID).Error)
	assert.Zero(t, subscription.AmountUsed)
}

func TestViolationFeeRequiresCompleteSubscriptionReservation(t *testing.T) {
	truncate(t)
	configureViolationFee(t, 0.001)

	const (
		userID         = 771
		channelID      = 772
		planID         = 773
		subscriptionID = 774
	)
	seedUser(t, userID, 5000)
	seedChannel(t, channelID)
	require.NoError(t, model.DB.Create(&model.SubscriptionPlan{
		Id:               planID,
		Title:            "Limited violation fee plan",
		DurationUnit:     "month",
		DurationValue:    1,
		Enabled:          true,
		TotalAmount:      200,
		QuotaResetPeriod: "never",
	}).Error)
	require.NoError(t, model.DB.Create(&model.UserSubscription{
		Id:          subscriptionID,
		UserId:      userID,
		PlanId:      planID,
		AmountTotal: 200,
		Status:      "active",
		StartTime:   time.Now().Add(-time.Hour).Unix(),
		EndTime:     time.Now().Add(time.Hour).Unix(),
	}).Error)
	ctx := newViolationFeeTestContext()
	info := &relaycommon.RelayInfo{
		RequestId:       "limited-subscription-violation",
		UserId:          userID,
		IsPlayground:    true,
		OriginModelName: "paid-model",
		UsingGroup:      "default",
		UserSetting: dto.UserSetting{
			BillingPreference: "subscription_only",
		},
		PriceData: types.PriceData{GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}},
		StartTime: time.Now(),
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId: channelID,
		},
	}
	require.Nil(t, PreConsumeBilling(ctx, 100, info))

	charged := ChargeViolationFeeIfNeeded(ctx, info, violationFeeError())

	require.False(t, charged)
	assert.True(t, info.Billing.NeedsRefund())
	assert.Equal(t, 100, info.Billing.GetPreConsumedQuota())
	var subscription model.UserSubscription
	require.NoError(t, model.DB.First(&subscription, subscriptionID).Error)
	assert.Equal(t, int64(100), subscription.AmountUsed)
	var user model.User
	require.NoError(t, model.DB.First(&user, userID).Error)
	assert.Zero(t, user.UsedQuota)
	var logCount int64
	require.NoError(t, model.DB.Model(&model.Log{}).
		Where("content = ?", "Violation fee charged").Count(&logCount).Error)
	assert.Zero(t, logCount)
}

func TestViolationFeeRejectsSaturatedFeeOnEverySessionPath(t *testing.T) {
	configureViolationFee(t, math.MaxFloat64)

	t.Run("rejects before billing mutation", func(t *testing.T) {
		truncate(t)
		const (
			userID    = 799
			channelID = 800
		)
		seedUser(t, userID, 5000)
		seedChannel(t, channelID)
		settler := &rejectingViolationFeeSettler{}
		info := newWalletViolationRelayInfo(userID, channelID)
		info.Billing = settler

		charged := ChargeViolationFeeIfNeeded(newViolationFeeTestContext(), info, violationFeeError())

		require.False(t, charged)
		assert.Zero(t, settler.reserveTarget)
		assert.Zero(t, settler.settleCalls)
	})

	tests := []struct {
		name            string
		createSession   bool
		wantWalletQuota int
	}{
		{name: "existing paid reservation", createSession: true, wantWalletQuota: 4900},
		{name: "otherwise free request", wantWalletQuota: 5000},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			truncate(t)
			userID := 780 + index*10
			channelID := userID + 1
			seedUser(t, userID, 5000)
			seedChannel(t, channelID)
			ctx := newViolationFeeTestContext()
			info := newWalletViolationRelayInfo(userID, channelID)
			if test.createSession {
				require.Nil(t, PreConsumeBilling(ctx, 100, info))
			}

			charged := ChargeViolationFeeIfNeeded(ctx, info, violationFeeError())

			require.False(t, charged)
			require.NotNil(t, info.QuotaClamp)
			assert.Equal(t, common.QuotaClampOverflow, info.QuotaClamp.Kind)
			var user model.User
			require.NoError(t, model.DB.First(&user, userID).Error)
			assert.Equal(t, test.wantWalletQuota, user.Quota)
			assert.Zero(t, user.UsedQuota)
			var logCount int64
			require.NoError(t, model.DB.Model(&model.Log{}).
				Where("content = ?", "Violation fee charged").Count(&logCount).Error)
			assert.Zero(t, logCount)
		})
	}
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

func TestViolationFeeSkipsSettlementAndLedgerWhenStrictReservationFails(t *testing.T) {
	truncate(t)
	configureViolationFee(t, 0.001)

	const (
		userID    = 715
		channelID = 716
		feeQuota  = 500
	)
	seedUser(t, userID, 5000)
	seedChannel(t, channelID)
	ctx := newViolationFeeTestContext()
	settler := &rejectingViolationFeeSettler{}
	info := newWalletViolationRelayInfo(userID, channelID)
	info.Billing = settler

	charged := ChargeViolationFeeIfNeeded(ctx, info, violationFeeError())

	require.False(t, charged)
	assert.Equal(t, feeQuota, settler.reserveTarget)
	assert.Zero(t, settler.settleCalls)
	var user model.User
	require.NoError(t, model.DB.First(&user, userID).Error)
	assert.Zero(t, user.UsedQuota)
	var channel model.Channel
	require.NoError(t, model.DB.First(&channel, channelID).Error)
	assert.Zero(t, channel.UsedQuota)
	var logCount int64
	require.NoError(t, model.DB.Model(&model.Log{}).
		Where("content = ?", "Violation fee charged").Count(&logCount).Error)
	assert.Zero(t, logCount)
}

// 资金来源未提交时，违规费收费未发生：
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
