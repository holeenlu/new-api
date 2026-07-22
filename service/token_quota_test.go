package service

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPreConsumeTokenQuotaRejectsStaleCacheAboveDatabaseBalance(t *testing.T) {
	truncate(t)

	previousRedisEnabled := common.RedisEnabled
	previousBatchUpdateEnabled := common.BatchUpdateEnabled
	previousRDB := common.RDB
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	common.RedisEnabled = true
	common.BatchUpdateEnabled = false
	common.RDB = client
	t.Cleanup(func() {
		_ = client.Close()
		common.RedisEnabled = previousRedisEnabled
		common.BatchUpdateEnabled = previousBatchUpdateEnabled
		common.RDB = previousRDB
	})

	const (
		userID   = 901
		tokenID  = 902
		tokenKey = "stale-cache-limited-token"
	)
	seedToken(t, tokenID, userID, tokenKey, 100)
	cachedToken := model.Token{
		Id:             tokenID,
		UserId:         userID,
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		RemainQuota:    1000,
		UnlimitedQuota: false,
	}
	cacheKey := "token:" + common.GenerateHMAC(tokenKey)
	require.NoError(t, common.RedisHSetObj(cacheKey, &cachedToken, time.Minute))
	cachedQuota, err := common.RDB.HGet(t.Context(), cacheKey, "RemainQuota").Result()
	require.NoError(t, err)
	require.Equal(t, "1000", cachedQuota, "the regression requires a stale cache balance above the database balance")

	err = PreConsumeTokenQuota(&relaycommon.RelayInfo{
		TokenId:  tokenID,
		TokenKey: tokenKey,
	}, 500)

	require.ErrorIs(t, err, model.ErrInsufficientTokenQuota)
	var stored model.Token
	require.NoError(t, model.DB.First(&stored, tokenID).Error)
	assert.Equal(t, 100, stored.RemainQuota)
	assert.Zero(t, stored.UsedQuota)
}

func TestBillingSessionTrustBypassStillReservesTokenQuota(t *testing.T) {
	truncate(t)

	const (
		userID   = 906
		tokenID  = 907
		tokenKey = "finite-token-trust-boundary"
		quota    = 100
	)
	initialQuota := common.GetTrustQuota() + 1000
	seedUser(t, userID, initialQuota)
	seedToken(t, tokenID, userID, tokenKey, initialQuota)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{
		RequestId: "finite-token-trust-boundary-request",
		UserId:    userID,
		UserQuota: initialQuota,
		TokenId:   tokenID,
		TokenKey:  tokenKey,
		UserSetting: dto.UserSetting{
			BillingPreference: "wallet_only",
		},
	}

	require.Nil(t, PreConsumeBilling(ctx, quota, info))
	session, ok := info.Billing.(*BillingSession)
	require.True(t, ok)
	assert.Zero(t, session.GetPreConsumedQuota())

	var user model.User
	require.NoError(t, model.DB.First(&user, userID).Error)
	assert.Equal(t, initialQuota, user.Quota)
	var token model.Token
	require.NoError(t, model.DB.First(&token, tokenID).Error)
	assert.Equal(t, initialQuota-quota, token.RemainQuota)
	assert.Equal(t, quota, token.UsedQuota)

	require.NoError(t, session.Settle(quota))
	require.NoError(t, model.DB.First(&user, userID).Error)
	assert.Equal(t, initialQuota-quota, user.Quota)
	require.NoError(t, model.DB.First(&token, tokenID).Error)
	assert.Equal(t, initialQuota-quota, token.RemainQuota)
	assert.Equal(t, quota, token.UsedQuota)
}

