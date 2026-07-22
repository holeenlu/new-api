package model

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	commonRelay "github.com/QuantumNous/new-api/relay/common"
	"gorm.io/gorm"
)

type TaskStatus string

func (t TaskStatus) ToVideoStatus() string {
	var status string
	switch t {
	case TaskStatusPendingSubmit, TaskStatusNotStart, TaskStatusQueued, TaskStatusSubmitted:
		status = dto.VideoStatusQueued
	case TaskStatusInProgress:
		status = dto.VideoStatusInProgress
	case TaskStatusSuccess:
		status = dto.VideoStatusCompleted
	case TaskStatusFailure:
		status = dto.VideoStatusFailed
	default:
		status = dto.VideoStatusUnknown // Default fallback
	}
	return status
}

const (
	TaskStatusPendingSubmit TaskStatus = "PENDING_SUBMIT"
	TaskStatusNotStart                 = "NOT_START"
	TaskStatusSubmitted                = "SUBMITTED"
	TaskStatusQueued                   = "QUEUED"
	TaskStatusInProgress               = "IN_PROGRESS"
	TaskStatusFailure                  = "FAILURE"
	TaskStatusSuccess                  = "SUCCESS"
	TaskStatusUnknown                  = "UNKNOWN"
)

// TaskRefundLegacyCutoff separates legacy timeout tasks that intentionally
// do not receive automatic refunds from tasks covered by reconciliation.
const TaskRefundLegacyCutoff int64 = 1740182400 // 2025-02-22 00:00:00 UTC

type Task struct {
	ID         int64                 `json:"id" gorm:"primary_key;AUTO_INCREMENT"`
	CreatedAt  int64                 `json:"created_at" gorm:"index"`
	UpdatedAt  int64                 `json:"updated_at"`
	TaskID     string                `json:"task_id" gorm:"type:varchar(191);index"` // 第三方id，不一定有/ song id\ Task id
	Platform   constant.TaskPlatform `json:"platform" gorm:"type:varchar(30);index"` // 平台
	UserId     int                   `json:"user_id" gorm:"index"`
	Group      string                `json:"group" gorm:"type:varchar(50)"` // 修正计费用
	ChannelId  int                   `json:"channel_id" gorm:"index"`
	Quota      int                   `json:"quota"`
	Action     string                `json:"action" gorm:"type:varchar(40);index"` // 任务类型, song, lyrics, description-mode
	Status     TaskStatus            `json:"status" gorm:"type:varchar(20);index"` // 任务状态
	FailReason string                `json:"fail_reason"`
	SubmitTime int64                 `json:"submit_time" gorm:"index"`
	StartTime  int64                 `json:"start_time" gorm:"index"`
	FinishTime int64                 `json:"finish_time" gorm:"index"`
	Progress   string                `json:"progress" gorm:"type:varchar(20);index"`
	Properties Properties            `json:"properties" gorm:"type:json"`
	Username   string                `json:"username,omitempty" gorm:"-"`
	// 禁止返回给用户，内部可能包含key等隐私信息
	PrivateData TaskPrivateData `json:"-" gorm:"column:private_data;type:json"`
	Data        json.RawMessage `json:"data" gorm:"type:json"`
}

func (t *Task) SetData(data any) {
	b, _ := common.Marshal(data)
	t.Data = json.RawMessage(b)
}

func (t *Task) GetData(v any) error {
	return common.Unmarshal(t.Data, &v)
}

type Properties struct {
	Input             string `json:"input"`
	UpstreamModelName string `json:"upstream_model_name,omitempty"`
	OriginModelName   string `json:"origin_model_name,omitempty"`
}

func (m *Properties) Scan(val interface{}) error {
	bytesValue, _ := val.([]byte)
	if len(bytesValue) == 0 {
		*m = Properties{}
		return nil
	}
	return common.Unmarshal(bytesValue, m)
}

func (m Properties) Value() (driver.Value, error) {
	if m == (Properties{}) {
		return nil, nil
	}
	return common.Marshal(m)
}

