package model

import (
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestMain(m *testing.M) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		panic("failed to open test db: " + err.Error())
	}
	DB = db
	LOG_DB = db

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false
	common.BatchUpdateEnabled = false
	common.LogConsumeEnabled = true
	initCol()

	sqlDB, err := db.DB()
	if err != nil {
		panic("failed to get sql.DB: " + err.Error())
	}
	sqlDB.SetMaxOpenConns(1)

	if err := db.AutoMigrate(
		&Task{},
		&User{},
		&UserSession{},
		&AuthFlow{},
		&ExternalIdentityClaim{},
		&Token{},
		&PasskeyCredential{},
		&TwoFA{},
		&TwoFABackupCode{},
		&Log{},
		&Channel{},
		&QuotaData{},
		&Ability{},
		&TopUp{},
		&SubscriptionPlan{},
		&SubscriptionOrder{},
		&UserSubscription{},
		&UserOAuthBinding{},
		&PerfMetric{},
		&SystemInstance{},
		&SystemTask{},
		&SystemTaskLock{},
	); err != nil {
		panic("failed to migrate: " + err.Error())
	}

	os.Exit(m.Run())
}

func truncateTables(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		DB.Exec("DELETE FROM tasks")
		DB.Exec("DELETE FROM auth_flows")
		DB.Exec("DELETE FROM external_identity_claims")
		DB.Exec("DELETE FROM user_sessions")
		DB.Exec("DELETE FROM passkey_credentials")
		DB.Exec("DELETE FROM two_fa_backup_codes")
		DB.Exec("DELETE FROM two_fas")
		DB.Exec("DELETE FROM tokens")
		DB.Exec("DELETE FROM user_oauth_bindings")
		DB.Exec("DELETE FROM users")
		DB.Exec("DELETE FROM logs")
		DB.Exec("DELETE FROM channels")
		DB.Exec("DELETE FROM quota_data")
		DB.Exec("DELETE FROM abilities")
		DB.Exec("DELETE FROM top_ups")
		DB.Exec("DELETE FROM subscription_orders")
		DB.Exec("DELETE FROM subscription_plans")
		DB.Exec("DELETE FROM user_subscriptions")
		DB.Exec("DELETE FROM perf_metrics")
		DB.Exec("DELETE FROM system_instances")
		DB.Exec("DELETE FROM system_task_locks")
		DB.Exec("DELETE FROM system_tasks")
	})
}

func insertTask(t *testing.T, task *Task) {
	t.Helper()
	task.CreatedAt = time.Now().Unix()
	task.UpdatedAt = time.Now().Unix()
	require.NoError(t, DB.Create(task).Error)
}

// ---------------------------------------------------------------------------
// Snapshot / Equal — pure logic tests (no DB)
// ---------------------------------------------------------------------------

func TestSnapshotEqual_Same(t *testing.T) {
	s := taskSnapshot{
		Status:     TaskStatusInProgress,
		Progress:   "50%",
		StartTime:  1000,
		FinishTime: 0,
		FailReason: "",
		ResultURL:  "",
		Data:       json.RawMessage(`{"key":"value"}`),
	}
	assert.True(t, s.Equal(s))
}

func TestSnapshotEqual_DifferentStatus(t *testing.T) {
	a := taskSnapshot{Status: TaskStatusInProgress, Data: json.RawMessage(`{}`)}
	b := taskSnapshot{Status: TaskStatusSuccess, Data: json.RawMessage(`{}`)}
	assert.False(t, a.Equal(b))
}

func TestSnapshotEqual_DifferentProgress(t *testing.T) {
	a := taskSnapshot{Status: TaskStatusInProgress, Progress: "30%", Data: json.RawMessage(`{}`)}
	b := taskSnapshot{Status: TaskStatusInProgress, Progress: "60%", Data: json.RawMessage(`{}`)}
	assert.False(t, a.Equal(b))
}

func TestSnapshotEqual_DifferentData(t *testing.T) {
	a := taskSnapshot{Status: TaskStatusInProgress, Data: json.RawMessage(`{"a":1}`)}
	b := taskSnapshot{Status: TaskStatusInProgress, Data: json.RawMessage(`{"a":2}`)}
	assert.False(t, a.Equal(b))
}

