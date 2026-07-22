package service

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func registerFailingTaskUpdate(t *testing.T, table string, injected error) *bool {
	t.Helper()
	callbackName := "test:fail_task_settlement_" + table
	enabled := true
	require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if enabled && tx.Statement.Table == table {
			tx.AddError(injected)
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, model.DB.Callback().Update().Remove(callbackName))
	})
	return &enabled
}

func TestMain(m *testing.M) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		panic("failed to open test db: " + err.Error())
	}
	sqlDB, err := db.DB()
	if err != nil {
		panic("failed to get sql.DB: " + err.Error())
	}
	sqlDB.SetMaxOpenConns(1)

	model.DB = db
	model.LOG_DB = db

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false
	common.BatchUpdateEnabled = false
	common.LogConsumeEnabled = true

	if err := db.AutoMigrate(
		&model.Task{},
		&model.User{},
		&model.Token{},
		&model.Log{},
		&model.Channel{},
		&model.TopUp{},
		&model.SubscriptionPlan{},
		&model.UserSubscription{},
		&model.SubscriptionPreConsumeRecord{},
		&model.SystemTask{},
		&model.SystemTaskLock{},
	); err != nil {
		panic("failed to migrate: " + err.Error())
	}

	os.Exit(m.Run())
}

// ---------------------------------------------------------------------------
// Seed helpers
// ---------------------------------------------------------------------------

func truncate(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		model.DB.Exec("DELETE FROM tasks")
		model.DB.Exec("DELETE FROM users")
		model.DB.Exec("DELETE FROM tokens")
		model.DB.Exec("DELETE FROM logs")
		model.DB.Exec("DELETE FROM channels")
		model.DB.Exec("DELETE FROM top_ups")
		model.DB.Exec("DELETE FROM subscription_pre_consume_records")
		model.DB.Exec("DELETE FROM user_subscriptions")
		model.DB.Exec("DELETE FROM subscription_plans")
		model.DB.Exec("DELETE FROM system_task_locks")
		model.DB.Exec("DELETE FROM system_tasks")
	})
}

func seedUser(t *testing.T, id int, quota int) {
	t.Helper()
	user := &model.User{Id: id, Username: "test_user", Quota: quota, Status: common.UserStatusEnabled}
	require.NoError(t, model.DB.Create(user).Error)
}

func seedToken(t *testing.T, id int, userId int, key string, remainQuota int) {
	t.Helper()
	token := &model.Token{
		Id:          id,
		UserId:      userId,
		Key:         key,
		Name:        "test_token",
		Status:      common.TokenStatusEnabled,
		RemainQuota: remainQuota,
		UsedQuota:   0,
	}
	require.NoError(t, model.DB.Create(token).Error)
}

func seedReservedToken(t *testing.T, id int, userId int, key string, remainQuota int, reservedQuota int) {
	t.Helper()
	token := &model.Token{
		Id:          id,
		UserId:      userId,
		Key:         key,
		Name:        "test_token",
		Status:      common.TokenStatusEnabled,
		RemainQuota: remainQuota,
		UsedQuota:   reservedQuota,
	}
	require.NoError(t, model.DB.Create(token).Error)
}

func seedSubscription(t *testing.T, id int, userId int, amountTotal int64, amountUsed int64) {
	t.Helper()
	sub := &model.UserSubscription{
		Id:          id,
		UserId:      userId,
		AmountTotal: amountTotal,
		AmountUsed:  amountUsed,
		Status:      "active",
		StartTime:   time.Now().Unix(),
		EndTime:     time.Now().Add(30 * 24 * time.Hour).Unix(),
	}
	require.NoError(t, model.DB.Create(sub).Error)
}

func seedChannel(t *testing.T, id int) {
	t.Helper()
	ch := &model.Channel{Id: id, Name: "test_channel", Key: "sk-test", Status: common.ChannelStatusEnabled}
	require.NoError(t, model.DB.Create(ch).Error)
}

func makeTask(userId, channelId, quota, tokenId int, billingSource string, subscriptionId int) *model.Task {
	return &model.Task{
		TaskID:    "task_" + time.Now().Format("150405.000"),
		UserId:    userId,
		ChannelId: channelId,
		Quota:     quota,
		Status:    model.TaskStatus(model.TaskStatusInProgress),
		Group:     "default",
		Data:      json.RawMessage(`{}`),
		CreatedAt: time.Now().Unix(),
		UpdatedAt: time.Now().Unix(),
		Properties: model.Properties{
			OriginModelName: "test-model",
		},
		PrivateData: model.TaskPrivateData{
			BillingSource:  billingSource,
			SubscriptionId: subscriptionId,
			TokenId:        tokenId,
			BillingContext: &model.TaskBillingContext{
				ModelPrice:      0.02,
				GroupRatio:      1.0,
				OriginModelName: "test-model",
			},
		},
	}
}

func makeAcceptedTaskWithReservation(userId, quota, tokenId int, billingSource string, subscriptionId int) *model.Task {
	return &model.Task{
		TaskID:   "task_settlement_" + time.Now().Format("150405.000000"),
		UserId:   userId,
		Quota:    quota,
		Status:   model.TaskStatusSubmitted,
		Progress: "0%",
		Data:     json.RawMessage(`{}`),
		PrivateData: model.TaskPrivateData{
			BillingSource:  billingSource,
			SubscriptionId: subscriptionId,
			TokenId:        tokenId,
			BillingReservation: &model.TaskBillingReservation{
				FundingQuota: quota,
				TokenQuota:   quota,
				TokenTracked: tokenId > 0,
			},
		},
	}
}

func makeWalletTaskBillingInfo(userId, tokenId, estimate int, tokenKey string) *relaycommon.RelayInfo {
	info := &relaycommon.RelayInfo{
		UserId:        userId,
		TokenId:       tokenId,
		TokenKey:      tokenKey,
		BillingSource: BillingSourceWallet,
	}
	info.Billing = &BillingSession{
		relayInfo:        info,
		funding:          &WalletFunding{userId: userId, consumed: estimate},
		preConsumedQuota: estimate,
		tokenConsumed:    estimate,
	}
	return info
}