type TaskPrivateData struct {
	Key            string `json:"key,omitempty"`
	UpstreamTaskID string `json:"upstream_task_id,omitempty"` // 上游真实 task ID
	ResultURL      string `json:"result_url,omitempty"`       // 任务成功后的结果 URL（视频地址等）
	// 计费上下文：用于异步退款/差额结算（轮询阶段读取）
	BillingSource  string              `json:"billing_source,omitempty"`  // "wallet" 或 "subscription"
	SubscriptionId int                 `json:"subscription_id,omitempty"` // 订阅 ID，用于订阅退款
	TokenId        int                 `json:"token_id,omitempty"`        // 令牌 ID，用于令牌额度退款
	NodeName       string              `json:"node_name,omitempty"`       // 发起任务的节点名，轮询结算阶段据此归属日志而非最后查询节点
	BillingContext *TaskBillingContext `json:"billing_context,omitempty"` // 计费参数快照（用于轮询阶段重新计算）
	// BillingReservation is the authoritative task-owned refund ledger. A nil
	// value identifies legacy rows whose funding and token reservations both
	// equal Task.Quota. New rows preserve the two ledgers independently so a
	// later failed-task refund can never reverse more than was committed.
	BillingReservation *TaskBillingReservation `json:"billing_reservation,omitempty"`
}

type TaskBillingReservation struct {
	FundingQuota int  `json:"funding_quota"`
	TokenQuota   int  `json:"token_quota"`
	TokenTracked bool `json:"token_tracked"`
}

// TaskBillingContext 记录任务提交时的计费参数，以便轮询阶段可以重新计算额度。
type TaskBillingContext struct {
	ModelPrice      float64            `json:"model_price,omitempty"`       // 模型单价
	GroupRatio      float64            `json:"group_ratio,omitempty"`       // 分组倍率
	ModelRatio      float64            `json:"model_ratio,omitempty"`       // 模型倍率
	OtherRatios     map[string]float64 `json:"other_ratios,omitempty"`      // 附加倍率（时长、分辨率等）
	OriginModelName string             `json:"origin_model_name,omitempty"` // 模型名称，必须为OriginModelName
	PerCallBilling  bool               `json:"per_call_billing,omitempty"`  // 按次计费：跳过轮询阶段的差额结算
}

// GetUpstreamTaskID 获取上游真实 task ID（用于与 provider 通信）
// 旧数据没有 UpstreamTaskID 时，TaskID 本身就是上游 ID
func (t *Task) GetUpstreamTaskID() string {
	if t.PrivateData.UpstreamTaskID != "" {
		return t.PrivateData.UpstreamTaskID
	}
	return t.TaskID
}

// GetResultURL 获取任务结果 URL（视频地址等）
// 新数据存在 PrivateData.ResultURL 中；旧数据回退到 FailReason（历史兼容）
func (t *Task) GetResultURL() string {
	if t.PrivateData.ResultURL != "" {
		return t.PrivateData.ResultURL
	}
	return t.FailReason
}

// GenerateTaskID 生成对外暴露的 task_xxxx 格式 ID
func GenerateTaskID() string {
	key, _ := common.GenerateRandomCharsKey(32)
	return "task_" + key
}

func (p *TaskPrivateData) Scan(val interface{}) error {
	bytesValue, _ := val.([]byte)
	if len(bytesValue) == 0 {
		return nil
	}
	return common.Unmarshal(bytesValue, p)
}

func (p TaskPrivateData) Value() (driver.Value, error) {
	if (p == TaskPrivateData{}) {
		return nil, nil
	}
	return common.Marshal(p)
}

// SyncTaskQueryParams 用于包含所有搜索条件的结构体，可以根据需求添加更多字段
type SyncTaskQueryParams struct {
	Platform       constant.TaskPlatform
	ChannelID      string
	TaskID         string
	UserID         string
	Action         string
	Status         string
	StartTimestamp int64
	EndTimestamp   int64
	UserIDs        []int
}

func InitTask(platform constant.TaskPlatform, relayInfo *commonRelay.RelayInfo) *Task {
	properties := Properties{}
	privateData := TaskPrivateData{}
	if relayInfo != nil && relayInfo.ChannelMeta != nil {
		if relayInfo.ChannelMeta.ChannelType == constant.ChannelTypeGemini ||
			relayInfo.ChannelMeta.ChannelType == constant.ChannelTypeVertexAi {
			privateData.Key = relayInfo.ChannelMeta.ApiKey
		}
		if relayInfo.UpstreamModelName != "" {
			properties.UpstreamModelName = relayInfo.UpstreamModelName
		}
		if relayInfo.OriginModelName != "" {
			properties.OriginModelName = relayInfo.OriginModelName
		}
	}

	// 使用预生成的公开 ID（如果有），否则新生成
	taskID := ""
	if relayInfo.TaskRelayInfo != nil && relayInfo.TaskRelayInfo.PublicTaskID != "" {
		taskID = relayInfo.TaskRelayInfo.PublicTaskID
	} else {
		taskID = GenerateTaskID()
	}

	t := &Task{
		TaskID:      taskID,
		UserId:      relayInfo.UserId,
		Group:       relayInfo.UsingGroup,
		SubmitTime:  time.Now().Unix(),
		Status:      TaskStatusNotStart,
		Progress:    "0%",
		ChannelId:   relayInfo.ChannelId,
		Platform:    platform,
		Properties:  properties,
		PrivateData: privateData,
	}
	return t
}