func TestSnapshotEqual_NilVsEmpty(t *testing.T) {
	a := taskSnapshot{Status: TaskStatusInProgress, Data: nil}
	b := taskSnapshot{Status: TaskStatusInProgress, Data: json.RawMessage{}}
	// bytes.Equal(nil, []byte{}) == true
	assert.True(t, a.Equal(b))
}

func TestSnapshot_Roundtrip(t *testing.T) {
	task := &Task{
		Status:     TaskStatusInProgress,
		Progress:   "42%",
		StartTime:  1234,
		FinishTime: 5678,
		FailReason: "timeout",
		PrivateData: TaskPrivateData{
			ResultURL: "https://example.com/result.mp4",
		},
		Data: json.RawMessage(`{"model":"test-model"}`),
	}
	snap := task.Snapshot()
	assert.Equal(t, task.Status, snap.Status)
	assert.Equal(t, task.Progress, snap.Progress)
	assert.Equal(t, task.StartTime, snap.StartTime)
	assert.Equal(t, task.FinishTime, snap.FinishTime)
	assert.Equal(t, task.FailReason, snap.FailReason)
	assert.Equal(t, task.PrivateData.ResultURL, snap.ResultURL)
	assert.JSONEq(t, string(task.Data), string(snap.Data))
}

func TestPendingTaskLeaseRenewalInvalidatesStaleTimeoutCAS(t *testing.T) {
	truncateTables(t)

	task := &Task{
		TaskID:     "task_pending_lease",
		Status:     TaskStatusPendingSubmit,
		Progress:   "0%",
		SubmitTime: time.Now().Add(-time.Hour).Unix(),
		Quota:      100,
		Data:       json.RawMessage(`{}`),
	}
	insertTask(t, task)
	staleSubmitTime := task.SubmitTime

	renewed, err := RenewPendingTaskSubmission(task.ID)
	require.NoError(t, err)
	assert.Greater(t, renewed.SubmitTime, staleSubmitTime)

	task.Status = TaskStatusFailure
	task.Progress = "100%"
	won, err := task.UpdateTimedOutWithStatus(TaskStatusPendingSubmit, staleSubmitTime)
	require.NoError(t, err)
	assert.False(t, won, "a sweep selected before lease renewal must lose its CAS")

	stored, err := GetTaskByID(task.ID)
	require.NoError(t, err)
	assert.Equal(t, TaskStatus(TaskStatusPendingSubmit), stored.Status)
	assert.Equal(t, renewed.SubmitTime, stored.SubmitTime)
}

func TestPendingTaskLeaseRenewalRejectsTerminalMarker(t *testing.T) {
	truncateTables(t)

	task := &Task{
		TaskID:     "task_terminal_lease",
		Status:     TaskStatusFailure,
		Progress:   "100%",
		SubmitTime: time.Now().Unix(),
		Quota:      0,
		Data:       json.RawMessage(`{}`),
	}
	insertTask(t, task)

	_, err := RenewPendingTaskSubmission(task.ID)
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// UpdateWithStatus CAS — DB integration tests
// ---------------------------------------------------------------------------

func TestUpdateWithStatus_Win(t *testing.T) {
	truncateTables(t)

	task := &Task{
		TaskID:   "task_cas_win",
		Status:   TaskStatusInProgress,
		Progress: "50%",
		Data:     json.RawMessage(`{}`),
	}
	insertTask(t, task)

	task.Status = TaskStatusSuccess
	task.Progress = "100%"
	won, err := task.UpdateWithStatus(TaskStatusInProgress)
	require.NoError(t, err)
	assert.True(t, won)

	var reloaded Task
	require.NoError(t, DB.First(&reloaded, task.ID).Error)
	assert.EqualValues(t, TaskStatusSuccess, reloaded.Status)
	assert.Equal(t, "100%", reloaded.Progress)
}

func TestUpdateWithStatus_Lose(t *testing.T) {
	truncateTables(t)

	task := &Task{
		TaskID: "task_cas_lose",
		Status: TaskStatusFailure,
		Data:   json.RawMessage(`{}`),
	}
	insertTask(t, task)

	task.Status = TaskStatusSuccess
	won, err := task.UpdateWithStatus(TaskStatusInProgress) // wrong fromStatus
	require.NoError(t, err)
	assert.False(t, won)

	var reloaded Task
	require.NoError(t, DB.First(&reloaded, task.ID).Error)
	assert.EqualValues(t, TaskStatusFailure, reloaded.Status) // unchanged
}

func TestUpdateWithStatus_ConcurrentWinner(t *testing.T) {
	truncateTables(t)

	task := &Task{
		TaskID: "task_cas_race",
		Status: TaskStatusInProgress,
		Quota:  1000,
		Data:   json.RawMessage(`{}`),
	}
	insertTask(t, task)

	const goroutines = 5
	wins := make([]bool, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			t := &Task{}
			*t = Task{
				ID:       task.ID,
				TaskID:   task.TaskID,
				Status:   TaskStatusSuccess,
				Progress: "100%",
				Quota:    task.Quota,
				Data:     json.RawMessage(`{}`),
			}
			t.CreatedAt = task.CreatedAt
			t.UpdatedAt = time.Now().Unix()
			won, err := t.UpdateWithStatus(TaskStatusInProgress)
			if err == nil {
				wins[idx] = won
			}
		}(i)
	}
	wg.Wait()

	winCount := 0
	for _, w := range wins {
		if w {
			winCount++
		}
	}
	assert.Equal(t, 1, winCount, "exactly one goroutine should win the CAS")
}