func TestPriceDataOtherRatiosFilterAndSnapshot(t *testing.T) {
	priceData := types.PriceData{}

	priceData.AddOtherRatio("zero", 0)
	priceData.AddOtherRatio("negative", -0.5)
	priceData.AddOtherRatio("nan", math.NaN())
	priceData.AddOtherRatio("inf", math.Inf(1))
	priceData.AddOtherRatio("one", 1)
	priceData.AddOtherRatio("positive", 2.5)

	ratios := priceData.OtherRatios()
	require.Len(t, ratios, 2)
	assert.Equal(t, 1.0, ratios["one"])
	assert.Equal(t, 2.5, ratios["positive"])
	assert.True(t, priceData.HasOtherRatio("one"))
	assert.False(t, priceData.HasOtherRatio("zero"))

	ratios["positive"] = 99
	ratios["new"] = 3
	nextSnapshot := priceData.OtherRatios()
	assert.Equal(t, 2.5, nextSnapshot["positive"])
	assert.NotContains(t, nextSnapshot, "new")
}

func TestPriceDataReplaceAndApplyOtherRatios(t *testing.T) {
	priceData := types.PriceData{}

	replaced := priceData.ReplaceOtherRatios(map[string]float64{
		"zero":     0,
		"negative": -3,
		"nan":      math.NaN(),
		"inf":      math.Inf(1),
		"one":      1,
		"duration": 2,
		"size":     1.5,
	})

	require.True(t, replaced)
	assert.Equal(t, 3.0, priceData.OtherRatioMultiplier())
	assert.Equal(t, 30.0, priceData.ApplyOtherRatiosToFloat(10))
	assert.Equal(t, 10.0, priceData.RemoveOtherRatiosFromFloat(30))
	assert.True(t, decimal.NewFromInt(30).Equal(priceData.ApplyOtherRatiosToDecimal(decimal.NewFromInt(10))))

	replaced = priceData.ReplaceOtherRatios(map[string]float64{
		"zero": 0,
		"nan":  math.NaN(),
	})

	require.False(t, replaced)
	assert.Nil(t, priceData.OtherRatios())
	assert.Equal(t, 1.0, priceData.OtherRatioMultiplier())
}

func TestTaskBillingOtherFiltersHistoricalOtherRatios(t *testing.T) {
	task := makeTask(1, 1, 100, 0, BillingSourceWallet, 0)
	task.PrivateData.BillingContext.OtherRatios = map[string]float64{
		"seconds":  2,
		"identity": 1,
		"zero":     0,
		"negative": -1,
		"nan":      math.NaN(),
		"inf":      math.Inf(1),
	}

	other := taskBillingOther(task)

	assert.Equal(t, 2.0, other["seconds"])
	assert.Equal(t, 1.0, other["identity"])
	assert.NotContains(t, other, "zero")
	assert.NotContains(t, other, "negative")
	assert.NotContains(t, other, "nan")
	assert.NotContains(t, other, "inf")
}

func TestTaskBillingContextPriceDataFiltersMultiplier(t *testing.T) {
	priceData := taskBillingContextPriceData(&model.TaskBillingContext{
		OtherRatios: map[string]float64{
			"seconds":  2,
			"size":     3,
			"identity": 1,
			"zero":     0,
			"negative": -1,
			"nan":      math.NaN(),
			"inf":      math.Inf(1),
		},
	})

	require.NotNil(t, priceData)
	assert.Equal(t, 6.0, priceData.OtherRatioMultiplier())
	assert.Equal(t, map[string]float64{
		"seconds":  2,
		"size":     3,
		"identity": 1,
	}, priceData.OtherRatios())
}

// ---------------------------------------------------------------------------
// Read-back helpers
// ---------------------------------------------------------------------------

func getUserQuota(t *testing.T, id int) int {
	t.Helper()
	var user model.User
	require.NoError(t, model.DB.Select("quota").Where("id = ?", id).First(&user).Error)
	return user.Quota
}

func getTokenRemainQuota(t *testing.T, id int) int {
	t.Helper()
	var token model.Token
	require.NoError(t, model.DB.Select("remain_quota").Where("id = ?", id).First(&token).Error)
	return token.RemainQuota
}

func getTokenUsedQuota(t *testing.T, id int) int {
	t.Helper()
	var token model.Token
	require.NoError(t, model.DB.Select("used_quota").Where("id = ?", id).First(&token).Error)
	return token.UsedQuota
}

func getSubscriptionUsed(t *testing.T, id int) int64 {
	t.Helper()
	var sub model.UserSubscription
	require.NoError(t, model.DB.Select("amount_used").Where("id = ?", id).First(&sub).Error)
	return sub.AmountUsed
}

func getTaskQuota(t *testing.T, id int64) int {
	t.Helper()
	var task model.Task
	require.NoError(t, model.DB.Select("quota").Where("id = ?", id).First(&task).Error)
	return task.Quota
}

func getLastLog(t *testing.T) *model.Log {
	t.Helper()
	var log model.Log
	err := model.LOG_DB.Order("id desc").First(&log).Error
	if err != nil {
		return nil
	}
	return &log
}

func countLogs(t *testing.T) int64 {
	t.Helper()
	var count int64
	model.LOG_DB.Model(&model.Log{}).Count(&count)
	return count
}

// ===========================================================================
// RefundTaskQuota tests
// ===========================================================================

func TestRefundTaskQuota_Wallet(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 1, 1, 1
	const initQuota, preConsumed = 10000, 3000
	const tokenRemain = 5000

	seedUser(t, userID, initQuota)
	seedReservedToken(t, tokenID, userID, "sk-test-key", tokenRemain, preConsumed)
	seedChannel(t, channelID)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	task.Status = model.TaskStatusFailure
	require.NoError(t, model.DB.Create(task).Error)

	assert.True(t, RefundTaskQuota(ctx, task, "task failed: upstream error"))

	// User quota should increase by preConsumed
	assert.Equal(t, initQuota+preConsumed, getUserQuota(t, userID))

	// The token starts from the real post-reservation ledger and the refund
	// reverses that reservation exactly.
	assert.Equal(t, tokenRemain+preConsumed, getTokenRemainQuota(t, tokenID))
	assert.Zero(t, getTokenUsedQuota(t, tokenID))

	// A refund log should be created
	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)
	assert.Equal(t, preConsumed, log.Quota)
	assert.Equal(t, "test-model", log.ModelName)
	assert.Zero(t, task.Quota)
	assert.Zero(t, getTaskQuota(t, task.ID))
}