func TaskGetAllUserTask(userId int, startIdx int, num int, queryParams SyncTaskQueryParams) []*Task {
	var tasks []*Task
	var err error

	// 初始化查询构建器
	query := DB.Where("user_id = ?", userId)

	if queryParams.TaskID != "" {
		query = query.Where("task_id = ?", queryParams.TaskID)
	}
	if queryParams.Action != "" {
		query = query.Where("action = ?", queryParams.Action)
	}
	if queryParams.Status != "" {
		query = query.Where("status = ?", queryParams.Status)
	}
	if queryParams.Platform != "" {
		query = query.Where("platform = ?", queryParams.Platform)
	}
	if queryParams.StartTimestamp != 0 {
		// 假设您已将前端传来的时间戳转换为数据库所需的时间格式，并处理了时间戳的验证和解析
		query = query.Where("submit_time >= ?", queryParams.StartTimestamp)
	}
	if queryParams.EndTimestamp != 0 {
		query = query.Where("submit_time <= ?", queryParams.EndTimestamp)
	}

	// 获取数据
	err = query.Omit("channel_id").Order("id desc").Limit(num).Offset(startIdx).Find(&tasks).Error
	if err != nil {
		return nil
	}

	return tasks
}

func TaskGetAllTasks(startIdx int, num int, queryParams SyncTaskQueryParams) []*Task {
	var tasks []*Task
	var err error

	// 初始化查询构建器
	query := DB

	// 添加过滤条件
	if queryParams.ChannelID != "" {
		query = query.Where("channel_id = ?", queryParams.ChannelID)
	}
	if queryParams.Platform != "" {
		query = query.Where("platform = ?", queryParams.Platform)
	}
	if queryParams.UserID != "" {
		query = query.Where("user_id = ?", queryParams.UserID)
	}
	if len(queryParams.UserIDs) != 0 {
		query = query.Where("user_id in (?)", queryParams.UserIDs)
	}
	if queryParams.TaskID != "" {
		query = query.Where("task_id = ?", queryParams.TaskID)
	}
	if queryParams.Action != "" {
		query = query.Where("action = ?", queryParams.Action)
	}
	if queryParams.Status != "" {
		query = query.Where("status = ?", queryParams.Status)
	}
	if queryParams.StartTimestamp != 0 {
		query = query.Where("submit_time >= ?", queryParams.StartTimestamp)
	}
	if queryParams.EndTimestamp != 0 {
		query = query.Where("submit_time <= ?", queryParams.EndTimestamp)
	}

	// 获取数据
	err = query.Order("id desc").Limit(num).Offset(startIdx).Find(&tasks).Error
	if err != nil {
		return nil
	}

	return tasks
}

func GetTimedOutUnfinishedTasks(cutoffUnix int64, limit int) []*Task {
	var tasks []*Task
	err := DB.Where("progress != ?", "100%").
		Where("status NOT IN ?", []string{TaskStatusFailure, TaskStatusSuccess}).
		Where("submit_time < ?", cutoffUnix).
		Order("submit_time").
		Limit(limit).
		Find(&tasks).Error
	if err != nil {
		return nil
	}
	return tasks
}

// GetUnrefundedFailedTasks returns failed tasks whose non-zero quota marks a
// pending refund. Legacy timeout tasks are excluded before LIMIT is applied so
// they cannot starve refundable tasks from the reconciliation sweep.
func GetUnrefundedFailedTasks(updatedBefore int64, limit int) []*Task {
	if limit <= 0 {
		return nil
	}

	var tasks []*Task
	err := DB.Where("status = ?", TaskStatusFailure).
		Where("quota != ?", 0).
		Where("updated_at <= ?", updatedBefore).
		Where("(submit_time <= ? OR submit_time >= ?)", 0, TaskRefundLegacyCutoff).
		Order("id").
		Limit(limit).
		Find(&tasks).Error
	if err != nil {
		return nil
	}
	return tasks
}

