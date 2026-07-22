package model

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReserveTokenQuotaCommitsAtMostOneCompetingLimitedReservation(t *testing.T) {
	setupUserUpdateTestState(t)
	common.BatchUpdateEnabled = true

	token := Token{Id: 91, UserId: 1, Key: "strict-token", RemainQuota: 1000}
	require.NoError(t, DB.Create(&token).Error)

	errs := make([]error, 2)
	var wg sync.WaitGroup
	for index := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[index] = ReserveTokenQuota(token.Id, token.Key, 700)
		}()
	}
	wg.Wait()

	successes := 0
	insufficient := 0
	for _, err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrInsufficientTokenQuota):
			insufficient++
		default:
			require.NoError(t, err)
		}
	}
	assert.Equal(t, 1, successes)
	assert.Equal(t, 1, insufficient)

	var stored Token
	require.NoError(t, DB.First(&stored, token.Id).Error)
	assert.Equal(t, 300, stored.RemainQuota)
	assert.Equal(t, 700, stored.UsedQuota)
}

func TestReserveTokenQuotaInvalidatesStaleCacheWithBatchUpdates(t *testing.T) {
	setupUserUpdateTestState(t)
	server := useUserCacheMiniRedis(t)

	token := Token{
		Id:             92,
		UserId:         1,
		Key:            "batch-database-token",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		RemainQuota:    1000,
		UnlimitedQuota: false,
	}
	require.NoError(t, DB.Create(&token).Error)
	require.NoError(t, cacheSetToken(token))
	common.BatchUpdateEnabled = true

	err := ReserveTokenQuota(token.Id, token.Key, 700)

	require.NoError(t, err)
	var stored Token
	require.NoError(t, DB.First(&stored, token.Id).Error)
	assert.Equal(t, 300, stored.RemainQuota)
	assert.Equal(t, 700, stored.UsedQuota)
	assert.False(t, server.Exists("token:"+common.GenerateHMAC(token.Key)))
}

func TestDatabaseBackedTokenReservationRollbackKeepsCacheComplete(t *testing.T) {
	setupUserUpdateTestState(t)
	useUserCacheMiniRedis(t)

	token := Token{
		Id:             93,
		UserId:         1,
		Key:            "strict-rollback-token",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		RemainQuota:    1000,
		UnlimitedQuota: false,
	}
	require.NoError(t, DB.Create(&token).Error)
	require.NoError(t, cacheSetToken(token))
	require.NoError(t, ReserveTokenQuota(token.Id, token.Key, 700))
	require.NoError(t, RestoreTokenQuota(token.Id, token.Key, 700))

	_, err := cacheGetTokenByKey(token.Key)
	require.Error(t, err, "strict rollback must leave no quota-only cache entry")
	common.RedisEnabled = false
	validated, err := ValidateUserToken(token.Key)
	require.NoError(t, err)
	require.NotNil(t, validated)
	assert.Equal(t, token.Id, validated.Id)
	assert.Equal(t, 1000, validated.RemainQuota)
	assert.Zero(t, validated.UsedQuota)
}

func TestReserveTokenQuotaAllowsPersistedUnlimitedTokenWithZeroBalance(t *testing.T) {
	setupUserUpdateTestState(t)

	token := Token{
		Id:             96,
		UserId:         1,
		Key:            "persisted-unlimited-token",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		RemainQuota:    0,
		UnlimitedQuota: true,
	}
	require.NoError(t, DB.Create(&token).Error)

	require.NoError(t, ReserveTokenQuota(token.Id, token.Key, 700))

	var stored Token
	require.NoError(t, DB.First(&stored, token.Id).Error)
	assert.Equal(t, -700, stored.RemainQuota)
	assert.Equal(t, 700, stored.UsedQuota)
}

func TestConsumeTokenQuotaRecordsCommittedDebtBelowZero(t *testing.T) {
	setupUserUpdateTestState(t)

	token := Token{
		Id:          97,
		UserId:      1,
		Key:         "committed-token-debt",
		RemainQuota: 100,
		UsedQuota:   400,
	}
	require.NoError(t, DB.Create(&token).Error)

	require.NoError(t, ConsumeTokenQuota(token.Id, token.Key, 200))

	var stored Token
	require.NoError(t, DB.First(&stored, token.Id).Error)
	assert.Equal(t, -100, stored.RemainQuota)
	assert.Equal(t, 600, stored.UsedQuota)
}

func TestGetTokenByKeyReplacesPartialTokenCacheWithDatabaseRecord(t *testing.T) {
	setupUserUpdateTestState(t)
	useUserCacheMiniRedis(t)

	token := Token{
		Id:          95,
		UserId:      1,
		Key:         "partial-cache-token",
		Status:      common.TokenStatusEnabled,
		ExpiredTime: -1,
		RemainQuota: 1000,
	}
	require.NoError(t, DB.Create(&token).Error)
	common.BatchUpdateEnabled = true

	cacheKey := "token:" + common.GenerateHMAC(token.Key)
	require.NoError(t, common.RDB.HSet(t.Context(), cacheKey, constant.TokenFiledRemainQuota, 123).Err())

	got, err := GetTokenByKey(token.Key, false)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, token.Id, got.Id)
	require.Eventually(t, func() bool {
		cached, cacheErr := cacheGetTokenByKey(token.Key)
		return cacheErr == nil && cached.Id == token.Id && cached.RemainQuota == token.RemainQuota
	}, time.Second, time.Millisecond, "a partial identity cache must be replaced with the database record")
}