func TestRefundTaskQuota_Subscription(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID, subID = 2, 2, 2, 1
	const preConsumed = 2000
	const subTotal, subUsed int64 = 100000, 50000
	const tokenRemain = 8000

	seedUser(t, userID, 0)
	seedReservedToken(t, tokenID, userID, "sk-sub-key", tokenRemain, preConsumed)
	seedChannel(t, channelID)
	seedSubscription(t, subID, userID, subTotal, subUsed)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceSubscription, subID)
	task.Status = model.TaskStatusFailure
	require.NoError(t, model.DB.Create(task).Error)

	assert.True(t, RefundTaskQuota(ctx, task, "subscription task failed"))

	// Subscription used should decrease by preConsumed
	assert.Equal(t, subUsed-int64(preConsumed), getSubscriptionUsed(t, subID))

	// Token should also be refunded
	assert.Equal(t, tokenRemain+preConsumed, getTokenRemainQuota(t, tokenID))
	assert.Zero(t, getTokenUsedQuota(t, tokenID))

	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)
	assert.Zero(t, getTaskQuota(t, task.ID))
}

func TestRefundTaskQuota_ZeroQuota(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID = 3
	seedUser(t, userID, 5000)

	task := makeTask(userID, 0, 0, 0, BillingSourceWallet, 0)

	assert.True(t, RefundTaskQuota(ctx, task, "zero quota task"))

	// No change to user quota
	assert.Equal(t, 5000, getUserQuota(t, userID))

	// No log created
	assert.Equal(t, int64(0), countLogs(t))
}

func TestRefundTaskQuota_NoToken(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, channelID = 4, 4
	const initQuota, preConsumed = 10000, 1500

	seedUser(t, userID, initQuota)
	seedChannel(t, channelID)

	task := makeTask(userID, channelID, preConsumed, 0, BillingSourceWallet, 0) // TokenId=0
	task.Status = model.TaskStatusFailure
	require.NoError(t, model.DB.Create(task).Error)

	assert.True(t, RefundTaskQuota(ctx, task, "no token task failed"))

	// User quota refunded
	assert.Equal(t, initQuota+preConsumed, getUserQuota(t, userID))

	// Log created
	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)
	assert.Zero(t, getTaskQuota(t, task.ID))
}

func TestRefundTaskQuota_FundingFailureKeepsPendingMarker(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, preConsumed = 5, 1200
	seedUser(t, userID, 5000)
	task := makeTask(userID, 0, preConsumed, 0, BillingSourceSubscription, 9999)
	task.Status = model.TaskStatusFailure
	require.NoError(t, model.DB.Create(task).Error)

	assert.False(t, RefundTaskQuota(ctx, task, "subscription missing"))
	assert.Equal(t, preConsumed, task.Quota)
	assert.Equal(t, preConsumed, getTaskQuota(t, task.ID))
	assert.Equal(t, int64(0), countLogs(t))
}

func TestCommitTaskSubmissionFundingFailurePreservesExactRefundLedger(t *testing.T) {
	truncate(t)

	const (
		userID       = 101
		tokenID      = 102
		estimate     = 100
		finalQuota   = 300
		initialUser  = 1000
		initialToken = 1000
	)
	tokenKey := "task-funding-failure-token"
	// Seed the authoritative ledgers after the estimate has already been
	// reserved by the request-scoped BillingSession.
	seedUser(t, userID, initialUser-estimate)
	seedReservedToken(t, tokenID, userID, tokenKey, initialToken-estimate, estimate)
	task := makeAcceptedTaskWithReservation(userID, estimate, tokenID, BillingSourceWallet, 0)
	task.Status = model.TaskStatusPendingSubmit
	task.SubmitTime = time.Now().Unix()
	task.PrivateData.UpstreamTaskID = "upstream-funded-task"
	require.NoError(t, model.DB.Create(task).Error)
	info := makeWalletTaskBillingInfo(userID, tokenID, estimate, tokenKey)

	injected := errors.New("injected funding settlement failure")
	failUpdates := registerFailingTaskUpdate(t, "users", injected)
	err := CommitTaskSubmission(nil, info, task, finalQuota)
	*failUpdates = false
	require.ErrorIs(t, err, injected)

	var storedTask model.Task
	require.NoError(t, model.DB.First(&storedTask, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSubmitted), storedTask.Status)
	assert.Equal(t, "upstream-funded-task", storedTask.PrivateData.UpstreamTaskID)
	assert.Equal(t, estimate, storedTask.Quota)
	require.NotNil(t, storedTask.PrivateData.BillingReservation)
	assert.Equal(t, estimate, storedTask.PrivateData.BillingReservation.FundingQuota)
	assert.Equal(t, estimate, storedTask.PrivateData.BillingReservation.TokenQuota)
	assert.Equal(t, initialUser-estimate, getUserQuota(t, userID))
	assert.Equal(t, initialToken-estimate, getTokenRemainQuota(t, tokenID))
	assert.Equal(t, estimate, getTokenUsedQuota(t, tokenID))

	storedTask.Status = model.TaskStatusFailure
	require.NoError(t, model.DB.Model(&model.Task{}).Where("id = ?", storedTask.ID).Update("status", storedTask.Status).Error)
	assert.True(t, RefundTaskQuota(context.Background(), &storedTask, "upstream task failed"))
	assert.Equal(t, initialUser, getUserQuota(t, userID))
	assert.Equal(t, initialToken, getTokenRemainQuota(t, tokenID))
	assert.Zero(t, getTokenUsedQuota(t, tokenID))

}