func GetAllUnFinishSyncTasks(limit int) []*Task {
	var tasks []*Task
	var err error
	// get all tasks progress is not 100%
	err = DB.Where("progress != ?", "100%").
		Where("status NOT IN ?", []TaskStatus{TaskStatusPendingSubmit, TaskStatusFailure, TaskStatusSuccess}).
		Limit(limit).
		Order("id").
		Find(&tasks).Error
	if err != nil {
		return nil
	}
	return tasks
}

// HasUnfinishedSyncTasks reports whether at least one async (Suno/video) task is
// still in progress. It is a cheap existence check (LIMIT 1) used to decide
// whether the async_task_poll system task needs to run; when no task is pending
// the scheduler skips creating a row entirely.
func HasUnfinishedSyncTasks() bool {
	var id int64
	err := DB.Model(&Task{}).
		Where("progress != ?", "100%").
		Where("status != ?", TaskStatusFailure).
		Where("status != ?", TaskStatusSuccess).
		Limit(1).
		Pluck("id", &id).Error
	return err == nil && id != 0
}

// HasTaskPollingWork reports whether polling has either an unfinished task or
// a failed task with a pending, non-legacy refund. The latter keeps the system
// task scheduler active when reconciliation is the only work left.
func HasTaskPollingWork() bool {
	if HasUnfinishedSyncTasks() {
		return true
	}

	var id int64
	err := DB.Model(&Task{}).
		Where("status = ?", TaskStatusFailure).
		Where("quota != ?", 0).
		Where("(submit_time <= ? OR submit_time >= ?)", 0, TaskRefundLegacyCutoff).
		Limit(1).
		Pluck("id", &id).Error
	return err == nil && id != 0
}

func GetByOnlyTaskId(taskId string) (*Task, bool, error) {
	if taskId == "" {
		return nil, false, nil
	}
	var task *Task
	var err error
	err = DB.Where("task_id = ?", taskId).First(&task).Error
	exist, err := RecordExist(err)
	if err != nil {
		return nil, false, err
	}
	return task, exist, err
}

func GetTaskByID(id int64) (*Task, error) {
	if id <= 0 {
		return nil, errors.New("invalid task id")
	}
	var task Task
	if err := DB.Where("id = ?", id).First(&task).Error; err != nil {
		return nil, err
	}
	return &task, nil
}

