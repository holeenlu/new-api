package model

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func useSubscriptionRefundTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	previousDB := DB
	previousMainDatabaseType := common.MainDatabaseType()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&UserSubscription{}, &SubscriptionPreConsumeRecord{}))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	DB = db
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	t.Cleanup(func() {
		DB = previousDB
		common.SetMainDatabaseType(previousMainDatabaseType)
		_ = sqlDB.Close()
	})
	return db
}

func seedSubscriptionRefund(t *testing.T, db *gorm.DB, requestID string) UserSubscription {
	t.Helper()
	subscription := UserSubscription{
		UserId:      901,
		PlanId:      902,
		AmountTotal: 1_000,
		AmountUsed:  400,
		Status:      "active",
	}
	require.NoError(t, db.Create(&subscription).Error)
	require.NoError(t, db.Create(&SubscriptionPreConsumeRecord{
		RequestId:          requestID,
		UserId:             subscription.UserId,
		UserSubscriptionId: subscription.Id,
		PreConsumed:        200,
		Status:             "consumed",
	}).Error)
	return subscription
}

func requireSubscriptionRefundState(
	t *testing.T,
	db *gorm.DB,
	subscriptionID int,
	requestID string,
	wantUsed int64,
	wantStatus string,
) {
	t.Helper()
	var subscription UserSubscription
	require.NoError(t, db.First(&subscription, subscriptionID).Error)
	assert.Equal(t, wantUsed, subscription.AmountUsed)
	var record SubscriptionPreConsumeRecord
	require.NoError(t, db.Where("request_id = ?", requestID).First(&record).Error)
	assert.Equal(t, wantStatus, record.Status)
}

func TestRefundSubscriptionPreConsumeRollsBackQuotaWhenRecordUpdateFails(t *testing.T) {
	db := useSubscriptionRefundTestDB(t)
	const requestID = "subscription-refund-rollback"
	subscription := seedSubscriptionRefund(t, db, requestID)
	forcedUpdateError := errors.New("forced refund record update failure")
	failRecordUpdate := true
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register(
		"test:fail_subscription_refund_record_update",
		func(tx *gorm.DB) {
			if failRecordUpdate && tx.Statement.Schema != nil &&
				tx.Statement.Schema.Name == "SubscriptionPreConsumeRecord" {
				tx.AddError(forcedUpdateError)
			}
		},
	))

	err := RefundSubscriptionPreConsume(requestID)
	require.ErrorIs(t, err, forcedUpdateError)
	requireSubscriptionRefundState(t, db, subscription.Id, requestID, 400, "consumed")

	failRecordUpdate = false
	require.NoError(t, RefundSubscriptionPreConsume(requestID))
	requireSubscriptionRefundState(t, db, subscription.Id, requestID, 200, "refunded")
}

func TestRefundSubscriptionPreConsumeIsIdempotent(t *testing.T) {
	db := useSubscriptionRefundTestDB(t)
	const requestID = "subscription-refund-idempotent"
	subscription := seedSubscriptionRefund(t, db, requestID)

	require.NoError(t, RefundSubscriptionPreConsume(requestID))
	require.NoError(t, RefundSubscriptionPreConsume(requestID))
	requireSubscriptionRefundState(t, db, subscription.Id, requestID, 200, "refunded")
}

func TestCommittedSubscriptionUsageRetainsDebtButReservationStillCaps(t *testing.T) {
	db := useSubscriptionRefundTestDB(t)
	subscription := UserSubscription{
		UserId:      903,
		AmountTotal: 150,
		AmountUsed:  100,
		Status:      "active",
	}
	require.NoError(t, db.Create(&subscription).Error)

	require.Error(t, PostConsumeUserSubscriptionDelta(subscription.Id, 100), "pre-use reservation must remain capped")
	require.NoError(t, CommitUserSubscriptionUsageDelta(subscription.Id, 200))
	require.NoError(t, db.First(&subscription, subscription.Id).Error)
	assert.Equal(t, int64(300), subscription.AmountUsed)

	require.NoError(t, CommitUserSubscriptionUsageDelta(subscription.Id, -200))
	require.NoError(t, db.First(&subscription, subscription.Id).Error)
	assert.Equal(t, int64(100), subscription.AmountUsed)
	require.Error(t, CommitUserSubscriptionUsageDelta(subscription.Id, -101), "committed refunds must not clamp below zero")
}

func TestCommittedSubscriptionUsageRejectsInt64Overflow(t *testing.T) {
	db := useSubscriptionRefundTestDB(t)
	subscription := UserSubscription{
		UserId:      904,
		AmountTotal: 0,
		AmountUsed:  math.MaxInt64 - 5,
		Status:      "active",
	}
	require.NoError(t, db.Create(&subscription).Error)

	require.Error(t, CommitUserSubscriptionUsageDelta(subscription.Id, 6))
	require.NoError(t, db.First(&subscription, subscription.Id).Error)
	assert.Equal(t, int64(math.MaxInt64-5), subscription.AmountUsed)
	require.Error(t, CommitUserSubscriptionUsageDelta(subscription.Id, math.MinInt64))
}

func TestSubscriptionReservationRejectsInt64AddOverflowAndUnderflow(t *testing.T) {
	db := useSubscriptionRefundTestDB(t)
	tests := []struct {
		name       string
		amountUsed int64
		delta      int64
	}{
		{name: "positive overflow", amountUsed: math.MaxInt64 - 5, delta: 6},
		{name: "negative underflow", amountUsed: math.MinInt64 + 5, delta: -6},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			subscription := UserSubscription{
				UserId:      910 + index,
				AmountTotal: 0,
				AmountUsed:  test.amountUsed,
				Status:      "active",
			}
			require.NoError(t, db.Create(&subscription).Error)

			require.Error(t, PostConsumeUserSubscriptionDelta(subscription.Id, test.delta))
			require.NoError(t, db.First(&subscription, subscription.Id).Error)
			assert.Equal(t, test.amountUsed, subscription.AmountUsed)
		})
	}
}