func TestCommitTaskSubmissionTokenFailureRollsBackFundingBeforeTaskRefund(t *testing.T) {
	truncate(t)

	const (
		userID       = 103
		tokenID      = 104
		estimate     = 100
		finalQuota   = 300
		initialUser  = 1000
		initialToken = 1000
	)
	tokenKey := "task-token-failure-token"
	seedUser(t, userID, initialUser-estimate)
	seedReservedToken(t, tokenID, userID, tokenKey, initialToken-estimate, estimate)
	task := makeAcceptedTaskWithReservation(userID, estimate, tokenID, BillingSourceWallet, 0)
	require.NoError(t, model.DB.Create(task).Error)
	info := makeWalletTaskBillingInfo(userID, tokenID, estimate, tokenKey)

	injected := errors.New("injected token settlement failure")
	failUpdates := registerFailingTaskUpdate(t, "tokens", injected)
	err := CommitTaskSubmission(nil, info, task, finalQuota)
	*failUpdates = false
	require.ErrorIs(t, err, injected)

	// The wallet update happened first inside the transaction, but the token
	// failure rolls it back together with the marker update.
	var storedTask model.Task
	require.NoError(t, model.DB.First(&storedTask, task.ID).Error)
	assert.Equal(t, estimate, storedTask.Quota)
	assert.Equal(t, initialUser-estimate, getUserQuota(t, userID))
	assert.Equal(t, initialToken-estimate, getTokenRemainQuota(t, tokenID))
	assert.Equal(t, estimate, getTokenUsedQuota(t, tokenID))

	storedTask.Status = model.TaskStatusFailure
	require.NoError(t, model.DB.Model(&model.Task{}).Where("id = ?", storedTask.ID).Update("status", storedTask.Status).Error)
	assert.True(t, RefundTaskQuota(context.Background(), &storedTask, "upstream task failed"))
	assert.Equal(t, initialUser, getUserQuota(t, userID))
	assert.Equal(t, initialToken, getTokenRemainQuota(t, tokenID))
	assert.Zero(t, getTokenUsedQuota(t, tokenID))

}

func TestCommitTaskSubmissionRetainsCommittedSubscriptionDebt(t *testing.T) {
	truncate(t)

	const (
		userID       = 105
		tokenID      = 106
		subID        = 107
		estimate     = 100
		finalQuota   = 300
		initialToken = 1000
	)
	tokenKey := "task-subscription-debt-token"
	seedUser(t, userID, 0)
	seedReservedToken(t, tokenID, userID, tokenKey, initialToken-estimate, estimate)
	seedSubscription(t, subID, userID, 150, estimate)
	task := makeAcceptedTaskWithReservation(userID, estimate, tokenID, BillingSourceSubscription, subID)
	require.NoError(t, model.DB.Create(task).Error)
	info := &relaycommon.RelayInfo{
		UserId:         userID,
		TokenId:        tokenID,
		TokenKey:       tokenKey,
		BillingSource:  BillingSourceSubscription,
		SubscriptionId: subID,
	}
	info.Billing = &BillingSession{
		relayInfo:        info,
		funding:          &SubscriptionFunding{subscriptionId: subID, preConsumed: estimate},
		preConsumedQuota: estimate,
		tokenConsumed:    estimate,
	}

	require.NoError(t, CommitTaskSubmission(nil, info, task, finalQuota))
	var subscription model.UserSubscription
	require.NoError(t, model.DB.First(&subscription, subID).Error)
	assert.Equal(t, int64(finalQuota), subscription.AmountUsed)
	assert.Equal(t, finalQuota, task.Quota)
	assert.Equal(t, initialToken-finalQuota, getTokenRemainQuota(t, tokenID))
	assert.Equal(t, finalQuota, getTokenUsedQuota(t, tokenID))

	task.Status = model.TaskStatusFailure
	require.NoError(t, model.DB.Model(&model.Task{}).Where("id = ?", task.ID).Update("status", task.Status).Error)
	assert.True(t, RefundTaskQuota(context.Background(), task, "upstream task failed"))
	require.NoError(t, model.DB.First(&subscription, subID).Error)
	assert.Zero(t, subscription.AmountUsed)
	assert.Equal(t, initialToken, getTokenRemainQuota(t, tokenID))
	assert.Zero(t, getTokenUsedQuota(t, tokenID))
}

func TestBillingSessionSettlementRetainsCommittedSubscriptionDebt(t *testing.T) {
	truncate(t)

	const (
		userID     = 110
		subID      = 111
		estimate   = 100
		finalQuota = 300
	)
	seedUser(t, userID, 0)
	seedSubscription(t, subID, userID, 150, estimate)
	info := &relaycommon.RelayInfo{
		UserId:         userID,
		IsPlayground:   true,
		BillingSource:  BillingSourceSubscription,
		SubscriptionId: subID,
	}
	session := &BillingSession{
		relayInfo:        info,
		funding:          &SubscriptionFunding{subscriptionId: subID, preConsumed: estimate},
		preConsumedQuota: estimate,
	}

	require.NoError(t, session.Settle(finalQuota))
	var subscription model.UserSubscription
	require.NoError(t, model.DB.First(&subscription, subID).Error)
	assert.Equal(t, int64(finalQuota), subscription.AmountUsed)
	assert.True(t, session.FundingCommitted())
}

func TestPendingTaskMarkerOwnsTerminalFailureRefund(t *testing.T) {
	truncate(t)

	const (
		userID       = 108
		tokenID      = 109
		estimate     = 100
		initialUser  = 1000
		initialToken = 1000
	)
	tokenKey := "pending-task-marker-token"
	seedUser(t, userID, initialUser-estimate)
	seedReservedToken(t, tokenID, userID, tokenKey, initialToken-estimate, estimate)
	info := makeWalletTaskBillingInfo(userID, tokenID, estimate, tokenKey)
	info.TaskRelayInfo = &relaycommon.TaskRelayInfo{PublicTaskID: "task_pending_marker"}
	info.ChannelMeta = &relaycommon.ChannelMeta{ChannelId: 1}
	info.OriginModelName = "test-model"

	task, err := CreateTaskSubmissionMarker(info, "test-platform")
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatusPendingSubmit, task.Status)
	assert.Empty(t, model.GetAllUnFinishSyncTasks(10), "pending markers must not be polled before upstream acceptance")

	assert.True(t, FailTaskSubmission(context.Background(), task.ID, "transport failed"))
	stored, err := model.GetTaskByID(task.ID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), stored.Status)
	assert.Zero(t, stored.Quota)
	assert.Equal(t, initialUser, getUserQuota(t, userID))
	assert.Equal(t, initialToken, getTokenRemainQuota(t, tokenID))
	assert.Zero(t, getTokenUsedQuota(t, tokenID))

	_, err = CreateTaskSubmissionMarker(info, "test-platform")
	require.Error(t, err, "a retry must not reuse a marker already failed by timeout or terminal handling")
}