// RenewPendingTaskSubmission moves the pending marker's timeout lease forward
// before an upstream retry (and once more after the accepted response). The
// conditional update serializes with the timeout sweeper: a stale sweep must
// also match the previous SubmitTime and therefore cannot invalidate a renewed
// marker immediately before an upstream write.
func RenewPendingTaskSubmission(id int64) (*Task, error) {
	if id <= 0 {
		return nil, errors.New("invalid task id")
	}
	var renewed Task
	err := DB.Transaction(func(tx *gorm.DB) error {
		var task Task
		if err := lockForUpdate(tx).Where("id = ?", id).First(&task).Error; err != nil {
			return err
		}
		if task.Status != TaskStatusPendingSubmit {
			return fmt.Errorf("task %d cannot renew submission from status %s", id, task.Status)
		}
		previousSubmitTime := task.SubmitTime
		newSubmitTime := time.Now().Unix()
		if newSubmitTime <= previousSubmitTime {
			if previousSubmitTime == math.MaxInt64 {
				return fmt.Errorf("task %d submission lease overflow", id)
			}
			newSubmitTime = previousSubmitTime + 1
		}
		update := tx.Model(&Task{}).
			Where("id = ? AND status = ? AND submit_time = ?", id, TaskStatusPendingSubmit, previousSubmitTime).
			Update("submit_time", newSubmitTime)
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return fmt.Errorf("task %d pending submission lease changed concurrently", id)
		}
		task.SubmitTime = newSubmitTime
		renewed = task
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &renewed, nil
}

func GetByTaskId(userId int, taskId string) (*Task, bool, error) {
	if taskId == "" {
		return nil, false, nil
	}
	var task *Task
	var err error
	err = DB.Where("user_id = ? and task_id = ?", userId, taskId).
		First(&task).Error
	exist, err := RecordExist(err)
	if err != nil {
		return nil, false, err
	}
	return task, exist, err
}

func GetByTaskIds(userId int, taskIds []any) ([]*Task, error) {
	if len(taskIds) == 0 {
		return nil, nil
	}
	var task []*Task
	var err error
	err = DB.Where("user_id = ? and task_id in (?)", userId, taskIds).
		Find(&task).Error
	if err != nil {
		return nil, err
	}
	return task, nil
}

func (Task *Task) Insert() error {
	var err error
	err = DB.Create(Task).Error
	return err
}

type taskSnapshot struct {
	Status     TaskStatus
	Progress   string
	StartTime  int64
	FinishTime int64
	FailReason string
	ResultURL  string
	Data       json.RawMessage
}

func (s taskSnapshot) Equal(other taskSnapshot) bool {
	return s.Status == other.Status &&
		s.Progress == other.Progress &&
		s.StartTime == other.StartTime &&
		s.FinishTime == other.FinishTime &&
		s.FailReason == other.FailReason &&
		s.ResultURL == other.ResultURL &&
		bytes.Equal(s.Data, other.Data)
}

func (t *Task) Snapshot() taskSnapshot {
	return taskSnapshot{
		Status:     t.Status,
		Progress:   t.Progress,
		StartTime:  t.StartTime,
		FinishTime: t.FinishTime,
		FailReason: t.FailReason,
		ResultURL:  t.PrivateData.ResultURL,
		Data:       t.Data,
	}
}

func (Task *Task) Update() error {
	var err error
	err = DB.Save(Task).Error
	return err
}

func (t *Task) UpdateQuota() error {
	return DB.Model(t).Update("quota", t.Quota).Error
}

func taskReservationQuotas(task *Task) (fundingQuota int, tokenQuota int, tokenTracked bool, err error) {
	if task == nil {
		return 0, 0, false, errors.New("task is nil")
	}
	if task.PrivateData.BillingReservation == nil {
		// Legacy rows used Task.Quota as the common funding/token reservation.
		if task.Quota < 0 || task.Quota > common.MaxQuota {
			return 0, 0, false, fmt.Errorf("task %d has invalid legacy quota %d", task.ID, task.Quota)
		}
		tokenTracked = task.PrivateData.TokenId > 0
		if tokenTracked {
			tokenQuota = task.Quota
		}
		return task.Quota, tokenQuota, tokenTracked, nil
	}
	reservation := task.PrivateData.BillingReservation
	if reservation.FundingQuota < 0 || reservation.FundingQuota > common.MaxQuota ||
		reservation.TokenQuota < 0 || reservation.TokenQuota > common.MaxQuota {
		return 0, 0, false, fmt.Errorf(
			"task %d has invalid billing reservation funding=%d token=%d",
			task.ID,
			reservation.FundingQuota,
			reservation.TokenQuota,
		)
	}
	return reservation.FundingQuota, reservation.TokenQuota, reservation.TokenTracked, nil
}

// SettleTaskQuotaAtomically owns the post-acceptance task settlement boundary.
// Funding, token usage, and the persisted refund ledger either all advance to
// actualQuota or all remain at their previous reservation. This prevents a
// later FAILURE refund from reversing a quota delta that never committed. The
// funding delta is calculated from the locked persisted reservation rather than
// from a caller snapshot. The boolean result reports whether this call changed
// any persisted ledger; duplicate callers can therefore skip usage statistics
// and billing logs without publishing a stale delta.
func SettleTaskQuotaAtomically(id int64, actualQuota int) (*Task, int, bool, error) {
	if id <= 0 {
		return nil, 0, false, errors.New("invalid task id")
	}
	if actualQuota < 0 || actualQuota > common.MaxQuota {
		return nil, 0, false, fmt.Errorf("invalid task settlement quota: %d", actualQuota)
	}

	var settledTask Task
	committedFundingDelta := 0
	settlementChanged := false
	walletUserId := 0
	tokenKey := ""
	err := DB.Transaction(func(tx *gorm.DB) error {
		var task Task
		if err := lockForUpdate(tx).Where("id = ?", id).First(&task).Error; err != nil {
			return err
		}
		if task.Status == TaskStatusPendingSubmit {
			return fmt.Errorf("task %d is not accepted upstream", id)
		}
		if task.Status == TaskStatusFailure {
			return fmt.Errorf("task %d cannot settle from status %s", id, task.Status)
		}

		fundingQuota, tokenQuota, tokenTracked, err := taskReservationQuotas(&task)
		if err != nil {
			return err
		}
		fundingDelta := int64(actualQuota) - int64(fundingQuota)
		targetTokenQuota := 0
		if tokenTracked {
			targetTokenQuota = actualQuota
		}
		tokenDelta := int64(targetTokenQuota) - int64(tokenQuota)
		if fundingDelta == 0 && tokenDelta == 0 {
			settledTask = task
			return nil
		}

		if fundingDelta != 0 {
			if task.PrivateData.BillingSource == "subscription" && task.PrivateData.SubscriptionId > 0 {
				if err := commitUserSubscriptionUsageDeltaTx(tx, task.PrivateData.SubscriptionId, fundingDelta); err != nil {
					return err
				}
			} else {
				wallet := tx.Model(&User{}).Where("id = ?", task.UserId).
					Update("quota", gorm.Expr("quota - ?", fundingDelta))
				if wallet.Error != nil {
					return wallet.Error
				}
				if wallet.RowsAffected != 1 {
					return fmt.Errorf("task %d settlement updated %d users", id, wallet.RowsAffected)
				}
				walletUserId = task.UserId
			}
		}

		if tokenDelta != 0 {
			if task.PrivateData.TokenId <= 0 {
				return fmt.Errorf("task %d tracks token quota without a token id", id)
			}
			var token Token
			if err := tx.Select("id", "key").Where("id = ?", task.PrivateData.TokenId).First(&token).Error; err != nil {
				return err
			}
			tokenUpdate := tx.Model(&Token{}).Where("id = ?", token.Id).Updates(
				map[string]interface{}{
					"remain_quota":  gorm.Expr("remain_quota - ?", tokenDelta),
					"used_quota":    gorm.Expr("used_quota + ?", tokenDelta),
					"accessed_time": common.GetTimestamp(),
				},
			)
			if tokenUpdate.Error != nil {
				return tokenUpdate.Error
			}
			if tokenUpdate.RowsAffected != 1 {
				return fmt.Errorf("task %d settlement updated %d tokens", id, tokenUpdate.RowsAffected)
			}
			tokenKey = token.Key
		}

		if task.PrivateData.BillingReservation == nil {
			task.PrivateData.BillingReservation = &TaskBillingReservation{}
		}
		task.PrivateData.BillingReservation.FundingQuota = actualQuota
		task.PrivateData.BillingReservation.TokenQuota = targetTokenQuota
		task.PrivateData.BillingReservation.TokenTracked = tokenTracked
		task.Quota = actualQuota
		updated := tx.Model(&Task{}).Where("id = ?", id).Updates(map[string]interface{}{
			"quota":        task.Quota,
			"private_data": task.PrivateData,
		})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return fmt.Errorf("task %d settlement marker updated %d rows", id, updated.RowsAffected)
		}
		settledTask = task
		committedFundingDelta = int(fundingDelta)
		settlementChanged = true
		return nil
	})
	if err != nil {
		return nil, 0, false, err
	}

	if walletUserId > 0 && common.RedisEnabled {
		if err := invalidateUserCache(walletUserId); err != nil {
			common.SysLog("failed to invalidate user cache after atomic task settlement: " + err.Error())
		}
	}
	if tokenKey != "" && common.RedisEnabled {
		if err := cacheDeleteToken(tokenKey); err != nil {
			common.SysLog("failed to invalidate token cache after atomic task settlement: " + err.Error())
		}
	}
	return &settledTask, committedFundingDelta, settlementChanged, nil
}