func TestUpdateWithStatusPreservesSettledTaskLedgerFromStalePoller(t *testing.T) {
	truncateTables(t)

	user := User{Id: 61, Username: "task-stale-ledger-user", Quota: 900}
	require.NoError(t, DB.Create(&user).Error)
	token := Token{
		Id:          62,
		UserId:      user.Id,
		Key:         "task-stale-ledger-token",
		RemainQuota: 900,
		UsedQuota:   100,
	}
	require.NoError(t, DB.Create(&token).Error)
	task := &Task{
		TaskID:   "task_stale_poll_ledger",
		UserId:   user.Id,
		Status:   TaskStatusSubmitted,
		Quota:    100,
		Progress: "50%",
		Data:     json.RawMessage(`{"status":"submitted"}`),
		PrivateData: TaskPrivateData{
			BillingSource: "wallet",
			TokenId:       token.Id,
			BillingReservation: &TaskBillingReservation{
				FundingQuota: 100,
				TokenQuota:   100,
				TokenTracked: true,
			},
		},
	}
	insertTask(t, task)

	var stalePoller Task
	require.NoError(t, DB.First(&stalePoller, task.ID).Error)

	settled, delta, changed, err := SettleTaskQuotaAtomically(task.ID, 300)
	require.NoError(t, err)
	require.True(t, changed)
	assert.Equal(t, 200, delta)
	assert.Equal(t, 300, settled.Quota)

	stalePoller.Status = TaskStatusSuccess
	stalePoller.Progress = "100%"
	stalePoller.PrivateData.ResultURL = "https://example.com/final.mp4"
	stalePoller.Data = json.RawMessage(`{"status":"success"}`)
	won, err := stalePoller.UpdateWithStatus(TaskStatusSubmitted)
	require.NoError(t, err)
	require.True(t, won)

	var storedTask Task
	require.NoError(t, DB.First(&storedTask, task.ID).Error)
	assert.Equal(t, TaskStatus(TaskStatusSuccess), storedTask.Status)
	assert.Equal(t, "100%", storedTask.Progress)
	assert.Equal(t, "https://example.com/final.mp4", storedTask.PrivateData.ResultURL)
	assert.JSONEq(t, `{"status":"success"}`, string(storedTask.Data))
	assert.Equal(t, 300, storedTask.Quota)
	require.NotNil(t, storedTask.PrivateData.BillingReservation)
	assert.Equal(t, 300, storedTask.PrivateData.BillingReservation.FundingQuota)
	assert.Equal(t, 300, storedTask.PrivateData.BillingReservation.TokenQuota)
	assert.True(t, storedTask.PrivateData.BillingReservation.TokenTracked)

	var storedUser User
	require.NoError(t, DB.First(&storedUser, user.Id).Error)
	assert.Equal(t, 700, storedUser.Quota)
	var storedToken Token
	require.NoError(t, DB.First(&storedToken, token.Id).Error)
	assert.Equal(t, 700, storedToken.RemainQuota)
	assert.Equal(t, 300, storedToken.UsedQuota)

	_, delta, changed, err = SettleTaskQuotaAtomically(task.ID, 300)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Zero(t, delta)
	require.NoError(t, DB.First(&storedUser, user.Id).Error)
	assert.Equal(t, 700, storedUser.Quota)
	require.NoError(t, DB.First(&storedToken, token.Id).Error)
	assert.Equal(t, 700, storedToken.RemainQuota)
	assert.Equal(t, 300, storedToken.UsedQuota)
}