func TestBillingSessionReserveStrictSucceedsForLimitedToken(t *testing.T) {
	truncate(t)

	const (
		userID   = 911
		tokenID  = 912
		tokenKey = "strict-success-limited-token"
	)
	seedUser(t, userID, 1000)
	seedToken(t, tokenID, userID, tokenKey, 1000)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{
		RequestId:       "strict-success-request",
		UserId:          userID,
		TokenId:         tokenID,
		TokenKey:        tokenKey,
		IsPlayground:    false,
		ForcePreConsume: true,
		UserSetting: dto.UserSetting{
			BillingPreference: "wallet_only",
		},
	}

	require.Nil(t, PreConsumeBilling(ctx, 100, info))
	require.NoError(t, info.Billing.ReserveStrict(500))
	assert.Equal(t, 500, info.Billing.GetPreConsumedQuota())

	var user model.User
	require.NoError(t, model.DB.First(&user, userID).Error)
	assert.Equal(t, 500, user.Quota)
	var token model.Token
	require.NoError(t, model.DB.First(&token, tokenID).Error)
	assert.Equal(t, 500, token.RemainQuota)
	assert.Equal(t, 500, token.UsedQuota)

	require.NoError(t, info.Billing.Settle(500))
	assert.True(t, info.Billing.FundingCommitted())
	assert.False(t, info.Billing.NeedsRefund())
	info.Billing.Refund(ctx)
	require.NoError(t, model.DB.First(&user, userID).Error)
	assert.Equal(t, 500, user.Quota)
	require.NoError(t, model.DB.First(&token, tokenID).Error)
	assert.Equal(t, 500, token.RemainQuota)
	assert.Equal(t, 500, token.UsedQuota)
}

func TestBillingSessionPositiveSettlementRecordsCommittedTokenDebtInBatchMode(t *testing.T) {
	truncate(t)
	previousBatchUpdateEnabled := common.BatchUpdateEnabled
	common.BatchUpdateEnabled = true
	t.Cleanup(func() { common.BatchUpdateEnabled = previousBatchUpdateEnabled })

	const (
		tokenID          = 913
		preConsumedQuota = 400
		actualQuota      = 600
	)
	token := model.Token{
		Id:          tokenID,
		UserId:      1,
		Key:         "batch-positive-settlement-token",
		Status:      common.TokenStatusEnabled,
		ExpiredTime: -1,
		RemainQuota: 100,
		UsedQuota:   preConsumedQuota,
	}
	require.NoError(t, model.DB.Create(&token).Error)
	funding := &recordingFundingSource{}
	session := &BillingSession{
		relayInfo: &relaycommon.RelayInfo{
			TokenId:  tokenID,
			TokenKey: token.Key,
		},
		funding:          funding,
		preConsumedQuota: preConsumedQuota,
		tokenConsumed:    preConsumedQuota,
	}

	require.NoError(t, session.Settle(actualQuota))

	assert.Equal(t, 1, funding.settleCalls)
	assert.True(t, session.FundingCommitted())
	assert.False(t, session.NeedsRefund())
	var stored model.Token
	require.NoError(t, model.DB.First(&stored, tokenID).Error)
	assert.Equal(t, -100, stored.RemainQuota)
	assert.Equal(t, actualQuota, stored.UsedQuota)
}

func TestBillingSessionTrustedReserveFullyReservesFinalOutboundTarget(t *testing.T) {
	truncate(t)

	const (
		userID   = 921
		tokenID  = 922
		tokenKey = "trusted-reserve-token"
	)
	initialQuota := common.GetTrustQuota() + 1000
	seedUser(t, userID, initialQuota)
	seedToken(t, tokenID, userID, tokenKey, initialQuota)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{
		RequestId: "trusted-reserve-request",
		UserId:    userID,
		UserQuota: initialQuota,
		TokenId:   tokenID,
		TokenKey:  tokenKey,
		UserSetting: dto.UserSetting{
			BillingPreference: "wallet_only",
		},
	}

	require.Nil(t, PreConsumeBilling(ctx, 100, info))
	require.NoError(t, info.Billing.Reserve(300))
	assert.Equal(t, 300, info.Billing.GetPreConsumedQuota())

	var user model.User
	require.NoError(t, model.DB.First(&user, userID).Error)
	assert.Equal(t, initialQuota-300, user.Quota)
	var token model.Token
	require.NoError(t, model.DB.First(&token, tokenID).Error)
	assert.Equal(t, initialQuota-300, token.RemainQuota)
	assert.Equal(t, 300, token.UsedQuota)

	require.NoError(t, info.Billing.Settle(300))
	require.NoError(t, model.DB.First(&user, userID).Error)
	assert.Equal(t, initialQuota-300, user.Quota)
	require.NoError(t, model.DB.First(&token, tokenID).Error)
	assert.Equal(t, initialQuota-300, token.RemainQuota, "settlement must not debit either reservation twice")
	assert.Equal(t, 300, token.UsedQuota)
}