// RefundTaskQuotaAtomically claims a task's persisted refund marker and
// reverses its funding and token reservations in the same database transaction.
// A false result with no error means another caller already completed the
// refund. Cache synchronization happens only after the transaction commits.
func RefundTaskQuotaAtomically(id int64, expectedQuota int) (bool, error) {
	if id <= 0 {
		return false, errors.New("invalid task id")
	}
	if expectedQuota <= 0 {
		return false, errors.New("invalid task refund quota")
	}

	refunded := false
	walletUserId := 0
	tokenKey := ""
	missingTokenId := 0
	err := DB.Transaction(func(tx *gorm.DB) error {
		var task Task
		if err := lockForUpdate(tx).Where("id = ?", id).First(&task).Error; err != nil {
			return err
		}
		if task.Quota == 0 {
			return nil
		}
		if task.Status != TaskStatusFailure {
			return fmt.Errorf("task %d cannot refund from status %s", id, task.Status)
		}
		if task.Quota != expectedQuota {
			return fmt.Errorf("task %d refund quota changed from %d to %d", id, expectedQuota, task.Quota)
		}

		claim := tx.Model(&Task{}).
			Where("id = ? AND status = ? AND quota = ?", id, TaskStatusFailure, expectedQuota).
			Update("quota", 0)
		if claim.Error != nil {
			return claim.Error
		}
		if claim.RowsAffected == 0 {
			return fmt.Errorf("task %d refund marker was concurrently changed", id)
		}

		fundingQuota, tokenQuota, _, err := taskReservationQuotas(&task)
		if err != nil {
			return err
		}
		if task.PrivateData.BillingSource == "subscription" && task.PrivateData.SubscriptionId > 0 {
			// A task marker owns an exact committed reservation. If the
			// subscription ledger no longer contains that debit, preserve the
			// marker for reconciliation instead of silently clamping to zero.
			if err := commitUserSubscriptionUsageDeltaTx(tx, task.PrivateData.SubscriptionId, -int64(fundingQuota)); err != nil {
				return err
			}
		} else if fundingQuota > 0 {
			wallet := tx.Model(&User{}).Where("id = ?", task.UserId).
				Update("quota", gorm.Expr("quota + ?", fundingQuota))
			if wallet.Error != nil {
				return wallet.Error
			}
			if wallet.RowsAffected != 1 {
				return fmt.Errorf("task %d wallet refund updated %d users", id, wallet.RowsAffected)
			}
			walletUserId = task.UserId
		}

		if task.PrivateData.TokenId > 0 && tokenQuota > 0 {
			var token Token
			if err := tx.Where("id = ?", task.PrivateData.TokenId).First(&token).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					missingTokenId = task.PrivateData.TokenId
				} else {
					return err
				}
			} else {
				tokenUpdate := tx.Model(&Token{}).Where("id = ?", token.Id).Updates(
					map[string]interface{}{
						"remain_quota":  gorm.Expr("remain_quota + ?", tokenQuota),
						"used_quota":    gorm.Expr("used_quota - ?", tokenQuota),
						"accessed_time": common.GetTimestamp(),
					},
				)
				if tokenUpdate.Error != nil {
					return tokenUpdate.Error
				}
				if tokenUpdate.RowsAffected != 1 {
					return fmt.Errorf("task %d token refund updated %d tokens", id, tokenUpdate.RowsAffected)
				}
				tokenKey = token.Key
			}
		}
		refunded = true
		return nil
	})
	if err != nil || !refunded {
		return refunded, err
	}

	if walletUserId > 0 && common.RedisEnabled {
		if err := invalidateUserCache(walletUserId); err != nil {
			common.SysLog("failed to update user cache after atomic task refund: " + err.Error())
		}
	}
	if tokenKey != "" && common.RedisEnabled {
		if err := cacheDeleteToken(tokenKey); err != nil {
			common.SysLog("failed to invalidate token cache after atomic task refund: " + err.Error())
		}
	}
	if missingTokenId > 0 {
		common.SysLog(fmt.Sprintf("task %d token %d was missing during atomic refund", id, missingTokenId))
	}
	return true, nil
}