func TestTaskMarkerInsertFailureDoesNotTransferRefundOwnership(t *testing.T) {
	truncate(t)

	info := makeWalletTaskBillingInfo(111, 112, 100, "task-marker-insert-token")
	info.TaskRelayInfo = &relaycommon.TaskRelayInfo{PublicTaskID: "task_marker_insert_failure"}
	info.ChannelMeta = &relaycommon.ChannelMeta{ChannelId: 1}
	info.OriginModelName = "test-model"

	injected := errors.New("injected task marker insert failure")
	failCreate := true
	callbackName := "test:fail_task_marker_insert"
	require.NoError(t, model.DB.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if failCreate && tx.Statement.Table == "tasks" {
			tx.AddError(injected)
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, model.DB.Callback().Create().Remove(callbackName))
	})

	task, err := CreateTaskSubmissionMarker(info, "test-platform")
	failCreate = false
	require.ErrorIs(t, err, injected)
	assert.Nil(t, task)
	assert.Zero(t, info.PersistedTaskID, "BillingSession remains the refund owner until marker insert commits")
	var count int64
	require.NoError(t, model.DB.Model(&model.Task{}).Count(&count).Error)
	assert.Zero(t, count)
}

// ===========================================================================
// RecalculateTaskQuota tests
// ===========================================================================

func TestRecalculate_PositiveDelta(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 10, 10, 10
	const initQuota, preConsumed = 10000, 2000
	const actualQuota = 3000 // under-charged by 1000
	const tokenRemain = 500  // committed delta exceeds the remaining token balance

	seedUser(t, userID, initQuota)
	seedReservedToken(t, tokenID, userID, "sk-recalc-pos", tokenRemain, preConsumed)
	seedChannel(t, channelID)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	task.Status = model.TaskStatusSuccess
	require.NoError(t, model.DB.Create(task).Error)

	RecalculateTaskQuota(ctx, task, actualQuota, "adaptor adjustment")

	// User quota should decrease by the delta (1000 additional charge)
	assert.Equal(t, initQuota-(actualQuota-preConsumed), getUserQuota(t, userID))

	// Committed usage is retained as token debt instead of disappearing when the
	// remaining token balance no longer covers the post-settlement delta.
	assert.Equal(t, tokenRemain-(actualQuota-preConsumed), getTokenRemainQuota(t, tokenID))
	assert.Equal(t, actualQuota, getTokenUsedQuota(t, tokenID))

	// task.Quota should be updated to actualQuota
	assert.Equal(t, actualQuota, task.Quota)

	// Log type should be Consume (additional charge)
	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeConsume, log.Type)
	assert.Equal(t, actualQuota-preConsumed, log.Quota)
}

func TestRecalculate_NegativeDelta(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 11, 11, 11
	const initQuota, preConsumed = 10000, 5000
	const actualQuota = 3000 // over-charged by 2000
	const tokenRemain = 5000

	seedUser(t, userID, initQuota)
	seedReservedToken(t, tokenID, userID, "sk-recalc-neg", tokenRemain, preConsumed)
	seedChannel(t, channelID)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	task.Status = model.TaskStatusSuccess
	require.NoError(t, model.DB.Create(task).Error)

	RecalculateTaskQuota(ctx, task, actualQuota, "adaptor adjustment")

	// User quota should increase by abs(delta) = 2000 (refund overpayment)
	assert.Equal(t, initQuota+(preConsumed-actualQuota), getUserQuota(t, userID))

	// Token should be refunded the difference
	assert.Equal(t, tokenRemain+(preConsumed-actualQuota), getTokenRemainQuota(t, tokenID))
	assert.Equal(t, actualQuota, getTokenUsedQuota(t, tokenID))

	// task.Quota updated
	assert.Equal(t, actualQuota, task.Quota)

	// Log type should be Refund
	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)
	assert.Equal(t, preConsumed-actualQuota, log.Quota)
}