func TestBillingSessionFinalOutboundReserveUsesAuthoritativeWalletWithBatching(t *testing.T) {
	truncate(t)
	previousBatchUpdateEnabled := common.BatchUpdateEnabled
	common.BatchUpdateEnabled = false
	t.Cleanup(func() { common.BatchUpdateEnabled = previousBatchUpdateEnabled })

	const (
		userID   = 931
		tokenID  = 932
		tokenKey = "batched-final-outbound-reserve"
	)
	initialQuota := common.GetTrustQuota() + 1000
	seedUser(t, userID, initialQuota)
	seedToken(t, tokenID, userID, tokenKey, initialQuota)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
	info := &relaycommon.RelayInfo{
		RequestId: "batched-final-outbound-reserve-request",
		UserId:    userID,
		UserQuota: initialQuota,
		TokenId:   tokenID,
		TokenKey:  tokenKey,
		UserSetting: dto.UserSetting{
			BillingPreference: "wallet_only",
		},
	}
	require.Nil(t, PreConsumeBilling(ctx, 100, info))
	common.BatchUpdateEnabled = true

	err := info.Billing.Reserve(300)

	require.NoError(t, err)
	var user model.User
	require.NoError(t, model.DB.First(&user, userID).Error)
	assert.Equal(t, initialQuota-300, user.Quota)
	var token model.Token
	require.NoError(t, model.DB.First(&token, tokenID).Error)
	assert.Equal(t, initialQuota-300, token.RemainQuota)
	assert.Equal(t, 300, token.UsedQuota)
}

func TestBillingSessionSettleRestoresIndependentTokenOverReservation(t *testing.T) {
	truncate(t)

	const (
		tokenID  = 923
		tokenKey = "independent-token-over-reservation"
	)
	token := model.Token{
		Id:          tokenID,
		UserId:      1,
		Key:         tokenKey,
		Status:      common.TokenStatusEnabled,
		ExpiredTime: -1,
		RemainQuota: 800,
		UsedQuota:   200,
	}
	require.NoError(t, model.DB.Create(&token).Error)
	funding := &recordingFundingSource{}
	session := &BillingSession{
		relayInfo:        &relaycommon.RelayInfo{TokenId: tokenID, TokenKey: tokenKey},
		funding:          funding,
		preConsumedQuota: 100,
		tokenConsumed:    200,
	}

	require.NoError(t, session.Settle(100))

	assert.Equal(t, 1, funding.settleCalls)
	assert.True(t, session.FundingCommitted())
	assert.False(t, session.NeedsRefund())
	var stored model.Token
	require.NoError(t, model.DB.First(&stored, tokenID).Error)
	assert.Equal(t, 900, stored.RemainQuota)
	assert.Equal(t, 100, stored.UsedQuota)
}