// UpdateWithStatus performs a conditional UPDATE guarded by fromStatus (CAS).
// Returns (true, nil) if this caller won the update, (false, nil) if
// another process already moved the task out of fromStatus. MySQL commonly
// reports changed rows rather than matched rows, so a same-value no-op update
// can also return false even when the status predicate still matched.
//
// Uses Model().Select("*").Updates() instead of Save() because GORM's Save
// falls back to INSERT ON CONFLICT when the WHERE-guarded UPDATE matches
// zero rows, which silently bypasses the CAS guard.
func (t *Task) UpdateWithStatus(fromStatus TaskStatus) (bool, error) {
	updated := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		var persisted Task
		if err := lockForUpdate(tx).Where("id = ?", t.ID).First(&persisted).Error; err != nil {
			return err
		}
		if persisted.Status != fromStatus {
			return nil
		}

		// Status pollers own provider metadata, while the locked row owns the
		// funding/token ledger. A poller can hold a snapshot loaded before task
		// settlement; never let that snapshot restore an older reservation.
		t.Quota = persisted.Quota
		t.PrivateData.BillingReservation = persisted.PrivateData.BillingReservation

		result := tx.Model(&Task{}).
			Where("id = ? AND status = ?", t.ID, fromStatus).
			Select("*").
			Updates(t)
		if result.Error != nil {
			return result.Error
		}
		updated = result.RowsAffected > 0
		return nil
	})
	return updated, err
}

// UpdateTimedOutWithStatus is the timeout-specific CAS. SubmitTime is the
// pending-submission lease generation, so a retry that renewed the marker after
// the sweep selected it makes this update lose instead of refunding a live
// upstream attempt.
func (t *Task) UpdateTimedOutWithStatus(fromStatus TaskStatus, expectedSubmitTime int64) (bool, error) {
	updated := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		var persisted Task
		if err := lockForUpdate(tx).Where("id = ?", t.ID).First(&persisted).Error; err != nil {
			return err
		}
		if persisted.Status != fromStatus || persisted.SubmitTime != expectedSubmitTime {
			return nil
		}

		t.Quota = persisted.Quota
		t.PrivateData.BillingReservation = persisted.PrivateData.BillingReservation

		result := tx.Model(&Task{}).
			Where("id = ? AND status = ? AND submit_time = ?", t.ID, fromStatus, expectedSubmitTime).
			Select("*").
			Updates(t)
		if result.Error != nil {
			return result.Error
		}
		updated = result.RowsAffected > 0
		return nil
	})
	return updated, err
}