func TestRecalculateTaskQuotaStaleCallersSettleOnce(t *testing.T) {
	tests := []struct {
		name                 string
		preConsumedQuota     int
		actualQuota          int
		expectedLogType      int
		expectedUsedQuota    int
		expectedRequestCount int
		expectedChannelQuota int64
	}{
		{
			name:                 "additional charge",
			preConsumedQuota:     2_000,
			actualQuota:          3_000,
			expectedLogType:      model.LogTypeConsume,
			expectedUsedQuota:    1_000,
			expectedRequestCount: 1,
			expectedChannelQuota: 1_000,
		},
		{
			name:                 "partial refund",
			preConsumedQuota:     5_000,
			actualQuota:          3_000,
			expectedLogType:      model.LogTypeRefund,
			expectedRequestCount: 1,
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			truncate(t)

			userID := 100 + index
			tokenID := 200 + index
			channelID := 300 + index
			const initialUserQuota = 10_000
			const initialTokenRemain = 6_000

			seedUser(t, userID, initialUserQuota)
			require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", userID).Update("request_count", 1).Error)
			seedReservedToken(t, tokenID, userID, "sk-stale-task-settlement", initialTokenRemain, test.preConsumedQuota)
			seedChannel(t, channelID)

			stored := makeTask(userID, channelID, test.preConsumedQuota, tokenID, BillingSourceWallet, 0)
			stored.Status = model.TaskStatusSuccess
			require.NoError(t, model.DB.Create(stored).Error)

			var firstCaller, staleCaller model.Task
			require.NoError(t, model.DB.First(&firstCaller, stored.ID).Error)
			require.NoError(t, model.DB.First(&staleCaller, stored.ID).Error)
			require.Equal(t, test.preConsumedQuota, staleCaller.Quota)

			RecalculateTaskQuota(context.Background(), &firstCaller, test.actualQuota, "completion adjustment")
			RecalculateTaskQuota(context.Background(), &staleCaller, test.actualQuota, "completion adjustment")

			delta := test.actualQuota - test.preConsumedQuota
			assert.Equal(t, initialUserQuota-delta, getUserQuota(t, userID))
			assert.Equal(t, initialTokenRemain-delta, getTokenRemainQuota(t, tokenID))
			assert.Equal(t, test.actualQuota, getTokenUsedQuota(t, tokenID))
			assert.Equal(t, test.actualQuota, getTaskQuota(t, stored.ID))
			assert.Equal(t, test.actualQuota, firstCaller.Quota)
			assert.Equal(t, test.actualQuota, staleCaller.Quota)

			var user model.User
			require.NoError(t, model.DB.Select("used_quota", "request_count").First(&user, userID).Error)
			assert.Equal(t, test.expectedUsedQuota, user.UsedQuota)
			assert.Equal(t, test.expectedRequestCount, user.RequestCount)

			var channel model.Channel
			require.NoError(t, model.DB.Select("used_quota").First(&channel, channelID).Error)
			assert.Equal(t, test.expectedChannelQuota, channel.UsedQuota)

			assert.Equal(t, int64(1), countLogs(t))
			log := getLastLog(t)
			require.NotNil(t, log)
			assert.Equal(t, test.expectedLogType, log.Type)
			expectedLogQuota := delta
			if expectedLogQuota < 0 {
				expectedLogQuota = -expectedLogQuota
			}
			assert.Equal(t, expectedLogQuota, log.Quota)
		})
	}
}

func TestRecalculateTaskQuotaStaleCallerRecordsPersistedDelta(t *testing.T) {
	truncate(t)

	const (
		userID            = 151
		tokenID           = 251
		channelID         = 351
		initialUserQuota  = 10_000
		initialTokenQuota = 6_000
		preConsumedQuota  = 2_000
		firstActualQuota  = 3_000
		secondActualQuota = 3_500
	)
	seedUser(t, userID, initialUserQuota)
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", userID).Update("request_count", 1).Error)
	seedReservedToken(t, tokenID, userID, "sk-stale-task-delta", initialTokenQuota, preConsumedQuota)
	seedChannel(t, channelID)

	stored := makeTask(userID, channelID, preConsumedQuota, tokenID, BillingSourceWallet, 0)
	stored.Status = model.TaskStatusSuccess
	require.NoError(t, model.DB.Create(stored).Error)

	var firstCaller, staleCaller model.Task
	require.NoError(t, model.DB.First(&firstCaller, stored.ID).Error)
	require.NoError(t, model.DB.First(&staleCaller, stored.ID).Error)

	RecalculateTaskQuota(context.Background(), &firstCaller, firstActualQuota, "first completion adjustment")
	RecalculateTaskQuota(context.Background(), &staleCaller, secondActualQuota, "revised completion adjustment")

	expectedTotalDelta := secondActualQuota - preConsumedQuota
	assert.Equal(t, initialUserQuota-expectedTotalDelta, getUserQuota(t, userID))
	assert.Equal(t, initialTokenQuota-expectedTotalDelta, getTokenRemainQuota(t, tokenID))
	assert.Equal(t, secondActualQuota, getTokenUsedQuota(t, tokenID))
	assert.Equal(t, secondActualQuota, getTaskQuota(t, stored.ID))

	var user model.User
	require.NoError(t, model.DB.Select("used_quota", "request_count").First(&user, userID).Error)
	assert.Equal(t, expectedTotalDelta, user.UsedQuota)
	assert.Equal(t, 1, user.RequestCount)

	var channel model.Channel
	require.NoError(t, model.DB.Select("used_quota").First(&channel, channelID).Error)
	assert.Equal(t, int64(expectedTotalDelta), channel.UsedQuota)

	var logs []model.Log
	require.NoError(t, model.LOG_DB.Order("id").Find(&logs).Error)
	require.Len(t, logs, 2)
	assert.Equal(t, firstActualQuota-preConsumedQuota, logs[0].Quota)
	assert.Equal(t, secondActualQuota-firstActualQuota, logs[1].Quota)
	var other map[string]any
	require.NoError(t, common.UnmarshalJsonStr(logs[1].Other, &other))
	assert.Equal(t, float64(firstActualQuota), other["pre_consumed_quota"])
}

func TestRecalculate_ZeroDelta(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID = 12
	const initQuota, preConsumed = 10000, 3000

	seedUser(t, userID, initQuota)

	task := makeTask(userID, 0, preConsumed, 0, BillingSourceWallet, 0)
	task.Status = model.TaskStatusSuccess
	require.NoError(t, model.DB.Create(task).Error)

	RecalculateTaskQuota(ctx, task, preConsumed, "exact match")

	// No change to user quota
	assert.Equal(t, initQuota, getUserQuota(t, userID))

	// No log created (delta is zero)
	assert.Equal(t, int64(0), countLogs(t))
}

func TestRecalculate_ActualQuotaZero(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID = 13
	const initQuota = 10000

	seedUser(t, userID, initQuota)

	task := makeTask(userID, 0, 5000, 0, BillingSourceWallet, 0)

	RecalculateTaskQuota(ctx, task, 0, "zero actual")

	// No change (early return)
	assert.Equal(t, initQuota, getUserQuota(t, userID))
	assert.Equal(t, int64(0), countLogs(t))
}