func TestBillingSessionTokenAdjustmentFailureStillCommitsExactFundingReservation(t *testing.T) {
	funding := &recordingFundingSource{}
	session := &BillingSession{
		relayInfo:        &relaycommon.RelayInfo{TokenId: 924, TokenKey: "deleted-token"},
		funding:          funding,
		preConsumedQuota: 100,
		tokenConsumed:    200,
	}

	err := session.Settle(100)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "quota restore updated 0 rows")
	assert.Equal(t, 1, funding.settleCalls)
	assert.True(t, session.FundingCommitted())
	assert.False(t, session.NeedsRefund(), "committed funding must never be refunded after token reconciliation fails")
	repeatedErr := session.Settle(100)
	require.Error(t, repeatedErr, "a persistent token reconciliation error must not become success on a repeated settle")
	assert.Equal(t, err.Error(), repeatedErr.Error())
	assert.Equal(t, 1, funding.settleCalls)
}

func TestBillingSessionReserveReconcilesFundingAndTokenBaselinesIndependently(t *testing.T) {
	tests := []struct {
		name               string
		userID             int
		fundingReserved    int
		tokenReserved      int
		wantUserQuota      int
		wantTokenRemaining int
	}{
		{
			name:               "funding already covers target",
			userID:             925,
			fundingReserved:    500,
			tokenReserved:      100,
			wantUserQuota:      500,
			wantTokenRemaining: 500,
		},
		{
			name:               "token already covers target",
			userID:             927,
			fundingReserved:    100,
			tokenReserved:      500,
			wantUserQuota:      500,
			wantTokenRemaining: 500,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			truncate(t)
			tokenID := test.userID + 1
			tokenKey := "independent-reserve-" + test.name
			seedUser(t, test.userID, 1000-test.fundingReserved)
			token := model.Token{
				Id:          tokenID,
				UserId:      test.userID,
				Key:         tokenKey,
				Status:      common.TokenStatusEnabled,
				ExpiredTime: -1,
				RemainQuota: 1000 - test.tokenReserved,
				UsedQuota:   test.tokenReserved,
			}
			require.NoError(t, model.DB.Create(&token).Error)
			session := &BillingSession{
				relayInfo: &relaycommon.RelayInfo{
					UserId:   test.userID,
					TokenId:  tokenID,
					TokenKey: tokenKey,
				},
				funding:          &WalletFunding{userId: test.userID, consumed: test.fundingReserved},
				preConsumedQuota: test.fundingReserved,
				tokenConsumed:    test.tokenReserved,
			}

			require.NoError(t, session.Reserve(500))

			var user model.User
			require.NoError(t, model.DB.First(&user, test.userID).Error)
			assert.Equal(t, test.wantUserQuota, user.Quota)
			var stored model.Token
			require.NoError(t, model.DB.First(&stored, tokenID).Error)
			assert.Equal(t, test.wantTokenRemaining, stored.RemainQuota)
			assert.Equal(t, 500, stored.UsedQuota)
		})
	}
}

func TestBillingSessionRefundRestoresDifferentFundingAndTokenReservations(t *testing.T) {
	truncate(t)

	const (
		userID   = 929
		tokenID  = 930
		tokenKey = "different-refund-baselines"
	)
	seedUser(t, userID, 800)
	token := model.Token{
		Id:          tokenID,
		UserId:      userID,
		Key:         tokenKey,
		Status:      common.TokenStatusEnabled,
		ExpiredTime: -1,
		RemainQuota: 700,
		UsedQuota:   300,
	}
	require.NoError(t, model.DB.Create(&token).Error)
	session := &BillingSession{
		relayInfo:        &relaycommon.RelayInfo{UserId: userID, TokenId: tokenID, TokenKey: tokenKey},
		funding:          &WalletFunding{userId: userID, consumed: 200},
		preConsumedQuota: 200,
		tokenConsumed:    300,
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	session.Refund(ctx)

	require.Eventually(t, func() bool {
		var user model.User
		if err := model.DB.First(&user, userID).Error; err != nil || user.Quota != 1000 {
			return false
		}
		var stored model.Token
		return model.DB.First(&stored, tokenID).Error == nil &&
			stored.RemainQuota == 1000 && stored.UsedQuota == 0
	}, time.Second, time.Millisecond)
}