// TaskBulkUpdate performs an unconditional bulk UPDATE by upstream task_id strings.
// Same caveats as TaskBulkUpdateByID — no CAS guard.
func TaskBulkUpdate(taskIds []string, params map[string]any) error {
	if len(taskIds) == 0 {
		return nil
	}
	return DB.Model(&Task{}).
		Where("task_id in (?)", taskIds).
		Updates(params).Error
}

// TaskBulkUpdateByID performs an unconditional bulk UPDATE by primary key IDs.
// WARNING: This function has NO CAS (Compare-And-Swap) guard — it will overwrite
// any concurrent status changes. DO NOT use in billing/quota lifecycle flows
// (e.g., timeout, success, failure transitions that trigger refunds or settlements).
// For status transitions that involve billing, use Task.UpdateWithStatus() instead.
func TaskBulkUpdateByID(ids []int64, params map[string]any) error {
	if len(ids) == 0 {
		return nil
	}
	return DB.Model(&Task{}).
		Where("id in (?)", ids).
		Updates(params).Error
}

type TaskQuotaUsage struct {
	Mode  string  `json:"mode"`
	Count float64 `json:"count"`
}

// TaskCountAllTasks returns total tasks that match the given query params (admin usage)
func TaskCountAllTasks(queryParams SyncTaskQueryParams) int64 {
	var total int64
	query := DB.Model(&Task{})
	if queryParams.ChannelID != "" {
		query = query.Where("channel_id = ?", queryParams.ChannelID)
	}
	if queryParams.Platform != "" {
		query = query.Where("platform = ?", queryParams.Platform)
	}
	if queryParams.UserID != "" {
		query = query.Where("user_id = ?", queryParams.UserID)
	}
	if len(queryParams.UserIDs) != 0 {
		query = query.Where("user_id in (?)", queryParams.UserIDs)
	}
	if queryParams.TaskID != "" {
		query = query.Where("task_id = ?", queryParams.TaskID)
	}
	if queryParams.Action != "" {
		query = query.Where("action = ?", queryParams.Action)
	}
	if queryParams.Status != "" {
		query = query.Where("status = ?", queryParams.Status)
	}
	if queryParams.StartTimestamp != 0 {
		query = query.Where("submit_time >= ?", queryParams.StartTimestamp)
	}
	if queryParams.EndTimestamp != 0 {
		query = query.Where("submit_time <= ?", queryParams.EndTimestamp)
	}
	_ = query.Count(&total).Error
	return total
}

// TaskCountAllUserTask returns total tasks for given user
func TaskCountAllUserTask(userId int, queryParams SyncTaskQueryParams) int64 {
	var total int64
	query := DB.Model(&Task{}).Where("user_id = ?", userId)
	if queryParams.TaskID != "" {
		query = query.Where("task_id = ?", queryParams.TaskID)
	}
	if queryParams.Action != "" {
		query = query.Where("action = ?", queryParams.Action)
	}
	if queryParams.Status != "" {
		query = query.Where("status = ?", queryParams.Status)
	}
	if queryParams.Platform != "" {
		query = query.Where("platform = ?", queryParams.Platform)
	}
	if queryParams.StartTimestamp != 0 {
		query = query.Where("submit_time >= ?", queryParams.StartTimestamp)
	}
	if queryParams.EndTimestamp != 0 {
		query = query.Where("submit_time <= ?", queryParams.EndTimestamp)
	}
	_ = query.Count(&total).Error
	return total
}
func (t *Task) ToOpenAIVideo() *dto.OpenAIVideo {
	openAIVideo := dto.NewOpenAIVideo()
	openAIVideo.ID = t.TaskID
	openAIVideo.Status = t.Status.ToVideoStatus()
	openAIVideo.Model = t.Properties.OriginModelName
	openAIVideo.SetProgressStr(t.Progress)
	openAIVideo.CreatedAt = t.CreatedAt
	openAIVideo.CompletedAt = t.UpdatedAt
	openAIVideo.SetMetadata("url", t.GetResultURL())
	return openAIVideo
}