func TestUpdateTimedOutWithStatusPreservesSettledTaskLedger(t *testing.T) {
	truncateTables(t)

	user := User{Id: 63, Username: "task-timeout-ledger-user", Quota: 900}
	require.NoError(t, DB.Create(&user).Error)
	token := Token{Id: 64, UserId: user.Id, Key: "task-timeout-ledger-token", RemainQuota: 900, UsedQuota: 100}
	require.NoError(t, DB.Create(&token).Error)
	task := &Task{
		TaskID:     "task_stale_timeout_ledger",
		UserId:     user.Id,
		Status:     TaskStatusSubmitted,
		SubmitTime: TaskRefundLegacyCutoff,
		Quota:      100,
		Progress:   "50%",
		Data:       json.RawMessage(`{}`),
		PrivateData: TaskPrivateData{
			BillingSource: "wallet",
			TokenId:       token.Id,
			BillingReservation: &TaskBillingReservation{
				FundingQuota: 100,
				TokenQuota:   100,
				TokenTracked: true,
			},
		},
	}
	insertTask(t, task)

	var staleTimeout Task
	require.NoError(t, DB.First(&staleTimeout, task.ID).Error)
	_, _, changed, err := SettleTaskQuotaAtomically(task.ID, 300)
	require.NoError(t, err)
	require.True(t, changed)

	staleTimeout.Status = TaskStatusFailure
	staleTimeout.Progress = "100%"
	staleTimeout.FailReason = "timed out"
	won, err := staleTimeout.UpdateTimedOutWithStatus(TaskStatusSubmitted, task.SubmitTime)
	require.NoError(t, err)
	require.True(t, won)

	var storedTask Task
	require.NoError(t, DB.First(&storedTask, task.ID).Error)
	assert.Equal(t, 300, storedTask.Quota)
	require.NotNil(t, storedTask.PrivateData.BillingReservation)
	assert.Equal(t, 300, storedTask.PrivateData.BillingReservation.FundingQuota)
	assert.Equal(t, 300, storedTask.PrivateData.BillingReservation.TokenQuota)
}

func TestRefundTaskQuotaAtomicallyCommitsWalletAndTokenOnce(t *testing.T) {
	truncateTables(t)

	user := User{Id: 71, Username: "task-refund-user", Quota: 700}
	require.NoError(t, DB.Create(&user).Error)
	token := Token{
		Id:          72,
		UserId:      user.Id,
		Key:         "task-refund-token",
		RemainQuota: 700,
		UsedQuota:   300,
	}
	require.NoError(t, DB.Create(&token).Error)
	task := &Task{
		TaskID:      "task_refund_atomic",
		UserId:      user.Id,
		Status:      TaskStatusFailure,
		Quota:       300,
		Data:        json.RawMessage(`{}`),
		PrivateData: TaskPrivateData{BillingSource: "wallet", TokenId: token.Id},
	}
	insertTask(t, task)

	refunded, err := RefundTaskQuotaAtomically(task.ID, task.Quota)
	require.NoError(t, err)
	assert.True(t, refunded)

	refunded, err = RefundTaskQuotaAtomically(task.ID, task.Quota)
	require.NoError(t, err)
	assert.False(t, refunded)

	var reloaded Task
	require.NoError(t, DB.First(&reloaded, task.ID).Error)
	assert.Zero(t, reloaded.Quota)
	var storedUser User
	require.NoError(t, DB.First(&storedUser, user.Id).Error)
	assert.Equal(t, 1000, storedUser.Quota)
	var storedToken Token
	require.NoError(t, DB.First(&storedToken, token.Id).Error)
	assert.Equal(t, 1000, storedToken.RemainQuota)
	assert.Zero(t, storedToken.UsedQuota)
}