func TestRecalculate_Subscription_NegativeDelta(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID, subID = 14, 14, 14, 2
	const preConsumed = 5000
	const actualQuota = 2000 // over-charged by 3000
	const subTotal, subUsed int64 = 100000, 50000
	const tokenRemain = 8000

	seedUser(t, userID, 0)
	seedReservedToken(t, tokenID, userID, "sk-sub-recalc", tokenRemain, preConsumed)
	seedChannel(t, channelID)
	seedSubscription(t, subID, userID, subTotal, subUsed)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceSubscription, subID)
	task.Status = model.TaskStatusSuccess
	require.NoError(t, model.DB.Create(task).Error)

	RecalculateTaskQuota(ctx, task, actualQuota, "subscription over-charge")

	// Subscription used should decrease by delta (refund 3000)
	assert.Equal(t, subUsed-int64(preConsumed-actualQuota), getSubscriptionUsed(t, subID))

	// Token refunded
	assert.Equal(t, tokenRemain+(preConsumed-actualQuota), getTokenRemainQuota(t, tokenID))
	assert.Equal(t, actualQuota, getTokenUsedQuota(t, tokenID))

	assert.Equal(t, actualQuota, task.Quota)

	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)
}

func TestRecalculateTaskQuotaTokenFailureRollsBackFundingAndMarker(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 15, 15, 15
	const initialUserQuota, preConsumed, actualQuota = 10000, 2000, 3000
	const tokenRemain = 5000

	seedUser(t, userID, initialUserQuota)
	seedReservedToken(t, tokenID, userID, "sk-recalc-atomic-failure", tokenRemain, preConsumed)
	seedChannel(t, channelID)
	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	task.Status = model.TaskStatusSuccess
	require.NoError(t, model.DB.Create(task).Error)

	injected := errors.New("injected completion token settlement failure")
	failTokenUpdate := registerFailingTaskUpdate(t, "tokens", injected)
	RecalculateTaskQuota(ctx, task, actualQuota, "completion adjustment")
	*failTokenUpdate = false

	assert.Equal(t, initialUserQuota, getUserQuota(t, userID))
	assert.Equal(t, tokenRemain, getTokenRemainQuota(t, tokenID))
	assert.Equal(t, preConsumed, getTokenUsedQuota(t, tokenID))
	assert.Equal(t, preConsumed, task.Quota)
	assert.Equal(t, preConsumed, getTaskQuota(t, task.ID))
	assert.Zero(t, countLogs(t))
}

// ===========================================================================
// CAS + Billing integration tests
// Simulates the flow in updateVideoSingleTask (service/task_polling.go)
// ===========================================================================

// simulatePollBilling reproduces the CAS + billing logic from updateVideoSingleTask.
// It takes a persisted task (already in DB), applies the new status, and performs
// the conditional update + billing exactly as the polling loop does.
func simulatePollBilling(ctx context.Context, task *model.Task, newStatus model.TaskStatus, actualQuota int) {
	snap := task.Snapshot()

	shouldRefund := false
	shouldSettle := false
	quota := task.Quota

	task.Status = newStatus
	switch string(newStatus) {
	case model.TaskStatusSuccess:
		task.Progress = "100%"
		task.FinishTime = 9999
		shouldSettle = true
	case model.TaskStatusFailure:
		task.Progress = "100%"
		task.FinishTime = 9999
		task.FailReason = "upstream error"
		if quota != 0 {
			shouldRefund = true
		}
	default:
		task.Progress = "50%"
	}

	isDone := task.Status == model.TaskStatus(model.TaskStatusSuccess) || task.Status == model.TaskStatus(model.TaskStatusFailure)
	if isDone && snap.Status != task.Status {
		won, err := task.UpdateWithStatus(snap.Status)
		if err != nil {
			shouldRefund = false
			shouldSettle = false
		} else if !won {
			shouldRefund = false
			shouldSettle = false
		}
	} else if !snap.Equal(task.Snapshot()) {
		_, _ = task.UpdateWithStatus(snap.Status)
	}

	if shouldSettle && actualQuota > 0 {
		RecalculateTaskQuota(ctx, task, actualQuota, "test settle")
	}
	if shouldRefund {
		RefundTaskQuota(ctx, task, task.FailReason)
	}
}

func TestCASGuardedRefund_Win(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 20, 20, 20
	const initQuota, preConsumed = 10000, 4000
	const tokenRemain = 6000

	seedUser(t, userID, initQuota)
	seedReservedToken(t, tokenID, userID, "sk-cas-refund-win", tokenRemain, preConsumed)
	seedChannel(t, channelID)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	task.Status = model.TaskStatus(model.TaskStatusInProgress)
	require.NoError(t, model.DB.Create(task).Error)

	simulatePollBilling(ctx, task, model.TaskStatus(model.TaskStatusFailure), 0)

	// CAS wins: task in DB should now be FAILURE
	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.EqualValues(t, model.TaskStatusFailure, reloaded.Status)
	assert.Zero(t, reloaded.Quota)

	// Refund should have happened
	assert.Equal(t, initQuota+preConsumed, getUserQuota(t, userID))
	assert.Equal(t, tokenRemain+preConsumed, getTokenRemainQuota(t, tokenID))
	assert.Zero(t, getTokenUsedQuota(t, tokenID))

	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)
}

func TestCASGuardedRefund_Lose(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 21, 21, 21
	const initQuota, preConsumed = 10000, 4000
	const tokenRemain = 6000

	seedUser(t, userID, initQuota)
	seedReservedToken(t, tokenID, userID, "sk-cas-refund-lose", tokenRemain, preConsumed)
	seedChannel(t, channelID)

	// Create task with IN_PROGRESS in DB
	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	task.Status = model.TaskStatus(model.TaskStatusInProgress)
	require.NoError(t, model.DB.Create(task).Error)

	// Simulate another process already transitioning to FAILURE
	model.DB.Model(&model.Task{}).Where("id = ?", task.ID).Update("status", model.TaskStatusFailure)

	// Our process still has the old in-memory state (IN_PROGRESS) and tries to transition
	// task.Status is still IN_PROGRESS in the snapshot
	simulatePollBilling(ctx, task, model.TaskStatus(model.TaskStatusFailure), 0)

	// CAS lost: user quota should NOT change (no double refund)
	assert.Equal(t, initQuota, getUserQuota(t, userID))
	assert.Equal(t, tokenRemain, getTokenRemainQuota(t, tokenID))

	// No billing log should be created
	assert.Equal(t, int64(0), countLogs(t))
}