func TestRefundTaskQuotaAtomicallyUsesSeparateReservationAmounts(t *testing.T) {
	truncateTables(t)

	user := User{Id: 771, Username: "task-split-refund-user", Quota: 700}
	require.NoError(t, DB.Create(&user).Error)
	token := Token{
		Id:          772,
		UserId:      user.Id,
		Key:         "task-split-refund-token",
		RemainQuota: 900,
		UsedQuota:   100,
	}
	require.NoError(t, DB.Create(&token).Error)
	task := &Task{
		TaskID: "task_split_refund_atomic",
		UserId: user.Id,
		Status: TaskStatusFailure,
		Quota:  300,
		Data:   json.RawMessage(`{}`),
		PrivateData: TaskPrivateData{
			BillingSource: "wallet",
			TokenId:       token.Id,
			BillingReservation: &TaskBillingReservation{
				FundingQuota: 300,
				TokenQuota:   100,
				TokenTracked: true,
			},
		},
	}
	insertTask(t, task)

	refunded, err := RefundTaskQuotaAtomically(task.ID, task.Quota)
	require.NoError(t, err)
	assert.True(t, refunded)

	var storedUser User
	require.NoError(t, DB.First(&storedUser, user.Id).Error)
	assert.Equal(t, 1000, storedUser.Quota)
	var storedToken Token
	require.NoError(t, DB.First(&storedToken, token.Id).Error)
	assert.Equal(t, 1000, storedToken.RemainQuota)
	assert.Zero(t, storedToken.UsedQuota)
}

func TestGetUnrefundedFailedTasks_FiltersAndLimits(t *testing.T) {
	truncateTables(t)

	tasks := []*Task{
		{TaskID: "failed_refundable_1", Status: TaskStatusFailure, Quota: 100, SubmitTime: TaskRefundLegacyCutoff, Data: json.RawMessage(`{}`)},
		{TaskID: "failed_refundable_2", Status: TaskStatusFailure, Quota: 200, SubmitTime: TaskRefundLegacyCutoff + 1, Data: json.RawMessage(`{}`)},
		{TaskID: "legacy_failed", Status: TaskStatusFailure, Quota: 400, SubmitTime: TaskRefundLegacyCutoff - 1, Data: json.RawMessage(`{}`)},
		{TaskID: "failed_without_quota", Status: TaskStatusFailure, Quota: 0, Data: json.RawMessage(`{}`)},
		{TaskID: "successful_with_quota", Status: TaskStatusSuccess, Quota: 300, Data: json.RawMessage(`{}`)},
	}
	for _, task := range tasks {
		insertTask(t, task)
	}

	updatedBefore := time.Now().Unix() + 1
	found := GetUnrefundedFailedTasks(updatedBefore, 1)
	require.Len(t, found, 1)
	assert.Equal(t, tasks[0].ID, found[0].ID)

	found = GetUnrefundedFailedTasks(updatedBefore, 10)
	require.Len(t, found, 2)
	assert.Equal(t, []int64{tasks[0].ID, tasks[1].ID}, []int64{found[0].ID, found[1].ID})

	assert.Empty(t, GetUnrefundedFailedTasks(updatedBefore, 0))
}

func TestRefundTaskQuotaAtomicallyRollsBackMarkerAndTokenOnFundingFailure(t *testing.T) {
	truncateTables(t)

	token := Token{Id: 73, UserId: 1, Key: "failed-task-refund-token", RemainQuota: 250, UsedQuota: 750}
	require.NoError(t, DB.Create(&token).Error)
	task := &Task{
		TaskID:      "task_refund_rollback",
		Status:      TaskStatusFailure,
		Quota:       750,
		Data:        json.RawMessage(`{}`),
		PrivateData: TaskPrivateData{BillingSource: "subscription", SubscriptionId: 9999, TokenId: token.Id},
	}
	insertTask(t, task)

	refunded, err := RefundTaskQuotaAtomically(task.ID, task.Quota)
	require.Error(t, err)
	assert.False(t, refunded)

	var reloaded Task
	require.NoError(t, DB.First(&reloaded, task.ID).Error)
	assert.Equal(t, task.Quota, reloaded.Quota)
	var storedToken Token
	require.NoError(t, DB.First(&storedToken, token.Id).Error)
	assert.Equal(t, token.RemainQuota, storedToken.RemainQuota)
	assert.Equal(t, token.UsedQuota, storedToken.UsedQuota)
}