func TestCASGuardedSettle_Win(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 22, 22, 22
	const initQuota, preConsumed = 10000, 5000
	const actualQuota = 3000 // over-charged, should get partial refund
	const tokenRemain = 8000

	seedUser(t, userID, initQuota)
	seedReservedToken(t, tokenID, userID, "sk-cas-settle-win", tokenRemain, preConsumed)
	seedChannel(t, channelID)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	task.Status = model.TaskStatus(model.TaskStatusInProgress)
	require.NoError(t, model.DB.Create(task).Error)

	simulatePollBilling(ctx, task, model.TaskStatus(model.TaskStatusSuccess), actualQuota)

	// CAS wins: task should be SUCCESS
	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.EqualValues(t, model.TaskStatusSuccess, reloaded.Status)

	// Settlement should refund the over-charge (5000 - 3000 = 2000 back to user)
	assert.Equal(t, initQuota+(preConsumed-actualQuota), getUserQuota(t, userID))
	assert.Equal(t, tokenRemain+(preConsumed-actualQuota), getTokenRemainQuota(t, tokenID))
	assert.Equal(t, actualQuota, getTokenUsedQuota(t, tokenID))

	// task.Quota should be updated to actualQuota
	assert.Equal(t, actualQuota, task.Quota)
}

func TestNonTerminalUpdate_NoBilling(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, channelID = 23, 23
	const initQuota, preConsumed = 10000, 3000

	seedUser(t, userID, initQuota)
	seedChannel(t, channelID)

	task := makeTask(userID, channelID, preConsumed, 0, BillingSourceWallet, 0)
	task.Status = model.TaskStatus(model.TaskStatusInProgress)
	task.Progress = "20%"
	require.NoError(t, model.DB.Create(task).Error)

	// Simulate a non-terminal poll update (still IN_PROGRESS, progress changed)
	simulatePollBilling(ctx, task, model.TaskStatus(model.TaskStatusInProgress), 0)

	// User quota should NOT change
	assert.Equal(t, initQuota, getUserQuota(t, userID))

	// No billing log
	assert.Equal(t, int64(0), countLogs(t))

	// Task progress should be updated in DB
	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.Equal(t, "50%", reloaded.Progress)
}

// ===========================================================================
// Mock adaptor for settleTaskBillingOnComplete tests
// ===========================================================================

type mockAdaptor struct {
	adjustReturn int
}

func (m *mockAdaptor) Init(_ *relaycommon.RelayInfo) {}
func (m *mockAdaptor) FetchTask(string, string, map[string]any, string) (*http.Response, error) {
	return nil, nil
}
func (m *mockAdaptor) ParseTaskResult([]byte) (*relaycommon.TaskInfo, error) { return nil, nil }
func (m *mockAdaptor) AdjustBillingOnComplete(_ *model.Task, _ *relaycommon.TaskInfo) int {
	return m.adjustReturn
}

// ===========================================================================
// PerCallBilling tests — settleTaskBillingOnComplete
// ===========================================================================

func TestSettle_PerCallBilling_SkipsAdaptorAdjust(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 30, 30, 30
	const initQuota, preConsumed = 10000, 5000
	const tokenRemain = 8000

	seedUser(t, userID, initQuota)
	seedReservedToken(t, tokenID, userID, "sk-percall-adaptor", tokenRemain, preConsumed)
	seedChannel(t, channelID)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	task.PrivateData.BillingContext.PerCallBilling = true

	adaptor := &mockAdaptor{adjustReturn: 2000}
	taskResult := &relaycommon.TaskInfo{Status: model.TaskStatusSuccess}

	settleTaskBillingOnComplete(ctx, adaptor, task, taskResult)

	// Per-call: no adjustment despite adaptor returning 2000
	assert.Equal(t, initQuota, getUserQuota(t, userID))
	assert.Equal(t, tokenRemain, getTokenRemainQuota(t, tokenID))
	assert.Equal(t, preConsumed, task.Quota)
	assert.Equal(t, int64(0), countLogs(t))
}

func TestSettle_PerCallBilling_SkipsTotalTokens(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 31, 31, 31
	const initQuota, preConsumed = 10000, 4000
	const tokenRemain = 7000

	seedUser(t, userID, initQuota)
	seedReservedToken(t, tokenID, userID, "sk-percall-tokens", tokenRemain, preConsumed)
	seedChannel(t, channelID)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	task.PrivateData.BillingContext.PerCallBilling = true

	adaptor := &mockAdaptor{adjustReturn: 0}
	taskResult := &relaycommon.TaskInfo{Status: model.TaskStatusSuccess, TotalTokens: 9999}

	settleTaskBillingOnComplete(ctx, adaptor, task, taskResult)

	// Per-call: no recalculation by tokens
	assert.Equal(t, initQuota, getUserQuota(t, userID))
	assert.Equal(t, tokenRemain, getTokenRemainQuota(t, tokenID))
	assert.Equal(t, preConsumed, task.Quota)
	assert.Equal(t, int64(0), countLogs(t))
}

func TestSettle_NonPerCallBilling_AppliesAdaptorAdjustment(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 32, 32, 32
	const initQuota, preConsumed = 10000, 5000
	const adaptorQuota = 3000
	const tokenRemain = 8000

	seedUser(t, userID, initQuota)
	seedReservedToken(t, tokenID, userID, "sk-nonpercall-adj", tokenRemain, preConsumed)
	seedChannel(t, channelID)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	// PerCallBilling defaults to false
	task.Status = model.TaskStatusSuccess
	require.NoError(t, model.DB.Create(task).Error)

	adaptor := &mockAdaptor{adjustReturn: adaptorQuota}
	taskResult := &relaycommon.TaskInfo{Status: model.TaskStatusSuccess}

	settleTaskBillingOnComplete(ctx, adaptor, task, taskResult)

	// Non-per-call: adaptor adjustment applies (refund 2000)
	assert.Equal(t, initQuota+(preConsumed-adaptorQuota), getUserQuota(t, userID))
	assert.Equal(t, tokenRemain+(preConsumed-adaptorQuota), getTokenRemainQuota(t, tokenID))
	assert.Equal(t, adaptorQuota, getTokenUsedQuota(t, tokenID))
	assert.Equal(t, adaptorQuota, task.Quota)

	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)
}