func TestRefundTaskQuotaAtomicallyPreservesMarkerOnSubscriptionUnderflow(t *testing.T) {
	truncateTables(t)

	subscription := UserSubscription{
		Id:          77,
		UserId:      78,
		AmountTotal: 1_000,
		AmountUsed:  100,
	}
	require.NoError(t, DB.Create(&subscription).Error)
	task := &Task{
		TaskID: "task_subscription_refund_underflow",
		UserId: subscription.UserId,
		Status: TaskStatusFailure,
		Quota:  300,
		Data:   json.RawMessage(`{}`),
		PrivateData: TaskPrivateData{
			BillingSource:  "subscription",
			SubscriptionId: subscription.Id,
			BillingReservation: &TaskBillingReservation{
				FundingQuota: 300,
			},
		},
	}
	insertTask(t, task)

	refunded, err := RefundTaskQuotaAtomically(task.ID, task.Quota)
	require.Error(t, err)
	assert.False(t, refunded)

	var storedTask Task
	require.NoError(t, DB.First(&storedTask, task.ID).Error)
	assert.Equal(t, task.Quota, storedTask.Quota)
	var storedSubscription UserSubscription
	require.NoError(t, DB.First(&storedSubscription, subscription.Id).Error)
	assert.Equal(t, subscription.AmountUsed, storedSubscription.AmountUsed)
}

func TestRefundTaskQuotaAtomicallyRejectsNonFailedTask(t *testing.T) {
	truncateTables(t)

	user := User{Id: 74, Username: "successful-task-user", Quota: 700}
	require.NoError(t, DB.Create(&user).Error)
	token := Token{Id: 75, UserId: user.Id, Key: "successful-task-token", RemainQuota: 700, UsedQuota: 300}
	require.NoError(t, DB.Create(&token).Error)
	task := &Task{
		TaskID:      "task_refund_success_rejected",
		UserId:      user.Id,
		Status:      TaskStatusSuccess,
		Quota:       300,
		Data:        json.RawMessage(`{}`),
		PrivateData: TaskPrivateData{BillingSource: "wallet", TokenId: token.Id},
	}
	insertTask(t, task)

	refunded, err := RefundTaskQuotaAtomically(task.ID, task.Quota)
	require.Error(t, err)
	assert.False(t, refunded)

	var storedTask Task
	require.NoError(t, DB.First(&storedTask, task.ID).Error)
	assert.Equal(t, task.Quota, storedTask.Quota)
	var storedUser User
	require.NoError(t, DB.First(&storedUser, user.Id).Error)
	assert.Equal(t, user.Quota, storedUser.Quota)
	var storedToken Token
	require.NoError(t, DB.First(&storedToken, token.Id).Error)
	assert.Equal(t, token.RemainQuota, storedToken.RemainQuota)
	assert.Equal(t, token.UsedQuota, storedToken.UsedQuota)
}

func TestRefundTaskQuotaAtomicallyCompletesWhenReservedTokenWasDeleted(t *testing.T) {
	truncateTables(t)

	user := User{Id: 76, Username: "deleted-token-task-user", Quota: 700}
	require.NoError(t, DB.Create(&user).Error)
	task := &Task{
		TaskID:      "task_refund_deleted_token",
		UserId:      user.Id,
		Status:      TaskStatusFailure,
		Quota:       300,
		Data:        json.RawMessage(`{}`),
		PrivateData: TaskPrivateData{BillingSource: "wallet", TokenId: 9999},
	}
	insertTask(t, task)

	refunded, err := RefundTaskQuotaAtomically(task.ID, task.Quota)
	require.NoError(t, err)
	assert.True(t, refunded)

	var storedTask Task
	require.NoError(t, DB.First(&storedTask, task.ID).Error)
	assert.Zero(t, storedTask.Quota)
	var storedUser User
	require.NoError(t, DB.First(&storedUser, user.Id).Error)
	assert.Equal(t, user.Quota+task.Quota, storedUser.Quota)
}

func TestHasTaskPollingWork_IncludesOnlyRefundableFailedTasks(t *testing.T) {
	truncateTables(t)
	assert.False(t, HasTaskPollingWork())

	legacy := &Task{
		TaskID:     "legacy_failed_work",
		Status:     TaskStatusFailure,
		Progress:   "100%",
		Quota:      500,
		SubmitTime: TaskRefundLegacyCutoff - 1,
		Data:       json.RawMessage(`{}`),
	}
	insertTask(t, legacy)
	assert.False(t, HasTaskPollingWork())

	refundable := &Task{
		TaskID:     "refundable_failed_work",
		Status:     TaskStatusFailure,
		Progress:   "100%",
		Quota:      500,
		SubmitTime: TaskRefundLegacyCutoff,
		Data:       json.RawMessage(`{}`),
	}
	insertTask(t, refundable)
	assert.True(t, HasTaskPollingWork())
}
